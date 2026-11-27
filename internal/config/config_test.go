package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", cfg.SchemaVersion)
	}
	if cfg.DelegationMode != DelegationModeFull {
		t.Fatalf("DelegationMode = %q, want %q", cfg.DelegationMode, DelegationModeFull)
	}
	if !cfg.RDDEnabled {
		t.Fatalf("RDDEnabled = false, want true")
	}
	if cfg.InstallMode != InstallModeLocal {
		t.Fatalf("InstallMode = %q, want %q", cfg.InstallMode, InstallModeLocal)
	}
	if cfg.BinaryCommand == "" {
		t.Fatalf("BinaryCommand is empty, want non-empty default")
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig(DefaultConfig()) unexpected error: %v", err)
	}
}

func TestConfigPath(t *testing.T) {
	got := ConfigPath("/tmp/my-repo")
	want := filepath.Join("/tmp/my-repo", ".agents", "odd", "config.json")
	if got != want {
		t.Fatalf("ConfigPath = %q, want %q", got, want)
	}
}

func TestValidateConfigRejectsInvalid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"bad schema", func(c *Config) { c.SchemaVersion = 0 }},
		{"bad delegation mode", func(c *Config) { c.DelegationMode = "chaos" }},
		{"bad install mode", func(c *Config) { c.InstallMode = "global" }},
		{"empty binary command", func(c *Config) { c.BinaryCommand = "   " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(&cfg)
			if err := ValidateConfig(cfg); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestLoadAndSaveConfigPreservesStoredOnRejection(t *testing.T) {
	dir := t.TempDir()

	// Missing file returns defaults.
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig missing file error: %v", err)
	}
	if cfg != DefaultConfig() {
		t.Fatalf("LoadConfig missing file = %+v, want %+v", cfg, DefaultConfig())
	}

	// Save valid config.
	cfg.DelegationMode = DelegationModeOrganic
	cfg.RDDEnabled = false
	cfg.InstallMode = InstallModeStandalone
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatalf("SaveConfig valid error: %v", err)
	}

	path := ConfigPath(dir)
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}

	loaded, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig saved error: %v", err)
	}
	if loaded != cfg {
		t.Fatalf("loaded = %+v, want %+v", loaded, cfg)
	}

	// Attempt to save invalid config -> must fail and keep stored bytes intact.
	invalid := loaded
	invalid.DelegationMode = "invalid-mode"
	if err := SaveConfig(dir, invalid); err == nil {
		t.Fatalf("SaveConfig invalid expected error, got nil")
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) after failed save: %v", path, err)
	}
	if string(afterBytes) != string(beforeBytes) {
		t.Fatalf("stored config changed after rejected SaveConfig:\nbefore: %s\nafter:  %s", beforeBytes, afterBytes)
	}
}
