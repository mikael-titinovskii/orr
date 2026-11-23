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
	"unicode/utf8"
)

// The sidecar assets travel inside the orr binary: install writes them to the
// per-user data directory, and an upgrade therefore ships a matching policy and
// runner without a second download.
var (
	//go:embed guardrails_assets/config.yml
	guardrailsDefaultConfig []byte
	//go:embed guardrails_assets/requirements.txt
	guardrailsRequirements []byte
	//go:embed guardrails_assets/requirements-classifier.txt
	guardrailsClassifierRequirements []byte
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

	// The gated classifier is installed only when a Hugging Face token can fetch
	// it. Revision pins the weights so an install and a later check agree even
	// after the repository is updated upstream.
	guardrailsClassifierRepo     = "meta-llama/Llama-Prompt-Guard-2-22M"
	guardrailsClassifierRevision = "11614a155199674a0a95e6602d6ab0417b790ed0"
	guardrailsClassifierName     = "prompt_guard"
	guardrailsClassifierLabel    = "Llama-Prompt-Guard-2-22M"
	guardrailsClassifierTorch    = "torch==2.9.1"
	// The PyPI torch wheel for Linux carries a CUDA runtime this sidecar never
	// uses. The CPU index publishes the same version for Linux, Windows and
	// macOS, so every install uses it.
	guardrailsTorchCPUIndex = "https://download.pytorch.org/whl/cpu"

	// guardrailModelTimeHeader carries Prompt Guard's own share of a check, in
	// milliseconds, measured inside the sidecar. Without it the proxy can time a
	// check but cannot tell the classifier from the rule rails and the round trip.
	guardrailModelTimeHeader = "X-Orr-Guard-Model-Ms"

	// guardrailLabelHeader carries the classifier's own verdict for the text it
	// judged, such as "MALICIOUS 0.973". A rail reports blocked or allowed and
	// drops the number behind it, and the number is what tells a false positive
	// from a true one.
	guardrailLabelHeader = "X-Orr-Guard-Label"
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
	Mode    string
	Action  string
	URL     string
	Port    int
	Home    string
	Listen  string
	HFToken string
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
	cfg.HFToken = strings.TrimSpace(value("HF_TOKEN"))
	if cfg.HFToken == "" {
		cfg.HFToken = strings.TrimSpace(value("HUGGING_FACE_HUB_TOKEN"))
	}
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

// guardrailsHFHome keeps the classifier inside the installation, so the model
// travels with the sidecar rather than the operator's global Hugging Face cache,
// and so removing the installation removes the weights.
func guardrailsHFHome(dir string) string {
	return filepath.Join(dir, "hf")
}

func guardrailsClassifierStampPath(dir string) string {
	return filepath.Join(dir, "hf", "model.stamp")
}

// guardrailsClassifierEnabled reports whether this installation carries Prompt
// Guard. It does when the pinned weights are already here, or when a token can
// fetch the gated repository — so a token in .env makes the classifier part of
// the install and `orr upgrade` adds it to an installation that predates it.
// Without a token the deterministic policy stays, which keeps an account with
// Meta a requirement of one rail rather than of starting orr.
func guardrailsClassifierEnabled(settings guardrailsConfig, dir string) bool {
	return guardrailsClassifierReady(dir) || guardrailsHFToken(settings) != ""
}

// guardrailsClassifierReady reports whether the pinned revision is on disk.
func guardrailsClassifierReady(dir string) bool {
	stamp, err := os.ReadFile(guardrailsClassifierStampPath(dir))
	if err != nil || strings.TrimSpace(string(stamp)) != guardrailsClassifierRevision {
		return false
	}
	_, err = os.Stat(filepath.Join(guardrailsHFHome(dir), "hub",
		"models--"+strings.ReplaceAll(guardrailsClassifierRepo, "/", "--")))
	return err == nil
}

// guardrailsHFToken resolves the token that unlocks the gated repository. A
// token from `huggingface-cli login` counts, so an operator who already has one
// does not need to repeat it in .env.
func guardrailsHFToken(settings guardrailsConfig) string {
	if token := strings.TrimSpace(settings.HFToken); token != "" {
		return token
	}
	for _, name := range []string{"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token
		}
	}
	for _, path := range guardrailsHFTokenPaths() {
		if data, err := os.ReadFile(path); err == nil {
			if token := strings.TrimSpace(string(data)); token != "" {
				return token
			}
		}
	}
	return ""
}

