package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The sidecar assets travel inside the orr binary: install writes them to the
// per-user data directory, and an upgrade therefore ships a matching policy and
// runner without a second download.
var (
	//go:embed guardrails_assets/config.yml
	guardrailsDefaultConfig []byte
	//go:embed guardrails_assets/requirements.txt
	guardrailsRequirements []byte
	//go:embed guardrails_assets/serve.py
	guardrailsServeScript []byte
)

const (
	guardrailsModeOff = "off"
	guardrailsModeOn  = "on"

	guardrailsActionOff     = "off"
	guardrailsActionObserve = "observe"
	guardrailsActionBlock   = "block"

	defaultGuardrailsPort   = 8765
	guardrailsConfigID      = "orr"
	guardrailsHealthTimeout = 45 * time.Second
	guardrailsStopGrace     = 5 * time.Second
	guardrailsMaxRestarts   = 5
	guardrailsStableUptime  = time.Minute
)

// guardrailsHealthWait is a variable so tests do not sleep out a full startup
// window when they exercise the unhealthy path.
var guardrailsHealthWait = guardrailsHealthTimeout

// guardrailsConfig is the required NeMo Guardrails sidecar. serve installs it
// on first use and refuses to start without it; off is a development escape
// hatch, logged as unsupported. URL points at a sidecar somebody else runs, in
// which case orr supervises nothing but still requires it to be healthy.
// Action governs traffic: off skips checks, observe records verdicts without
// changing the response, block refuses a request the sidecar rejects.
type guardrailsConfig struct {
	Mode   string
	Action string
	URL    string
	Port   int
	Home   string
	Listen string
}

func defaultGuardrailsConfig() guardrailsConfig {
	return guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, Port: defaultGuardrailsPort}
}

// parseGuardrailsConfig reads the guardrails settings shared by serve and the
// guardrails subcommands, so `orr guardrails status` reports what serve will do.
func parseGuardrailsConfig(value func(string) string) (guardrailsConfig, error) {
	cfg := defaultGuardrailsConfig()
	if raw := strings.TrimSpace(value("ORR_GUARDRAILS")); raw != "" {
		mode := strings.ToLower(raw)
		switch mode {
		case guardrailsModeOff, guardrailsModeOn:
			cfg.Mode = mode
		default:
			return guardrailsConfig{}, fmt.Errorf("parse ORR_GUARDRAILS: must be %s or %s", guardrailsModeOff, guardrailsModeOn)
		}
	}
	if raw := strings.TrimSpace(value("ORR_GUARDRAILS_ACTION")); raw != "" {
		action := strings.ToLower(raw)
		switch action {
		case guardrailsActionOff, guardrailsActionObserve, guardrailsActionBlock:
			cfg.Action = action
		default:
			return guardrailsConfig{}, fmt.Errorf("parse ORR_GUARDRAILS_ACTION: must be %s, %s, or %s", guardrailsActionOff, guardrailsActionObserve, guardrailsActionBlock)
		}
	}
	if raw := strings.TrimSpace(value("ORR_GUARDRAILS_PORT")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return guardrailsConfig{}, fmt.Errorf("parse ORR_GUARDRAILS_PORT: must be a port between 1 and 65535")
		}
		cfg.Port = port
	}
	cfg.URL = strings.TrimRight(strings.TrimSpace(value("ORR_GUARDRAILS_URL")), "/")
	if cfg.URL != "" {
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return guardrailsConfig{}, fmt.Errorf("ORR_GUARDRAILS_URL must be a valid http(s) URL")
		}
	}
	cfg.Home = strings.TrimSpace(value("ORR_GUARDRAILS_HOME"))
	cfg.Listen = strings.TrimSpace(value("ORR_LISTEN"))
	return cfg, nil
}

