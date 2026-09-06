package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchOpenCodeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.json")
	original := `{"theme":"dark","provider":{"openrouter":{"options":{"timeout":30}}}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := patchOpenCodeConfig(path, "new-key", false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `"baseURL":"`+proxyBaseURL+`"`) || !strings.Contains(string(got), `"apiKey":"new-key"`) || !strings.Contains(string(got), `"theme":"dark"`) {
		t.Fatalf("unexpected config: %s", got)
	}
	backup, _ := os.ReadFile(path + ".orr-backup")
	if string(backup) != original {
		t.Fatalf("backup = %q", backup)
	}
}

func TestPatchOpenCodeConfigPreservesJSONC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	original := "{\n  // Keep this comment.\n  \"provider\": {\n    \"openrouter\": {\n      \"options\": {\n        \"timeout\": 30,\n      },\n    },\n  },\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := patchOpenCodeConfig(path, "new-key", false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "// Keep this comment.") || !strings.Contains(string(got), `"baseURL":"`+proxyBaseURL+`"`) || !strings.Contains(string(got), `"apiKey":"new-key"`) {
		t.Fatalf("unexpected config: %s", got)
	}
	backup, _ := os.ReadFile(path + ".orr-backup")
	if string(backup) != original {
		t.Fatalf("backup = %q", backup)
	}
}

func TestOpenCodeConfigPathPrefersExistingJSONC(t *testing.T) {
	t.Setenv("OPENCODE_CONFIG", "")
	home := t.TempDir()
	directory := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(directory, "opencode.json")
	jsoncPath := filepath.Join(directory, "opencode.jsonc")
	if err := os.WriteFile(jsonPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsoncPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := openCodeConfigPath(home); got != jsoncPath {
		t.Fatalf("path = %q, want %q", got, jsoncPath)
	}
}

func TestOpenCodeConfigPathDefaultsToJSONC(t *testing.T) {
	t.Setenv("OPENCODE_CONFIG", "")
	home := t.TempDir()
	want := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	if got := openCodeConfigPath(home); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestPatchKimiConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[providers.router]\r\ntype = \"openai_legacy\"\r\nbase_url = \"https://openrouter.ai/api/v1\" # keep\r\napi_key = \"secret\"\r\n\r\n[models.test]\r\nprovider = \"router\"\r\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := patchKimiConfig(path, "new-key", false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `base_url = "`+proxyBaseURL+`" # keep`) || !strings.Contains(string(got), `api_key = "new-key"`) {
		t.Fatalf("unexpected config: %s", got)
	}
	if !strings.Contains(string(got), "\r\n") {
		t.Fatal("CRLF was not preserved")
	}
}

func TestPatchKimiConfigIgnoresOtherProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[providers.kimi]\nbase_url = \"https://api.kimi.com/coding/v1\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := patchKimiConfig(path, "new-key", false)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}

func TestPatchFreshClientConfigs(t *testing.T) {
	dir := t.TempDir()
	openCodePath := filepath.Join(dir, "opencode", "opencode.json")
	changed, err := patchOpenCodeConfig(openCodePath, "fresh-key", true)
	if err != nil || !changed {
		t.Fatalf("OpenCode changed=%v err=%v", changed, err)
	}
	openCode, _ := os.ReadFile(openCodePath)
	if !strings.Contains(string(openCode), `"apiKey":"fresh-key"`) {
		t.Fatalf("OpenCode config: %s", openCode)
	}

	kimiPath := filepath.Join(dir, "kimi", "config.toml")
	changed, err = patchKimiConfig(kimiPath, "fresh-key", true)
	if err != nil || !changed {
		t.Fatalf("Kimi changed=%v err=%v", changed, err)
	}
	kimi, _ := os.ReadFile(kimiPath)
	if !strings.Contains(string(kimi), `api_key = "fresh-key"`) || !strings.Contains(string(kimi), `base_url = "`+proxyBaseURL+`"`) {
		t.Fatalf("Kimi config: %s", kimi)
	}
}