// guardrailsHFTokenPaths is a variable so tests do not depend on the machine's
// own Hugging Face login.
var guardrailsHFTokenPaths = guardrailsHFTokenFiles

func guardrailsHFTokenFiles() []string {
	var paths []string
	if hfHome := strings.TrimSpace(os.Getenv("HF_HOME")); hfHome != "" {
		paths = append(paths, filepath.Join(hfHome, "token"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".cache", "huggingface", "token"))
	}
	return paths
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
	guardrailsFindPython    = findGuardrailsPython
	guardrailsExec          = runGuardrailsCommand
	guardrailsDownloadModel = downloadGuardrailsClassifier
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
	return installGuardrails(settings, opts.Python, opts.Force, false, opts.Output)
}

// installGuardrails creates the venv and installs the pinned dependencies.
func installGuardrails(settings guardrailsConfig, pythonOverride string, force, bestEffort bool, output io.Writer) error {
	dir, err := settings.dir()
	if err != nil {
		return err
	}
	repair := force || !guardrailsVenvUsable(dir)
	withClassifier := guardrailsClassifierEnabled(settings, dir)
	python, version, err := guardrailsFindPython(pythonOverride)
	if err != nil {
		return fmt.Errorf("find Python: %w", err)
	}
	fmt.Fprintf(output, "Using Python %s at %s\n", version, python)
	if err := writeGuardrailsAssets(dir, force, withClassifier, writeGuardrailsNotice(output)); err != nil {
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
	if withClassifier {
		if err := installGuardrailsClassifier(settings, dir, repair, output); err != nil {
			if !bestEffort {
				return err
			}
			if fallbackErr := dropGuardrailsClassifier(settings, dir, output, err); fallbackErr != nil {
				return fallbackErr
			}
		}
	} else {
		fmt.Fprintln(output, "No HF_TOKEN: skipping the Prompt Guard classifier and keeping the rule-based policy")
	}
	fmt.Fprintf(output, "Guardrails installed in %s\n", dir)
	return nil
}

// dropGuardrailsClassifier keeps a token that cannot fetch the model from
// becoming a startup requirement: the rails are re-rendered out of the policy and
// the run continues on the deterministic rails. The token is never printed.
func dropGuardrailsClassifier(settings guardrailsConfig, dir string, output io.Writer, cause error) error {
	fmt.Fprintf(output, "WARNING: %s is unavailable, continuing with the rule-based policy: %v\n", guardrailsClassifierLabel, cause)
	_ = os.Remove(filepath.Join(dir, "classifier.stamp"))
	return writeGuardrailsAssets(dir, false, false, writeGuardrailsNotice(output))
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
		return installGuardrails(settings, "", false, true, output)
	}
	withClassifier := guardrailsClassifierEnabled(settings, dir)
	if err := writeGuardrailsAssets(dir, false, withClassifier, writeGuardrailsNotice(output)); err != nil {
		return fmt.Errorf("refresh guardrails files: %w", err)
	}
	if err := installGuardrailsRequirements(settings, false, output); err != nil {
		return err
	}
	if withClassifier {
		if err := installGuardrailsClassifier(settings, dir, false, output); err != nil {
			return dropGuardrailsClassifier(settings, dir, output, err)
		}
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
	withClassifier := guardrailsClassifierEnabled(settings, dir)
	stampPath := filepath.Join(dir, "requirements.stamp")
	want := requirementsStamp(withClassifier)
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

// installGuardrailsClassifier installs torch and transformers, then downloads
// the pinned weights. It is idempotent: a matching stamp and a present model
// cost one file read, which matters because serve and `orr upgrade` call it.
// forceReinstall skips that shortcut because the stamp describes a venv that may
// no longer exist: a rebuild leaves the weights on disk and the packages gone.
func installGuardrailsClassifier(settings guardrailsConfig, dir string, forceReinstall bool, output io.Writer) error {
	want := classifierStamp()
	stampPath := filepath.Join(dir, "classifier.stamp")
	if !forceReinstall {
		if existing, err := os.ReadFile(stampPath); err == nil &&
			strings.TrimSpace(string(existing)) == want &&
			guardrailsClassifierReady(dir) {
			return nil
		}
	}
	if err := os.MkdirAll(guardrailsHFHome(dir), 0o755); err != nil {
		return fmt.Errorf("create classifier directory: %w", err)
	}
	pythonPath := guardrailsVenvPython(dir)
	fmt.Fprintf(output, "Installing %s dependencies...\n", guardrailsClassifierLabel)
	torchArgs := []string{"-m", "pip", "install", "--disable-pip-version-check", "--no-input", guardrailsClassifierTorch}
	// The CPU index is used on every platform, not only Linux: the default
	// Linux wheel carries a CUDA runtime this sidecar never uses, and the index
	// publishes the macOS and Windows builds of the same version, so one code
	// path installs the same thing everywhere.
	torchArgs = append(torchArgs, "--index-url", guardrailsTorchCPUIndex)
	if err := guardrailsExec(output, pythonPath, torchArgs...); err != nil {
		return fmt.Errorf("install classifier torch: %w", err)
	}
	if err := guardrailsExec(output, pythonPath, "-m", "pip", "install",
		"--disable-pip-version-check", "--no-input",
		"-r", filepath.Join(dir, "requirements-classifier.txt")); err != nil {
		return fmt.Errorf("install classifier dependencies: %w", err)
	}
	if err := guardrailsDownloadModel(settings, dir, output); err != nil {
		return err
	}
	return os.WriteFile(stampPath, []byte(want+"\n"), 0o644)
}

// guardrailsDownloadModel fetches the pinned revision. It writes a stamp only
// after the download returns, so an interrupted install is retried rather than
// mistaken for a complete one.
func downloadGuardrailsClassifier(settings guardrailsConfig, dir string, output io.Writer) error {
	if guardrailsClassifierReady(dir) {
		return nil
	}
	token := guardrailsHFToken(settings)
	if token == "" {
		return fmt.Errorf("%s is gated: accept the license at https://huggingface.co/%s and set HF_TOKEN", guardrailsClassifierLabel, guardrailsClassifierRepo)
	}
	fmt.Fprintf(output, "Downloading %s (%s)...\n", guardrailsClassifierLabel, guardrailsClassifierRevision[:12])
	script := strings.Join([]string{
		"from huggingface_hub import snapshot_download",
		"snapshot_download(repo_id=" + strconv.Quote(guardrailsClassifierRepo) +
			", revision=" + strconv.Quote(guardrailsClassifierRevision) + ")",
	}, "\n")
	// The download inherits the environment, like the pip calls above: a proxy
	// configured for the installer has to reach Hugging Face too. The token is
	// added only here — the long-running sidecar never receives it.
	cmd := exec.Command(guardrailsVenvPython(dir), "-c", script)
	cmd.Env = append(os.Environ(),
		"HF_HOME="+guardrailsHFHome(dir),
		"HF_TOKEN="+token,
		"HUGGING_FACE_HUB_TOKEN="+token,
		"HF_HUB_OFFLINE=0",
		"TRANSFORMERS_OFFLINE=0",
	)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("download %s: %w (accept the license at https://huggingface.co/%s and check HF_TOKEN)", guardrailsClassifierLabel, err, guardrailsClassifierRepo)
	}
	return os.WriteFile(guardrailsClassifierStampPath(dir), []byte(guardrailsClassifierRevision+"\n"), 0o644)
}

func requirementsStamp(withClassifier bool) string {
	sum := sha256.Sum256(guardrailsRequirementsFor(withClassifier))
	return hex.EncodeToString(sum[:])
}

// classifierStamp covers the pins that decide what gets installed, not the
// weights: the revision lives in the model stamp, which the download writes.
func classifierStamp() string {
	sum := sha256.Sum256([]byte(guardrailsClassifierTorch + "\n" + string(guardrailsClassifierRequirements)))
	return hex.EncodeToString(sum[:])
}

func guardrailsRequirementsFor(withClassifier bool) []byte {
	content := append([]byte{}, guardrailsRequirements...)
	if withClassifier {
		content = append(content, guardrailsClassifierRequirements...)
	}
	return content
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
func writeGuardrailsAssets(dir string, force, withClassifier bool, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	policy := guardrailsPolicyPath(dir)
	if err := os.MkdirAll(filepath.Dir(policy), 0o755); err != nil {
		return err
	}
	shipped := guardrailsPolicy(withClassifier)
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
			// untouched when it matches either shipped variant, so switching the
			// classifier on or off is not mistaken for an operator edit.
			matches := guardrailsPolicyContent(current) == guardrailsPolicyContent(guardrailsPolicy(false)) ||
				guardrailsPolicyContent(current) == guardrailsPolicyContent(guardrailsPolicy(true))
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
	if err := writeGuardrailsFile(filepath.Join(dir, "requirements-classifier.txt"), guardrailsClassifierRequirements); err != nil {
		return err
	}
	return writeGuardrailsFile(filepath.Join(dir, "serve.py"), guardrailsServeScript)
}

// guardrailsPolicy renders the shipped policy. The template carries three
// markers so the classifier block and its two flows appear only when a token
// made the gated model available, without a second policy file to keep in sync.
func guardrailsPolicy(withClassifier bool) []byte {
	policy := string(guardrailsDefaultConfig)
	markers := []struct {
		line    string
		content string
	}{
		{
			line: "    # orr:hf-classifier-config",
			content: `    hf_classifier:
      ` + guardrailsClassifierName + `:
        engine: local
        model: ` + guardrailsClassifierRepo + `
        task: text-classification
        # The 22M model recalls 88.7% of jailbreaks at a 1% false-positive rate;
        # 0.5 would trade that precision for recall this gate does not want. A
        # coding prompt blocked for sounding adversarial costs more than a
        # marginal attack that still has to get past the rails below.
        threshold: 0.9
        blocked_labels: ["MALICIOUS", "LABEL_1"]
        parameters:
          revision: ` + guardrailsClassifierRevision + `
          device: -1`,
		},
		{line: "      # orr:hf-classifier-input", content: "      - hf classifier check input $classifier=" + guardrailsClassifierName},
		{line: "      # orr:hf-classifier-output", content: "      - hf classifier check output $classifier=" + guardrailsClassifierName},
	}
	for _, marker := range markers {
		replacement := ""
		if withClassifier {
			replacement = marker.content
		}
		policy = strings.Replace(policy, marker.line, replacement, 1)
	}
	return []byte(policy)
}

// guardrailsPolicyUsesClassifier reports whether a policy declares the model
// rail, which is also how the status command says whether it is active.
func guardrailsPolicyUsesClassifier(policy []byte) bool {
	return bytes.Contains(policy, []byte("hf classifier check"))
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
	shipped := guardrailsPolicy(guardrailsPolicyUsesClassifier(current))
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

// guardrailsDependenciesState reports whether the installed requirements match
// what an install would put there now. It asks the same question the installer
// asks — whether the classifier is enabled for this installation — rather than
// reading the policy: deleting the classifier rails is the documented way to
// turn the model off, and that leaves the token, and so the installed
// requirements, unchanged.
func guardrailsDependenciesState(settings guardrailsConfig, dir string) string {
	stamp, err := os.ReadFile(filepath.Join(dir, "requirements.stamp"))
	if err != nil {
		return "pending"
	}
	if strings.TrimSpace(string(stamp)) != requirementsStamp(guardrailsClassifierEnabled(settings, dir)) {
		return "pending"
	}
	return "current"
}

// guardrailsClassifierState describes the model rail: whether the weights are
// here, and whether the policy actually runs them.
func guardrailsClassifierState(settings guardrailsConfig, dir string) string {
	policy, _ := os.ReadFile(guardrailsPolicyPath(dir))
	active := guardrailsPolicyUsesClassifier(policy)
	switch {
	case guardrailsClassifierReady(dir) && active:
		return guardrailsClassifierLabel + " (ready)"
	case guardrailsClassifierReady(dir):
		return guardrailsClassifierLabel + " (downloaded, not in the policy)"
	case guardrailsHFToken(settings) != "":
		return guardrailsClassifierLabel + " (not downloaded)"
	case active:
		return guardrailsClassifierLabel + " (in the policy, weights missing)"
	default:
		return "off (no HF_TOKEN)"
	}
}

func writeGuardrailsFile(path string, content []byte) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(content) {
		return nil
	}
	return os.WriteFile(path, content, 0o644)
}

func runGuardrailsStatus(settings guardrailsConfig, full bool, output io.Writer) error {
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
	fmt.Fprintf(output, "dependencies: %s\n", guardrailsDependenciesState(settings, dir))
	fmt.Fprintf(output, "classifier: %s\n", guardrailsClassifierState(settings, dir))
	if guardrailsHFToken(settings) == "" && !guardrailsClassifierReady(dir) {
		fmt.Fprintf(output, "classifier hint: set HF_TOKEN to install %s\n", guardrailsClassifierRepo)
	}
	verdicts, verdictErr := fetchGuardrailVerdicts(settings)
	printGuardrailVerdicts(output, verdicts, verdictErr, full)
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
	if guardrailsClassifierEnabled(s.settings, s.dir) {
		// The weights are already here. Offline mode makes that a guarantee: a
		// check loads the pinned revision from disk and never reaches the
		// network, and the token stays out of a process that outlives the
		// install.
		sidecarEnv = append(sidecarEnv,
			"HF_HOME="+guardrailsHFHome(s.dir),
			"HF_HUB_OFFLINE=1",
			"TRANSFORMERS_OFFLINE=1",
			"HF_HUB_DISABLE_TELEMETRY=1",
		)
	}
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
	Reason string `json:"reason"`
	// Label is the classifier's own verdict, read from the response header
	// rather than the body. A sidecar that does not report one leaves it empty.
	Label string `json:"-"`
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
	verdict, err := p.guardrailCheck(ctx, record, model, messages, []string{"input"})
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
	record.GuardRail = guardrailUnnamedRail(verdict.Rail, verdict.Reason)
	record.GuardLabel = verdict.Label
	record.GuardText = guardrailInspectText(messages)
	if p.guardrailsEnforce(action) {
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
	messages, railTypes := withToolResultInput(toolRun)
	verdict, err := p.guardrailCheck(ctx, record, model, messages, railTypes)
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
	record.GuardRail = guardrailUnnamedRail(verdict.Rail, verdict.Reason)
	record.GuardLabel = verdict.Label
	record.GuardText = guardrailInspectText(toolRun)
	if p.guardrailsEnforce(action) {
		record.GuardBlocked = true
	} else {
		record.GuardObserved = true
	}
}

// withToolResultInput sends the trailing tool output through the input rails
// alongside the tool-result rails. NeMo has no model-backed flow for the
// tool-result surface, so tool output reaches the classifier only as a user
// turn; appending one to the check lets the input rails — including Prompt
// Guard when installed — judge the text, while the tool-result rails keep
// linkage and the per-tool secret patterns.
func withToolResultInput(toolRun []guardrailMessage) ([]guardrailMessage, []string) {
	text := guardrailToolResultText(toolRun)
	if text == "" {
		return toolRun, []string{"tool_result"}
	}
	content, err := json.Marshal(text)
	if err != nil {
		return toolRun, []string{"tool_result"}
	}
	messages := append(append([]guardrailMessage{}, toolRun...), guardrailMessage{Role: "user", Content: content})
	return messages, []string{"tool_result", "input"}
}

// guardrailToolResultText renders the trailing run's tool output as one block.
// Only role "tool" messages are included: the assistant's calls are judged as
// calls, and repeating their arguments here would present the model's own text
// as untrusted input. The block is bounded like a verdict sample, because a
// tool result can be arbitrarily large and the gate runs on every request.
func guardrailToolResultText(messages []guardrailMessage) string {
	var builder strings.Builder
	for _, message := range messages {
		if message.Role != "tool" || len(message.Content) == 0 {
			continue
		}
		content := string(message.Content)
		var unquoted string
		if err := json.Unmarshal(message.Content, &unquoted); err == nil {
			content = unquoted
		}
		content = strings.TrimSpace(content)
		// A file read on Windows often begins with a UTF-8 BOM. The input
		// rails treat U+FEFF as smuggling, and a leading BOM is metadata, not
		// text the model should judge; a BOM anywhere else still matches.
		content = strings.TrimPrefix(content, "\ufeff")
		if content == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(content)
		if builder.Len() >= guardrailSampleLimit {
			break
		}
	}
	return guardrailSample(builder.String())
}

func (p *proxy) guardrailCheck(ctx context.Context, record *requestRecord, model string, messages []guardrailMessage, railTypes []string) (guardrailVerdict, error) {
	started := time.Now()
	verdict, modelTime, err := p.sendGuardrailCheck(ctx, model, messages, railTypes)
	accountGuardrailCheck(record, time.Since(started), modelTime)
	return verdict, err
}

// sendGuardrailCheck performs one check and returns the classifier's share of
// it when the sidecar reports it. Everything the model did is already inside
// the wall time the caller measures; the header only says how much of it was
// Prompt Guard.
func (p *proxy) sendGuardrailCheck(ctx context.Context, model string, messages []guardrailMessage, railTypes []string) (guardrailVerdict, time.Duration, error) {
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
		return guardrailVerdict{}, 0, err
	}
	endpoint := p.cfg.Guardrails.url() + "/v1/checks"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return guardrailVerdict{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := p.guardrailsClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return guardrailVerdict{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return guardrailVerdict{}, 0, guardrailCheckStatusError(resp)
	}
	var verdict guardrailVerdict
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&verdict); err != nil {
		return guardrailVerdict{}, 0, err
	}
	verdict.Label = strings.TrimSpace(resp.Header.Get(guardrailLabelHeader))
	return verdict, guardrailModelTime(resp.Header.Get(guardrailModelTimeHeader)), nil
}

// guardrailErrorBodyLimit bounds how much of a sidecar error body a failed check
// quotes. The status line and the body are all a client sees of a sidecar that
// answered instead of dropping the connection, and a body naming its own reason
// is otherwise lost.
const guardrailErrorBodyLimit = 512

func guardrailCheckStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, guardrailErrorBodyLimit))
	if detail := previewLine(string(body)); detail != "" {
		return fmt.Errorf("check returned %s: %s", resp.Status, detail)
	}
	return fmt.Errorf("check returned %s", resp.Status)
}