// validate is deliberately lenient about zero values: serve always parses this
// through parseGuardrailsConfig, and a zero value means the defaults.
func (g guardrailsConfig) validate() error {
	mode := g.Mode
	if mode == "" {
		mode = guardrailsModeOn
	}
	switch mode {
	case guardrailsModeOff, guardrailsModeOn:
	default:
		return fmt.Errorf("ORR_GUARDRAILS must be %s or %s", guardrailsModeOff, guardrailsModeOn)
	}
	action := g.Action
	if action == "" {
		action = guardrailsActionBlock
	}
	switch action {
	case guardrailsActionOff, guardrailsActionObserve, guardrailsActionBlock:
	default:
		return fmt.Errorf("ORR_GUARDRAILS_ACTION must be %s, %s, or %s", guardrailsActionOff, guardrailsActionObserve, guardrailsActionBlock)
	}
	port := g.Port
	if port == 0 {
		port = defaultGuardrailsPort
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("ORR_GUARDRAILS_PORT must be a port between 1 and 65535")
	}
	if g.URL != "" {
		u, err := url.Parse(g.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("ORR_GUARDRAILS_URL must be a valid http(s) URL")
		}
	}
	return nil
}

// dir is where the venv, policy, and runner live. ORR_GUARDRAILS_HOME moves the
// whole tree, which keeps it out of the database's data directory when desired.
func (g guardrailsConfig) dir() (string, error) {
	if g.Home != "" {
		return filepath.Abs(g.Home)
	}
	base, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "guardrails"), nil
}

func (g guardrailsConfig) url() string {
	if g.URL != "" {
		return g.URL
	}
	return fmt.Sprintf("http://127.0.0.1:%d", g.Port)
}

func guardrailsPolicyPath(dir string) string {
	return filepath.Join(dir, "configs", guardrailsConfigID, "config.yml")
}

// guardrailsVenvUsable reports whether the venv is actually a venv, not just a
// directory containing python.exe. A failed reinstall can leave the launcher
// in place with pyvenv.cfg gone, and that needs a repair, not a skip.
func guardrailsVenvUsable(dir string) bool {
	return fileExists(guardrailsVenvPython(dir)) && fileExists(filepath.Join(dir, "venv", "pyvenv.cfg"))
}

func guardrailsVenvPython(dir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "venv", "Scripts", "python.exe")
	}
	return filepath.Join(dir, "venv", "bin", "python")
}

// loadGuardrailsConfigWithOptions resolves only the guardrails settings, so the
// install and status commands do not need a valid OpenRouter key or a database.
func loadGuardrailsConfigWithOptions(envPath string, preferEnvFile bool) (guardrailsConfig, error) {
	fileEnv, err := readDotEnv(envPath)
	if err != nil {
		return guardrailsConfig{}, err
	}
	return parseGuardrailsConfig(func(name string) string { return envValue(fileEnv, name, preferEnvFile) })
}

type guardrailsInstallOptions struct {
	Python string
	Force  bool
	Output io.Writer
}

// The indirections keep the auto-install path testable without creating a real
// Python environment in the test process.
var (
	guardrailsFindPython = findGuardrailsPython
	guardrailsExec       = runGuardrailsCommand
)

func runGuardrailsInstall(envPath string, preferEnvFile bool, opts guardrailsInstallOptions) error {
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	settings, err := loadGuardrailsConfigWithOptions(envPath, preferEnvFile)
	if err != nil {
		return err
	}
	dir, err := settings.dir()
	if err != nil {
		return err
	}
	if guardrailsVenvUsable(dir) && !opts.Force {
		// Idempotent by design: the installers and `orr upgrade` run this on
		// every existing installation, so an up-to-date one must not fail.
		if err := ensureGuardrails(settings, opts.Output); err != nil {
			return err
		}
		fmt.Fprintf(opts.Output, "Guardrails are already installed in %s\n", dir)
		return nil
	}
	return installGuardrails(settings, opts.Python, opts.Force, opts.Output)
}

// installGuardrails creates the venv and installs the pinned dependencies.
func installGuardrails(settings guardrailsConfig, pythonOverride string, force bool, output io.Writer) error {
	dir, err := settings.dir()
	if err != nil {
		return err
	}
	repair := force || !guardrailsVenvUsable(dir)
	python, version, err := guardrailsFindPython(pythonOverride)
	if err != nil {
		return fmt.Errorf("find Python: %w", err)
	}
	fmt.Fprintf(output, "Using Python %s at %s\n", version, python)
	if err := writeGuardrailsAssets(dir, force, writeGuardrailsNotice(output)); err != nil {
		return fmt.Errorf("write guardrails files: %w", err)
	}
	if force {
		if err := os.RemoveAll(filepath.Join(dir, "venv")); err != nil {
			return fmt.Errorf("remove existing environment: %w", err)
		}
	}
	fmt.Fprintln(output, "Creating the Python environment...")
	if err := guardrailsExec(output, python, "-m", "venv", filepath.Join(dir, "venv")); err != nil {
		return fmt.Errorf("create the Python environment: %w (on Debian/Ubuntu install python3-venv)", err)
	}
	if err := installGuardrailsRequirements(settings, repair, output); err != nil {
		return err
	}
	fmt.Fprintf(output, "Guardrails installed in %s\n", dir)
	return nil
}

