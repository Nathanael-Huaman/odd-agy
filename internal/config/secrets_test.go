package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSecretsPathAndKeyValidation(t *testing.T) {
	home := t.TempDir()
	got, err := SecretsPath(home)
	if err != nil {
		t.Fatalf("SecretsPath(%q) unexpected error: %v", home, err)
	}
	want := filepath.Join(home, ".config", "odd-agy", "secrets.json")
	if got != want {
		t.Fatalf("SecretsPath(%q) = %q, want %q", home, got, want)
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	gotEmpty, err := SecretsPath("")
	if err != nil {
		t.Fatalf("SecretsPath(\"\") unexpected error: %v", err)
	}
	wantEmpty := filepath.Join(fakeHome, ".config", "odd-agy", "secrets.json")
	if gotEmpty != wantEmpty {
		t.Fatalf("SecretsPath(\"\") = %q, want %q", gotEmpty, wantEmpty)
	}

	for _, valid := range []string{"DEEPSEEK_API_KEY", "NINEROUTER_API_KEY", "_PRIVATE_1", "a"} {
		if err := ValidateSecretKeyName(valid); err != nil {
			t.Errorf("ValidateSecretKeyName(%q) unexpected error: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "   ", "1BAD", "BAD-KEY", "BAD KEY", "BAD/KEY", "BAD.KEY", "BAD\nKEY", "BAD\x00KEY"} {
		if err := ValidateSecretKeyName(invalid); err == nil {
			t.Errorf("ValidateSecretKeyName(%q) expected error, got nil", invalid)
		}
	}
}

func TestSetAndLoadSecretsPermissionsAndClear(t *testing.T) {
	home := t.TempDir()

	// Missing secrets.json returns empty map cleanly.
	initial, err := LoadSecrets(home)
	if err != nil {
		t.Fatalf("LoadSecrets missing: %v", err)
	}
	if len(initial) != 0 {
		t.Fatalf("expected empty map on missing file, got %d entries", len(initial))
	}

	const secretA = "sk-live-secret-alpha-123"
	const secretB = "  sk-keep-leading-space  "
	if err := SetSecret(home, "DEEPSEEK_API_KEY", secretA); err != nil {
		t.Fatalf("SetSecret A: %v", err)
	}
	if err := SetSecret(home, "NINEROUTER_API_KEY", secretB); err != nil {
		t.Fatalf("SetSecret B: %v", err)
	}

	secretsPath, err := SecretsPath(home)
	if err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Dir(secretsPath)

	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(secretsDir)
		if err != nil {
			t.Fatalf("stat secrets dir: %v", err)
		}
		if gotPerm := dirInfo.Mode().Perm(); gotPerm != 0o700 {
			t.Fatalf("secrets dir perm = %04o, want 0700", gotPerm)
		}

		fileInfo, err := os.Stat(secretsPath)
		if err != nil {
			t.Fatalf("stat secrets file: %v", err)
		}
		if gotPerm := fileInfo.Mode().Perm(); gotPerm != 0o600 {
			t.Fatalf("secrets file perm = %04o, want 0600", gotPerm)
		}
	}

	loaded, err := LoadSecrets(home)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if loaded["DEEPSEEK_API_KEY"] != secretA {
		t.Fatalf("DEEPSEEK_API_KEY mismatch")
	}
	if loaded["NINEROUTER_API_KEY"] != secretB {
		t.Fatalf("NINEROUTER_API_KEY exact value not preserved")
	}

	// Clearing via empty or whitespace-only value deletes the key.
	if err := SetSecret(home, "DEEPSEEK_API_KEY", "   "); err != nil {
		t.Fatalf("SetSecret clear: %v", err)
	}
	loadedAfterClear, err := LoadSecrets(home)
	if err != nil {
		t.Fatalf("LoadSecrets after clear: %v", err)
	}
	if _, exists := loadedAfterClear["DEEPSEEK_API_KEY"]; exists {
		t.Fatalf("expected DEEPSEEK_API_KEY to be deleted on whitespace value")
	}
	if loadedAfterClear["NINEROUTER_API_KEY"] != secretB {
		t.Fatalf("expected NINEROUTER_API_KEY to remain after clearing DEEPSEEK_API_KEY")
	}
}

func TestSecretsRejectsControlCharsSymlinksAndLeaks(t *testing.T) {
	home := t.TempDir()

	// Control characters rejected without echoing secret value.
	for _, badVal := range []string{"sk-bad-xyz\nvalue-abc", "sk-bad-xyz\rvalue-abc", "sk-bad-xyz\x00value-abc"} {
		err := SetSecret(home, "DEEPSEEK_API_KEY", badVal)
		if err == nil {
			t.Fatalf("expected control char rejection")
		}
		if strings.Contains(err.Error(), "sk-bad-xyz") || strings.Contains(err.Error(), "value-abc") {
			t.Fatalf("SetSecret error leaked secret value: %v", err)
		}
	}

	// Invalid key name rejected.
	if err := SetSecret(home, "BAD/KEY", "sk-never-stored"); err == nil {
		t.Fatalf("expected invalid key name rejection")
	} else if strings.Contains(err.Error(), "sk-never-stored") {
		t.Fatalf("SetSecret invalid key error leaked secret: %v", err)
	}

	// Symlink on secrets file rejected on both LoadSecrets and SetSecret.
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "outside.json")
	const sentinel = `{"schemaVersion":1,"keys":{"DEEPSEEK_API_KEY":"sk-outside-do-not-read"}}`
	if err := os.WriteFile(outsideTarget, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgDir := filepath.Join(home, ".config", "odd-agy")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(cfgDir, "secrets.json")
	if err := os.Symlink(outsideTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadSecrets(home); err == nil {
		t.Fatalf("expected LoadSecrets to reject symlinked secrets.json")
	} else if strings.Contains(err.Error(), "sk-outside-do-not-read") {
		t.Fatalf("LoadSecrets error leaked secret: %v", err)
	}
	if err := SetSecret(home, "DEEPSEEK_API_KEY", "sk-new"); err == nil {
		t.Fatalf("expected SetSecret to reject symlinked secrets.json")
	}
	if got, _ := os.ReadFile(outsideTarget); string(got) != sentinel {
		t.Fatalf("symlink target was mutated: %s", string(got))
	}

	// Symlink on odd-agy directory rejected.
	home2 := t.TempDir()
	outsideDir2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home2, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir2, filepath.Join(home2, ".config", "odd-agy")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecrets(home2); err == nil {
		t.Fatalf("expected LoadSecrets to reject symlinked odd-agy dir")
	}
	if err := SetSecret(home2, "DEEPSEEK_API_KEY", "sk-new"); err == nil {
		t.Fatalf("expected SetSecret to reject symlinked odd-agy dir")
	}

	// Corrupted JSON never leaks secret fragments in error.
	home3 := t.TempDir()
	sp3, _ := SecretsPath(home3)
	if err := os.MkdirAll(filepath.Dir(sp3), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp3, []byte(`{"schemaVersion":1,"keys":{"DEEPSEEK_API_KEY":"sk-corrupt-leak"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecrets(home3); err == nil {
		t.Fatalf("expected LoadSecrets to fail on corrupt JSON")
	} else if strings.Contains(err.Error(), "sk-corrupt-leak") {
		t.Fatalf("LoadSecrets corrupt JSON error leaked secret: %v", err)
	}
}

func TestLookupSecretPrecedenceAndEnvWrappers(t *testing.T) {
	home := t.TempDir()
	if err := SetSecret(home, "DEEPSEEK_API_KEY", "sk-stored-value"); err != nil {
		t.Fatal(err)
	}

	// Empty keyEnv returns "", "".
	if v, src := LookupSecret(home, "", func(string) string { return "ignored" }); v != "" || src != "" {
		t.Fatalf("LookupSecret empty key = (%q, %q), want (\"\", \"\")", v, src)
	}

	// Env takes precedence over stored.
	v, src := LookupSecret(home, "DEEPSEEK_API_KEY", func(k string) string {
		if k == "DEEPSEEK_API_KEY" {
			return "sk-env-value"
		}
		return ""
	})
	if v != "sk-env-value" || src != "env" {
		t.Fatalf("LookupSecret env precedence = (%q, %q), want (sk-env-value, env)", v, src)
	}

	// Stored returned when env is empty.
	v, src = LookupSecret(home, "DEEPSEEK_API_KEY", func(string) string { return "" })
	if v != "sk-stored-value" || src != "stored" {
		t.Fatalf("LookupSecret stored = (%q, %q), want (sk-stored-value, stored)", v, src)
	}

	// Unset key returns "", "".
	v, src = LookupSecret(home, "OPENAI_API_KEY", func(string) string { return "" })
	if v != "" || src != "" {
		t.Fatalf("LookupSecret unset = (%q, %q), want (\"\", \"\")", v, src)
	}

	// NewSecretsEnvLookup returns combined string lookup.
	envLookup := NewSecretsEnvLookup(home, func(k string) string {
		if k == "OPENAI_API_KEY" {
			return "sk-openai-env"
		}
		return ""
	})
	if got := envLookup("DEEPSEEK_API_KEY"); got != "sk-stored-value" {
		t.Fatalf("NewSecretsEnvLookup(DEEPSEEK_API_KEY) = %q, want sk-stored-value", got)
	}
	if got := envLookup("OPENAI_API_KEY"); got != "sk-openai-env" {
		t.Fatalf("NewSecretsEnvLookup(OPENAI_API_KEY) = %q, want sk-openai-env", got)
	}
	if got := envLookup("MISSING_KEY"); got != "" {
		t.Fatalf("NewSecretsEnvLookup(MISSING_KEY) = %q, want \"\"", got)
	}
}
