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
	original := "[providers.router]\r\ntype = \"openai\"\r\nbase_url = \"https://openrouter.ai/api/v1\" # keep\r\napi_key = \"secret\"\r\n\r\n[models.test]\r\nprovider = \"router\"\r\n"
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
	for _, want := range []string{
		`type = "openai"`,
		`default_model = "openrouter/moonshotai/kimi-k3"`,
		`[models."openrouter/moonshotai/kimi-k3"]`,
		`[providers.openrouter.source]`,
		`kind = "apiJson"`,
		`url = "` + proxyRegistryURL + `"`,
		`apiKey = "fresh-key"`,
		`provider = "openrouter"`,
		`model = "moonshotai/kimi-k3"`,
		`max_context_size = 1048576`,
		`"tool_use"`,
	} {
		if !strings.Contains(string(kimi), want) {
			t.Errorf("fresh Kimi config missing %s", want)
		}
	}
	if changed, err := patchKimiConfig(kimiPath, "fresh-key", true); err != nil || changed {
		t.Fatalf("repeat integration changed=%v err=%v", changed, err)
	}
	if fileExists(kimiPath + ".orr-backup") {
		t.Fatal("idempotent integration created a backup")
	}
}

func TestKimiConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", "")
	t.Setenv("KIMI_SHARE_DIR", filepath.Join(home, "legacy"))
	if got, want := kimiConfigPath(home), filepath.Join(home, ".kimi-code", "config.toml"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	custom := filepath.Join(home, "custom")
	t.Setenv("KIMI_CODE_HOME", custom)
	if got, want := kimiConfigPath(home), filepath.Join(custom, "config.toml"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestPatchKimiConfigRejectsUnsupportedOpenRouterProtocols(t *testing.T) {
	for _, providerType := range []string{"openai_legacy", "google-genai", "vertexai"} {
		t.Run(providerType, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			original := "[providers.openrouter]\ntype = \"" + providerType + "\"\napi_key = \"secret\"\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			changed, err := patchKimiConfig(path, "new-key", true)
			if changed || err == nil || !strings.Contains(err.Error(), "unsupported Kimi provider type") {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("error exposed credentials")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != original || fileExists(path+".orr-backup") {
				t.Fatal("unsupported config was modified")
			}
		})
	}
}

func TestPatchKimiConfigPreservesProviderProtocolsAndUsesTheirBasePaths(t *testing.T) {
	for _, providerType := range []string{"openai", "openai_responses", "kimi", "anthropic"} {
		t.Run(providerType, func(t *testing.T) {
			for _, existingURL := range []string{"", "base_url = \"" + proxyBaseURL + "\" # keep\n"} {
				path := filepath.Join(t.TempDir(), "config.toml")
				model := "\n[models.chosen]\nprovider = \"openrouter\"\nmodel = \"some/model\"\n"
				original := "default_model = \"chosen\"\n\n[providers.openrouter]\ntype = \"" + providerType + "\"\n" + existingURL + model
				if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
				if changed, err := patchKimiConfig(path, "key", false); !changed || err != nil {
					t.Fatalf("integration changed=%v err=%v", changed, err)
				}
				baseURL := proxyBaseURL
				if providerType == "anthropic" {
					baseURL = strings.TrimSuffix(baseURL, "/v1")
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"base_url = \"" + baseURL + "\"", "type = \"" + providerType + "\"", "default_model = \"chosen\"", model} {
					if !strings.Contains(string(got), want) {
						t.Fatalf("protocol, base path, or selection changed: missing %q", want)
					}
				}
				if existingURL != "" && !strings.Contains(string(got), "# keep") {
					t.Fatal("base URL comment was removed")
				}
				if changed, err := patchKimiConfig(path, "key", false); changed || err != nil {
					t.Fatalf("repeat integration changed=%v err=%v", changed, err)
				}
			}
		})
	}
}

func TestPatchKimiConfigPreservesSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "default_model = \"chosen\"\n\n[providers.openrouter]\ntype = \"openai_responses\"\n\n[models.chosen]\nprovider = \"openrouter\"\nmodel = \"some/model\"\nmax_context_size = 32000\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := patchKimiConfig(path, "new-key", false); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	for _, want := range []string{`default_model = "chosen"`, `type = "openai_responses"`, strings.Split(original, "[models.chosen]")[1]} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("existing selection changed: missing %q", want)
		}
	}
	backup, _ := os.ReadFile(path + ".orr-backup")
	if string(backup) != original {
		t.Fatal("backup differs from original")
	}
}

func TestPatchKimiConfigMissingAndEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if changed, err := patchKimiConfig(path, "key", false); changed || err != nil || fileExists(path) {
		t.Fatalf("absent client: changed=%v err=%v", changed, err)
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := patchKimiConfig(path, "key", false); !changed || err != nil {
		t.Fatalf("empty config: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `default_model = "openrouter/moonshotai/kimi-k3"`) {
		t.Fatal("empty config did not receive a default model")
	}
}

func TestPatchKimiRegistrySource(t *testing.T) {
	for _, tc := range []struct{ name, source, key, wantKey string }{
		{"add", "", "new-key", `"new-key"`},
		{"existing-key", "", "", `'existing-key'`},
		{"refresh", "\n[providers.'openrouter'.source]\nkind = 'apiJson' # kind\nurl = 'https://old.example/registry' # url\napiKey = 'old-key' # key\n", "new-key", `"new-key"`},
		{"partial", "\n[providers.openrouter.source]\nkind = \"apiJson\"\n", "new-key", `"new-key"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			model := "\n[models.chosen]\nprovider = 'openrouter'\nmodel = 'some/model'\n"
			original := "default_model = 'chosen'\n[providers.'openrouter']\ntype = 'openai'\nbase_url = '" + proxyBaseURL + "'\napi_key = 'existing-key'\n" + tc.source + model
			original = strings.ReplaceAll(original, "\n", "\r\n")
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if changed, err := patchKimiConfig(path, tc.key, false); err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			data, _ := os.ReadFile(path)
			got := string(data)
			for _, want := range []string{`kind = "apiJson"`, `url = "` + proxyRegistryURL + `"`, "apiKey = " + tc.wantKey, "default_model = 'chosen'", strings.ReplaceAll(model, "\n", "\r\n")} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q", want)
				}
			}
			if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
				t.Error("CRLF not preserved")
			}
			if tc.name == "refresh" {
				for _, comment := range []string{"# kind", "# url", "# key"} {
					if !strings.Contains(got, comment) {
						t.Errorf("lost %s", comment)
					}
				}
			}
			backup, _ := os.ReadFile(path + ".orr-backup")
			if string(backup) != original {
				t.Fatal("backup differs")
			}
			if changed, err := patchKimiConfig(path, tc.key, false); err != nil || changed {
				t.Fatalf("repeat changed=%v err=%v", changed, err)
			}
			backup, _ = os.ReadFile(path + ".orr-backup")
			if string(backup) != original {
				t.Fatal("repeat overwrote backup")
			}
		})
	}
}

func TestPatchKimiRegistryDoesNotAttachToOtherIdentities(t *testing.T) {
	for _, tc := range []struct{ name, protocol string }{
		{"router", "openai"}, {"openrouter", "anthropic"}, {"openrouter", "openai_responses"}, {"openrouter", "kimi"},
	} {
		t.Run(tc.name+"/"+tc.protocol, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			original := "[providers." + tc.name + "]\ntype = \"" + tc.protocol + "\"\nbase_url = \"https://openrouter.ai/api/v1\"\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := patchKimiConfig(path, "key", false); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			if strings.Contains(string(got), ".source]") {
				t.Fatal("registry would replace provider identity or protocol")
			}
		})
	}
}