// ensureGuardrails makes an existing installation current: assets are refreshed
// and changed pinned requirements are installed. serve and `orr upgrade` call
// it so neither needs a separate operator step.
func ensureGuardrails(settings guardrailsConfig, output io.Writer) error {
	dir, err := settings.dir()
	if err != nil {
		return err
	}
	if !guardrailsVenvUsable(dir) {
		fmt.Fprintln(output, "Guardrails are required and not installed yet; installing...")
		_ = os.Remove(filepath.Join(dir, "requirements.stamp"))
		return installGuardrails(settings, "", false, output)
	}
	if err := writeGuardrailsAssets(dir, false, writeGuardrailsNotice(output)); err != nil {
		return fmt.Errorf("refresh guardrails files: %w", err)
	}
	if err := installGuardrailsRequirements(settings, false, output); err != nil {
		return err
	}
	return nil
}

// guardrailsVenvUsable reports whether the venv is actually a venv, not just a
// model cannot be fetched. A token from `huggingface-cli login` counts too.

func installGuardrailsRequirements(settings guardrailsConfig, forceReinstall bool, output io.Writer) error {
	dir, err := settings.dir()
	if err != nil {
		return err
	}
	stampPath := filepath.Join(dir, "requirements.stamp")
	want := requirementsStamp()
	if !forceReinstall {
		if existing, err := os.ReadFile(stampPath); err == nil && strings.TrimSpace(string(existing)) == want {
			return nil
		}
	}
	pythonPath := guardrailsVenvPython(dir)
	fmt.Fprintln(output, "Installing guardrail dependencies...")
	// The venv's own pip installs the pinned set. Upgrading it first would fetch
	// whatever pip is newest on every install, adding an unpinned download to a
	// step that runs automatically and runs again on `orr upgrade`.
	args := []string{"-m", "pip", "install", "--disable-pip-version-check", "--no-input"}
	if forceReinstall {
		args = append(args, "--force-reinstall")
	}
	args = append(args, "-r", filepath.Join(dir, "requirements.txt"))
	if err := guardrailsExec(output, pythonPath, args...); err != nil {
		return fmt.Errorf("install guardrail dependencies: %w", err)
	}
	return os.WriteFile(stampPath, []byte(want+"\n"), 0o644)
}

func requirementsStamp() string {
	sum := sha256.Sum256(guardrailsRequirements)
	return hex.EncodeToString(sum[:])
}

func runGuardrailsCommand(output io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = output
	cmd.Stderr = output
	return cmd.Run()
}

// findGuardrailsPython locates an interpreter NeMo Guardrails supports. The
// version window is the package's own: 3.11 through 3.13, exclusive of 3.14.
func findGuardrailsPython(explicit string) (string, string, error) {
	candidates := []string{explicit}
	if explicit == "" {
		if runtime.GOOS == "windows" {
			candidates = []string{"python3", "python", "py"}
		} else {
			candidates = []string{"python3.12", "python3.13", "python3.11", "python3", "python"}
		}
	}
	var problems []string
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		version, err := pythonVersion(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if supportedPythonVersion(version) {
			return path, version, nil
		}
		problems = append(problems, fmt.Sprintf("%s is Python %s; NeMo Guardrails requires 3.11 through 3.13", path, version))
	}
	if len(problems) == 0 {
		return "", "", errors.New("no Python interpreter found; install Python 3.11 through 3.13 or pass --python")
	}
	return "", "", fmt.Errorf("no supported Python interpreter found: %s", strings.Join(problems, "; "))
}

