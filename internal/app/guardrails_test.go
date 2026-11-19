package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseGuardrailsConfig(t *testing.T) {
	values := func(pairs map[string]string) func(string) string {
		return func(name string) string { return pairs[name] }
	}
	tests := []struct {
		name    string
		env     map[string]string
		want    guardrailsConfig
		wantErr string
	}{
		{name: "defaults", env: nil, want: guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, Port: defaultGuardrailsPort}},
		{name: "off", env: map[string]string{"ORR_GUARDRAILS": "off"}, want: guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOff, Port: defaultGuardrailsPort}},
		{name: "on", env: map[string]string{"ORR_GUARDRAILS": "on"}, want: guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, Port: defaultGuardrailsPort}},
		{name: "auto is not accepted", env: map[string]string{"ORR_GUARDRAILS": "auto"}, wantErr: "ORR_GUARDRAILS"},
		{name: "unknown mode", env: map[string]string{"ORR_GUARDRAILS": "sometimes"}, wantErr: "ORR_GUARDRAILS"},
		{name: "port", env: map[string]string{"ORR_GUARDRAILS_PORT": "9000"}, want: guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, Port: 9000}},
		{name: "port zero", env: map[string]string{"ORR_GUARDRAILS_PORT": "0"}, wantErr: "ORR_GUARDRAILS_PORT"},
		{name: "port too high", env: map[string]string{"ORR_GUARDRAILS_PORT": "70000"}, wantErr: "ORR_GUARDRAILS_PORT"},
		{name: "port not a number", env: map[string]string{"ORR_GUARDRAILS_PORT": "http"}, wantErr: "ORR_GUARDRAILS_PORT"},
		{
			name: "url",
			env:  map[string]string{"ORR_GUARDRAILS_URL": "http://127.0.0.1:9001/"},
			want: guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, Port: defaultGuardrailsPort, URL: "http://127.0.0.1:9001"},
		},
		{name: "bad url", env: map[string]string{"ORR_GUARDRAILS_URL": "127.0.0.1:9001"}, wantErr: "ORR_GUARDRAILS_URL"},
		{name: "home", env: map[string]string{"ORR_GUARDRAILS_HOME": "/tmp/guardrails"}, want: guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, Port: defaultGuardrailsPort, Home: "/tmp/guardrails"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGuardrailsConfig(values(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one naming %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestGuardrailsConfigValidateToleratesZeroValue(t *testing.T) {
	if err := (guardrailsConfig{}).validate(); err != nil {
		t.Fatalf("zero guardrails config rejected: %v", err)
	}
	bad := guardrailsConfig{Action: guardrailsActionBlock, Mode: "sometimes"}
	if err := bad.validate(); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestSupportedPythonVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"3.10.13", false},
		{"3.11.0", true},
		{"3.12.7", true},
		{"3.13.1", true},
		{"3.14.0", false},
		{"2.7.18", false},
		{"3", false},
		{"three.twelve", false},
	}
	for _, tc := range tests {
		if got := supportedPythonVersion(tc.version); got != tc.want {
			t.Fatalf("supportedPythonVersion(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestWriteGuardrailsAssets(t *testing.T) {
	dir := t.TempDir()
	if err := writeGuardrailsAssets(dir, false, nil); err != nil {
		t.Fatalf("write assets: %v", err)
	}
	for _, name := range []string{
		filepath.Join(dir, "requirements.txt"),
		filepath.Join(dir, "serve.py"),
		guardrailsPolicyPath(dir),
	} {
		if !fileExists(name) {
			t.Fatalf("%s was not written", name)
		}
	}

	policy := guardrailsPolicyPath(dir)
	edited := []byte("models: []\n# operator edit\n")
	if err := os.WriteFile(policy, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeGuardrailsAssets(dir, false, nil); err != nil {
		t.Fatalf("rewrite assets: %v", err)
	}
	kept, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept, edited) {
		t.Fatal("a reinstall without --force overwrote the operator's policy")
	}
	if err := writeGuardrailsAssets(dir, true, nil); err != nil {
		t.Fatalf("force rewrite: %v", err)
	}
	reset, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reset, guardrailsDefaultConfig) {
		t.Fatal("--force did not restore the default policy")
	}
}

func TestWaitForGuardrails(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("requested %s, want /healthz", r.URL.Path)
		}
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := waitForGuardrails(context.Background(), server.URL, 2*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

func TestWaitForGuardrailsTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := waitForGuardrails(context.Background(), server.URL, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not healthy") {
		t.Fatalf("error = %v, want a health timeout", err)
	}
}

func TestGuardrailsLogWriterDropsBlankLines(t *testing.T) {
	var lines []string
	writer := guardrailsLogWriter{logf: func(format string, args ...any) {
		lines = append(lines, format)
	}}
	if _, err := writer.Write([]byte("first\r\n\nsecond\n")); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("logged %d lines, want 2: %v", len(lines), lines)
	}
}

func TestGuardrailsStatusNotInstalled(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runGuardrailsStatus(guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, Port: 8765, Home: dir, Listen: "127.0.0.1:1"}, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	text := out.String()
	for _, want := range []string{"installed: no", "mode: on", "endpoint: http://127.0.0.1:8765", "recent verdicts:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status output %q does not contain %q", text, want)
		}
	}
}

