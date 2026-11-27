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
	"strings"
	"testing"

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

	// init --allow-mcp grants global MCP rules.
	code, out, errOut = runCLI(t, dir, "init", "--allow-mcp")
	if code != 0 || errOut != "" || !strings.Contains(out, "globalMCP=enabled") {
		t.Fatalf("init --allow-mcp => code=%d out=%q err=%q", code, out, errOut)
	}
	enabled, err := installer.CheckGlobalMCPPermissions(fakeHome)
	if err != nil || !enabled {
		t.Fatalf("CheckGlobalMCPPermissions after init --allow-mcp = (%t, %v)", enabled, err)
	}

	// permissions status reports enabled.
	code, out, errOut = runCLI(t, dir, "permissions", "status")
	if code != 0 || errOut != "" || !strings.Contains(out, "enabled") {
		t.Fatalf("permissions status enabled => code=%d out=%q err=%q", code, out, errOut)
	}

	// permissions revoke-mcp removes rules.
	code, out, errOut = runCLI(t, dir, "permissions", "revoke-mcp")
	if code != 0 || errOut != "" || !strings.Contains(out, "revoked") {
		t.Fatalf("permissions revoke-mcp => code=%d out=%q err=%q", code, out, errOut)
	}
	enabled, err = installer.CheckGlobalMCPPermissions(fakeHome)
	if err != nil || enabled {
		t.Fatalf("CheckGlobalMCPPermissions after revoke-mcp = (%t, %v)", enabled, err)
	}

	// permissions allow-mcp re-grants rules.
	code, out, errOut = runCLI(t, dir, "permissions", "allow-mcp")
	if code != 0 || errOut != "" || !strings.Contains(out, "enabled") {
		t.Fatalf("permissions allow-mcp => code=%d out=%q err=%q", code, out, errOut)
	}
	enabled, err = installer.CheckGlobalMCPPermissions(fakeHome)
	if err != nil || !enabled {
		t.Fatalf("CheckGlobalMCPPermissions after allow-mcp = (%t, %v)", enabled, err)
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