func pythonVersion(path string) (string, error) {
	out, err := exec.Command(path, "-c", "import sys; print('%d.%d.%d' % sys.version_info[:3])").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func supportedPythonVersion(version string) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return major == 3 && minor >= 11 && minor <= 13
}

// writeGuardrailsAssets installs the policy, requirements, and runner. The
// policy is the operator's to edit, so an edited one survives an update; an
// untouched one follows the shipped default, and a changed default is left
// beside an edited policy as config.yml.new.
func writeGuardrailsAssets(dir string, force bool, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	policy := guardrailsPolicyPath(dir)
	if err := os.MkdirAll(filepath.Dir(policy), 0o755); err != nil {
		return err
	}
	shipped := guardrailsDefaultConfig
	stampPath := guardrailsPolicyStampPath(dir)
	defaultHash := guardrailsHash(shipped)
	installedHash, _ := os.ReadFile(stampPath)
	installed := strings.TrimSpace(string(installedHash))
	current, currentErr := os.ReadFile(policy)
	switch {
	case force || errors.Is(currentErr, os.ErrNotExist):
		if err := os.WriteFile(policy, shipped, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(stampPath, []byte(defaultHash+"\n"), 0o644); err != nil {
			return err
		}
	case currentErr != nil:
		return currentErr
	default:
		if installed == "" {
			// An installation from before the stamp existed: adopt the file as
			// untouched when it matches the shipped default, so a formatting
			// difference is not mistaken for an operator edit.
			matches := guardrailsPolicyContent(current) == guardrailsPolicyContent(guardrailsDefaultConfig)
			if matches {
				// The current hash, so the untouched branch below rewrites the file
				// to the rendered default and the formatting stops mattering.
				installed = guardrailsHash(current)
			} else {
				installed = "custom"
			}
			if err := os.WriteFile(stampPath, []byte(installed+"\n"), 0o644); err != nil {
				return err
			}
		}
		if guardrailsHash(current) == installed {
			// Untouched, so a newer shipped default may replace it.
			if installed != defaultHash {
				if err := os.WriteFile(policy, shipped, 0o644); err != nil {
					return err
				}
				if err := os.WriteFile(stampPath, []byte(defaultHash+"\n"), 0o644); err != nil {
					return err
				}
				logf("policy updated to the shipped default")
			}
		} else if guardrailsHash(current) == defaultHash {
			// The operator adopted the shipped default by copying it over an edited
			// policy. Re-key the stamp, or the file is treated as edited forever and
			// the adopted default stops following later shipped ones.
			if err := os.WriteFile(stampPath, []byte(defaultHash+"\n"), 0o644); err != nil {
				return err
			}
		} else if installed != defaultHash {
			// Edited and the shipped default moved: keep the edit and leave the
			// new default beside it, rather than discarding either.
			updated := policy + ".new"
			if err := os.WriteFile(updated, shipped, 0o644); err != nil {
				return err
			}
			logf("the shipped policy changed; review %s (the edited policy was kept)", updated)
		}
	}
	if err := writeGuardrailsFile(filepath.Join(dir, "requirements.txt"), guardrailsRequirements); err != nil {
		return err
	}
	return writeGuardrailsFile(filepath.Join(dir, "serve.py"), guardrailsServeScript)
}

func guardrailsPolicyStampPath(dir string) string {
	return filepath.Join(dir, "policy.stamp")
}

// guardrailsPolicyContent drops comments and blank lines so a documentation
// change in the shipped default is not mistaken for an operator edit when an
// older installation is adopted.
func guardrailsPolicyContent(data []byte) string {
	var content strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		content.WriteString(line)
		content.WriteByte('\n')
	}
	return content.String()
}

func guardrailsHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func guardrailsPolicyState(dir string) string {
	current, err := os.ReadFile(guardrailsPolicyPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return "missing"
	}
	if err != nil {
		return "unreadable"
	}
	stamp, _ := os.ReadFile(guardrailsPolicyStampPath(dir))
	installed := strings.TrimSpace(string(stamp))
	shipped := guardrailsDefaultConfig
	defaultHash := guardrailsHash(shipped)
	if installed == "" {
		if guardrailsPolicyContent(current) == guardrailsPolicyContent(shipped) {
			return "default"
		}
		return "edited"
	}
	if guardrailsHash(current) != installed {
		// Formatting-only drift from an older render is not an edit.
		if guardrailsPolicyContent(current) == guardrailsPolicyContent(shipped) {
			return "default"
		}
		return "edited"
	}
	if installed != defaultHash {
		return "default, update available"
	}
	return "default"
}