func TestGuardrailsConfigShowsDefault(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runGuardrailsConfig(guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, Port: 8765, Home: dir, Listen: "127.0.0.1:1"}, &out); err != nil {
		t.Fatalf("config: %v", err)
	}
	if !strings.Contains(out.String(), "showing the default shipped with orr") {
		t.Fatalf("config output did not mention the default policy: %q", out.String())
	}
	if !strings.Contains(out.String(), "injection_detection") {
		t.Fatalf("config output did not contain the default policy: %q", out.String())
	}
}

func TestStartGuardrailsSidecarSkipsDisabled(t *testing.T) {
	sidecar, err := startGuardrailsSidecar(context.Background(), guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOff}, func(string, ...any) {})
	if err != nil || sidecar != nil {
		t.Fatalf("off: got (%v, %v), want (nil, nil)", sidecar, err)
	}
}

func TestStartGuardrailsSidecarRequiresHealthyExternalEndpoint(t *testing.T) {
	oldWait := guardrailsHealthWait
	guardrailsHealthWait = 300 * time.Millisecond
	t.Cleanup(func() { guardrailsHealthWait = oldWait })
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	sidecar, err := startGuardrailsSidecar(context.Background(), guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, URL: healthy.URL}, func(string, ...any) {})
	if err != nil || sidecar != nil {
		t.Fatalf("healthy external endpoint: got (%v, %v), want (nil, nil)", sidecar, err)
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()
	_, err = startGuardrailsSidecar(context.Background(), guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, URL: unhealthy.URL}, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "not healthy") {
		t.Fatalf("error = %v, want an unhealthy endpoint failure", err)
	}
}

func stubGuardrailsTools(t *testing.T) {
	t.Helper()
	oldFind, oldExec := guardrailsFindPython, guardrailsExec
	t.Cleanup(func() {
		guardrailsFindPython = oldFind
		guardrailsExec = oldExec
	})
}

func TestEnsureGuardrailsInstallsMissingEnvironment(t *testing.T) {
	stubGuardrailsTools(t)
	dir := t.TempDir()
	var commands [][]string
	guardrailsFindPython = func(string) (string, string, error) {
		return "/usr/bin/python3.12", "3.12.7", nil
	}
	guardrailsExec = func(_ io.Writer, name string, args ...string) error {
		commands = append(commands, append([]string{name}, args...))
		return nil
	}
	if err := ensureGuardrails(guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, Port: 8765, Home: dir, Listen: "127.0.0.1:1"}, io.Discard); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !fileExists(guardrailsPolicyPath(dir)) {
		t.Fatal("ensure did not write the default policy")
	}
	if !fileExists(filepath.Join(dir, "requirements.stamp")) {
		t.Fatal("ensure did not record the installed requirements")
	}
	var sawVenv, sawInstall bool
	for _, command := range commands {
		if len(command) >= 4 && command[2] == "venv" {
			sawVenv = true
		}
		if len(command) >= 4 && command[2] == "pip" && command[3] == "install" && strings.Contains(strings.Join(command, " "), "-r") {
			sawInstall = true
		}
	}
	if !sawVenv || !sawInstall {
		t.Fatalf("ensure commands %v did not create a venv and install requirements", commands)
	}
}

