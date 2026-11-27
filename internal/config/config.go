package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	SchemaVersion = 1

	DelegationModeFull    = "full-delegation"
	DelegationModeOrganic = "organic"

	InstallModeLocal      = "local"
	InstallModeStandalone = "standalone"

	DefaultBinaryCommand = "odd-agy"
)

// Config holds project-scoped ODD/RDD harness settings stored at .agents/odd/config.json.
type Config struct {
	SchemaVersion  int    `json:"schemaVersion"`
	DelegationMode string `json:"delegationMode"`
	RDDEnabled     bool   `json:"rddEnabled"`
	InstallMode    string `json:"installMode"`
	BinaryCommand  string `json:"binaryCommand"`
}

// ConfigPath returns the path to .agents/odd/config.json under cwd.
func ConfigPath(cwd string) string {
	return filepath.Join(cwd, ".agents", "odd", "config.json")
}

// DefaultConfig returns the default project configuration.
func DefaultConfig() Config {
	return Config{
		SchemaVersion:  SchemaVersion,
		DelegationMode: DelegationModeFull,
		RDDEnabled:     true,
		InstallMode:    InstallModeLocal,
		BinaryCommand:  DefaultBinaryCommand,
	}
}

// ValidateConfig checks that all Config fields hold allowed values.
func ValidateConfig(cfg Config) error {
	if cfg.SchemaVersion != SchemaVersion {
		return fmt.Errorf("invalid schemaVersion %d: must be %d", cfg.SchemaVersion, SchemaVersion)
	}
	switch cfg.DelegationMode {
	case DelegationModeFull, DelegationModeOrganic:
	default:
		return fmt.Errorf("invalid delegationMode %q: must be %q or %q", cfg.DelegationMode, DelegationModeFull, DelegationModeOrganic)
	}
	switch cfg.InstallMode {
	case InstallModeLocal, InstallModeStandalone:
	default:
		return fmt.Errorf("invalid installMode %q: must be %q or %q", cfg.InstallMode, InstallModeLocal, InstallModeStandalone)
	}
	if strings.TrimSpace(cfg.BinaryCommand) == "" {
		return errors.New("invalid binaryCommand: must not be empty")
	}
	return nil
}

// LoadConfig reads .agents/odd/config.json under cwd, returning DefaultConfig if missing.
func LoadConfig(cwd string) (Config, error) {
	path := ConfigPath(cwd)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultConfig(), nil
		}
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := ValidateConfig(cfg); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", path, err)
	}
	return cfg, nil
}

// SaveConfig validates cfg and atomically writes .agents/odd/config.json under cwd.
func SaveConfig(cwd string, cfg Config) error {
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	return writeJSONAtomic(ConfigPath(cwd), cfg)
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json for %s: %w", path, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	cleanup = false
	return nil
}