// guardrailModelTime parses the classifier's share of a check. A missing or
// malformed header means the sidecar did not report one, which is what an older
// or external sidecar does: the time is then left in the non-model column
// rather than guessed at.
func guardrailModelTime(value string) time.Duration {
	milliseconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || milliseconds <= 0 {
		return 0
	}
	return time.Duration(milliseconds * float64(time.Millisecond))
}

// accountGuardrailCheck adds one check's wall time to the request, split into
// the classifier's share and everything else. The header cannot exceed the
// round trip it arrived in; clamping keeps a clock difference from crediting a
// request with time it never spent.
func accountGuardrailCheck(record *requestRecord, elapsed, modelTime time.Duration) {
	if record == nil {
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	modelTime = max(0, min(modelTime, elapsed))
	record.GuardModel += modelTime
	record.GuardNonModel += elapsed - modelTime
}

// guardrailSampleLimit bounds the text a verdict keeps, so the appended log
// stays an audit trail rather than a recording of traffic. A sample longer than
// this is cut: it is the beginning of what was judged, which is what a false
// positive needs, and the file already rotates at 5 MiB.
const guardrailSampleLimit = 8 << 10

// guardrailSample returns the text a verdict carries for one check.
func guardrailSample(text string) string {
	if len(text) <= guardrailSampleLimit {
		return text
	}
	cut := guardrailSampleLimit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// guardrailInspectText renders the messages a check was given, so a verdict can
// name exactly what a rail saw. Each message is a block: the role, then the
// content indented, so a multi-line tool result stays readable. A tool call and
// its result carry the function name, because which call a block came from is
// the first thing an operator asks. Content that is JSON null or empty is
// skipped rather than printed as a word no rail read.
func guardrailInspectText(messages []guardrailMessage) string {
	var builder strings.Builder
	names := guardrailToolCallNames(messages)
	for _, message := range messages {
		content := guardrailMessageContent(message)
		switch message.Role {
		case "assistant":
			guardrailWriteBlock(&builder, "assistant", content)
			for _, call := range guardrailToolCalls(message.ToolCalls) {
				label := "tool call"
				if call.name != "" {
					label += " " + call.name
				}
				guardrailWriteBlock(&builder, label, call.arguments)
			}
		case "tool":
			label := "tool result"
			if name := names[message.ToolCallID]; name != "" {
				label += " (" + name + ")"
			}
			guardrailWriteBlock(&builder, label, content)
		default:
			guardrailWriteBlock(&builder, message.Role, content)
		}
		if builder.Len() >= guardrailSampleLimit {
			break
		}
	}
	return guardrailSample(builder.String())
}

// guardrailMessageContent unquotes a message's JSON content. A body that will
// not unquote is passed through as it arrived rather than dropped; JSON null
// carries no text and becomes empty.
func guardrailMessageContent(message guardrailMessage) string {
	content := strings.TrimSpace(string(message.Content))
	if unquoted, err := strconv.Unquote(content); err == nil {
		content = unquoted
	}
	if content == "null" {
		return ""
	}
	return content
}

func guardrailWriteBlock(builder *strings.Builder, label, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	builder.WriteString(label)
	builder.WriteString(":\n")
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		if line == "" {
			builder.WriteString("\n")
			continue
		}
		builder.WriteString("  ")
		builder.WriteString(line)
		builder.WriteString("\n")
	}
}

// guardrailToolCall is one parsed tool call, kept to name the call a result
// belongs to and to show the arguments a tool-call check judged.
type guardrailToolCall struct {
	id        string
	name      string
	arguments string
}

func guardrailToolCalls(raw json.RawMessage) []guardrailToolCall {
	if len(raw) == 0 {
		return nil
	}
	var calls []rawToolCall
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil
	}
	parsed := make([]guardrailToolCall, 0, len(calls))
	for _, call := range calls {
		parsed = append(parsed, guardrailToolCall{
			id:        call.ID,
			name:      call.Function.Name,
			arguments: call.Function.Arguments,
		})
	}
	return parsed
}