func TestEnsureGuardrailsSkipsCurrentRequirements(t *testing.T) {
	stubGuardrailsTools(t)
	dir := t.TempDir()
	pythonPath := guardrailsVenvPython(dir)
	if err := os.MkdirAll(filepath.Dir(pythonPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pythonPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "venv", "pyvenv.cfg"), []byte("home = /usr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	guardrailsExec = func(io.Writer, string, ...string) error {
		calls++
		return nil
	}
	settings := guardrailsConfig{Action: guardrailsActionBlock, Mode: guardrailsModeOn, Port: 8765, Home: dir, Listen: "127.0.0.1:1"}
	if err := ensureGuardrails(settings, io.Discard); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	first := calls
	if first == 0 {
		t.Fatal("first ensure did not install dependencies")
	}
	if err := ensureGuardrails(settings, io.Discard); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if calls != first {
		t.Fatalf("second ensure reinstalled current requirements (%d calls, want %d)", calls, first)
	}
}

func TestStartGuardrailsSidecarFailsWhenInstallFails(t *testing.T) {
	stubGuardrailsTools(t)
	guardrailsFindPython = func(string) (string, string, error) {
		return "", "", errors.New("no Python interpreter found")
	}
	_, err := startGuardrailsSidecar(context.Background(), guardrailsConfig{
		Mode: guardrailsModeOn,
		Port: 8765,
		Home: t.TempDir(),
	}, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("error = %v, want a hard requirement failure", err)
	}
}

func TestGuardrailInputUsesLastUserMessage(t *testing.T) {
	body := []byte(`{"model":"a/b","messages":[
		{"role":"system","content":"rules"},
		{"role":"user","content":"first"},
		{"role":"assistant","content":"reply"},
		{"role":"user","content":"last"}]}`)
	model, messages, ok, undecodable := guardrailInput(body)
	if undecodable {
		t.Fatal("a decodable body was reported undecodable")
	}
	if !ok {
		t.Fatal("no user message found")
	}
	if model != "a/b" {
		t.Fatalf("model = %q, want a/b", model)
	}
	if len(messages) != 1 || string(messages[0].Content) != `"last"` {
		t.Fatalf("messages = %+v, want only the last user message", messages)
	}
}

func TestGuardrailInputIgnoresMissingUserMessage(t *testing.T) {
	if _, _, ok, undecodable := guardrailInput([]byte(`{"model":"a/b","messages":[{"role":"system","content":"x"}]}`)); ok || undecodable {
		t.Fatal("a body without a user message was accepted")
	}
}

func TestCheckGuardrailsInputBlockAndObserve(t *testing.T) {
	var seen guardrailCheckRequest
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Errorf("decode check body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"blocked","rail":"regex check input"}`))
	}))
	defer sidecar.Close()

	body := []byte(`{"model":"a/b","messages":[{"role":"user","content":"x"}]}`)
	for _, tc := range []struct {
		name     string
		action   string
		blocked  bool
		observed bool
	}{
		{name: "block", action: guardrailsActionBlock, blocked: true},
		{name: "observe", action: guardrailsActionObserve, observed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &proxy{
				cfg: config{Guardrails: guardrailsConfig{
					Mode: guardrailsModeOn, Action: tc.action, URL: sidecar.URL,
				}},
				guardrailsClient: &http.Client{},
			}
			var record requestRecord
			p.checkGuardrailsInput(context.Background(), body, &record)
			if record.GuardBlocked != tc.blocked || record.GuardObserved != tc.observed {
				t.Fatalf("record = %+v, want blocked=%v observed=%v", record, tc.blocked, tc.observed)
			}
			if record.GuardRail != "regex check input" || record.GuardPhase != "input" {
				t.Fatalf("record = %+v, want the input rail", record)
			}
		})
	}
	if seen.Model != "a/b" || len(seen.Guardrails.RailTypes) != 1 || seen.Guardrails.RailTypes[0] != "input" {
		t.Fatalf("check request = %+v, want model a/b and input rails", seen)
	}
}

func TestLogGuardMarker(t *testing.T) {
	if logGuardMarker(requestRecord{}) != "" {
		t.Fatal("a request without a verdict earned a guard marker")
	}
	if logGuardMarker(requestRecord{GuardBlocked: true}) != guardMark {
		t.Fatal("a blocked request did not earn the guard marker")
	}
	if logGuardMarker(requestRecord{GuardObserved: true}) != guardMark {
		t.Fatal("an observed request did not earn the guard marker")
	}
}

