package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	orrBinary string
	repoRoot  string
)

func TestMain(m *testing.M) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "locate e2e test source")
		os.Exit(1)
	}
	repoRoot = filepath.Dir(filepath.Dir(source))

	buildDir, err := os.MkdirTemp("", "orr-e2e-build-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create e2e build directory:", err)
		os.Exit(1)
	}
	name := "orr"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	orrBinary = filepath.Join(buildDir, name)
	command := exec.Command("go", "build", "-o", orrBinary, "./cmd/orr")
	command.Dir = repoRoot
	if output, err := command.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build orr for e2e tests: %v\n%s", err, output)
		os.Exit(1)
	}

	code := m.Run()
	if err := os.RemoveAll(buildDir); err != nil {
		fmt.Fprintln(os.Stderr, "remove e2e build directory:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// server is a real orr subprocess with an isolated config and user directory.
type server struct {
	baseURL       string
	providersPath string
	tempDir       string
	cancel        context.CancelFunc
	exited        chan struct{}
	waitErr       error
	stopOnce      sync.Once
	output        *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func startServer(t *testing.T, upstreamURL, providers string) *server {
	return startServerWithEnv(t, upstreamURL, providers, nil)
}

func startServerWithEnv(t *testing.T, upstreamURL, providers string, extraEnv map[string]string) *server {
	return startServerWithSetup(t, upstreamURL, providers, extraEnv, nil)
}

func startServerWithSetup(t *testing.T, upstreamURL, providers string, extraEnv map[string]string, setup func(string)) *server {
	t.Helper()
	tempDir := t.TempDir()
	if setup != nil {
		setup(tempDir)
	}
	return startServerInDirectory(t, tempDir, upstreamURL, &providers, extraEnv)
}

// restartServer stops a real orr process and starts another one over the same
// config and data directory. The providers and stats files are deliberately
// not rewritten, so lifecycle tests exercise the same persisted state a user
// gets after restarting the application.
func restartServer(t *testing.T, previous *server, upstreamURL string, extraEnv map[string]string) *server {
	t.Helper()
	previous.stop(t)
	return startServerInDirectory(t, previous.tempDir, upstreamURL, nil, extraEnv)
}

func startServerInDirectory(t *testing.T, tempDir, upstreamURL string, providers *string, extraEnv map[string]string) *server {
	t.Helper()
	providersPath := filepath.Join(tempDir, "providers.yaml")
	if providers != nil {
		contents := strings.ReplaceAll(*providers, "UPDATED_AT", time.Now().Format(time.RFC3339))
		if err := os.WriteFile(providersPath, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	} else if _, err := os.Stat(providersPath); err != nil {
		t.Fatalf("restart providers file: %v", err)
	}

	address := unusedAddress(t)
	envPath := filepath.Join(tempDir, ".env")
	env := fmt.Sprintf("ORR_LISTEN=%s\nORR_UPSTREAM=%s\nORR_PROVIDERS_FILE=%s\nORR_LOG_REQUESTS=true\nORR_TUI=false\n", address, upstreamURL, providersPath)
	for name, value := range extraEnv {
		env += name + "=" + value + "\n"
	}
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, orrBinary, "serve", "--env", envPath)
	command.Dir = tempDir
	command.Env = isolatedEnvironment(tempDir)
	output := &lockedBuffer{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		cancel()
		t.Fatalf("start orr: %v", err)
	}
	exited := make(chan struct{})

	s := &server{
		baseURL:       "http://" + address,
		providersPath: providersPath,
		tempDir:       tempDir,
		cancel:        cancel,
		exited:        exited,
		output:        output,
	}
	go func() {
		s.waitErr = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		s.stop(t)
	})
	s.waitUntilHealthy(t)
	return s
}

func (s *server) stop(t *testing.T) {
	t.Helper()
	s.stopOnce.Do(s.cancel)
	select {
	case <-s.exited:
	case <-time.After(5 * time.Second):
		t.Errorf("orr did not stop after cancellation")
	}
}

func (s *server) waitUntilHealthy(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.exited:
			t.Fatalf("orr exited before becoming healthy: %v\n%s", s.waitErr, s.output.String())
		default:
		}
		response, err := client.Get(s.baseURL + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("orr did not become healthy\n%s", s.output.String())
}

func unusedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func isolatedEnvironment(configDir string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "ORR_") || upper == "OPENROUTER_API_KEY" || upper == "APPDATA" || upper == "LOCALAPPDATA" || upper == "XDG_CONFIG_HOME" || upper == "XDG_DATA_HOME" || upper == "HOME" {
			continue
		}
		env = append(env, item)
	}
	return append(env,
		"APPDATA="+configDir,
		"LOCALAPPDATA="+configDir,
		"XDG_CONFIG_HOME="+configDir,
		"XDG_DATA_HOME="+configDir,
		"HOME="+configDir,
	)
}