func guardrailsDependenciesState(dir string) string {
	stamp, err := os.ReadFile(filepath.Join(dir, "requirements.stamp"))
	if err != nil {
		return "pending"
	}
	if strings.TrimSpace(string(stamp)) != requirementsStamp() {
		return "pending"
	}
	return "current"
}

func writeGuardrailsFile(path string, content []byte) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(content) {
		return nil
	}
	return os.WriteFile(path, content, 0o644)
}

func runGuardrailsStatus(settings guardrailsConfig, output io.Writer) error {
	dir, err := settings.dir()
	if err != nil {
		return err
	}
	pythonPath := guardrailsVenvPython(dir)
	installed := fileExists(pythonPath)
	fmt.Fprintf(output, "directory: %s\n", dir)
	if installed {
		version, versionErr := pythonVersion(pythonPath)
		if versionErr != nil {
			fmt.Fprintf(output, "installed: yes (could not read the Python version: %v)\n", versionErr)
		} else {
			fmt.Fprintf(output, "installed: yes (Python %s)\n", version)
		}
	} else {
		fmt.Fprintln(output, "installed: no")
	}
	mode := settings.Mode
	if mode == "" {
		mode = guardrailsModeOn
	}
	fmt.Fprintf(output, "mode: %s\n", mode)
	action := settings.Action
	if action == "" {
		action = guardrailsActionBlock
	}
	fmt.Fprintf(output, "action: %s\n", action)
	if settings.URL != "" {
		fmt.Fprintf(output, "endpoint: %s (external)\n", settings.URL)
	} else {
		fmt.Fprintf(output, "endpoint: %s\n", settings.url())
	}
	policy := guardrailsPolicyPath(dir)
	fmt.Fprintf(output, "policy: %s (%s)\n", policy, guardrailsPolicyState(dir))
	fmt.Fprintf(output, "dependencies: %s\n", guardrailsDependenciesState(dir))
	verdicts, verdictErr := fetchGuardrailVerdicts(settings)
	printGuardrailVerdicts(output, verdicts, verdictErr)
	return nil
}

// writeGuardrailsNotice adapts an io.Writer to the notice callback the asset
// writer uses, so install output and serve logs report the same policy events.
func writeGuardrailsNotice(output io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(output, format+"\n", args...)
	}
}

func runGuardrailsConfig(settings guardrailsConfig, output io.Writer) error {
	dir, err := settings.dir()
	if err != nil {
		return err
	}
	policy := guardrailsPolicyPath(dir)
	content, err := os.ReadFile(policy)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(output, "# %s does not exist; showing the default shipped with orr\n", policy)
		content = guardrailsDefaultConfig
	} else if err != nil {
		return err
	}
	fmt.Fprintf(output, "# %s\n", policy)
	_, err = output.Write(content)
	return err
}

// guardrailsSidecar owns the child process. The caller starts it once and calls
// Stop; the supervisor inside restarts an unexpected exit with backoff.
type guardrailsSidecar struct {
	settings guardrailsConfig
	dir      string
	python   string
	logf     func(string, ...any)

	mu     sync.Mutex
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
}