func TestWriteGuardrailError(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeGuardrailError(recorder, "guardrails blocked the request: regex check input")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != guardrailBlockedCode || !strings.Contains(body.Error.Message, "regex check input") {
		t.Fatalf("body = %+v, want the blocked code and rail", body)
	}
}

func TestGuardRowShowsLabel(t *testing.T) {
	record := requestRecord{Model: "a/b", GuardObserved: true}
	if text := logModelText(record, 36); !strings.Contains(text, "⊘ GUARD TRIPPED") {
		t.Fatalf("model cell = %q, want the guard label", text)
	}
	if logRowForeground(record) == errorStyle.GetForeground() {
		t.Fatal("a guard row rendered with the error colour")
	}
}

func TestWriteGuardrailsAssetsUpdatesUntouchedPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := writeGuardrailsAssets(dir, false, nil); err != nil {
		t.Fatal(err)
	}
	policy := guardrailsPolicyPath(dir)
	old := []byte("# an older shipped default\n")
	if err := os.WriteFile(policy, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guardrailsPolicyStampPath(dir), []byte(guardrailsHash(old)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeGuardrailsAssets(dir, false, nil); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, guardrailsDefaultConfig) {
		t.Fatal("an untouched policy was not updated to the shipped default")
	}
	if state := guardrailsPolicyState(dir); state != "default" {
		t.Fatalf("policy state = %q, want default", state)
	}
}

func TestWriteGuardrailsAssetsPreservesEditedPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := writeGuardrailsAssets(dir, false, nil); err != nil {
		t.Fatal(err)
	}
	policy := guardrailsPolicyPath(dir)
	edited := []byte("models: []\n# operator edit\n")
	if err := os.WriteFile(policy, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guardrailsPolicyStampPath(dir), []byte(guardrailsHash([]byte("# older\n"))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var notices []string
	if err := writeGuardrailsAssets(dir, false, func(format string, args ...any) {
		notices = append(notices, fmt.Sprintf(format, args...))
	}); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, edited) {
		t.Fatal("an edited policy was overwritten")
	}
	updated, err := os.ReadFile(policy + ".new")
	if err != nil {
		t.Fatalf("no new default was written beside the edited policy: %v", err)
	}
	if !bytes.Equal(updated, guardrailsDefaultConfig) {
		t.Fatal("the .new file does not hold the shipped default")
	}
	if len(notices) == 0 || !strings.Contains(notices[0], "review") {
		t.Fatalf("notices = %v, want one naming the new file", notices)
	}
	if state := guardrailsPolicyState(dir); state != "edited" {
		t.Fatalf("policy state = %q, want edited", state)
	}
}

func TestCheckGuardrailsToolResultsBlock(t *testing.T) {
	var seen guardrailCheckRequest
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Errorf("decode check body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"blocked","rail":"regex check tool result"}`))
	}))
	defer sidecar.Close()

	body := []byte(`{"model":"a/b","messages":[
		{"role":"user","content":"read the file"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"-----BEGIN RSA PRIVATE KEY-----"}]}`)
	p := &proxy{
		cfg:              config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}},
		guardrailsClient: &http.Client{},
	}
	var record requestRecord
	p.checkGuardrailsToolResults(context.Background(), body, &record)
	if !record.GuardBlocked || record.GuardPhase != "tool_result" || record.GuardRail != "regex check tool result" {
		t.Fatalf("record = %+v, want a blocked tool result", record)
	}
	if len(seen.Guardrails.RailTypes) != 1 || seen.Guardrails.RailTypes[0] != "tool_result" {
		t.Fatalf("rail types = %v, want tool_result", seen.Guardrails.RailTypes)
	}
	if len(seen.Messages) != 2 {
		t.Fatalf("messages = %d, want the assistant tool call and its result only", len(seen.Messages))
	}
}