func guardrailToolCallNames(messages []guardrailMessage) map[string]string {
	names := map[string]string{}
	for _, message := range messages {
		for _, call := range guardrailToolCalls(message.ToolCalls) {
			if call.id != "" && call.name != "" {
				names[call.id] = call.name
			}
		}
	}
	return names
}

// previewLine is the one-line form of a body for a log line or a status list:
// whitespace collapsed and the text cut, so a sample or an error message stays
// one line however many lines it arrived with.
const previewLimit = 160

func previewLine(text string) string {
	preview := strings.Join(strings.Fields(text), " ")
	if runes := []rune(preview); len(runes) > previewLimit {
		return string(runes[:previewLimit]) + "…"
	}
	return preview
}

// fetchGuardrailVerdicts reads the running instance's recent verdicts. An
// unreachable instance is reported, not fatal: status must still describe the
// installation itself.
func fetchGuardrailVerdicts(settings guardrailsConfig) ([]guardrailEvent, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + managementAddress(settings) + "/api/v1/guardrails")
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

// printGuardrailVerdicts renders the running instance's recent verdicts. The
// default is one header line per verdict plus a one-line preview: a sample is up
// to 8 KiB and twenty of them bury the lines that say what fired. full prints
// the whole sample, one line per line of the judged text, which is what
// reproducing a false positive needs.
func printGuardrailVerdicts(output io.Writer, events []guardrailEvent, err error, full bool) {
	if err != nil {
		fmt.Fprintf(output, "recent verdicts: (unavailable: %v)\n", err)
		return
	}
	if len(events) == 0 {
		fmt.Fprintln(output, "recent verdicts: none")
		return
	}
	fmt.Fprintf(output, "recent verdicts (newest first): %d%s\n", len(events), guardrailActionSummary(events))
	previewed := false
	for index, event := range events {
		if full && index > 0 {
			fmt.Fprintln(output)
		}
		state := event.Action
		if event.Status != 0 {
			state = fmt.Sprintf("%s %d", event.Action, event.Status)
		}
		fmt.Fprintf(output, "  %s  %-11s  %-11s  %s  %s / %s",
			event.Time.Local().Format("2006-01-02 15:04:05"), event.Phase, state, event.Rail, event.Model, event.Provider)
		if event.Label != "" {
			fmt.Fprintf(output, "  %s", event.Label)
		}
		fmt.Fprintln(output)
		if event.Text == "" {
			continue
		}
		if !full {
			previewed = true
			fmt.Fprintf(output, "      %s\n", previewLine(event.Text))
			continue
		}
		fmt.Fprintln(output, "    text:")
		for _, line := range strings.Split(strings.TrimRight(event.Text, "\n"), "\n") {
			if line == "" {
				fmt.Fprintln(output)
				continue
			}
			fmt.Fprintf(output, "      %s\n", line)
		}
	}
	if previewed {
		fmt.Fprintln(output, "  (--full prints the whole text each verdict judged)")
	}
}

// guardrailActionSummary counts what the list decided, so the shape of recent
// traffic is readable without scanning every line.
func guardrailActionSummary(events []guardrailEvent) string {
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Action]++
	}
	parts := make([]string, 0, len(counts))
	for _, action := range []string{"blocked", "observed", "unavailable"} {
		if count := counts[action]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, action))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