// startGuardrailsSidecar returns (nil, nil) only when guardrails are explicitly
// disabled. Every other path treats them as a hard requirement: an external
// endpoint must answer its health check, and a local installation is created if
// missing, then started and verified.
func startGuardrailsSidecar(parent context.Context, settings guardrailsConfig, logf func(string, ...any)) (*guardrailsSidecar, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	mode := settings.Mode
	if mode == "" {
		mode = guardrailsModeOn
	}
	if mode == guardrailsModeOff {
		logf("guardrails: disabled by ORR_GUARDRAILS=off; this is not a supported production configuration")
		return nil, nil
	}
	if settings.URL != "" {
		if err := waitForGuardrails(parent, settings.URL, guardrailsHealthWait); err != nil {
			return nil, fmt.Errorf("guardrails endpoint %s is not healthy: %w", settings.URL, err)
		}
		logf("guardrails: using external endpoint %s", settings.URL)
		return nil, nil
	}
	dir, err := settings.dir()
	if err != nil {
		return nil, fmt.Errorf("locate guardrails: %w", err)
	}
	if err := ensureGuardrails(settings, guardrailsLogWriter{logf: logf}); err != nil {
		return nil, fmt.Errorf("guardrails are required but could not be installed: %w", err)
	}
	python := guardrailsVenvPython(dir)
	if !fileExists(python) {
		return nil, fmt.Errorf("guardrails installation in %s is incomplete: %s is missing; run: orr guardrails install --force", dir, python)
	}
	if !fileExists(filepath.Join(dir, "serve.py")) {
		return nil, fmt.Errorf("guardrails installation in %s is incomplete: serve.py is missing; run: orr guardrails install --force", dir)
	}

	runCtx, cancel := context.WithCancel(parent)
	sidecar := &guardrailsSidecar{
		settings: settings,
		dir:      dir,
		python:   python,
		logf:     logf,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	logf("guardrails: starting sidecar on %s", settings.url())
	cmd, err := sidecar.spawn(runCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start guardrails sidecar: %w", err)
	}
	if err := waitForGuardrails(runCtx, settings.url(), guardrailsHealthWait); err != nil {
		terminateGuardrailsChild(cmd)
		cancel()
		return nil, fmt.Errorf("guardrails sidecar did not become healthy: %w", err)
	}
	logf("guardrails: sidecar ready at %s", settings.url())
	go sidecar.supervise(runCtx, cmd)
	return sidecar, nil
}

func (s *guardrailsSidecar) spawn(ctx context.Context) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, s.python, filepath.Join(s.dir, "serve.py"))
	cmd.Dir = s.dir
	sidecarEnv := append(guardrailsChildEnv(),
		"ORR_GUARDRAILS_CONFIG="+filepath.Join(s.dir, "configs", guardrailsConfigID),
		"ORR_GUARDRAILS_CONFIG_ID="+guardrailsConfigID,
		"ORR_GUARDRAILS_PORT="+strconv.Itoa(s.settings.Port),
		"NEMO_GUARDRAILS_IORAILS_ENGINE=1",
		"NEMO_GUARDRAILS_DISABLE_CHAT_UI=true",
		"PYTHONUNBUFFERED=1",
	)
	cmd.Env = sidecarEnv
	cmd.Stdout = io.Discard
	cmd.Stderr = guardrailsLogWriter{logf: s.logf}
	configureGuardrailsChild(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()
	return cmd, nil
}

// guardrailsChildEnv is the environment the sidecar is allowed to see. The
// policy is model-free and needs no credentials, so orr's own secrets — the
// OpenRouter key above all — are not handed to a process whose dependency tree
// is not the one this binary was built from.
func guardrailsChildEnv() []string {
	keep := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
		"LANG": true, "LC_ALL": true, "TMPDIR": true, "TZ": true,
		"SYSTEMROOT": true, "WINDIR": true, "COMSPEC": true, "PATHEXT": true,
		"TEMP": true, "TMP": true, "USERPROFILE": true, "LOCALAPPDATA": true,
		"APPDATA": true, "PROGRAMDATA": true, "PROGRAMFILES": true,
	}
	env := make([]string, 0, len(keep))
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !keep[strings.ToUpper(name)] {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func (s *guardrailsSidecar) supervise(ctx context.Context, cmd *exec.Cmd) {
	defer close(s.done)
	backoff := time.Second
	failures := 0
	started := time.Now()
	for {
		err := cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		ran := time.Since(started)
		started = time.Now()
		// A child that served for a while before exiting starts the count again:
		// the budget bounds consecutive crashes, not the process lifetime. A child
		// that dies immediately keeps counting — including one that never owned the
		// port, since a leftover listener can answer the health probe — so a crash
		// loop still trips the limit instead of respawning forever.
		if ran >= guardrailsStableUptime {
			failures, backoff = 0, time.Second
		}
		failures++
		if failures > guardrailsMaxRestarts {
			s.logf("guardrails: sidecar exited %d times (%v); leaving it stopped", failures, err)
			return
		}
		s.logf("guardrails: sidecar exited (%v); restarting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
		next, startErr := s.spawn(ctx)
		if startErr != nil {
			s.logf("guardrails: restart failed: %v", startErr)
			started = time.Now()
			continue
		}
		cmd = next
		if healthErr := waitForGuardrails(ctx, s.settings.url(), guardrailsHealthWait); healthErr != nil {
			s.logf("guardrails: restarted sidecar is not healthy: %v", healthErr)
			terminateGuardrailsChild(cmd)
			started = time.Now()
			continue
		}
		s.logf("guardrails: sidecar recovered at %s", s.settings.url())
		// The clock starts where the child starts serving: a restart that spent
		// its backoff and health wait failing served nothing, so it must not count
		// as uptime and reset the budget.
		started = time.Now()
	}
}

func (s *guardrailsSidecar) Stop() {
	if s == nil {
		return
	}
	s.cancel()
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil {
		terminateGuardrailsChild(cmd)
	}
	select {
	case <-s.done:
	case <-time.After(guardrailsStopGrace):
		if cmd != nil {
			killGuardrailsChild(cmd)
		}
		select {
		case <-s.done:
		case <-time.After(time.Second):
		}
	}
}

func waitForGuardrails(ctx context.Context, baseURL string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, requestErr := client.Do(req)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("health check returned %s", resp.Status)
		} else {
			last = requestErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("not healthy within %s: %w", timeout, last)
		case <-ticker.C:
		}
	}
}

