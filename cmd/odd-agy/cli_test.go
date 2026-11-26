package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func runCLIWithIO(t *testing.T, cwd string, stdinData string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWithIO(args, strings.NewReader(stdinData), &stdout, &stderr, cwd)
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
	for _, cmd := range []string{"upstream", "does-not-exist"} {
		code, _, errOut := runCLI(t, dir, cmd)
		if code != 2 || errOut == "" {
			t.Fatalf("cmd %q => code=%d err=%q, want code=2", cmd, code, errOut)
		}
	}
}

func TestNoArgsAndUICommandInvokeTUI(t *testing.T) {
	dir := t.TempDir()
	orig := runTUI
	defer func() { runTUI = orig }()

	var calledWith []string
	runTUI = func(cwd string) error {
		calledWith = append(calledWith, cwd)
		return nil
	}

	code, _, errOut := runCLI(t, dir)
	if code != 0 || errOut != "" {
		t.Fatalf("no args => code=%d err=%q", code, errOut)
	}
	code, _, errOut = runCLI(t, dir, "ui", "--cwd", dir)
	if code != 0 || errOut != "" {
		t.Fatalf("ui => code=%d err=%q", code, errOut)
	}
	if len(calledWith) != 2 || calledWith[0] != dir || calledWith[1] != dir {
		t.Fatalf("runTUI called with %v, want [%q, %q]", calledWith, dir, dir)
	}
}

func TestInitSyncDoctorUninstallCommands(t *testing.T) {
	dir := t.TempDir()

	code, out, errOut := runCLI(t, dir, "init", "--mode=standalone", "--delegation=organic", "--profile=9router-hybrid")
	if code != 0 || errOut != "" {
		t.Fatalf("init => code=%d out=%q err=%q", code, out, errOut)
	}
	cfg, err := config.LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.InstallMode != config.InstallModeStandalone || cfg.DelegationMode != config.DelegationModeOrganic {
		t.Fatalf("unexpected config after init: %+v", cfg)
	}
	prof, err := config.LoadProfiles(dir)
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	if prof.ActiveProfile != "9router-hybrid" {
		t.Fatalf("ActiveProfile = %q, want 9router-hybrid", prof.ActiveProfile)
	}

	// Sync on clean workspace succeeds.
	code, out, errOut = runCLI(t, dir, "sync")
	if code != 0 || errOut != "" || !strings.Contains(out, "unchanged") {
		t.Fatalf("sync => code=%d out=%q err=%q", code, out, errOut)
	}

	// Doctor outputs check table (may exit 0 or 1 depending on agy on host PATH, so validate output markers).
	code, out, errOut = runCLI(t, dir, "doctor")
	if errOut != "" {
		t.Fatalf("doctor unexpected stderr: %q", errOut)
	}
	if !strings.Contains(out, "[ok]") || !strings.Contains(out, "managed-files") {
		t.Fatalf("doctor output missing expected checks:\n%s", out)
	}
	if code != 0 && code != 1 {
		t.Fatalf("doctor exit code = %d, want 0 or 1", code)
	}

	// Doctor on uninitialized dir fails with exit 1.
	emptyDir := t.TempDir()
	code, out, _ = runCLI(t, emptyDir, "doctor")
	if code != 1 || !strings.Contains(out, "[fail]") {
		t.Fatalf("doctor on empty dir => code=%d out=%q, want code=1 and [fail]", code, out)
	}

	// Uninstall cleans up manifest.
	code, out, errOut = runCLI(t, dir, "uninstall")
	if code != 0 || errOut != "" || !strings.Contains(out, "removed") {
		t.Fatalf("uninstall => code=%d out=%q err=%q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agents", "odd", ".managed-manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("manifest still exists after uninstall")
	}
}

func TestHookAndMCPCommands(t *testing.T) {
	dir := t.TempDir()
	// Save full-delegation config so parent write outside odd/tasks is denied.
	if err := config.SaveConfig(dir, config.DefaultConfig()); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	hookInput, _ := json.Marshal(map[string]any{
		"conversationId": "conv-1",
		"workspacePaths": []string{dir},
		"toolCall": map[string]any{
			"name": "write_to_file",
			"args": map[string]any{
				"TargetFile": filepath.Join(dir, "main.go"),
			},
		},
	})
	code, out, errOut := runCLIWithIO(t, dir, string(hookInput), "hook", "PreToolUse")
	if code != 0 || errOut != "" {
		t.Fatalf("hook PreToolUse => code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, `"decision":"deny"`) {
		t.Fatalf("expected deny decision from PreToolUse hook, got %q", out)
	}

	// MCP initialize + tools/list
	mcpInput := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"
	code, out, errOut = runCLIWithIO(t, dir, mcpInput, "mcp")
	if code != 0 || errOut != "" {
		t.Fatalf("mcp => code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, `"serverInfo"`) || !strings.Contains(out, `"odd_review_start"`) || !strings.Contains(out, `"odd_external_run"`) {
		t.Fatalf("unexpected mcp output:\n%s", out)
	}
}

func TestProfileTestAndProvidersHardening(t *testing.T) {
	dir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv("NINEROUTER_BASE_URL", srv.URL+"/v1")
	code, out, errOut := runCLI(t, dir, "profile", "test", "9router")
	if code != 0 || errOut != "" || !strings.Contains(out, "reachable=true") {
		t.Fatalf("profile test reachable => code=%d out=%q err=%q", code, out, errOut)
	}

	// Unreachable / missing key provider exits 1.
	t.Setenv("DEEPSEEK_API_KEY", "")
	code, out, errOut = runCLI(t, dir, "profile", "test", "deepseek")
	if code != 1 || !strings.Contains(out, "reachable=false") {
		t.Fatalf("profile test unreachable => code=%d out=%q err=%q, want code=1", code, out, errOut)
	}

	// Redacts userinfo/query in profile providers.
	t.Setenv("NINEROUTER_BASE_URL", "http://user:secret@127.0.0.1:29999/v1?token=leak")
	code, out, errOut = runCLI(t, dir, "profile", "providers")
	if code != 0 || errOut != "" {
		t.Fatalf("profile providers redacted => code=%d out=%q err=%q", code, out, errOut)
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "leak") {
		t.Fatalf("profile providers leaked URL credentials: %q", out)
	}

	// Invalid baseUrlEnv override causes profile providers to exit 1.
	t.Setenv("NINEROUTER_BASE_URL", "not-a-valid-url")
	code, out, _ = runCLI(t, dir, "profile", "providers")
	if code != 1 || !strings.Contains(out, "invalid") {
		t.Fatalf("profile providers invalid env => code=%d out=%q, want code=1", code, out)
	}
}
