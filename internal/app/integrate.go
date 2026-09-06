package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/tailscale/hujson"
)

const proxyBaseURL = "http://127.0.0.1:8787/v1"

var (
	tomlSection = regexp.MustCompile(`^\s*\[([^]]+)]\s*(?:#.*)?$`)
	tomlBaseURL = regexp.MustCompile(`^(\s*base_url\s*=\s*)(["']).*(["'])(\s*(?:#.*)?)$`)
	tomlAPIKey  = regexp.MustCompile(`^(\s*api_key\s*=\s*)(["']).*(["'])(\s*(?:#.*)?)$`)
)

func runIntegrate(envPath string, output io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}

	fileEnv, err := readDotEnv(envPath)
	if err != nil {
		return err
	}
	apiKey := envValue(fileEnv, "OPENROUTER_API_KEY")

	paths := []integrationPath{
		{path: filepath.Join(home, ".kimi", "config.toml"), kind: "kimi", create: commandExists("kimi")},
		{path: filepath.Join(home, ".kimi-code", "config.toml"), kind: "kimi", create: commandExists("kimi-code")},
	}
	if custom := strings.TrimSpace(os.Getenv("KIMI_SHARE_DIR")); custom != "" {
		paths = append([]integrationPath{{path: filepath.Join(custom, "config.toml"), kind: "kimi", create: commandExists("kimi") || commandExists("kimi-code")}}, paths...)
	}
	paths = append(paths, integrationPath{path: openCodeConfigPath(home), kind: "opencode", create: commandExists("opencode")})

	seen := make(map[string]bool)
	for _, candidate := range paths {
		path := filepath.Clean(candidate.path)
		if seen[path] {
			continue
		}
		seen[path] = true
		var changed bool
		if candidate.kind == "kimi" {
			changed, err = patchKimiConfig(path, apiKey, candidate.create)
		} else {
			changed, err = patchOpenCodeConfig(path, apiKey, candidate.create)
		}
		if err != nil {
			return err
		}
		if changed {
			if fileExists(path + ".orr-backup") {
				fmt.Fprintf(output, "Patched %s (backup: %s)\n", path, path+".orr-backup")
			} else {
				fmt.Fprintf(output, "Created %s\n", path)
			}
		} else if fileExists(path) {
			fmt.Fprintf(output, "Left %s unchanged\n", path)
		}
	}
	return nil
}

