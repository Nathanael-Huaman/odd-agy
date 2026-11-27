package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Nathanael-Huaman/odd-agy/internal/assets"
	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/installer"
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
	for _, cmd := range []string{"does-not-exist"} {
		code, _, errOut := runCLI(t, dir, cmd)
		if code != 2 || errOut == "" {
			t.Fatalf("cmd %q => code=%d err=%q, want code=2", cmd, code, errOut)
		}
	}
}

func TestUpstreamCommand(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	gsDir := filepath.Join(srcDir, "gentle-shell")
	if err := os.MkdirAll(filepath.Join(gsDir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gsDir, "assets", "orchestrator.md"), []byte("# orch v1\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	lockPath := filepath.Join(dir, "upstream.lock.json")
	lockJSON := `{
  "schemaVersion": 1,
  "updatedAt": "2026-04-20T00:00:00Z",
  "repos": [{"name": "Gentleman-Programming/gentle-shell", "ref": "v5.0.0"}],
  "entries": [{
    "id": "orch",
    "repo": "Gentleman-Programming/gentle-shell",
    "path": "assets/orchestrator.md",
    "category": "prompts-and-skills",
    "sha256": "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  }]
}
`
	if err := os.WriteFile(lockPath, []byte(lockJSON), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	// Drift detected -> exit 1 with classification output.
	code, out, errOut := runCLI(t, dir, "upstream", "check", "--lock", lockPath, "--source-dir", srcDir)
	if code != 1 || errOut != "" || !strings.Contains(out, "[prompts-and-skills]") || !strings.Contains(out, "[changed]") {
		t.Fatalf("upstream check drift => code=%d out=%q err=%q", code, out, errOut)
	}

	// Sync updates digests & validates contracts -> exit 0.
	code, out, errOut = runCLI(t, dir, "upstream", "sync", "--lock", lockPath, "--source-dir", srcDir, "--ref", "v5.0.1")
	if code != 0 || errOut != "" || !strings.Contains(out, "validated=true") {
		t.Fatalf("upstream sync => code=%d out=%q err=%q", code, out, errOut)
	}

	// Re-check -> exit 0 unchanged.
	code, out, errOut = runCLI(t, dir, "upstream", "check", "--lock", lockPath, "--source-dir", srcDir)
	if code != 0 || errOut != "" || !strings.Contains(out, "1 unchanged, 0 changed, 0 missing") {
		t.Fatalf("upstream check clean => code=%d out=%q err=%q", code, out, errOut)
	}

	// Help text no longer lists Planned commands.
	code, helpOut, _ := runCLI(t, dir, "--help")
	if code != 0 || strings.Contains(helpOut, "Planned commands:") || !strings.Contains(helpOut, "upstream check") {
		t.Fatalf("help output unexpected:\n%s", helpOut)
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
	t.Setenv("HOME", t.TempDir())

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

func TestLaunchCommand(t *testing.T) {
	dir := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	origLookPath := lookPath
	origRunner := runExternalCmd
	defer func() {
		lookPath = origLookPath
		runExternalCmd = origRunner
	}()

	var called bool
	var gotDir, gotBin string
	var gotArgs []string
	nextExit := 0

	lookPath = func(bin string) (string, error) {
		if bin == "agy" {
			return "/usr/local/bin/agy", nil
		}
		return "", errors.New("not found")
	}
	runExternalCmd = func(workDir string, _ io.Reader, _, _ io.Writer, name string, args ...string) int {
		called = true
		gotDir = workDir
		gotBin = name
		gotArgs = append([]string(nil), args...)
		return nextExit
	}

	// 1. Fails clearly when not initialized yet.
	code, _, errOut := runCLI(t, dir, "launch")
	if code != 1 || !strings.Contains(errOut, "odd-agy init") {
		t.Fatalf("launch before init => code=%d err=%q, want code=1 instructing odd-agy init", code, errOut)
	}
	if called {
		t.Fatalf("expected agy not to be invoked before init")
	}

	// Initialize workspace.
	code, _, errOut = runCLI(t, dir, "init")
	if code != 0 || errOut != "" {
		t.Fatalf("init => code=%d err=%q", code, errOut)
	}
	absSettings, err := filepath.Abs(installer.SessionSettingsPath(dir))
	if err != nil {
		t.Fatal(err)
	}

	// 2. Default launch passes --agent odd-orchestrator --settings <abs>
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 0 || errOut != "" {
		t.Fatalf("launch default => code=%d err=%q", code, errOut)
	}
	if !called || gotDir != dir || gotBin != "/usr/local/bin/agy" {
		t.Fatalf("unexpected runner call: called=%t dir=%q bin=%q", called, gotDir, gotBin)
	}
	wantDefault := []string{"--agent", "odd-orchestrator", "--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantDefault) {
		t.Fatalf("default launch args = %v, want %v", gotArgs, wantDefault)
	}

	// 3. Custom --agent and extra args after --
	called = false
	code, _, errOut = runCLI(t, dir, "launch", "--agent=custom-agent", "--", "--model", "flash", "-p", "hello")
	if code != 0 || errOut != "" {
		t.Fatalf("launch custom => code=%d err=%q", code, errOut)
	}
	wantCustom := []string{"--agent", "custom-agent", "--settings", absSettings, "--model", "flash", "-p", "hello"}
	if !reflect.DeepEqual(gotArgs, wantCustom) {
		t.Fatalf("custom launch args = %v, want %v", gotArgs, wantCustom)
	}

	// 4. --no-agent omits --agent
	called = false
	code, _, errOut = runCLI(t, dir, "launch", "--no-agent", "--", "--resume")
	if code != 0 || errOut != "" {
		t.Fatalf("launch --no-agent => code=%d err=%q", code, errOut)
	}
	wantNoAgent := []string{"--settings", absSettings, "--resume"}
	if !reflect.DeepEqual(gotArgs, wantNoAgent) {
		t.Fatalf("no-agent launch args = %v, want %v", gotArgs, wantNoAgent)
	}

	// 5. Propagates exact exit code from agy.
	nextExit = 7
	code, _, _ = runCLI(t, dir, "launch")
	if code != 7 {
		t.Fatalf("launch exit propagation => code=%d, want 7", code)
	}
	nextExit = 0

	// 6. Unknown flag before -- exits 2 without invoking agy; --cwd after -- is passed verbatim to agy.
	called = false
	code, _, errOut = runCLI(t, dir, "launch", "--unknown-flag")
	if code != 2 || errOut == "" || called {
		t.Fatalf("launch unknown flag => code=%d err=%q called=%t", code, errOut, called)
	}

	called = false
	code, _, errOut = runCLI(t, dir, "launch", "--agent", "")
	if code != 2 || errOut == "" || called {
		t.Fatalf("launch empty agent => code=%d err=%q called=%t", code, errOut, called)
	}

	called = false
	otherDir := t.TempDir()
	code, _, errOut = runCLI(t, otherDir, "--cwd", dir, "launch", "--", "--cwd", "/pass-through")
	if code != 0 || errOut != "" || !called || gotDir != dir {
		t.Fatalf("launch --cwd before & after -- => code=%d err=%q called=%t dir=%q", code, errOut, called, gotDir)
	}
	wantPassThrough := []string{"--agent", "odd-orchestrator", "--settings", absSettings, "--cwd", "/pass-through"}
	if !reflect.DeepEqual(gotArgs, wantPassThrough) {
		t.Fatalf("pass-through launch args = %v, want %v", gotArgs, wantPassThrough)
	}

	// 7. Missing agy on PATH fails clearly.
	lookPath = func(string) (string, error) {
		return "", errors.New("executable file not found")
	}
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 1 || !strings.Contains(errOut, "agy") {
		t.Fatalf("launch missing agy => code=%d err=%q", code, errOut)
	}

	// Zero writes to ~/.gemini/
	if _, err := os.Stat(filepath.Join(fakeHome, ".gemini")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("launch/init wrote ~/.gemini unexpectedly: err=%v", err)
	}
}

func TestInitAllowMCPAndPermissionsCommand(t *testing.T) {
	dir := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Without --allow-mcp, ~/.gemini/ stays untouched.
	code, _, errOut := runCLI(t, dir, "init")
	if code != 0 || errOut != "" {
		t.Fatalf("init => code=%d err=%q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(fakeHome, ".gemini")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("init without --allow-mcp touched ~/.gemini: err=%v", err)
	}

	// permissions with no args defaults to status.
	code, out, errOut := runCLI(t, dir, "permissions")
	if code != 0 || errOut != "" || !strings.Contains(out, "disabled") {
		t.Fatalf("permissions default status => code=%d out=%q err=%q", code, out, errOut)
	}

	// permissions status reports disabled.
	code, out, errOut = runCLI(t, dir, "permissions", "status")
	if code != 0 || errOut != "" || !strings.Contains(out, "disabled") {
		t.Fatalf("permissions status => code=%d out=%q err=%q", code, out, errOut)
	}

	// init --allow-mcp grants global MCP permissions AND registers global MCP server.
	code, out, errOut = runCLI(t, dir, "init", "--allow-mcp")
	if code != 0 || errOut != "" || !strings.Contains(out, "globalMCP=enabled") {
		t.Fatalf("init --allow-mcp => code=%d out=%q err=%q", code, out, errOut)
	}
	enabled, err := installer.CheckGlobalMCPPermissions(fakeHome)
	if err != nil || !enabled {
		t.Fatalf("CheckGlobalMCPPermissions after init --allow-mcp = (%t, %v)", enabled, err)
	}
	reg, cmd, err := installer.GlobalMCPServerRegistered(fakeHome)
	if err != nil || !reg || cmd == "" {
		t.Fatalf("GlobalMCPServerRegistered after init --allow-mcp = (%t, %q, %v)", reg, cmd, err)
	}

	// permissions status reports both enabled and registered.
	code, out, errOut = runCLI(t, dir, "permissions", "status")
	if code != 0 || errOut != "" || !strings.Contains(out, "enabled") || !strings.Contains(out, "registered") {
		t.Fatalf("permissions status enabled => code=%d out=%q err=%q", code, out, errOut)
	}

	// permissions revoke-mcp removes both rules and registration (PRESERVE).
	code, out, errOut = runCLI(t, dir, "permissions", "revoke-mcp")
	if code != 0 || errOut != "" || !strings.Contains(out, "revoked") {
		t.Fatalf("permissions revoke-mcp => code=%d out=%q err=%q", code, out, errOut)
	}
	enabled, err = installer.CheckGlobalMCPPermissions(fakeHome)
	if err != nil || enabled {
		t.Fatalf("CheckGlobalMCPPermissions after revoke-mcp = (%t, %v)", enabled, err)
	}
	reg, _, err = installer.GlobalMCPServerRegistered(fakeHome)
	if err != nil || reg {
		t.Fatalf("GlobalMCPServerRegistered after revoke-mcp = (%t, %v)", reg, err)
	}

	// permissions allow-mcp re-grants both (PRESERVE).
	code, out, errOut = runCLI(t, dir, "permissions", "allow-mcp")
	if code != 0 || errOut != "" || !strings.Contains(out, "enabled") {
		t.Fatalf("permissions allow-mcp => code=%d out=%q err=%q", code, out, errOut)
	}
	enabled, err = installer.CheckGlobalMCPPermissions(fakeHome)
	if err != nil || !enabled {
		t.Fatalf("CheckGlobalMCPPermissions after allow-mcp = (%t, %v)", enabled, err)
	}
	reg, _, err = installer.GlobalMCPServerRegistered(fakeHome)
	if err != nil || !reg {
		t.Fatalf("GlobalMCPServerRegistered after allow-mcp = (%t, %v)", reg, err)
	}

	// permissions disable & enable --binary=<path>.
	code, out, errOut = runCLI(t, dir, "permissions", "disable")
	if code != 0 || errOut != "" || !strings.Contains(out, "revoked") {
		t.Fatalf("permissions disable => code=%d out=%q err=%q", code, out, errOut)
	}
	if enabled, _ := installer.CheckGlobalMCPPermissions(fakeHome); enabled {
		t.Fatalf("expected permissions disabled after disable")
	}
	if reg, _, _ := installer.GlobalMCPServerRegistered(fakeHome); reg {
		t.Fatalf("expected MCP unregistered after disable")
	}

	code, out, errOut = runCLI(t, dir, "permissions", "enable", "--binary=/custom/odd-agy")
	if code != 0 || errOut != "" || !strings.Contains(out, "enabled") || !strings.Contains(out, "/custom/odd-agy") {
		t.Fatalf("permissions enable --binary => code=%d out=%q err=%q", code, out, errOut)
	}
	if enabled, _ := installer.CheckGlobalMCPPermissions(fakeHome); !enabled {
		t.Fatalf("expected permissions enabled after enable")
	}
	if reg, regCmd, _ := installer.GlobalMCPServerRegistered(fakeHome); !reg || regCmd != "/custom/odd-agy" {
		t.Fatalf("GlobalMCPServerRegistered after enable --binary = (%t, %q), want (true, /custom/odd-agy)", reg, regCmd)
	}

	// permissions status reports disabled when unregistered.
	_, _, _ = runCLI(t, dir, "permissions", "disable")
	code, out, errOut = runCLI(t, dir, "permissions", "status")
	if code != 0 || errOut != "" || !strings.Contains(out, "disabled") || !strings.Contains(out, "unregistered") {
		t.Fatalf("permissions status disabled => code=%d out=%q err=%q", code, out, errOut)
	}

	// Invalid permissions subcommand exits 2.
	code, _, errOut = runCLI(t, dir, "permissions", "bogus")
	if code != 2 || errOut == "" {
		t.Fatalf("permissions bogus => code=%d err=%q, want code=2", code, errOut)
	}

	// Help text includes launch and permissions.
	code, helpOut, _ := runCLI(t, dir, "--help")
	if code != 0 || !strings.Contains(helpOut, "launch") || !strings.Contains(helpOut, "permissions") || !strings.Contains(helpOut, "--allow-mcp") {
		t.Fatalf("help missing new commands:\n%s", helpOut)
	}
}

func TestLaunchRejectsConflictingAgentAndNoAgentFlags(t *testing.T) {
	dir := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	origLookPath := lookPath
	origRunner := runExternalCmd
	defer func() {
		lookPath = origLookPath
		runExternalCmd = origRunner
	}()

	called := false
	lookPath = func(bin string) (string, error) {
		if bin == "agy" {
			return "/usr/local/bin/agy", nil
		}
		return "", errors.New("not found")
	}
	runExternalCmd = func(_ string, _ io.Reader, _, _ io.Writer, _ string, _ ...string) int {
		called = true
		return 0
	}

	code, _, errOut := runCLI(t, dir, "init")
	if code != 0 || errOut != "" {
		t.Fatalf("init => code=%d err=%q", code, errOut)
	}

	cases := [][]string{
		{"launch", "--agent", "custom", "--no-agent"},
		{"launch", "--no-agent", "--agent=custom"},
	}
	for _, args := range cases {
		called = false
		code, out, errOut := runCLI(t, dir, args...)
		if code != exitUsage {
			t.Fatalf("args %v => code=%d (out=%q err=%q), want exitUsage=%d", args, code, out, errOut, exitUsage)
		}
		if !strings.Contains(errOut, "--agent") || !strings.Contains(errOut, "--no-agent") {
			t.Fatalf("args %v => stderr=%q, want clear conflicting flags message", args, errOut)
		}
		if called {
			t.Fatalf("args %v => agy must NOT be invoked when flags conflict", args)
		}
	}
}

func TestRunExternalCmdExitAndSignalHandling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh and SIGTERM signal termination test is Unix-only")
	}

	dir := t.TempDir()

	t.Run("exit_0", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runExternalCmd(dir, strings.NewReader(""), &stdout, &stderr, "sh", "-c", "exit 0")
		if code != 0 {
			t.Fatalf("code = %d, want 0 (stderr=%q)", code, stderr.String())
		}
		if stderr.String() != "" {
			t.Fatalf("unexpected stderr: %q", stderr.String())
		}
	})

	t.Run("exit_7", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runExternalCmd(dir, strings.NewReader(""), &stdout, &stderr, "sh", "-c", "exit 7")
		if code != 7 {
			t.Fatalf("code = %d, want 7 (stderr=%q)", code, stderr.String())
		}
		if stderr.String() != "" {
			t.Fatalf("unexpected stderr on normal non-zero exit: %q", stderr.String())
		}
	})

	t.Run("terminated_by_signal", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runExternalCmd(dir, strings.NewReader(""), &stdout, &stderr, "sh", "-c", "kill -TERM $$")
		if code != 1 {
			t.Fatalf("code = %d, want 1 (exitError) for signal termination (stderr=%q)", code, stderr.String())
		}
		errOut := stderr.String()
		if errOut == "" || !strings.Contains(errOut, "terminated by signal") {
			t.Fatalf("expected non-empty signal diagnostic on stderr, got %q", errOut)
		}
	})
}

func TestProfileUseSyncsAgentFrontmatterInInitializedWorkspace(t *testing.T) {
	dir := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	code, _, errOut := runCLI(t, dir, "init")
	if code != 0 || errOut != "" {
		t.Fatalf("init => code=%d err=%q", code, errOut)
	}

	profs, err := config.LoadProfiles(dir)
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	ds := profs.Profiles["deepseek-hybrid"]
	ds.Roles[config.RoleOrchestrator] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-medium"}
	ds.Roles["explore"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-low"}
	ds.Roles["worker"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "claude-sonnet-4-6"}
	profs.Profiles["deepseek-hybrid"] = ds
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	code, out, errOut := runCLI(t, dir, "profile", "use", "deepseek-hybrid")
	if code != 0 || errOut != "" || !strings.Contains(out, "deepseek-hybrid") {
		t.Fatalf("profile use deepseek-hybrid => code=%d out=%q err=%q", code, out, errOut)
	}

	orchBytes, err := os.ReadFile(filepath.Join(dir, ".agents", "agents", "odd-orchestrator.md"))
	if err != nil {
		t.Fatalf("read odd-orchestrator.md: %v", err)
	}
	orchFM, _, err := assets.ParseFrontmatter(orchBytes)
	if err != nil {
		t.Fatalf("ParseFrontmatter orchestrator: %v", err)
	}
	if orchFM.Model != "inherit" {
		t.Fatalf("odd-orchestrator.md model = %q, want inherit", orchFM.Model)
	}

	expBytes, err := os.ReadFile(filepath.Join(dir, ".agents", "agents", "gentle-ai-explore.md"))
	if err != nil {
		t.Fatalf("read gentle-ai-explore.md: %v", err)
	}
	expFM, _, err := assets.ParseFrontmatter(expBytes)
	if err != nil {
		t.Fatalf("ParseFrontmatter explore: %v", err)
	}
	if expFM.Model != "flash" {
		t.Fatalf("gentle-ai-explore.md model = %q, want flash", expFM.Model)
	}

	wrkBytes, err := os.ReadFile(filepath.Join(dir, ".agents", "agents", "gentle-ai-worker.md"))
	if err != nil {
		t.Fatalf("read gentle-ai-worker.md: %v", err)
	}
	wrkFM, _, err := assets.ParseFrontmatter(wrkBytes)
	if err != nil {
		t.Fatalf("ParseFrontmatter worker: %v", err)
	}
	if wrkFM.Model != "pro" {
		t.Fatalf("gentle-ai-worker.md model = %q, want pro", wrkFM.Model)
	}

	syncRes, err := installer.Sync(dir, installer.SyncOptions{})
	if err != nil || len(syncRes.Conflicts) != 0 || len(syncRes.Updated) != 0 {
		t.Fatalf("Sync after profile use: res=%+v err=%v", syncRes, err)
	}

	// Error from ApplyActiveProfileModels (symlink escape) must be printed to stderr and return exitError.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.md")
	const sentinel = "---\nname: outside\nmodel: original\n---\n\nbody\n"
	if err := os.WriteFile(outsideFile, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	explorePath := filepath.Join(dir, ".agents", "agents", "gentle-ai-explore.md")
	if err := os.Remove(explorePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, explorePath); err != nil {
		t.Fatal(err)
	}

	code, _, errOut = runCLI(t, dir, "profile", "use", "agy-oauth")
	if code != exitError || errOut == "" {
		t.Fatalf("profile use with symlink escape => code=%d err=%q, want code=%d and stderr error", code, errOut, exitError)
	}
	if got, _ := os.ReadFile(outsideFile); string(got) != sentinel {
		t.Fatalf("external symlink target was mutated: %q", string(got))
	}
}

func TestLaunchModelInjection(t *testing.T) {
	dir := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	origLookPath := lookPath
	origRunner := runExternalCmd
	defer func() {
		lookPath = origLookPath
		runExternalCmd = origRunner
	}()

	var gotArgs []string
	lookPath = func(bin string) (string, error) {
		if bin == "agy" {
			return "/usr/local/bin/agy", nil
		}
		return "", errors.New("not found")
	}
	runExternalCmd = func(_ string, _ io.Reader, _, _ io.Writer, _ string, args ...string) int {
		gotArgs = append([]string(nil), args...)
		return 0
	}

	code, _, errOut := runCLI(t, dir, "init")
	if code != 0 || errOut != "" {
		t.Fatalf("init => code=%d err=%q", code, errOut)
	}
	absSettings, err := filepath.Abs(installer.SessionSettingsPath(dir))
	if err != nil {
		t.Fatal(err)
	}

	// Update active profile orchestrator model to gemini-3.8-flash-high.
	profs, err := config.LoadProfiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	active := profs.Profiles[profs.ActiveProfile]
	active.Roles[config.RoleOrchestrator] = config.RoleRoute{
		Runtime: config.RuntimeAgyNative,
		Model:   "gemini-3.8-flash-high",
	}
	profs.Profiles[profs.ActiveProfile] = active
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatal(err)
	}

	// 1. Default odd-orchestrator without --model injects --model from active profile.
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 0 || errOut != "" {
		t.Fatalf("launch => code=%d err=%q", code, errOut)
	}
	wantInjected := []string{"--agent", "odd-orchestrator", "--model", "gemini-3.8-flash-high", "--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantInjected) {
		t.Fatalf("launch injected args = %v, want %v", gotArgs, wantInjected)
	}

	// 2. Explicit --model <id> in extraArgs suppresses injection.
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch", "--", "--model", "flash", "-p", "hi")
	if code != 0 || errOut != "" {
		t.Fatalf("launch with --model => code=%d err=%q", code, errOut)
	}
	wantExplicitSpace := []string{"--agent", "odd-orchestrator", "--settings", absSettings, "--model", "flash", "-p", "hi"}
	if !reflect.DeepEqual(gotArgs, wantExplicitSpace) {
		t.Fatalf("launch with --model args = %v, want %v", gotArgs, wantExplicitSpace)
	}

	// 3. Explicit --model=<id> in extraArgs suppresses injection.
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch", "--", "--model=claude-sonnet-4-6")
	if code != 0 || errOut != "" {
		t.Fatalf("launch with --model= => code=%d err=%q", code, errOut)
	}
	wantExplicitEq := []string{"--agent", "odd-orchestrator", "--settings", absSettings, "--model=claude-sonnet-4-6"}
	if !reflect.DeepEqual(gotArgs, wantExplicitEq) {
		t.Fatalf("launch with --model= args = %v, want %v", gotArgs, wantExplicitEq)
	}

	// 4. --no-agent suppresses injection.
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch", "--no-agent")
	if code != 0 || errOut != "" {
		t.Fatalf("launch --no-agent => code=%d err=%q", code, errOut)
	}
	wantNoAgent := []string{"--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantNoAgent) {
		t.Fatalf("launch --no-agent args = %v, want %v", gotArgs, wantNoAgent)
	}

	// 5. Custom --agent=<other> suppresses injection.
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch", "--agent=custom-agent")
	if code != 0 || errOut != "" {
		t.Fatalf("launch --agent=custom-agent => code=%d err=%q", code, errOut)
	}
	wantCustomAgent := []string{"--agent", "custom-agent", "--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantCustomAgent) {
		t.Fatalf("launch --agent=custom-agent args = %v, want %v", gotArgs, wantCustomAgent)
	}

	// 6. orchestrator model == "inherit" suppresses injection.
	active.Roles[config.RoleOrchestrator] = config.RoleRoute{
		Runtime: config.RuntimeAgyNative,
		Model:   "inherit",
	}
	profs.Profiles[profs.ActiveProfile] = active
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatal(err)
	}
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 0 || errOut != "" {
		t.Fatalf("launch inherit => code=%d err=%q", code, errOut)
	}
	wantInherit := []string{"--agent", "odd-orchestrator", "--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantInherit) {
		t.Fatalf("launch inherit args = %v, want %v", gotArgs, wantInherit)
	}

	// 7. Missing profiles.json suppresses injection cleanly.
	if err := os.Remove(config.ProfilesPath(dir)); err != nil {
		t.Fatal(err)
	}
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 0 || errOut != "" {
		t.Fatalf("launch without profiles.json => code=%d err=%q", code, errOut)
	}
	if !reflect.DeepEqual(gotArgs, wantInherit) {
		t.Fatalf("launch without profiles.json args = %v, want %v", gotArgs, wantInherit)
	}
}

func TestProfileUsePreservesProfilesJSONWhenModelSyncFails(t *testing.T) {
	t.Run("symlink_escape", func(t *testing.T) {
		dir := t.TempDir()
		fakeHome := t.TempDir()
		t.Setenv("HOME", fakeHome)

		code, _, errOut := runCLI(t, dir, "init")
		if code != 0 || errOut != "" {
			t.Fatalf("init => code=%d err=%q", code, errOut)
		}

		// Configure deepseek-hybrid with custom agy-native models so switching from agy-oauth
		// attempts to update both odd-orchestrator.md and gentle-ai-explore.md.
		profs, err := config.LoadProfiles(dir)
		if err != nil {
			t.Fatalf("LoadProfiles: %v", err)
		}
		ds := profs.Profiles["deepseek-hybrid"]
		ds.Roles[config.RoleOrchestrator] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-high"}
		ds.Roles["explore"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "gemini-3.8-flash-low"}
		profs.Profiles["deepseek-hybrid"] = ds
		if err := config.SaveProfiles(dir, profs); err != nil {
			t.Fatalf("SaveProfiles: %v", err)
		}

		beforeProfiles, err := os.ReadFile(config.ProfilesPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		orchPath := filepath.Join(dir, ".agents", "agents", "odd-orchestrator.md")
		beforeOrch, err := os.ReadFile(orchPath)
		if err != nil {
			t.Fatal(err)
		}

		outsideDir := t.TempDir()
		outsideFile := filepath.Join(outsideDir, "outside.md")
		const sentinel = "---\nname: outside\nmodel: original\n---\n\nbody\n"
		if err := os.WriteFile(outsideFile, []byte(sentinel), 0o644); err != nil {
			t.Fatal(err)
		}
		explorePath := filepath.Join(dir, ".agents", "agents", "gentle-ai-explore.md")
		if err := os.Remove(explorePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideFile, explorePath); err != nil {
			t.Fatal(err)
		}

		code, _, errOut = runCLI(t, dir, "profile", "use", "deepseek-hybrid")
		if code != exitError || errOut == "" {
			t.Fatalf("profile use with symlink escape => code=%d err=%q, want code=%d and stderr error", code, errOut, exitError)
		}

		afterProfiles, err := os.ReadFile(config.ProfilesPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(afterProfiles, beforeProfiles) {
			t.Fatalf("profiles.json was mutated despite failed model sync:\nbefore:\n%s\nafter:\n%s", beforeProfiles, afterProfiles)
		}
		afterOrch, err := os.ReadFile(orchPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(afterOrch, beforeOrch) {
			t.Fatalf("odd-orchestrator.md was partially written despite preflight failure")
		}
		if got, _ := os.ReadFile(outsideFile); string(got) != sentinel {
			t.Fatalf("external symlink target mutated: %q", string(got))
		}
	})

	t.Run("corrupted_manifest", func(t *testing.T) {
		dir := t.TempDir()
		fakeHome := t.TempDir()
		t.Setenv("HOME", fakeHome)

		code, _, errOut := runCLI(t, dir, "init")
		if code != 0 || errOut != "" {
			t.Fatalf("init => code=%d err=%q", code, errOut)
		}

		beforeProfiles, err := os.ReadFile(config.ProfilesPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(installer.ManifestPath(dir), []byte("{corrupted json"), 0o644); err != nil {
			t.Fatal(err)
		}

		code, _, errOut = runCLI(t, dir, "profile", "use", "deepseek-hybrid")
		if code != exitError || errOut == "" {
			t.Fatalf("profile use with corrupted manifest => code=%d err=%q, want code=%d and stderr error", code, errOut, exitError)
		}

		afterProfiles, err := os.ReadFile(config.ProfilesPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(afterProfiles, beforeProfiles) {
			t.Fatalf("profiles.json was mutated despite corrupted manifest:\nbefore:\n%s\nafter:\n%s", beforeProfiles, afterProfiles)
		}
	})
}

func TestLaunchModelInjectionEdgeCases(t *testing.T) {
	dir := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	origLookPath := lookPath
	origRunner := runExternalCmd
	defer func() {
		lookPath = origLookPath
		runExternalCmd = origRunner
	}()

	var called bool
	var gotArgs []string
	lookPath = func(bin string) (string, error) {
		if bin == "agy" {
			return "/usr/local/bin/agy", nil
		}
		return "", errors.New("not found")
	}
	runExternalCmd = func(_ string, _ io.Reader, _, _ io.Writer, _ string, args ...string) int {
		called = true
		gotArgs = append([]string(nil), args...)
		return 0
	}

	code, _, errOut := runCLI(t, dir, "init")
	if code != 0 || errOut != "" {
		t.Fatalf("init => code=%d err=%q", code, errOut)
	}
	absSettings, err := filepath.Abs(installer.SessionSettingsPath(dir))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Explicit --agent odd-orchestrator still injects active profile model.
	profs, err := config.LoadProfiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	active := profs.Profiles[profs.ActiveProfile]
	active.Roles[config.RoleOrchestrator] = config.RoleRoute{
		Runtime: config.RuntimeAgyNative,
		Model:   "gemini-3.8-flash-high",
	}
	profs.Profiles[profs.ActiveProfile] = active
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatal(err)
	}

	called = false
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch", "--agent", "odd-orchestrator")
	if code != 0 || errOut != "" || !called {
		t.Fatalf("launch --agent odd-orchestrator => code=%d err=%q called=%t", code, errOut, called)
	}
	wantExplicitOrch := []string{"--agent", "odd-orchestrator", "--model", "gemini-3.8-flash-high", "--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantExplicitOrch) {
		t.Fatalf("explicit --agent odd-orchestrator args = %v, want %v", gotArgs, wantExplicitOrch)
	}

	// 2. Whitespace-padded " inherit " suppresses --model injection.
	active.Roles[config.RoleOrchestrator] = config.RoleRoute{
		Runtime: config.RuntimeAgyNative,
		Model:   " inherit ",
	}
	profs.Profiles[profs.ActiveProfile] = active
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatal(err)
	}

	called = false
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 0 || errOut != "" || !called {
		t.Fatalf("launch with padded inherit => code=%d err=%q called=%t", code, errOut, called)
	}
	wantNoModel := []string{"--agent", "odd-orchestrator", "--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantNoModel) {
		t.Fatalf("padded inherit args = %v, want %v", gotArgs, wantNoModel)
	}

	// 3. Invalid profiles.json aborts without invoking agy.
	if err := os.WriteFile(config.ProfilesPath(dir), []byte("{invalid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	called = false
	code, _, errOut = runCLI(t, dir, "launch")
	if code != exitError || errOut == "" {
		t.Fatalf("launch with invalid profiles.json => code=%d err=%q, want code=%d and stderr error", code, errOut, exitError)
	}
	if called {
		t.Fatalf("agy must NOT be invoked when profiles.json is invalid")
	}
}

func TestLaunchEffortInjectionAndProfileShow(t *testing.T) {
	dir := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	origLookPath := lookPath
	origRunner := runExternalCmd
	defer func() {
		lookPath = origLookPath
		runExternalCmd = origRunner
	}()

	var gotArgs []string
	lookPath = func(bin string) (string, error) {
		if bin == "agy" {
			return "/usr/local/bin/agy", nil
		}
		return "", errors.New("not found")
	}
	runExternalCmd = func(_ string, _ io.Reader, _, _ io.Writer, _ string, args ...string) int {
		gotArgs = append([]string(nil), args...)
		return 0
	}

	code, _, errOut := runCLI(t, dir, "init")
	if code != 0 || errOut != "" {
		t.Fatalf("init => code=%d err=%q", code, errOut)
	}
	absSettings, err := filepath.Abs(installer.SessionSettingsPath(dir))
	if err != nil {
		t.Fatal(err)
	}

	// Set orchestrator model=claude-sonnet-4-6, effort=high.
	profs, err := config.LoadProfiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	active := profs.Profiles[profs.ActiveProfile]
	active.Roles[config.RoleOrchestrator] = config.RoleRoute{
		Runtime: config.RuntimeAgyNative,
		Model:   "claude-sonnet-4-6",
		Effort:  "high",
	}
	w := active.Roles["worker"]
	w.Effort = "xhigh"
	active.Roles["worker"] = w
	profs.Profiles[profs.ActiveProfile] = active
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatal(err)
	}

	// 1. profile show includes effort=<val> only for roles with non-empty effort.
	code, showOut, errOut := runCLI(t, dir, "profile", "show")
	if code != 0 || errOut != "" {
		t.Fatalf("profile show => code=%d err=%q", code, errOut)
	}
	if !strings.Contains(showOut, "effort=high") || !strings.Contains(showOut, "effort=xhigh") {
		t.Fatalf("profile show missing effort=<val>:\n%s", showOut)
	}
	for _, line := range strings.Split(showOut, "\n") {
		if strings.Contains(line, "explore ") && strings.Contains(line, "effort=") {
			t.Fatalf("explore has empty effort and should not print effort=: %q", line)
		}
	}

	// 2. Default launch injects --model and --effort.
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 0 || errOut != "" {
		t.Fatalf("launch => code=%d err=%q", code, errOut)
	}
	wantBoth := []string{"--agent", "odd-orchestrator", "--model", "claude-sonnet-4-6", "--effort", "high", "--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantBoth) {
		t.Fatalf("launch args = %v, want %v", gotArgs, wantBoth)
	}

	// 3. Explicit --model suppresses --model injection but still injects --effort.
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch", "--", "--model", "flash")
	if code != 0 || errOut != "" {
		t.Fatalf("launch -- --model flash => code=%d err=%q", code, errOut)
	}
	wantEffortOnly := []string{"--agent", "odd-orchestrator", "--effort", "high", "--settings", absSettings, "--model", "flash"}
	if !reflect.DeepEqual(gotArgs, wantEffortOnly) {
		t.Fatalf("launch with explicit --model args = %v, want %v", gotArgs, wantEffortOnly)
	}

	// 4. Explicit --effort <val> suppresses --effort injection (still injects --model).
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch", "--", "--effort", "low")
	if code != 0 || errOut != "" {
		t.Fatalf("launch -- --effort low => code=%d err=%q", code, errOut)
	}
	wantModelOnly := []string{"--agent", "odd-orchestrator", "--model", "claude-sonnet-4-6", "--settings", absSettings, "--effort", "low"}
	if !reflect.DeepEqual(gotArgs, wantModelOnly) {
		t.Fatalf("launch with explicit --effort args = %v, want %v", gotArgs, wantModelOnly)
	}

	// 5. Explicit --effort=<val> suppresses --effort injection.
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch", "--", "--effort=medium")
	if code != 0 || errOut != "" {
		t.Fatalf("launch -- --effort=medium => code=%d err=%q", code, errOut)
	}
	wantExplicitEq := []string{"--agent", "odd-orchestrator", "--model", "claude-sonnet-4-6", "--settings", absSettings, "--effort=medium"}
	if !reflect.DeepEqual(gotArgs, wantExplicitEq) {
		t.Fatalf("launch with --effort= args = %v, want %v", gotArgs, wantExplicitEq)
	}

	// 6. --no-agent suppresses --effort injection.
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch", "--no-agent")
	if code != 0 || errOut != "" {
		t.Fatalf("launch --no-agent => code=%d err=%q", code, errOut)
	}
	wantNoAgent := []string{"--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantNoAgent) {
		t.Fatalf("launch --no-agent args = %v, want %v", gotArgs, wantNoAgent)
	}

	// 7. Custom --agent=<other> suppresses --effort injection.
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch", "--agent=custom-agent")
	if code != 0 || errOut != "" {
		t.Fatalf("launch --agent=custom-agent => code=%d err=%q", code, errOut)
	}
	wantCustom := []string{"--agent", "custom-agent", "--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantCustom) {
		t.Fatalf("launch --agent=custom-agent args = %v, want %v", gotArgs, wantCustom)
	}

	// 8. Empty orchestrator effort suppresses --effort injection.
	active.Roles[config.RoleOrchestrator] = config.RoleRoute{
		Runtime: config.RuntimeAgyNative,
		Model:   "inherit",
		Effort:  "",
	}
	profs.Profiles[profs.ActiveProfile] = active
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatal(err)
	}
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 0 || errOut != "" {
		t.Fatalf("launch empty effort => code=%d err=%q", code, errOut)
	}
	wantDefault := []string{"--agent", "odd-orchestrator", "--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantDefault) {
		t.Fatalf("launch empty effort args = %v, want %v", gotArgs, wantDefault)
	}

	// 9. Effort-suffixed model ID suppresses --effort injection (prevents agy --model/--effort conflict).
	for _, tc := range []struct {
		model  string
		effort string
	}{
		{model: "gemini-3.8-flash-medium", effort: "medium"},
		{model: "gemini-3.8-flash-low", effort: "low"},
		{model: "gemini-4-hydrogen-high", effort: "high"},
		{model: "gemini-4-hydrogen-xhigh", effort: "xhigh"},
		{model: "custom-model-MAX", effort: "max"},
	} {
		active.Roles[config.RoleOrchestrator] = config.RoleRoute{
			Runtime: config.RuntimeAgyNative,
			Model:   tc.model,
			Effort:  tc.effort,
		}
		profs.Profiles[profs.ActiveProfile] = active
		if err := config.SaveProfiles(dir, profs); err != nil {
			t.Fatal(err)
		}
		gotArgs = nil
		code, _, errOut = runCLI(t, dir, "launch")
		if code != 0 || errOut != "" {
			t.Fatalf("launch %s => code=%d err=%q", tc.model, code, errOut)
		}
		wantModelOnly := []string{"--agent", "odd-orchestrator", "--model", tc.model, "--settings", absSettings}
		if !reflect.DeepEqual(gotArgs, wantModelOnly) {
			t.Fatalf("launch %s with effort=%s args = %v, want %v", tc.model, tc.effort, gotArgs, wantModelOnly)
		}
	}

	// 10. inherit + effort=low injects --effort low without --model.
	active.Roles[config.RoleOrchestrator] = config.RoleRoute{
		Runtime: config.RuntimeAgyNative,
		Model:   "inherit",
		Effort:  "low",
	}
	profs.Profiles[profs.ActiveProfile] = active
	if err := config.SaveProfiles(dir, profs); err != nil {
		t.Fatal(err)
	}
	gotArgs = nil
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 0 || errOut != "" {
		t.Fatalf("launch inherit + effort=low => code=%d err=%q", code, errOut)
	}
	wantInheritEffort := []string{"--agent", "odd-orchestrator", "--effort", "low", "--settings", absSettings}
	if !reflect.DeepEqual(gotArgs, wantInheritEffort) {
		t.Fatalf("launch inherit + effort=low args = %v, want %v", gotArgs, wantInheritEffort)
	}
}

func TestProfileTestAndProvidersUseStoredSecrets(t *testing.T) {
	dir := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("DEEPSEEK_API_KEY", "")

	const storedSecret = "sk-cli-stored-secret-42"
	if err := config.SetSecret(fakeHome, "DEEPSEEK_API_KEY", storedSecret); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv("DEEPSEEK_BASE_URL", srv.URL+"/v1")

	code, out, errOut := runCLI(t, dir, "profile", "providers")
	if code != 0 || errOut != "" {
		t.Fatalf("profile providers => code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, "deepseek") || !strings.Contains(out, "keyStatus=stored") {
		t.Fatalf("expected keyStatus=stored in profile providers output:\n%s", out)
	}
	if strings.Contains(out, storedSecret) {
		t.Fatalf("profile providers leaked stored secret: %q", out)
	}

	code, out, errOut = runCLI(t, dir, "profile", "test", "deepseek")
	if code != 0 || errOut != "" {
		t.Fatalf("profile test deepseek => code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, "reachable=true") || !strings.Contains(out, "keySource=stored") {
		t.Fatalf("expected reachable=true and keySource=stored, got:\n%s", out)
	}
	if gotAuth != "Bearer "+storedSecret {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer "+storedSecret)
	}
	if strings.Contains(out, storedSecret) {
		t.Fatalf("profile test leaked stored secret: %q", out)
	}
}

func TestLaunchWarnsWhenExternalMCPActiveAndUnregistered(t *testing.T) {
	dir := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	origLookPath := lookPath
	origRunner := runExternalCmd
	defer func() {
		lookPath = origLookPath
		runExternalCmd = origRunner
	}()
	lookPath = func(bin string) (string, error) {
		if bin == "agy" {
			return "/usr/local/bin/agy", nil
		}
		return "", errors.New("not found")
	}
	runExternalCmd = func(_ string, _ io.Reader, _, _ io.Writer, _ string, _ ...string) int {
		return 0
	}

	code, _, errOut := runCLI(t, dir, "init", "--profile=deepseek-hybrid")
	if code != 0 || errOut != "" {
		t.Fatalf("init => code=%d err=%q", code, errOut)
	}

	// Unregistered external-mcp profile emits single helpful note to stderr before launching agy.
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 0 {
		t.Fatalf("launch => code=%d err=%q", code, errOut)
	}
	wantNote := `note: active profile "deepseek-hybrid" uses external-mcp roles, but "odd-agy" is not registered in ~/.gemini/config/mcp_config.json. Run "odd-agy permissions enable" or "agy mcp add odd-agy odd-agy mcp" first.` + "\n"
	if errOut != wantNote {
		t.Fatalf("launch warning stderr = %q, want %q", errOut, wantNote)
	}

	// Registering odd-agy silences the warning.
	if _, err := installer.RegisterGlobalMCPServer(fakeHome, "odd-agy"); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runCLI(t, dir, "launch")
	if code != 0 || errOut != "" {
		t.Fatalf("launch after registration => code=%d err=%q (want empty stderr)", code, errOut)
	}
}


