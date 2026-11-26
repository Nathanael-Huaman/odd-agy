package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/version"
)

func runCLI(t *testing.T, cwd string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, cwd)
	return code, stdout.String(), stderr.String()
}

func TestVersionAndHelp(t *testing.T) {
	dir := t.TempDir()

	for _, args := range [][]string{{"--version"}, {"version"}} {
		code, out, errOut := runCLI(t, dir, args...)
		if code != 0 || errOut != "" || !strings.Contains(out, version.Version) {
			t.Fatalf("args %v => code=%d out=%q err=%q", args, code, out, errOut)
		}
	}

	for _, args := range [][]string{{"--help"}, {"help"}} {
		code, out, errOut := runCLI(t, dir, args...)
		if code != 0 || errOut != "" || !strings.Contains(out, "mode") || !strings.Contains(out, "rdd") || !strings.Contains(out, "profile") {
			t.Fatalf("args %v => code=%d out=%q err=%q", args, code, out, errOut)
		}
	}
}

func TestModeCommand(t *testing.T) {
	dir := t.TempDir()

	code, out, errOut := runCLI(t, dir, "mode", "status")
	if code != 0 || errOut != "" || !strings.Contains(out, config.DelegationModeFull) {
		t.Fatalf("mode status => code=%d out=%q err=%q", code, out, errOut)
	}

	code, out, errOut = runCLI(t, dir, "mode", "organic")
	if code != 0 || errOut != "" || !strings.Contains(out, "organic") {
		t.Fatalf("mode organic => code=%d out=%q err=%q", code, out, errOut)
	}

	before, err := os.ReadFile(config.ConfigPath(dir))
	if err != nil {
		t.Fatalf("ReadFile config: %v", err)
	}

	// Invalid mode must exit non-zero and leave stored config unchanged.
	code, _, errOut = runCLI(t, dir, "mode", "invalid")
	if code != 2 || errOut == "" {
		t.Fatalf("mode invalid => code=%d err=%q, want code=2", code, errOut)
	}
	after, err := os.ReadFile(config.ConfigPath(dir))
	if err != nil {
		t.Fatalf("ReadFile config after invalid: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config mutated on invalid mode")
	}
}

func TestRDDCommand(t *testing.T) {
	dir := t.TempDir()

	code, out, errOut := runCLI(t, dir, "rdd", "status")
	if code != 0 || errOut != "" || !strings.Contains(out, "enabled") {
		t.Fatalf("rdd status => code=%d out=%q err=%q", code, out, errOut)
	}

	code, out, errOut = runCLI(t, dir, "rdd", "disable")
	if code != 0 || errOut != "" || !strings.Contains(out, "disabled") {
		t.Fatalf("rdd disable => code=%d out=%q err=%q", code, out, errOut)
	}

	before, err := os.ReadFile(config.ConfigPath(dir))
	if err != nil {
		t.Fatalf("ReadFile config: %v", err)
	}

	code, _, errOut = runCLI(t, dir, "rdd", "bogus")
	if code != 2 || errOut == "" {
		t.Fatalf("rdd bogus => code=%d err=%q, want code=2", code, errOut)
	}
	after, err := os.ReadFile(config.ConfigPath(dir))
	if err != nil {
		t.Fatalf("ReadFile config after invalid: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config mutated on invalid rdd subcommand")
	}

	code, out, errOut = runCLI(t, dir, "rdd", "enable")
	if code != 0 || errOut != "" || !strings.Contains(out, "enabled") {
		t.Fatalf("rdd enable => code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestProfileCommandAndCwdFlag(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()

	// --cwd before and after subcommand.
	code, out, errOut := runCLI(t, other, "--cwd", dir, "profile", "list")
	if code != 0 || errOut != "" || !strings.Contains(out, "* agy-oauth") {
		t.Fatalf("profile list => code=%d out=%q err=%q", code, out, errOut)
	}

	code, out, errOut = runCLI(t, other, "profile", "use", "deepseek-hybrid", "--cwd", dir)
	if code != 0 || errOut != "" || !strings.Contains(out, "deepseek-hybrid") {
		t.Fatalf("profile use => code=%d out=%q err=%q", code, out, errOut)
	}

	before, err := os.ReadFile(config.ProfilesPath(dir))
	if err != nil {
		t.Fatalf("ReadFile profiles: %v", err)
	}

	code, _, errOut = runCLI(t, dir, "profile", "use", "unknown-profile")
	if code != 2 || errOut == "" {
		t.Fatalf("profile use unknown => code=%d err=%q, want code=2", code, errOut)
	}
	after, err := os.ReadFile(config.ProfilesPath(dir))
	if err != nil {
		t.Fatalf("ReadFile profiles after invalid: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("profiles.json mutated on invalid profile")
	}

	code, out, errOut = runCLI(t, dir, "profile", "show")
	if code != 0 || errOut != "" || !strings.Contains(out, "deepseek-hybrid") || !strings.Contains(out, "review-risk") {
		t.Fatalf("profile show => code=%d out=%q err=%q", code, out, errOut)
	}

	t.Setenv("NINEROUTER_BASE_URL", "http://127.0.0.1:29999/v1")
	code, out, errOut = runCLI(t, dir, "profile", "providers")
	if code != 0 || errOut != "" || !strings.Contains(out, "http://127.0.0.1:29999/v1") || !strings.Contains(out, "omniroute") {
		t.Fatalf("profile providers => code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestStubAndUnknownCommands(t *testing.T) {
	dir := t.TempDir()
	for _, cmd := range []string{"init", "sync", "doctor", "uninstall", "hook", "mcp", "ui", "upstream", "does-not-exist"} {
		code, _, errOut := runCLI(t, dir, cmd)
		if code != 2 || errOut == "" {
			t.Fatalf("cmd %q => code=%d err=%q, want code=2", cmd, code, errOut)
		}
	}
}