type guardrailsLogWriter struct {
	logf func(string, ...any)
}

func (w guardrailsLogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			w.logf("guardrails: %s", line)
		}
	}
	return len(p), nil
}

type guardrailMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type guardrailVerdict struct {
	Status string `json:"status"`
	Rail   string `json:"rail"`
}

type guardrailCheckRequest struct {
	Model      string             `json:"model"`
	Messages   []guardrailMessage `json:"messages"`
	Tools      []map[string]any   `json:"tools,omitempty"`
	Guardrails struct {
		ConfigID  string   `json:"config_id"`
		RailTypes []string `json:"rail_types"`
	} `json:"guardrails"`
}

// guardrailToolDeclarations declares the canonical tool classes the tool_call
// rails judge. The sidecar must receive them: it validates calls against the
// declared toolset and denies every call when nothing is declared, so a check
// without these denies even an ordinary `ls`.
func guardrailToolDeclarations() []map[string]any {
	declaration := func(name, argument string) map[string]any {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "Canonical tool class orr rewrites a harness call onto.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{argument: map[string]any{"type": "string"}},
					"required":   []string{argument},
				},
			},
		}
	}
	return []map[string]any{
		declaration("run_command", "command"),
		declaration("read_file", "path"),
		declaration("write_file", "path"),
	}
}

// guardrailInput picks the model and the last user message from a request body.
// Scanning the whole history on every turn would make the check grow with the
// conversation and re-check text already delivered to the client.
func guardrailInput(body []byte) (string, []guardrailMessage, bool, bool) {
	model, _, lastUser, _, undecodable := guardrailConversation(body)
	if lastUser == nil {
		return model, nil, false, undecodable
	}
	return model, []guardrailMessage{*lastUser}, true, undecodable
}

// checkGuardrailsInput asks the sidecar about the last user message. In observe
// mode the verdict is recorded and the request continues; in block mode the
// caller answers with a refusal instead of forwarding.
func (p *proxy) checkGuardrailsInput(ctx context.Context, body []byte, record *requestRecord) {
	if p.cfg.Guardrails.Mode != guardrailsModeOn {
		return
	}
	action := p.cfg.Guardrails.Action
	if action == "" {
		action = guardrailsActionBlock
	}
	if action == guardrailsActionOff {
		return
	}
	model, messages, ok, undecodable := guardrailInput(body)
	if undecodable {
		log.Printf("guardrails: request could not be normalised for the input check")
		if action == guardrailsActionBlock {
			record.GuardPhase = "input"
			record.GuardRail = "check unavailable"
			record.GuardUnavailable = true
		}
		return
	}
	if !ok {
		return
	}
	verdict, err := p.guardrailCheck(ctx, model, messages, []string{"input"})
	if err != nil {
		log.Printf("guardrails: input check failed: %v", err)
		if action == guardrailsActionBlock {
			record.GuardPhase = "input"
			record.GuardRail = "check unavailable"
			record.GuardUnavailable = true
		}
		return
	}
	if verdict.Status != "blocked" {
		return
	}
	record.GuardPhase = "input"
	record.GuardRail = verdict.Rail
	if action == guardrailsActionBlock {
		record.GuardBlocked = true
	} else {
		record.GuardObserved = true
	}
}

