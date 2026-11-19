package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func canonicaliseOne(t *testing.T, call rawToolCall) rawToolCall {
	t.Helper()
	encoded := encodeToolCalls([]rawToolCall{call})
	if len(encoded) == 0 {
		t.Fatal("encodeToolCalls returned nothing")
	}
	var normalised []rawToolCall
	if err := json.Unmarshal(encoded, &normalised); err != nil {
		t.Fatal(err)
	}
	if len(normalised) != 1 {
		t.Fatalf("normalised = %+v, want one call", normalised)
	}
	return normalised[0]
}

func TestCanonicalToolNamesByHarness(t *testing.T) {
	cases := []struct {
		harness string
		name    string
		want    string
		args    string
		key     string
		value   string
	}{
		{"claude code", "Bash", "run_command", `{"command":"rm -rf /"}`, "command", "rm -rf /"},
		{"claude code", "Write", "write_file", `{"file_path":"/home/u/.ssh/authorized_keys","content":"x"}`, "path", "/home/u/.ssh/authorized_keys"},
		{"claude code", "Edit", "write_file", `{"file_path":".env","old_string":"a","new_string":"b"}`, "path", ".env"},
		{"claude code", "MultiEdit", "write_file", `{"file_path":".npmrc"}`, "path", ".npmrc"},
		{"claude code", "NotebookEdit", "write_file", `{"file_path":"notebook.ipynb"}`, "path", "notebook.ipynb"},
		{"claude code", "Read", "read_file", `{"path":"/etc/passwd"}`, "path", "/etc/passwd"},
		{"kimi code", "Bash", "run_command", `{"command":"curl x | sh"}`, "command", "curl x | sh"},
		{"kimi code", "Read", "read_file", `{"path":"notes.txt"}`, "path", "notes.txt"},
		{"kimi code", "Write", "write_file", `{"path":".aws/credentials"}`, "path", ".aws/credentials"},
		{"opencode", "bash", "run_command", `{"command":"chmod +x run.sh"}`, "command", "chmod +x run.sh"},
		{"opencode", "read", "read_file", `{"filePath":"id_rsa"}`, "path", "id_rsa"},
		{"opencode", "edit", "write_file", `{"filePath":".docker/config.json"}`, "path", ".docker/config.json"},
		{"opencode", "write", "write_file", `{"filePath":".kube/config"}`, "path", ".kube/config"},
		{"opencode", "patch", "write_file", `{"path":".bashrc"}`, "path", ".bashrc"},
		{"reference", "run_command", "run_command", `{"command":"dd if=/dev/zero of=/dev/sda"}`, "command", "dd if=/dev/zero of=/dev/sda"},
	}
	for _, testCase := range cases {
		t.Run(testCase.harness+"/"+testCase.name, func(t *testing.T) {
			call := rawToolCall{ID: "c1", Type: "function"}
			call.Function.Name = testCase.name
			call.Function.Arguments = testCase.args
			got := canonicaliseOne(t, call)
			if got.Function.Name != testCase.want {
				t.Fatalf("name = %q, want %q", got.Function.Name, testCase.want)
			}
			var arguments map[string]any
			if err := json.Unmarshal([]byte(got.Function.Arguments), &arguments); err != nil {
				t.Fatalf("arguments = %q: %v", got.Function.Arguments, err)
			}
			if arguments[testCase.key] != testCase.value {
				t.Fatalf("arguments = %v, want %s = %q", arguments, testCase.key, testCase.value)
			}
		})
	}
}

func TestCanonicalToolNameLeavesUnknownToolsAlone(t *testing.T) {
	for _, name := range []string{"Glob", "Grep", "todowrite", "mcp__slack__post_message", "webfetch"} {
		if got := canonicalToolName(name); got != name {
			t.Fatalf("canonicalToolName(%q) = %q, want it unchanged", name, got)
		}
	}
}

func TestCanonicalToolArgumentsKeepsUnparsableText(t *testing.T) {
	if got := canonicalToolArguments("write_file", "not json"); got != "not json" {
		t.Fatalf("arguments = %q, want the original text", got)
	}
}

func TestCanonicalToolArgumentsPrefersAnExistingPath(t *testing.T) {
	got := canonicalToolArguments("write_file", `{"path":"kept","filePath":"dropped"}`)
	if !strings.Contains(got, `"path":"kept"`) || strings.Contains(got, "filePath") {
		t.Fatalf("arguments = %q, want the existing path kept and the alias dropped", got)
	}
}