func TestCheckGuardrailsToolResultsSkipsWhenNoTools(t *testing.T) {
	called := false
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"passed"}`))
	}))
	defer sidecar.Close()
	p := &proxy{
		cfg:              config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}},
		guardrailsClient: &http.Client{},
	}
	var record requestRecord
	p.checkGuardrailsToolResults(context.Background(), []byte(`{"model":"a/b","messages":[{"role":"user","content":"x"}]}`), &record)
	if called {
		t.Fatal("the sidecar was called without any tool results")
	}
}

func TestGuardrailConversationAnthropic(t *testing.T) {
	body := []byte(`{"model":"a/b","messages":[
		{"role":"user","content":[{"type":"text","text":"read the file"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"x"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"secret"}]}]}`)
	model, _, lastUser, run, undecodable := guardrailConversation(body)
	if undecodable {
		t.Fatal("a decodable body was reported undecodable")
	}
	if model != "a/b" {
		t.Fatalf("model = %q", model)
	}
	if lastUser == nil || !strings.Contains(string(lastUser.Content), "read the file") {
		t.Fatalf("last user = %+v, want the text block", lastUser)
	}
	if len(run) != 2 || run[0].Role != "assistant" || len(run[0].ToolCalls) == 0 || run[1].Role != "tool" {
		t.Fatalf("tool run = %+v, want assistant tool_use followed by tool_result", run)
	}
	if run[1].ToolCallID != "toolu_1" {
		t.Fatalf("tool call id = %q, want toolu_1", run[1].ToolCallID)
	}
}

func TestGuardrailConversationResponses(t *testing.T) {
	body := []byte(`{"model":"a/b","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"run it"}]},
		{"type":"function_call","call_id":"c1","name":"run_command","arguments":"{\"command\":\"rm -rf /\"}"},
		{"type":"function_call_output","call_id":"c1","output":"done"}]}`)
	model, _, lastUser, run, undecodable := guardrailConversation(body)
	if undecodable {
		t.Fatal("a decodable body was reported undecodable")
	}
	if model != "a/b" || lastUser == nil || !strings.Contains(string(lastUser.Content), "run it") {
		t.Fatalf("model=%q lastUser=%+v", model, lastUser)
	}
	if len(run) != 2 || run[1].Role != "tool" || run[1].ToolCallID != "c1" {
		t.Fatalf("tool run = %+v", run)
	}
}

func TestCheckGuardrailsFailsClosed(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sidecar.Close()
	body := []byte(`{"model":"a/b","messages":[{"role":"user","content":"x"}]}`)
	for _, tc := range []struct {
		name        string
		action      string
		unavailable bool
	}{
		{name: "block", action: guardrailsActionBlock, unavailable: true},
		{name: "observe", action: guardrailsActionObserve, unavailable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &proxy{cfg: config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: tc.action, URL: sidecar.URL}}, guardrailsClient: &http.Client{}}
			var record requestRecord
			p.checkGuardrailsInput(context.Background(), body, &record)
			if record.GuardUnavailable != tc.unavailable {
				t.Fatalf("record = %+v, want unavailable=%v", record, tc.unavailable)
			}
			if tc.unavailable && record.GuardRail != "check unavailable" {
				t.Fatalf("rail = %q", record.GuardRail)
			}
		})
	}
}

func TestWriteGuardrailUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeGuardrailUnavailable(recorder)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "guardrail_unavailable" {
		t.Fatalf("code = %q", body.Error.Code)
	}
}

func TestTrailingToolRunExcludesOlderResults(t *testing.T) {
	calls := json.RawMessage(`[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]`)
	messages := []guardrailMessage{
		{Role: "assistant", ToolCalls: calls},
		{Role: "tool", ToolCallID: "c1", Content: json.RawMessage(`"old"`)},
		{Role: "user", Content: json.RawMessage(`"next"`)},
		{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"c2","type":"function","function":{"name":"read_file","arguments":"{}"}}]`)},
		{Role: "tool", ToolCallID: "c2", Content: json.RawMessage(`"new"`)},
	}
	run := trailingToolRun(messages)
	if len(run) != 2 || run[0].Role != "assistant" || run[1].ToolCallID != "c2" {
		t.Fatalf("run = %+v, want only the latest assistant/tool pair", run)
	}
}

