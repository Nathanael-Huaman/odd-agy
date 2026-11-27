package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var secretKeyNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type secretsFile struct {
	SchemaVersion int               `json:"schemaVersion"`
	Keys          map[string]string `json:"keys"`
}

// SecretsPath resolves <homeDir>/.config/odd-agy/secrets.json (defaulting to os.UserHomeDir).
func SecretsPath(homeDir string) (string, error) {
	home := strings.TrimSpace(homeDir)
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		home = resolved
	}
	return filepath.Join(home, ".config", "odd-agy", "secrets.json"), nil
}

// ValidateSecretKeyName ensures keyEnv is a safe environment-variable identifier.
func ValidateSecretKeyName(keyEnv string) error {
	if !secretKeyNamePattern.MatchString(keyEnv) {
		return fmt.Errorf("invalid secret key name %q: must match ^[A-Za-z_][A-Za-z0-9_]*$", keyEnv)
	}
	return nil
}

func hasSecretControlChars(value string) bool {
	return strings.ContainsAny(value, "\r\n\x00")
}

func checkNotSymlinkDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat secrets directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("secrets directory %s must not be a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("secrets path %s is not a directory", dir)
	}
	return nil
}

// LoadSecrets reads user-scoped API keys from ~/.config/odd-agy/secrets.json,
// rejecting symlinks and control characters without ever echoing secret values in errors.
func LoadSecrets(homeDir string) (map[string]string, error) {
	path, err := SecretsPath(homeDir)
	if err != nil {
		return nil, err
	}
	if err := checkNotSymlinkDir(filepath.Dir(path)); err != nil {
		return nil, err
	}

	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("stat secrets %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("secrets file %s must not be a symlink", path)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("secrets path %s is a directory", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read secrets %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("parse secrets %s: invalid JSON", path)
	}

	var sf secretsFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse secrets %s: invalid JSON", path)
	}
	if sf.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("invalid secrets schemaVersion %d in %s: must be %d", sf.SchemaVersion, path, SchemaVersion)
	}

	out := make(map[string]string, len(sf.Keys))
	for k, v := range sf.Keys {
		if err := ValidateSecretKeyName(k); err != nil {
			return nil, err
		}
		if hasSecretControlChars(v) {
			return nil, fmt.Errorf("secret value for %s contains invalid control characters", k)
		}
		out[k] = v
	}
	return out, nil
}

// SetSecret stores or deletes keyEnv in ~/.config/odd-agy/secrets.json (0700 dir, 0600 file).
// When strings.TrimSpace(value) == "", keyEnv is removed from the stored map.
func SetSecret(homeDir, keyEnv, value string) error {
	if err := ValidateSecretKeyName(keyEnv); err != nil {
		return err
	}
	if hasSecretControlChars(value) {
		return fmt.Errorf("secret value for %s must not contain CR, LF, or NUL", keyEnv)
	}

	path, err := SecretsPath(homeDir)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)

	existing, err := LoadSecrets(homeDir)
	if err != nil {
		return err
	}

	if strings.TrimSpace(value) == "" {
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		delete(existing, keyEnv)
	} else {
		existing[keyEnv] = value
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("create config directory %s: %w", filepath.Dir(dir), err)
	}
	if err := checkNotSymlinkDir(dir); err != nil {
		return err
	}
	if _, statErr := os.Lstat(dir); errors.Is(statErr, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("create secrets directory %s: %w", dir, err)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod secrets directory %s: %w", dir, err)
	}

	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("secrets file %s must not be a symlink", path)
		}
		if info.IsDir() {
			return fmt.Errorf("secrets path %s is a directory", path)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stat secrets %s: %w", path, statErr)
	}

	payload, err := json.MarshalIndent(secretsFile{
		SchemaVersion: SchemaVersion,
		Keys:          existing,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal secrets %s", path)
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, ".tmp-secrets-*")
	if err != nil {
		return fmt.Errorf("create temp secrets file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp secrets file: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp secrets file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp secrets file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp secrets file to %s: %w", path, err)
	}
	cleanup = false
	return nil
}

// LookupSecret resolves keyEnv from getenv first ("env"), then ~/.config/odd-agy/secrets.json ("stored").
func LookupSecret(homeDir, keyEnv string, getenv func(string) string) (value string, source string) {
	key := strings.TrimSpace(keyEnv)
	if key == "" {
		return "", ""
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if val := getenv(key); strings.TrimSpace(val) != "" {
		return val, "env"
	}
	if secrets, err := LoadSecrets(homeDir); err == nil {
		if val, ok := secrets[key]; ok && strings.TrimSpace(val) != "" {
			return val, "stored"
		}
	}
	return "", ""
}

// NewSecretsEnvLookup returns a string-valued lookup function backed by getenv and stored secrets.
func NewSecretsEnvLookup(homeDir string, getenv func(string) string) func(string) string {
	return func(key string) string {
		val, _ := LookupSecret(homeDir, key, getenv)
		return val
	}
}

// LookupSecretWithEnv adapts a (string, bool) lookup function to LookupSecret.
func LookupSecretWithEnv(homeDir, keyEnv string, lookupEnv func(string) (string, bool)) (value string, source string) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return LookupSecret(homeDir, keyEnv, func(k string) string {
		v, ok := lookupEnv(k)
		if !ok {
			return ""
		}
		return v
	})
}

// NewSecretsLookupEnv returns an os.LookupEnv-compatible function backed by lookupEnv and stored secrets.
func NewSecretsLookupEnv(homeDir string, lookupEnv func(string) (string, bool)) func(string) (string, bool) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return func(key string) (string, bool) {
		if val, ok := lookupEnv(key); ok && strings.TrimSpace(val) != "" {
			return val, true
		}
		if secrets, err := LoadSecrets(homeDir); err == nil {
			if val, ok := secrets[key]; ok && strings.TrimSpace(val) != "" {
				return val, true
			}
		}
		return "", false
	}
}