type integrationPath struct {
	path   string
	kind   string
	create bool
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func openCodeConfigPath(home string) string {
	if custom := strings.TrimSpace(os.Getenv("OPENCODE_CONFIG")); custom != "" {
		return custom
	}
	directory := filepath.Join(home, ".config", "opencode")
	for _, name := range []string{"opencode.jsonc", "opencode.json"} {
		path := filepath.Join(directory, name)
		if fileExists(path) {
			return path
		}
	}
	return filepath.Join(directory, "opencode.jsonc")
}

func patchOpenCodeConfig(path, apiKey string, create bool) (bool, error) {
	data, err := os.ReadFile(path)
	original := data
	if os.IsNotExist(err) {
		if !create {
			return false, nil
		}
		data = []byte("{}\n")
		original = nil
		err = nil
	}
	if err != nil {
		return false, fmt.Errorf("read OpenCode config %s: %w", path, err)
	}
	standard, err := hujson.Standardize(bytes.Clone(data))
	if err != nil {
		return false, fmt.Errorf("parse OpenCode config %s: %w", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(standard, &root); err != nil {
		return false, fmt.Errorf("parse OpenCode config %s: %w", path, err)
	}
	providers, providersOK := root["provider"].(map[string]any)
	if !providersOK {
		providers = make(map[string]any)
		root["provider"] = providers
	}
	openrouter, openrouterOK := providers["openrouter"].(map[string]any)
	if !openrouterOK {
		openrouter = make(map[string]any)
		providers["openrouter"] = openrouter
	}
	options, optionsOK := openrouter["options"].(map[string]any)
	if !optionsOK {
		options = make(map[string]any)
		openrouter["options"] = options
	}
	unchanged := options["baseURL"] == proxyBaseURL
	if apiKey != "" {
		unchanged = unchanged && options["apiKey"] == apiKey
		options["apiKey"] = apiKey
	}
	if unchanged {
		return false, nil
	}
	options["baseURL"] = proxyBaseURL

	document, err := hujson.Parse(data)
	if err != nil {
		return false, fmt.Errorf("parse OpenCode config %s: %w", path, err)
	}
	switch {
	case !providersOK:
		err = setJSONCMember(&document, "provider", providers)
	case !openrouterOK:
		err = setJSONCMember(document.Find("/provider"), "openrouter", openrouter)
	case !optionsOK:
		err = setJSONCMember(document.Find("/provider/openrouter"), "options", options)
	default:
		optionsValue := document.Find("/provider/openrouter/options")
		err = setJSONCMember(optionsValue, "baseURL", proxyBaseURL)
		if err == nil && apiKey != "" {
			err = setJSONCMember(optionsValue, "apiKey", apiKey)
		}
	}
	if err != nil {
		return false, fmt.Errorf("update OpenCode config %s: %w", path, err)
	}
	updated := document.Pack()
	if len(updated) == 0 || updated[len(updated)-1] != '\n' {
		updated = append(updated, '\n')
	}
	return true, writeIntegratedFile(path, original, updated)
}

func setJSONCMember(parent *hujson.Value, name string, value any) error {
	if parent == nil {
		return errors.New("parent object is missing")
	}
	object, ok := parent.Value.(*hujson.Object)
	if !ok {
		return errors.New("parent value is not an object")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	replacement, err := hujson.Parse(encoded)
	if err != nil {
		return err
	}
	for i := range object.Members {
		literal, ok := object.Members[i].Name.Value.(hujson.Literal)
		if ok && literal.String() == name {
			object.Members[i].Value.Value = replacement.Value
			return nil
		}
	}
	object.Members = append(object.Members, hujson.ObjectMember{
		Name:  hujson.Value{Value: hujson.String(name)},
		Value: replacement,
	})
	return nil
}

func patchKimiConfig(path, apiKey string, create bool) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if !create {
			return false, nil
		}
		content := "[providers.openrouter]\n" +
			"type = \"openai_legacy\"\n" +
			"base_url = " + strconv.Quote(proxyBaseURL) + "\n"
		if apiKey != "" {
			content += "api_key = " + strconv.Quote(apiKey) + "\n"
		}
		return true, writeIntegratedFile(path, nil, []byte(content))
	}
	if err != nil {
		return false, fmt.Errorf("read Kimi config %s: %w", path, err)
	}
	newline := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		newline = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	changed := false
	for start := 0; start < len(lines); {
		match := tomlSection.FindStringSubmatch(lines[start])
		if match == nil || !strings.HasPrefix(match[1], "providers.") {
			start++
			continue
		}
		end := start + 1
		for end < len(lines) && tomlSection.FindStringSubmatch(lines[end]) == nil {
			end++
		}
		name := strings.Trim(strings.TrimPrefix(match[1], "providers."), `"' `)
		target := strings.EqualFold(name, "openrouter")
		baseLine := -1
		apiKeyLine := -1
		for i := start + 1; i < end; i++ {
			if tomlBaseURL.MatchString(lines[i]) {
				baseLine = i
				if strings.Contains(strings.ToLower(lines[i]), "openrouter.ai") {
					target = true
				}
			}
			if tomlAPIKey.MatchString(lines[i]) {
				apiKeyLine = i
			}
		}
		if target {
			if baseLine >= 0 && !strings.Contains(lines[baseLine], proxyBaseURL) {
				parts := tomlBaseURL.FindStringSubmatch(lines[baseLine])
				lines[baseLine] = parts[1] + strconv.Quote(proxyBaseURL) + parts[4]
				changed = true
			}
			if apiKey != "" && apiKeyLine >= 0 && !strings.Contains(lines[apiKeyLine], strconv.Quote(apiKey)) {
				parts := tomlAPIKey.FindStringSubmatch(lines[apiKeyLine])
				lines[apiKeyLine] = parts[1] + strconv.Quote(apiKey) + parts[4]
				changed = true
			}
			missing := make([]string, 0, 2)
			if baseLine < 0 {
				missing = append(missing, "base_url = "+strconv.Quote(proxyBaseURL))
			}
			if apiKey != "" && apiKeyLine < 0 {
				missing = append(missing, "api_key = "+strconv.Quote(apiKey))
			}
			if len(missing) > 0 {
				lines = append(lines[:start+1], append(missing, lines[start+1:]...)...)
				end += len(missing)
				changed = true
			}
		}
		start = end
	}
	if !changed {
		return false, nil
	}
	return true, writeIntegratedFile(path, data, []byte(strings.Join(lines, newline)))
}

func writeIntegratedFile(path string, original, updated []byte) error {
	if len(original) == 0 {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create config directory for %s: %w", path, err)
		}
		if err := os.WriteFile(path, updated, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return nil
	}
	return replaceWithBackup(path, original, updated)
}

func replaceWithBackup(path string, original, updated []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".orr-backup", original, info.Mode().Perm()); err != nil {
		return fmt.Errorf("back up %s: %w", path, err)
	}
	if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