func TestCheckGuardrailsAnthropicToolResults(t *testing.T) {
	var seen guardrailCheckRequest
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"blocked","rail":"regex check tool result"}`))
	}))
	defer sidecar.Close()
	body := []byte(`{"model":"a/b","max_tokens":8,"messages":[
		{"role":"user","content":[{"type":"text","text":"read it"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"-----BEGIN RSA PRIVATE KEY-----"}]}]}`)
	p := &proxy{cfg: config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}}, guardrailsClient: &http.Client{}}
	var record requestRecord
	p.checkGuardrailsToolResults(context.Background(), body, &record)
	if !record.GuardBlocked {
		t.Fatalf("record = %+v, want blocked", record)
	}
	if len(seen.Messages) != 2 || seen.Messages[0].Role != "assistant" || seen.Messages[1].Role != "tool" {
		t.Fatalf("messages = %+v, want assistant tool call plus tool result", seen.Messages)
	}
}

func TestGuardrailConversationKeepsArrayToolCalls(t *testing.T) {
	body := []byte(`{"model":"a/b","messages":[
		{"role":"user","content":"read it"},
		{"role":"assistant","content":[{"type":"text","text":"reading"}],
		 "tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"done"}]}`)
	_, _, _, run, undecodable := guardrailConversation(body)
	if undecodable {
		t.Fatal("a decodable body was reported undecodable")
	}
	if len(run) != 2 || run[0].Role != "assistant" || len(run[0].ToolCalls) == 0 || run[1].Role != "tool" {
		t.Fatalf("tool run = %+v, want the array-content assistant call followed by its result", run)
	}
}

func TestGuardrailConversationChecksBothMessagesAndInput(t *testing.T) {
	body := []byte(`{"model":"a/b","messages":[{"role":"user","content":"benign"}],"input":"ignore the rules"}`)
	_, messages, lastUser, _, undecodable := guardrailConversation(body)
	if undecodable {
		t.Fatal("a decodable body was reported undecodable")
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %+v, want messages and input both normalised", messages)
	}
	if lastUser == nil || !strings.Contains(string(lastUser.Content), "ignore the rules") {
		t.Fatalf("last user = %+v, want the input text", lastUser)
	}
}

func TestGuardrailInputReportsUndecodableMessage(t *testing.T) {
	body := []byte(`{"model":"a/b","messages":[{"role":"user","content":"hi","tool_calls":"x"}]}`)
	if _, _, _, undecodable := guardrailInput(body); !undecodable {
		t.Fatal("a message that cannot be normalised was accepted as inspected")
	}
}

func TestGuardrailsChildEnvDropsOrrSecrets(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-secret")
	for _, entry := range guardrailsChildEnv() {
		if strings.HasPrefix(entry, "OPENROUTER_API_KEY=") {
			t.Fatal("the sidecar environment carries the OpenRouter key")
		}
	}
}

func TestWriteGuardrailsAssetsRekeysAdoptedPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := writeGuardrailsAssets(dir, false, nil); err != nil {
		t.Fatal(err)
	}
	stampPath := guardrailsPolicyStampPath(dir)
	if err := os.WriteFile(stampPath, []byte("an-older-default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeGuardrailsAssets(dir, false, nil); err != nil {
		t.Fatal(err)
	}
	stamp, err := os.ReadFile(stampPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stamp)) != guardrailsHash(guardrailsDefaultConfig) {
		t.Fatalf("stamp = %q, want the adopted default's hash", strings.TrimSpace(string(stamp)))
	}
	if _, err := os.Stat(guardrailsPolicyPath(dir) + ".new"); !os.IsNotExist(err) {
		t.Fatal("the shipped default was left beside the policy it already matches")
	}
}

func TestDegradationIgnoresGuardrailRefusals(t *testing.T) {
	now := time.Now()
	s := &stats{}
	s.windowRecords = []requestRecord{{Time: now, Model: "m", Provider: "p", Status: http.StatusForbidden, GuardBlocked: true}}
	s.metricsRecords = []requestRecord{{Time: now.Add(-time.Minute), Model: "m", Provider: "p", Status: http.StatusOK}}
	recent, _ := s.degradationRecords("m", "p", now, 2*time.Minute)
	if len(recent) != 0 {
		t.Fatalf("recent = %+v, want a guard refusal to be ignored", recent)
	}
}

func TestGuardrailRefusalCountsOnlyRefusedRequests(t *testing.T) {
	if (requestRecord{GuardUnavailable: true, Status: http.StatusOK}).guardrailRefusal() {
		t.Fatal("an observe-mode check failure was counted as a refused request")
	}
	if !(requestRecord{GuardUnavailable: true, Status: http.StatusServiceUnavailable}).guardrailRefusal() {
		t.Fatal("a request refused because the check was unavailable was not counted")
	}
	if !(requestRecord{GuardBlocked: true, Status: http.StatusForbidden}).guardrailRefusal() {
		t.Fatal("a blocked request was not counted as a refusal")
	}
}
