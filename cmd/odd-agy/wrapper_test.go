package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Nathanael-Huaman/odd-agy/internal/version"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	return root
}

func TestWrapperBuildAndRun(t *testing.T) {
	root := repoRoot(t)

	buildCmd := exec.Command("node", filepath.Join(root, "scripts", "build-binaries.mjs"))
	buildCmd.Dir = root
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build-binaries.mjs failed: %v\n%s", err, out)
	}

	verCmd := exec.Command("node", filepath.Join(root, "bin", "odd-agy.mjs"), "--version")
	verCmd.Dir = root
	out, err := verCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bin/odd-agy.mjs --version failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), version.Version) {
		t.Fatalf("wrapper --version output = %q, want %q", string(out), version.Version)
	}

	// Verify child exit code propagation on invalid subcommand.
	errCmd := exec.Command("node", filepath.Join(root, "bin", "odd-agy.mjs"), "mode", "invalid")
	errCmd.Dir = root
	if _, err := errCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected non-zero exit code for invalid mode")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("exit code = %v, want 2", err)
	}

	// Verify ODD_AGY_BIN override.
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	builtBin := filepath.Join(root, "dist", "bin", "odd-agy"+ext)
	overrideCmd := exec.Command("node", filepath.Join(root, "bin", "odd-agy.mjs"), "--version")
	overrideCmd.Dir = root
	overrideCmd.Env = append(overrideCmd.Environ(), "ODD_AGY_BIN="+builtBin)
	out2, err := overrideCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ODD_AGY_BIN override failed: %v\n%s", err, out2)
	}
	if !strings.Contains(string(out2), version.Version) {
		t.Fatalf("override output = %q, want %q", string(out2), version.Version)
	}

	// Verify ODD_AGY_BIN pointing to missing binary fails cleanly.
	missingCmd := exec.Command("node", filepath.Join(root, "bin", "odd-agy.mjs"), "--version")
	missingCmd.Dir = root
	missingCmd.Env = append(missingCmd.Environ(), "ODD_AGY_BIN="+filepath.Join(t.TempDir(), "missing-bin"))
	if _, err := missingCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected error for missing ODD_AGY_BIN")
	}
}
