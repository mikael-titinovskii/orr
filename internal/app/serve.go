package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
)

func serve(cfg config) error {
	p, err := newProxy(cfg)
	if err != nil {
		return err
	}
	path, err := statsFilePath()
	if err != nil {
		return fmt.Errorf("locate stats file: %w", err)
	}
	if err := p.stats.load(path); err != nil {
		return fmt.Errorf("load stats: %w", err)
	}
	p.routing.restorePins(p.stats.providerPinsSnapshot())

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	statsDone := make(chan struct{})
	go saveStatsPeriodically(ctx, p.stats, path, statsDone)
	go pollCredits(ctx, &http.Client{Timeout: 20 * time.Second}, cfg.Upstream, cfg.openRouterKey(), p.stats)
	go prefetchEndpoints(ctx, p)

	tuiEnabled := term.IsTerminal(int(os.Stdout.Fd())) && (cfg.TUI == nil || *cfg.TUI)
	if tuiEnabled {
		p.cfg.LogRequests = false
		p.trackEndpoints = true
	}
	serverErrors := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
		close(serverErrors)
	}()

	log.Printf("orr listening on http://%s/v1", cfg.Listen)
	log.Printf("forwarding to %s", strings.TrimRight(cfg.Upstream, "/"))
	if tuiEnabled {
		// The TUI owns the terminal now. Send log writes to a file instead of
		// stderr: a stray stderr write desyncs the alternate-screen cursor, so
		// later frames render at wrong offsets and the dashboard shows ghosted
		// rows with a misplaced selection highlight.
		logFile, logErr := openServeLog(path)
		if logErr != nil {
			log.SetOutput(io.Discard)
		} else {
			defer logFile.Close()
			log.SetOutput(logFile)
		}
		err = runDashboard(ctx, cfg, p, serverErrors)
	} else {
		select {
		case <-ctx.Done():
		case serveErr := <-serverErrors:
			err = serveErr
		}
	}

	cancel()
	p.stopDailyBenchmarks()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	<-statsDone
	p.routing.waitForRefreshes()
	p.waitForDailyBenchmarks()
	saveErr := p.stats.save(path)
	if err != nil {
		return err
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	return saveErr
}

func prefetchEndpoints(ctx context.Context, p *proxy) {
	for model := range p.routing.modelsSnapshot() {
		if ctx.Err() != nil {
			return
		}
		p.routing.refreshEndpoints(model, p.client, p.cfg.Upstream, p.cfg.openRouterKey())
	}
}

// openServeLog creates the log file the TUI mode writes to, next to the stats
// file. The standard logger is redirected there so log writes cannot corrupt
// the alternate-screen dashboard.
func openServeLog(statsPath string) (*os.File, error) {
	dir := filepath.Dir(statsPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.Create(filepath.Join(dir, "orr.log"))
}

func runDashboard(ctx context.Context, cfg config, p *proxy, serverErrors <-chan error) error {
	dashboard := newDashboard(cfg, p.stats, p.routing)
	dashboard.refresh = func(model string) {
		p.routing.forceRefreshModelOrder(model, p.client, p.cfg.Upstream, p.cfg.openRouterKey(), p.cfg.UpdateMaxProviders, p.cfg.UpdateCacheOnly)
	}
	dashboard.testProvider = func(model, provider string) (providerTestSample, error) {
		return p.testProvider(model, provider, providerTestRounds)
	}
	dashboard.testAllProviders = func(model string, providers []string) []providerTestResult {
		results, _ := p.benchmarkProviders(model, providers, time.Now().Add(manualBenchmarkGate), false, len(providers))
		return results
	}
	program := tea.NewProgram(dashboard, tea.WithAltScreen())
	go func() {
		select {
		case <-ctx.Done():
			program.Quit()
		case err, ok := <-serverErrors:
			if ok && err != nil {
				program.Send(serverErrorMsg{err: err})
			}
		}
	}()
	model, err := program.Run()
	if err != nil {
		return err
	}
	if dashboard, ok := model.(dashboardModel); ok && dashboard.serverErr != nil {
		return dashboard.serverErr
	}
	return nil
}