// checkGuardrailsToolResults applies the tool-result rails to a request that
// carries tool output. Tool output is where a compromised MCP or tool returns
// instructions to the harness, so it is checked as an input too.
func (p *proxy) checkGuardrailsToolResults(ctx context.Context, body []byte, record *requestRecord) {
	if p.cfg.Guardrails.Mode != guardrailsModeOn {
		return
	}
	action := p.cfg.Guardrails.Action
	if action == "" {
		action = guardrailsActionBlock
	}
	if action == guardrailsActionOff {
		return
	}
	model, _, _, toolRun, undecodable := guardrailConversation(body)
	if undecodable {
		log.Printf("guardrails: request could not be normalised for the tool result check")
		if action == guardrailsActionBlock {
			record.GuardPhase = "tool_result"
			record.GuardRail = "check unavailable"
			record.GuardUnavailable = true
		}
		return
	}
	if len(toolRun) == 0 {
		return
	}
	verdict, err := p.guardrailCheck(ctx, model, toolRun, []string{"tool_result"})
	if err != nil {
		log.Printf("guardrails: tool result check failed: %v", err)
		if action == guardrailsActionBlock {
			record.GuardPhase = "tool_result"
			record.GuardRail = "check unavailable"
			record.GuardUnavailable = true
		}
		return
	}
	if verdict.Status != "blocked" {
		return
	}
	record.GuardPhase = "tool_result"
	record.GuardRail = verdict.Rail
	if action == guardrailsActionBlock {
		record.GuardBlocked = true
	} else {
		record.GuardObserved = true
	}
}

func (p *proxy) guardrailCheck(ctx context.Context, model string, messages []guardrailMessage, railTypes []string) (guardrailVerdict, error) {
	request := guardrailCheckRequest{Model: model, Messages: messages}
	if request.Model == "" {
		request.Model = guardrailsConfigID
	}
	request.Guardrails.ConfigID = guardrailsConfigID
	request.Guardrails.RailTypes = railTypes
	if slices.Contains(railTypes, "tool_call") {
		request.Tools = guardrailToolDeclarations()
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return guardrailVerdict{}, err
	}
	endpoint := p.cfg.Guardrails.url() + "/v1/checks"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return guardrailVerdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := p.guardrailsClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return guardrailVerdict{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return guardrailVerdict{}, fmt.Errorf("check returned %s", resp.Status)
	}
	var verdict guardrailVerdict
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&verdict); err != nil {
		return guardrailVerdict{}, err
	}
	return verdict, nil
}

// fetchGuardrailVerdicts reads the running instance's recent verdicts. An
// unreachable instance is reported, not fatal: status must still describe the
// installation itself.
func fetchGuardrailVerdicts(settings guardrailsConfig) ([]guardrailEvent, error) {
	base := settings.Listen
	if base == "" {
		base = "127.0.0.1:8787"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + base + "/api/v1/guardrails")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("management API returned %s", resp.Status)
	}
	var payload struct {
		Verdicts []guardrailEvent `json:"verdicts"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Verdicts, nil
}

func printGuardrailVerdicts(output io.Writer, events []guardrailEvent, err error) {
	if err != nil {
		fmt.Fprintf(output, "recent verdicts: (unavailable: %v)\n", err)
		return
	}
	if len(events) == 0 {
		fmt.Fprintln(output, "recent verdicts: none")
		return
	}
	fmt.Fprintln(output, "recent verdicts:")
	for _, event := range events {
		fmt.Fprintf(output, "  %s  %-12s %-28s %s / %s  %s\n",
			event.Time.Local().Format("15:04:05"), event.Phase, event.Rail, event.Model, event.Provider, event.Action)
	}
}
