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

	"github.com/mikael-titinovskii/orr/internal/store"
	"gopkg.in/yaml.v3"
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
	// On Windows a test server's process can exit a moment before the OS
	// releases its executable image, so one RemoveAll races the file system and
	// turns a passing run into a failure. Retry briefly instead.
	if err := removeAllWithRetry(buildDir, 20, 100*time.Millisecond); err != nil {
		fmt.Fprintln(os.Stderr, "remove e2e build directory:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func removeAllWithRetry(dir string, attempts int, delay time.Duration) error {
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return err
}

// server is a real orr subprocess with an isolated config and user directory.
type server struct {
	baseURL  string
	dbPath   string
	tempDir  string
	cancel   context.CancelFunc
	exited   chan struct{}
	waitErr  error
	stopOnce sync.Once
	output   *lockedBuffer
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
	// The database lives in the platform data directory the way a default install
	// does, so the startup import sees the same layout users get.
	dbPath := filepath.Join(tempDir, "orr", "orr.db")
	if providers != nil {
		seedModels(t, dbPath, *providers)
	} else if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("restart database: %v", err)
	}

	address := unusedAddress(t)
	envPath := filepath.Join(tempDir, ".env")
	env := fmt.Sprintf("ORR_LISTEN=%s\nORR_UPSTREAM=%s\nORR_DB_FILE=%s\nORR_LOG_REQUESTS=true\nORR_TUI=false\n", address, upstreamURL, dbPath)
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
		baseURL: "http://" + address,
		dbPath:  dbPath,
		tempDir: tempDir,
		cancel:  cancel,
		exited:  exited,
		output:  output,
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

// seedFile is the test fixture language the e2e suite uses to describe routing
// state. It mirrors the providers file orr used to keep, so fixtures stay
// readable; the harness writes the parsed entries into the database.
type seedFile struct {
	Version int                  `yaml:"version"`
	Models  map[string]seedModel `yaml:"models"`
}

type seedModel struct {
	Order             []string      `yaml:"order"`
	Only              []string      `yaml:"only"`
	Ignore            []string      `yaml:"ignore"`
	AllowFallbacks    *bool         `yaml:"allow_fallbacks"`
	RequireParameters *bool         `yaml:"require_parameters"`
	DataCollection    string        `yaml:"data_collection"`
	ZDR               *bool         `yaml:"zdr"`
	Sort              string        `yaml:"sort"`
	ManualPin         string        `yaml:"manual_pin"`
	UpdatedAt         time.Time     `yaml:"updated_at"`
	Blocked           []seedBlocked `yaml:"blocked"`
}

type seedBlocked struct {
	Provider   string    `yaml:"provider"`
	Reason     string    `yaml:"reason"`
	DetectedAt time.Time `yaml:"detected_at"`
}

const seedUpdatedAtPlaceholder = "UPDATED_AT"

// seedModels writes the fixture's models into the database at path.
func seedModels(t *testing.T, path, contents string) {
	t.Helper()
	contents = strings.ReplaceAll(contents, seedUpdatedAtPlaceholder, time.Now().Format(time.RFC3339))
	var parsed seedFile
	if err := yaml.Unmarshal([]byte(contents), &parsed); err != nil {
		t.Fatalf("parse seeded models: %v", err)
	}
	models := make(map[string]store.Model, len(parsed.Models))
	for name, seed := range parsed.Models {
		entry := store.Model{
			Order:             seed.Order,
			Only:              seed.Only,
			Ignore:            seed.Ignore,
			AllowFallbacks:    seed.AllowFallbacks,
			RequireParameters: seed.RequireParameters,
			DataCollection:    seed.DataCollection,
			ZDR:               seed.ZDR,
			Sort:              seed.Sort,
			ManualPin:         seed.ManualPin,
			UpdatedAt:         seed.UpdatedAt,
		}
		for _, blocked := range seed.Blocked {
			entry.Blocked = append(entry.Blocked, store.Blocked{
				Provider:   blocked.Provider,
				Reason:     blocked.Reason,
				DetectedAt: blocked.DetectedAt,
			})
		}
		models[name] = entry
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open seeded database: %v", err)
	}
	defer db.Close()
	if err := db.SaveModels(models); err != nil {
		t.Fatalf("seed models: %v", err)
	}
}

// persistedModels reads the routing state another process is serving. The
// database is opened read-only, so the server's own writes are never blocked by
// the assertion.
func persistedModels(t *testing.T, path string) map[string]store.Model {
	t.Helper()
	db, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open persisted database: %v", err)
	}
	defer db.Close()
	models, err := db.Models()
	if err != nil {
		t.Fatalf("read persisted models: %v", err)
	}
	return models
}

// waitForFreshModels polls until every named model has a refreshed catalog and
// a non-empty provider order.
func waitForFreshModels(t *testing.T, s *server, names ...string) map[string]store.Model {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		models := persistedModels(t, s.dbPath)
		fresh := true
		for _, name := range names {
			entry, found := models[name]
			if !found || len(entry.Order) == 0 || strings.HasPrefix(entry.UpdatedAt.Format(time.DateOnly), "2000-01-01") {
				fresh = false
				break
			}
		}
		if fresh {
			return models
		}
		if time.Now().After(deadline) {
			t.Fatalf("models %v were not refreshed: %+v", names, models)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForPersistedOrder polls until some model's persisted order contains the
// providers in the given sequence and its catalog has been refreshed away from
// the fixture's placeholder date.
func waitForPersistedOrder(t *testing.T, s *server, providers ...string) store.Model {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last map[string]store.Model
	for {
		last = persistedModels(t, s.dbPath)
		for _, model := range last {
			if !strings.HasPrefix(model.UpdatedAt.Format(time.DateOnly), "2000-01-01") && orderContains(model.Order, providers) {
				return model
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider order did not become %v: %+v", providers, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func orderContains(order, providers []string) bool {
	position := 0
	for _, provider := range order {
		if position < len(providers) && provider == providers[position] {
			position++
		}
	}
	return position == len(providers)
}

// containsAll reports whether order holds every one of wanted.
func containsAll(order []string, wanted ...string) bool {
	for _, want := range wanted {
		found := false
		for _, provider := range order {
			if provider == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
