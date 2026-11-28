package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/rdd"
)

func initMCPRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	runGit("add", "main.go")
	runGit("commit", "-m", "initial")

	t.Cleanup(func() {
		candRoot := filepath.Join(dir, ".git", "odd-agy", "candidates")
		_ = filepath.WalkDir(candRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(path, 0o755)
			} else {
				_ = os.Chmod(path, 0o644)
			}
			return nil
		})
	})
	return dir
}

type rpcClient struct {
	t      *testing.T
	pw     *io.PipeWriter
	reader *bufio.Reader
	nextID int
}

func (c *rpcClient) notify(method string, params any) {
	c.t.Helper()
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	b, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatalf("marshal notification: %v", err)
	}
	b = append(b, '\n')
	if _, err := c.pw.Write(b); err != nil {
		c.t.Fatalf("write notification: %v", err)
	}
}

func (c *rpcClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.nextID++
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID,
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	b, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatalf("marshal request: %v", err)
	}
	b = append(b, '\n')
	if _, err := c.pw.Write(b); err != nil {
		c.t.Fatalf("write request: %v", err)
	}

	line, err := c.reader.ReadString('\n')
	if err != nil {
		c.t.Fatalf("read response for %s: %v", method, err)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		c.t.Fatalf("unmarshal response %q: %v", line, err)
	}
	return resp
}

func (c *rpcClient) callTool(name string, args map[string]any) (string, bool) {
	c.t.Helper()
	resp := c.call("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if errObj, hasErr := resp["error"]; hasErr {
		c.t.Fatalf("tools/call(%s) JSON-RPC error: %v", name, errObj)
	}
	res := resp["result"].(map[string]any)
	isErr, _ := res["isError"].(bool)
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	return text, isErr
}

func TestStdioMCPEndToEnd(t *testing.T) {
	var llmTurns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := llmTurns.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "m1", "model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "",
						"reasoning_content": "checking main.go",
						"tool_calls": []map[string]any{{
							"id":   "call_read",
							"type": "function",
							"function": map[string]any{
								"name":      "read_file",
								"arguments": `{"path":"main.go"}`,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "m2", "model": "deepseek-v4-pro",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           "verified main.go",
					"reasoning_content": "verified",
				},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	repo := initMCPRepo(t)
	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	ds := profiles.Providers["deepseek"]
	ds.BaseURL = srv.URL + "/v1"
	profiles.Providers["deepseek"] = ds
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	const secretKey = "super-secret-deepseek-token"
	server := NewServer(ServerConfig{
		Workspace: repo,
		LookupEnv: func(k string) (string, bool) {
			if k == "DEEPSEEK_API_KEY" {
				return secretKey, true
			}
			return "", false
		},
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, inR, outW)
		_ = outW.Close()
	}()

	cli := &rpcClient{
		t:      t,
		pw:     inW,
		reader: bufio.NewReader(outR),
	}

	// 1. initialize + notifications/initialized + ping
	initResp := cli.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
	})
	if initResp["error"] != nil {
		t.Fatalf("initialize error: %v", initResp["error"])
	}
	cli.notify("notifications/initialized", map[string]any{})
	pingResp := cli.call("ping", map[string]any{})
	if pingResp["error"] != nil {
		t.Fatalf("ping error: %v", pingResp["error"])
	}

	// 2. tools/list contains all expected tools
	listResp := cli.call("tools/list", map[string]any{})
	toolsRaw := listResp["result"].(map[string]any)["tools"].([]any)
	toolNames := make(map[string]bool)
	for _, item := range toolsRaw {
		toolNames[item.(map[string]any)["name"].(string)] = true
	}
	for _, required := range []string{
		"todo",
		"odd_review_mode", "odd_review_assess", "odd_review_start", "odd_review_consent",
		"odd_review_scope", "odd_review_submit", "odd_review_run_lenses", "odd_review_apply_correction", "odd_review_status", "odd_review_ack",
		"odd_external_profiles", "odd_external_run", "odd_external_continue", "odd_external_status",
	} {
		if !toolNames[required] {
			t.Fatalf("tools/list missing %q", required)
		}
	}

	// 3. todo write + list
	todoOut, isErr := cli.callTool("todo", map[string]any{
		"action": "write",
		"tasks": []map[string]any{
			{"title": "Wire MCP", "status": "in_progress"},
			{"title": "Run tests", "status": "pending"},
		},
	})
	if isErr {
		t.Fatalf("todo write failed: %s", todoOut)
	}
	todoListOut, isErr := cli.callTool("todo", map[string]any{"action": "list"})
	if isErr || !strings.Contains(todoListOut, "Wire MCP") {
		t.Fatalf("todo list unexpected (err=%v): %s", isErr, todoListOut)
	}

	// 4. RDD lifecycle over MCP
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() { println(1) }\n"), 0o644); err != nil {
		t.Fatalf("edit main.go: %v", err)
	}
	modeOut, isErr := cli.callTool("odd_review_mode", map[string]any{"action": "status"})
	if isErr || !strings.Contains(modeOut, `"rddEnabled": true`) {
		t.Fatalf("odd_review_mode unexpected (err=%v): %s", isErr, modeOut)
	}
	assessOut, isErr := cli.callTool("odd_review_assess", map[string]any{})
	if isErr || !strings.Contains(assessOut, `"reviewDue": true`) {
		t.Fatalf("odd_review_assess unexpected (err=%v): %s", isErr, assessOut)
	}
	startOut, isErr := cli.callTool("odd_review_start", map[string]any{
		"lenses":       []string{"risk"},
		"lensesReason": "main.go changed",
	})
	if isErr {
		t.Fatalf("odd_review_start failed: %s", startOut)
	}
	var startRes rdd.StartResult
	if err := json.Unmarshal([]byte(startOut), &startRes); err != nil {
		t.Fatalf("unmarshal StartResult: %v", err)
	}
	if _, isErr := cli.callTool("odd_review_consent", map[string]any{
		"lineageId": startRes.LineageID,
		"answer":    "granted",
	}); isErr {
		t.Fatal("odd_review_consent failed")
	}
	scopeOut, isErr := cli.callTool("odd_review_scope", map[string]any{
		"lineageId": startRes.LineageID,
		"cursor":    0,
		"limit":     10,
	})
	if isErr || !strings.Contains(scopeOut, "main.go") {
		t.Fatalf("odd_review_scope unexpected (err=%v): %s", isErr, scopeOut)
	}
	if submitOut, isErr := cli.callTool("odd_review_submit", map[string]any{
		"lineageId": startRes.LineageID,
		"role":      "risk",
		"findings":  []any{},
	}); isErr {
		t.Fatalf("odd_review_submit failed: %s", submitOut)
	}
	statusOut, isErr := cli.callTool("odd_review_status", map[string]any{
		"lineageId": startRes.LineageID,
	})
	if isErr || !strings.Contains(statusOut, rdd.StatusApprovedPending) {
		t.Fatalf("odd_review_status unexpected (err=%v): %s", isErr, statusOut)
	}
	ackOut, isErr := cli.callTool("odd_review_ack", map[string]any{
		"lineageId": startRes.LineageID,
	})
	if isErr || !strings.Contains(ackOut, rdd.AuthorityBurned) {
		t.Fatalf("odd_review_ack unexpected (err=%v): %s", isErr, ackOut)
	}

	// 5. odd_external_profiles must never leak API key values.
	profOut, isErr := cli.callTool("odd_external_profiles", map[string]any{})
	if isErr {
		t.Fatalf("odd_external_profiles failed: %s", profOut)
	}
	if strings.Contains(profOut, secretKey) {
		t.Fatalf("odd_external_profiles leaked API key value!")
	}
	if !strings.Contains(profOut, "deepseek-hybrid") || !strings.Contains(profOut, `"apiKeyPresent": true`) {
		t.Fatalf("odd_external_profiles missing expected fields: %s", profOut)
	}

	// 6. odd_external_run + odd_external_status + odd_external_continue
	runOut, isErr := cli.callTool("odd_external_run", map[string]any{
		"role": "verify",
		"task": "Verify main.go",
	})
	if isErr {
		t.Fatalf("odd_external_run failed: %s", runOut)
	}
	var runObj map[string]any
	if err := json.Unmarshal([]byte(runOut), &runObj); err != nil {
		t.Fatalf("unmarshal runOut: %v", err)
	}
	taskID := runObj["taskId"].(string)
	if runObj["status"] != "completed" || strings.Contains(runOut, secretKey) {
		t.Fatalf("unexpected odd_external_run output: %s", runOut)
	}

	extStatusOut, isErr := cli.callTool("odd_external_status", map[string]any{"taskId": taskID})
	if isErr || !strings.Contains(extStatusOut, taskID) {
		t.Fatalf("odd_external_status unexpected (err=%v): %s", isErr, extStatusOut)
	}

	contOut, isErr := cli.callTool("odd_external_continue", map[string]any{
		"taskId": taskID,
		"prompt": "Double check",
	})
	if isErr || !strings.Contains(contOut, "verified main.go") {
		t.Fatalf("odd_external_continue unexpected (err=%v): %s", isErr, contOut)
	}

	// 7. RDD refuter + apply_correction lifecycle via MCP
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() { println(2) }\n"), 0o644); err != nil {
		t.Fatalf("edit main.go v2: %v", err)
	}
	start2Out, isErr := cli.callTool("odd_review_start", map[string]any{
		"lenses":       []string{"risk"},
		"lensesReason": "verify refuter and correction over MCP",
	})
	if isErr {
		t.Fatalf("odd_review_start 2 failed: %s", start2Out)
	}
	var start2 rdd.StartResult
	if err := json.Unmarshal([]byte(start2Out), &start2); err != nil {
		t.Fatalf("unmarshal start2: %v", err)
	}
	if _, isErr := cli.callTool("odd_review_consent", map[string]any{
		"lineageId": start2.LineageID,
		"answer":    "granted",
	}); isErr {
		t.Fatal("odd_review_consent 2 failed")
	}
	if out, isErr := cli.callTool("odd_review_submit", map[string]any{
		"lineageId": start2.LineageID,
		"role":      "review-risk",
		"findings": []map[string]any{{
			"id":        "F-MCP-1",
			"severity":  "severe",
			"causality": "candidate",
			"location":  "main.go:3",
			"claim":     "bad constant",
		}},
	}); isErr || !strings.Contains(out, rdd.StatusCollectingRefuter) {
		t.Fatalf("odd_review_submit blocker unexpected (err=%v): %s", isErr, out)
	}
	if out, isErr := cli.callTool("odd_review_submit", map[string]any{
		"lineageId": start2.LineageID,
		"role":      "review-refuter",
		"refuterVerdicts": []map[string]any{{
			"findingId": "F-MCP-1",
			"verdict":   "uphold",
			"rationale": "confirmed",
		}},
	}); isErr || !strings.Contains(out, rdd.StatusCorrectionReq) {
		t.Fatalf("odd_review_submit refuter unexpected (err=%v): %s", isErr, out)
	}
	if out, isErr := cli.callTool("odd_review_apply_correction", map[string]any{
		"lineageId": start2.LineageID,
	}); isErr || !strings.Contains(out, `"revision": 2`) {
		t.Fatalf("odd_review_apply_correction unexpected (err=%v): %s", isErr, out)
	}

	// Verify graceful EOF shutdown.
	_ = inW.Close()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve returned error on EOF: %v", err)
	}
	_ = bytes.Buffer{}
}

func TestMCPDefaultServerUsesStoredSecretsAndEnvPrecedence(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("DEEPSEEK_API_KEY", "")

	const storedKey = "sk-mcp-stored-secret-111"
	const envKey = "sk-mcp-env-override-222"
	if err := config.SetSecret(fakeHome, "DEEPSEEK_API_KEY", storedKey); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	var gotAuths []string
	var llmTurns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuths = append(gotAuths, r.Header.Get("Authorization"))
		llmTurns.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "m-stored", "model": "deepseek-v4-pro",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           "ok from mock",
					"reasoning_content": "checked",
				},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	repo := initMCPRepo(t)
	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	ds := profiles.Providers["deepseek"]
	ds.BaseURL = srv.URL + "/v1"
	profiles.Providers["deepseek"] = ds
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	startClient := func(server *Server) (*rpcClient, func()) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- server.Serve(ctx, inR, outW)
			_ = outW.Close()
		}()
		cli := &rpcClient{t: t, pw: inW, reader: bufio.NewReader(outR)}
		_ = cli.call("initialize", map[string]any{
			"protocolVersion": "2024-11-05",
			"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
		})
		cli.notify("notifications/initialized", map[string]any{})
		return cli, func() {
			_ = inW.Close()
			cancel()
			if err := <-done; err != nil && err != context.Canceled {
				t.Fatalf("Serve error: %v", err)
			}
		}
	}

	// 1) Default Server (LookupEnv == nil) reads stored secret from ~/.config/odd-agy/secrets.json.
	defaultSrv := NewServer(ServerConfig{Workspace: repo})
	cli, stopDefault := startClient(defaultSrv)

	profOut, isErr := cli.callTool("odd_external_profiles", map[string]any{})
	if isErr {
		t.Fatalf("odd_external_profiles failed: %s", profOut)
	}
	if strings.Contains(profOut, storedKey) {
		t.Fatalf("odd_external_profiles leaked stored secret!")
	}
	var profSummary struct {
		Providers map[string]struct {
			APIKeyPresent bool   `json:"apiKeyPresent"`
			KeySource     string `json:"keySource"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(profOut), &profSummary); err != nil {
		t.Fatalf("unmarshal profiles summary: %v\n%s", err, profOut)
	}
	if !profSummary.Providers["deepseek"].APIKeyPresent || profSummary.Providers["deepseek"].KeySource != "stored" {
		t.Fatalf("deepseek summary with stored key = %+v, want apiKeyPresent=true keySource=stored", profSummary.Providers["deepseek"])
	}

	runOut, isErr := cli.callTool("odd_external_run", map[string]any{
		"role": "verify",
		"task": "Verify main.go",
	})
	if isErr {
		t.Fatalf("odd_external_run failed: %s", runOut)
	}
	var runObj map[string]any
	if err := json.Unmarshal([]byte(runOut), &runObj); err != nil {
		t.Fatalf("unmarshal runOut: %v", err)
	}
	if runObj["status"] != "completed" {
		t.Fatalf("odd_external_run status = %v, want completed: %s", runObj["status"], runOut)
	}
	if len(gotAuths) != 1 || gotAuths[0] != "Bearer "+storedKey {
		t.Fatalf("odd_external_run Authorization = %v, want [Bearer %s]", gotAuths, storedKey)
	}

	taskID := runObj["taskId"].(string)
	contOut, isErr := cli.callTool("odd_external_continue", map[string]any{
		"taskId": taskID,
		"prompt": "Continue with stored key",
	})
	if isErr || !strings.Contains(contOut, "ok from mock") {
		t.Fatalf("odd_external_continue failed (err=%v): %s", isErr, contOut)
	}
	if len(gotAuths) != 2 || gotAuths[1] != "Bearer "+storedKey {
		t.Fatalf("odd_external_continue Authorization = %v, want second call Bearer %s", gotAuths, storedKey)
	}

	// 2) Environment variable overrides stored secret.
	t.Setenv("DEEPSEEK_API_KEY", envKey)
	profEnvOut, isErr := cli.callTool("odd_external_profiles", map[string]any{})
	if isErr {
		t.Fatalf("odd_external_profiles (env) failed: %s", profEnvOut)
	}
	if err := json.Unmarshal([]byte(profEnvOut), &profSummary); err != nil {
		t.Fatalf("unmarshal profiles summary (env): %v", err)
	}
	if !profSummary.Providers["deepseek"].APIKeyPresent || profSummary.Providers["deepseek"].KeySource != "env" {
		t.Fatalf("deepseek summary with env override = %+v, want apiKeyPresent=true keySource=env", profSummary.Providers["deepseek"])
	}

	runEnvOut, isErr := cli.callTool("odd_external_run", map[string]any{
		"role": "verify",
		"task": "Verify main.go with env key",
	})
	if isErr || !strings.Contains(runEnvOut, "completed") {
		t.Fatalf("odd_external_run (env) failed (err=%v): %s", isErr, runEnvOut)
	}
	if len(gotAuths) != 3 || gotAuths[2] != "Bearer "+envKey {
		t.Fatalf("env override Authorization = %v, want 3rd call Bearer %s", gotAuths, envKey)
	}
	stopDefault()

	// 3) Caller-injected LookupEnv != nil stays 100% hermetic (does NOT read stored secrets).
	hermeticSrv := NewServer(ServerConfig{
		Workspace: repo,
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	hCli, stopHermetic := startClient(hermeticSrv)
	defer stopHermetic()

	hProfOut, isErr := hCli.callTool("odd_external_profiles", map[string]any{})
	if isErr {
		t.Fatalf("hermetic odd_external_profiles failed: %s", hProfOut)
	}
	if err := json.Unmarshal([]byte(hProfOut), &profSummary); err != nil {
		t.Fatalf("unmarshal hermetic profiles summary: %v", err)
	}
	if profSummary.Providers["deepseek"].APIKeyPresent || profSummary.Providers["deepseek"].KeySource != "missing" {
		t.Fatalf("hermetic server must ignore stored secrets, got %+v", profSummary.Providers["deepseek"])
	}
}

func TestMCPConcurrentExternalRunsExecuteInParallel(t *testing.T) {
	const parallelCount = 3
	var inFlight atomic.Int32
	barrierReady := make(chan struct{})
	var barrierOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := inFlight.Add(1); n == parallelCount {
			barrierOnce.Do(func() { close(barrierReady) })
		}
		select {
		case <-barrierReady:
		case <-time.After(3 * time.Second):
			http.Error(w, "timed out waiting for concurrent peers (server is sequential!)", http.StatusGatewayTimeout)
			return
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "par-1", "model": "deepseek-v4-pro",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           "parallel ok",
					"reasoning_content": "done",
				},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	repo := initMCPRepo(t)
	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	ds := profiles.Providers["deepseek"]
	ds.BaseURL = srv.URL + "/v1"
	profiles.Providers["deepseek"] = ds
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	server := NewServer(ServerConfig{
		Workspace: repo,
		LookupEnv: func(k string) (string, bool) {
			if k == "DEEPSEEK_API_KEY" {
				return "ds-secret", true
			}
			return "", false
		},
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, inR, outW)
		_ = outW.Close()
	}()

	cli := &rpcClient{t: t, pw: inW, reader: bufio.NewReader(outR)}
	_ = cli.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "parallel-test", "version": "1.0"},
	})
	cli.notify("notifications/initialized", map[string]any{})

	// Write 3 tools/call requests concurrently while reading responses concurrently (avoids io.Pipe deadlock on regressions).
	var writeMu sync.Mutex
	var writeWG sync.WaitGroup
	for i := 1; i <= parallelCount; i++ {
		writeWG.Add(1)
		id := 100 + i
		go func(reqID int) {
			defer writeWG.Done()
			msg := map[string]any{
				"jsonrpc": "2.0",
				"id":      reqID,
				"method":  "tools/call",
				"params": map[string]any{
					"name": "odd_external_run",
					"arguments": map[string]any{
						"role": "verify",
						"task": "Verify main.go",
					},
				},
			}
			b, _ := json.Marshal(msg)
			b = append(b, '\n')
			writeMu.Lock()
			_, err := inW.Write(b)
			writeMu.Unlock()
			if err != nil {
				t.Errorf("write req %d: %v", reqID, err)
			}
		}(id)
	}

	// Read all 3 responses (order may vary due to concurrency).
	seenIDs := make(map[int]bool)
	for i := 0; i < parallelCount; i++ {
		line, err := cli.reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read parallel response %d: %v", i, err)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("unmarshal response %q: %v", line, err)
		}
		if resp["error"] != nil {
			t.Fatalf("unexpected JSON-RPC error: %v", resp["error"])
		}
		id := int(resp["id"].(float64))
		seenIDs[id] = true
		res := resp["result"].(map[string]any)
		if isErr, _ := res["isError"].(bool); isErr {
			t.Fatalf("id %d tool returned error: %v", id, res)
		}
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(text, `"status": "completed"`) {
			t.Fatalf("id %d unexpected output: %s", id, text)
		}
	}
	writeWG.Wait()
	if len(seenIDs) != parallelCount {
		t.Fatalf("seenIDs = %v, want 3 distinct responses", seenIDs)
	}

	_ = inW.Close()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve returned error on EOF: %v", err)
	}
}

func TestMCPServeContextCancellationUnblocksAndWaitsInFlight(t *testing.T) {
	inFlightStarted := make(chan struct{})
	inFlightCanceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(inFlightStarted)
		select {
		case <-r.Context().Done():
			close(inFlightCanceled)
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	repo := initMCPRepo(t)
	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	ds := profiles.Providers["deepseek"]
	ds.BaseURL = srv.URL + "/v1"
	profiles.Providers["deepseek"] = ds
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	server := NewServer(ServerConfig{
		Workspace: repo,
		LookupEnv: func(k string) (string, bool) {
			if k == "DEEPSEEK_API_KEY" {
				return "ds-secret", true
			}
			return "", false
		},
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	defer outR.Close()

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, inR, outW)
		_ = outW.Close()
	}()
	// Drain outR so in-flight error response (if written before ctx check) never blocks pipe.
	go func() { _, _ = io.Copy(io.Discard, outR) }()

	cli := &rpcClient{t: t, pw: inW, reader: bufio.NewReader(outR)}
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "odd_external_run",
			"arguments": map[string]any{"role": "verify", "task": "Verify main.go"},
		},
	}
	b, _ := json.Marshal(msg)
	b = append(b, '\n')
	if _, err := cli.pw.Write(b); err != nil {
		t.Fatalf("write request: %v", err)
	}

	<-inFlightStarted
	cancel()

	select {
	case <-inFlightCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for in-flight HTTP request cancellation")
	}

	select {
	case err := <-serveDone:
		if err != context.Canceled {
			t.Fatalf("Serve error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Serve to return after cancel")
	}
}

func TestMCPExternalReviewRoleUsesFrozenScopeOnlyAndSubmitsDirectly(t *testing.T) {
	const envSecret = "SUPER_SECRET_TOKEN_XYZ"
	var step atomic.Int32
	var seenToolNames []string
	var sawRedactedEnv atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		n := step.Add(1)
		if n == 1 {
			for _, item := range payload["tools"].([]any) {
				seenToolNames = append(seenToolNames, item.(map[string]any)["function"].(map[string]any)["name"].(string))
			}
		}
		// Inspect tool outputs sent back on turn 2+ to confirm .env content is redacted.
		for _, rawMsg := range payload["messages"].([]any) {
			m := rawMsg.(map[string]any)
			if m["role"] == "tool" {
				content, _ := m["content"].(string)
				if strings.Contains(content, envSecret) {
					t.Errorf("frozen scope leaked raw .env secret in tool output: %s", content)
				}
				if strings.Contains(content, "[REDACTED SENSITIVE FILE CONTENT]") {
					sawRedactedEnv.Store(true)
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "m1", "model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "",
						"reasoning_content": "reading scope",
						"tool_calls": []map[string]any{{
							"id":   "c1",
							"type": "function",
							"function": map[string]any{
								"name":      "read_review_scope",
								"arguments": `{"cursor":0}`,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "m2", "model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "",
						"reasoning_content": "submitting warning only",
						"tool_calls": []map[string]any{{
							"id":   "c2",
							"type": "function",
							"function": map[string]any{
								"name":      "submit_review_findings",
								"arguments": `{"findings":[{"id":"R-ENV","severity":"WARNING","causality":"candidate","location":".env:1","claim":"env file added"}]}`,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "m3", "model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "submitted risk lens cleanly",
						"reasoning_content": "done",
					},
					"finish_reason": "stop",
				}},
			})
		}
	}))
	defer srv.Close()

	repo := initMCPRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() { println(42) }\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("API_KEY="+envSecret+"\n"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	ds := profiles.Providers["deepseek"]
	ds.BaseURL = srv.URL + "/v1"
	profiles.Providers["deepseek"] = ds
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	server := NewServer(ServerConfig{
		Workspace: repo,
		LookupEnv: func(k string) (string, bool) {
			if k == "DEEPSEEK_API_KEY" {
				return "ds-secret", true
			}
			return "", false
		},
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, inR, outW)
		_ = outW.Close()
	}()

	cli := &rpcClient{t: t, pw: inW, reader: bufio.NewReader(outR)}
	_ = cli.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "review-test", "version": "1.0"},
	})
	cli.notify("notifications/initialized", map[string]any{})

	startOut, isErr := cli.callTool("odd_review_start", map[string]any{
		"lenses":       []string{"risk"},
		"lensesReason": "verify frozen scope redaction and direct submit",
	})
	if isErr {
		t.Fatalf("odd_review_start failed: %s", startOut)
	}
	var startRes rdd.StartResult
	if err := json.Unmarshal([]byte(startOut), &startRes); err != nil {
		t.Fatalf("unmarshal StartResult: %v", err)
	}
	if _, isErr := cli.callTool("odd_review_consent", map[string]any{
		"lineageId": startRes.LineageID,
		"answer":    "granted",
	}); isErr {
		t.Fatal("odd_review_consent failed")
	}

	runOut, isErr := cli.callTool("odd_external_run", map[string]any{
		"role":      "review-risk",
		"lineageId": startRes.LineageID,
		"task":      "Review the frozen snapshot.",
	})
	if isErr {
		t.Fatalf("odd_external_run(review-risk) failed: %s", runOut)
	}
	var runObj map[string]any
	if err := json.Unmarshal([]byte(runOut), &runObj); err != nil {
		t.Fatalf("unmarshal runOut: %v", err)
	}
	if runObj["status"] != "completed" || runObj["submittedToLineage"] != true {
		t.Fatalf("unexpected review-risk runObj: %s", runOut)
	}

	if len(seenToolNames) != 2 {
		t.Fatalf("expected 2 tools for review-risk, got %v", seenToolNames)
	}
	for _, tn := range seenToolNames {
		if tn != "read_review_scope" && tn != "submit_review_findings" {
			t.Fatalf("unexpected tool exposed to review-risk: %q", tn)
		}
	}
	if !sawRedactedEnv.Load() {
		t.Fatal("expected read_review_scope output to contain [REDACTED SENSITIVE FILE CONTENT]")
	}

	// Lineage should now be approved_pending_ack and acknowledgeable cleanly!
	statusOut, isErr := cli.callTool("odd_review_status", map[string]any{
		"lineageId": startRes.LineageID,
	})
	if isErr || !strings.Contains(statusOut, rdd.StatusApprovedPending) {
		t.Fatalf("odd_review_status unexpected (err=%v): %s", isErr, statusOut)
	}
	ackOut, isErr := cli.callTool("odd_review_ack", map[string]any{
		"lineageId": startRes.LineageID,
	})
	if isErr || !strings.Contains(ackOut, rdd.AuthorityBurned) {
		t.Fatalf("odd_review_ack unexpected (err=%v): %s", isErr, ackOut)
	}

	_ = inW.Close()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve error: %v", err)
	}
}

func TestMCPToolsListSchemas(t *testing.T) {
	repo := initMCPRepo(t)
	server := NewServer(ServerConfig{Workspace: repo})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, inR, outW)
		_ = outW.Close()
	}()

	cli := &rpcClient{t: t, pw: inW, reader: bufio.NewReader(outR)}
	_ = cli.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})
	cli.notify("notifications/initialized", map[string]any{})

	listResp := cli.call("tools/list", map[string]any{})
	toolsRaw := listResp["result"].(map[string]any)["tools"].([]any)
	byName := make(map[string]map[string]any)
	for _, raw := range toolsRaw {
		obj := raw.(map[string]any)
		byName[obj["name"].(string)] = obj
	}

	// Verify odd_review_submit schema has typed findings.items.required and enums.
	submitSchema := byName["odd_review_submit"]["inputSchema"].(map[string]any)
	submitProps := submitSchema["properties"].(map[string]any)
	findingsSchema := submitProps["findings"].(map[string]any)
	if findingsSchema["type"] != "array" {
		t.Fatalf("odd_review_submit findings.type = %v, want array", findingsSchema["type"])
	}
	items := findingsSchema["items"].(map[string]any)
	reqSlice := items["required"].([]any)
	gotReq := make([]string, 0, len(reqSlice))
	for _, v := range reqSlice {
		gotReq = append(gotReq, v.(string))
	}
	wantReq := []string{"id", "severity", "causality", "location", "claim"}
	if strings.Join(gotReq, ",") != strings.Join(wantReq, ",") {
		t.Fatalf("findings.items.required = %v, want %v", gotReq, wantReq)
	}
	itemProps := items["properties"].(map[string]any)
	sevEnum := itemProps["severity"].(map[string]any)["enum"].([]any)
	wantSev := []string{"BLOCKER", "SEVERE", "WARNING", "SUGGESTION"}
	if len(sevEnum) != len(wantSev) {
		t.Fatalf("severity.enum = %v, want %v", sevEnum, wantSev)
	}
	for i, w := range wantSev {
		if sevEnum[i] != w {
			t.Fatalf("severity.enum[%d] = %v, want %v (full=%v)", i, sevEnum[i], w, sevEnum)
		}
	}

	// Verify odd_review_run_lenses schema exists and requires lineageId.
	runLensesTool, hasRunLenses := byName["odd_review_run_lenses"]
	if !hasRunLenses {
		t.Fatal("tools/list missing odd_review_run_lenses")
	}
	runLensesSchema := runLensesTool["inputSchema"].(map[string]any)
	runLensesProps := runLensesSchema["properties"].(map[string]any)
	if _, ok := runLensesProps["lineageId"]; !ok {
		t.Fatalf("odd_review_run_lenses missing lineageId property: %+v", runLensesProps)
	}
	if lensesProp, ok := runLensesProps["lenses"].(map[string]any); !ok || lensesProp["type"] != "array" {
		t.Fatalf("odd_review_run_lenses missing array lenses property: %+v", runLensesProps)
	}
	causEnum := itemProps["causality"].(map[string]any)["enum"].([]any)
	if len(causEnum) != 3 || causEnum[0] != "candidate" || causEnum[1] != "pre_existing" || causEnum[2] != "unknown" {
		t.Fatalf("causality.enum = %v, want [candidate pre_existing unknown]", causEnum)
	}

	// Verify odd_external_run schema documents lineageId.
	extRunSchema := byName["odd_external_run"]["inputSchema"].(map[string]any)
	extRunProps := extRunSchema["properties"].(map[string]any)
	linProp, ok := extRunProps["lineageId"].(map[string]any)
	if !ok || linProp["type"] != "string" {
		t.Fatalf("odd_external_run missing string lineageId property: %+v", extRunProps)
	}

	// Verify odd_review_submit accepts BLOCKER severity from schema (PRESERVE: SEVERE also still works).
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() { println(99) }\n"), 0o644); err != nil {
		t.Fatalf("edit main.go v3: %v", err)
	}
	start3Out, isErr := cli.callTool("odd_review_start", map[string]any{
		"lenses":       []string{"risk"},
		"lensesReason": "verify BLOCKER severity mapping",
	})
	if isErr {
		t.Fatalf("odd_review_start 3 failed: %s", start3Out)
	}
	var start3 rdd.StartResult
	if err := json.Unmarshal([]byte(start3Out), &start3); err != nil {
		t.Fatalf("unmarshal start3: %v", err)
	}
	if _, isErr := cli.callTool("odd_review_consent", map[string]any{
		"lineageId": start3.LineageID,
		"answer":    "granted",
	}); isErr {
		t.Fatal("odd_review_consent 3 failed")
	}
	if out, isErr := cli.callTool("odd_review_submit", map[string]any{
		"lineageId": start3.LineageID,
		"role":      "risk",
		"findings": []map[string]any{{
			"id":        "F-BLK-MAP",
			"severity":  "BLOCKER",
			"causality": "candidate",
			"location":  "main.go:3",
			"claim":     "blocker mapped to SEVERE",
		}},
	}); isErr || !strings.Contains(out, rdd.StatusCollectingRefuter) || !strings.Contains(out, rdd.SeveritySEVERE) {
		t.Fatalf("odd_review_submit BLOCKER unexpected (err=%v): %s", isErr, out)
	}

	_ = inW.Close()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve error: %v", err)
	}
}

func TestMCPReviewRunLensesExecutesAllExternalLensesInParallel(t *testing.T) {
	const expectedLenses = 3
	var inFlight atomic.Int32
	barrierReady := make(chan struct{})
	var barrierOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msgs := payload["messages"].([]any)
		var rolePrompt string
		if len(msgs) >= 1 {
			rolePrompt, _ = msgs[0].(map[string]any)["content"].(string)
		}
		var toolCount int
		for _, rawM := range msgs {
			if rawM.(map[string]any)["role"] == "tool" {
				toolCount++
			}
		}

		w.Header().Set("Content-Type", "application/json")
		switch toolCount {
		case 0:
			// Barrier on first turn across all 3 external lenses.
			if n := inFlight.Add(1); n == expectedLenses {
				barrierOnce.Do(func() { close(barrierReady) })
			}
			select {
			case <-barrierReady:
			case <-time.After(3 * time.Second):
				http.Error(w, "timed out waiting for all 3 lenses to run concurrently", http.StatusGatewayTimeout)
				return
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "t1", "model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "",
						"reasoning_content": "reading frozen scope",
						"tool_calls": []map[string]any{{
							"id":   "call_scope",
							"type": "function",
							"function": map[string]any{
								"name":      "read_review_scope",
								"arguments": `{"cursor":0}`,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
		case 1:
			// Submit finding or empty findings depending on lens; use SEVERE+pre_existing on one to verify SEVERE schema & non-blocking advisory.
			argsJSON := `{"findings":[]}`
			switch {
			case strings.Contains(rolePrompt, "review-risk"):
				argsJSON = `{"findings":[{"id":"RISK-ADV-1","severity":"SEVERE","causality":"pre_existing","location":"main.go:1","claim":"pre-existing note"}]}`
			case strings.Contains(rolePrompt, "review-reliability"):
				argsJSON = `{"findings":[{"id":"REL-1","severity":"WARNING","causality":"candidate","location":"main.go:3","claim":"minor note"}]}`
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "t2", "model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "",
						"reasoning_content": "submitting lens findings",
						"tool_calls": []map[string]any{{
							"id":   "call_submit",
							"type": "function",
							"function": map[string]any{
								"name":      "submit_review_findings",
								"arguments": argsJSON,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "t3", "model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "lens complete",
						"reasoning_content": "done",
					},
					"finish_reason": "stop",
				}},
			})
		}
	}))
	defer srv.Close()

	repo := initMCPRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() { println(7) }\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	ds := profiles.Providers["deepseek"]
	ds.BaseURL = srv.URL + "/v1"
	profiles.Providers["deepseek"] = ds
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	server := NewServer(ServerConfig{
		Workspace: repo,
		LookupEnv: func(k string) (string, bool) {
			if k == "DEEPSEEK_API_KEY" {
				return "ds-secret", true
			}
			return "", false
		},
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, inR, outW)
		_ = outW.Close()
	}()

	cli := &rpcClient{t: t, pw: inW, reader: bufio.NewReader(outR)}
	_ = cli.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})
	cli.notify("notifications/initialized", map[string]any{})

	startOut, isErr := cli.callTool("odd_review_start", map[string]any{
		"lenses":       []string{"risk", "reliability", "readability"},
		"lensesReason": "verify parallel grouped external lens execution",
	})
	if isErr {
		t.Fatalf("odd_review_start failed: %s", startOut)
	}
	var startRes rdd.StartResult
	if err := json.Unmarshal([]byte(startOut), &startRes); err != nil {
		t.Fatalf("unmarshal startRes: %v", err)
	}

	// Before consent, odd_review_run_lenses must fail cleanly.
	preConsentOut, preConsentErr := cli.callTool("odd_review_run_lenses", map[string]any{
		"lineageId": startRes.LineageID,
	})
	if !preConsentErr {
		t.Fatalf("expected error before consent, got: %s", preConsentOut)
	}

	if _, isErr := cli.callTool("odd_review_consent", map[string]any{
		"lineageId": startRes.LineageID,
		"answer":    "granted",
	}); isErr {
		t.Fatal("odd_review_consent failed")
	}

	runOut, isErr := cli.callTool("odd_review_run_lenses", map[string]any{
		"lineageId": startRes.LineageID,
	})
	if isErr {
		t.Fatalf("odd_review_run_lenses failed: %s", runOut)
	}

	var runRes struct {
		LineageID      string `json:"lineageId"`
		ExecutedLenses []struct {
			Lens               string `json:"lens"`
			Role               string `json:"role"`
			Provider           string `json:"provider"`
			Model              string `json:"model"`
			Status             string `json:"status"`
			SubmittedToLineage bool   `json:"submittedToLineage"`
			TaskID             string `json:"taskId"`
			Error              string `json:"error"`
			FallbackToNative   bool   `json:"fallbackToNative"`
		} `json:"executedLenses"`
		NativeLensesPending []string          `json:"nativeLensesPending"`
		NextAction          string            `json:"nextAction"`
		ReviewStatus        rdd.LineageStatus `json:"reviewStatus"`
	}
	if err := json.Unmarshal([]byte(runOut), &runRes); err != nil {
		t.Fatalf("unmarshal odd_review_run_lenses result: %v\n%s", err, runOut)
	}
	if runRes.LineageID != startRes.LineageID {
		t.Fatalf("lineageId = %q, want %q", runRes.LineageID, startRes.LineageID)
	}
	if len(runRes.ExecutedLenses) != 3 {
		t.Fatalf("executedLenses len = %d, want 3: %s", len(runRes.ExecutedLenses), runOut)
	}
	for _, ex := range runRes.ExecutedLenses {
		if ex.Status != "completed" || !ex.SubmittedToLineage || ex.FallbackToNative || ex.TaskID == "" {
			t.Fatalf("unexpected executed lens result %+v", ex)
		}
	}
	if len(runRes.NativeLensesPending) != 0 {
		t.Fatalf("expected no nativeLensesPending, got %v", runRes.NativeLensesPending)
	}
	if runRes.ReviewStatus.Status != rdd.StatusApprovedPending {
		t.Fatalf("reviewStatus.Status = %q, want %q", runRes.ReviewStatus.Status, rdd.StatusApprovedPending)
	}

	// Ack closes/burns cleanly.
	ackOut, isErr := cli.callTool("odd_review_ack", map[string]any{
		"lineageId": startRes.LineageID,
	})
	if isErr || !strings.Contains(ackOut, rdd.AuthorityBurned) {
		t.Fatalf("odd_review_ack unexpected (err=%v): %s", isErr, ackOut)
	}

	_ = inW.Close()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve error: %v", err)
	}
}

func TestMCPReviewRunLensesMixedAndNativeOnlyAndFailureFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		msgs := payload["messages"].([]any)
		var toolCount int
		for _, rawM := range msgs {
			if rawM.(map[string]any)["role"] == "tool" {
				toolCount++
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if toolCount == 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "m1", "model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "",
						"reasoning_content": "submit directly",
						"tool_calls": []map[string]any{{
							"id":   "c1",
							"type": "function",
							"function": map[string]any{
								"name":      "submit_review_findings",
								"arguments": `{"findings":[]}`,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "m2", "model": "deepseek-v4-pro",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           "ok",
					"reasoning_content": "done",
				},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	repo := initMCPRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() { println(8) }\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	// 1. Native-only profile (agy-oauth): reports nativeLensesPending cleanly without failing.
	if err := config.SaveProfiles(repo, config.DefaultProfilesConfig()); err != nil {
		t.Fatalf("SaveProfiles agy-oauth: %v", err)
	}

	server := NewServer(ServerConfig{
		Workspace: repo,
		LookupEnv: func(k string) (string, bool) {
			if k == "DEEPSEEK_API_KEY" {
				return "ds-secret", true
			}
			return "", false
		},
	})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, inR, outW)
		_ = outW.Close()
	}()
	cli := &rpcClient{t: t, pw: inW, reader: bufio.NewReader(outR)}
	_ = cli.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})
	cli.notify("notifications/initialized", map[string]any{})

	startOut, isErr := cli.callTool("odd_review_start", map[string]any{
		"lenses":       []string{"risk", "reliability", "readability"},
		"lensesReason": "verify native and mixed profiles",
	})
	if isErr {
		t.Fatalf("odd_review_start failed: %s", startOut)
	}
	var startRes rdd.StartResult
	_ = json.Unmarshal([]byte(startOut), &startRes)
	_, _ = cli.callTool("odd_review_consent", map[string]any{
		"lineageId": startRes.LineageID,
		"answer":    "granted",
	})

	nativeOut, isErr := cli.callTool("odd_review_run_lenses", map[string]any{
		"lineageId": startRes.LineageID,
	})
	if isErr {
		t.Fatalf("native-only odd_review_run_lenses should succeed, got error: %s", nativeOut)
	}
	var nativeRes struct {
		ExecutedLenses      []any             `json:"executedLenses"`
		NativeLensesPending []string          `json:"nativeLensesPending"`
		NextAction          string            `json:"nextAction"`
		ReviewStatus        rdd.LineageStatus `json:"reviewStatus"`
	}
	if err := json.Unmarshal([]byte(nativeOut), &nativeRes); err != nil {
		t.Fatalf("unmarshal nativeRes: %v", err)
	}
	if len(nativeRes.ExecutedLenses) != 0 {
		t.Fatalf("expected 0 executedLenses for agy-oauth, got %d", len(nativeRes.ExecutedLenses))
	}
	if len(nativeRes.NativeLensesPending) != 3 {
		t.Fatalf("expected 3 nativeLensesPending, got %v", nativeRes.NativeLensesPending)
	}
	if !strings.Contains(nativeRes.NextAction, "invoke_subagent") {
		t.Fatalf("expected nextAction to mention invoke_subagent, got %q", nativeRes.NextAction)
	}

	// 2. Mixed profile: review-risk=external-mcp, review-reliability=external-mcp (missing openai key -> fallback), review-readability=agy-native.
	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	ds := profiles.Providers["deepseek"]
	ds.BaseURL = srv.URL + "/v1"
	profiles.Providers["deepseek"] = ds
	prof := profiles.Profiles["deepseek-hybrid"]
	prof.Roles["review-readability"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "pro"}
	prof.Roles["review-reliability"] = config.RoleRoute{
		Runtime:  config.RuntimeExternalMCP,
		Provider: "openai-compatible",
		Model:    "gpt-4.1",
		Fallback: []config.RoleTarget{{Runtime: config.RuntimeAgyNative, Model: "pro"}},
	}
	profiles.Profiles["deepseek-hybrid"] = prof
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles mixed: %v", err)
	}

	mixedOut, isErr := cli.callTool("odd_review_run_lenses", map[string]any{
		"lineageId": startRes.LineageID,
	})
	if isErr {
		t.Fatalf("mixed odd_review_run_lenses failed: %s", mixedOut)
	}
	var mixedRes struct {
		ExecutedLenses []struct {
			Lens               string `json:"lens"`
			Status             string `json:"status"`
			SubmittedToLineage bool   `json:"submittedToLineage"`
			FallbackToNative   bool   `json:"fallbackToNative"`
		} `json:"executedLenses"`
		NativeLensesPending []string          `json:"nativeLensesPending"`
		NextAction          string            `json:"nextAction"`
		ReviewStatus        rdd.LineageStatus `json:"reviewStatus"`
	}
	if err := json.Unmarshal([]byte(mixedOut), &mixedRes); err != nil {
		t.Fatalf("unmarshal mixedRes: %v", err)
	}
	if len(mixedRes.ExecutedLenses) != 2 {
		t.Fatalf("expected 2 executedLenses in mixed profile, got %d: %s", len(mixedRes.ExecutedLenses), mixedOut)
	}
	// review-risk should succeed; review-reliability (missing OPENAI_API_KEY) should mark fallbackToNative=true and appear in nativeLensesPending alongside review-readability.
	if len(mixedRes.NativeLensesPending) != 2 {
		t.Fatalf("expected 2 nativeLensesPending ([review-reliability, review-readability]), got %v", mixedRes.NativeLensesPending)
	}
	pendingSet := map[string]bool{}
	for _, r := range mixedRes.NativeLensesPending {
		pendingSet[r] = true
	}
	if !pendingSet["review-reliability"] || !pendingSet["review-readability"] {
		t.Fatalf("nativeLensesPending = %v, want review-reliability and review-readability", mixedRes.NativeLensesPending)
	}
	if !strings.Contains(mixedRes.NextAction, "invoke_subagent") {
		t.Fatalf("expected nextAction to mention invoke_subagent, got %q", mixedRes.NextAction)
	}

	// 3. Optional lenses subset and invalid lens error handling.
	// Submit reliability and readability manually so lineage closes, then verify closed lineage fails cleanly.
	if _, isErr := cli.callTool("odd_review_submit", map[string]any{
		"lineageId": startRes.LineageID,
		"role":      "reliability",
		"findings":  []any{},
	}); isErr {
		t.Fatal("submit reliability failed")
	}

	// Invalid lens name fails cleanly.
	if badLensOut, badLensErr := cli.callTool("odd_review_run_lenses", map[string]any{
		"lineageId": startRes.LineageID,
		"lenses":    []string{"not-a-lens"},
	}); !badLensErr {
		t.Fatalf("expected error for invalid lens, got: %s", badLensOut)
	}

	// Already completed lens ("risk") fails cleanly.
	if doneLensOut, doneLensErr := cli.callTool("odd_review_run_lenses", map[string]any{
		"lineageId": startRes.LineageID,
		"lenses":    []string{"risk"},
	}); !doneLensErr {
		t.Fatalf("expected error for already-completed lens risk, got: %s", doneLensOut)
	}

	// Run subset ["readability"] via odd_review_run_lenses when readability is external-mcp.
	prof.Roles["review-readability"] = config.RoleRoute{
		Runtime:  config.RuntimeExternalMCP,
		Provider: "deepseek",
		Model:    "deepseek-v4-pro",
	}
	profiles.Profiles["deepseek-hybrid"] = prof
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles readability external: %v", err)
	}
	subOut, isErr := cli.callTool("odd_review_run_lenses", map[string]any{
		"lineageId": startRes.LineageID,
		"lenses":    []string{"readability"},
	})
	if isErr {
		t.Fatalf("subset odd_review_run_lenses failed: %s", subOut)
	}
	var subRes struct {
		ExecutedLenses      []struct{ Lens string } `json:"executedLenses"`
		NativeLensesPending []string                `json:"nativeLensesPending"`
		ReviewStatus        rdd.LineageStatus       `json:"reviewStatus"`
	}
	if err := json.Unmarshal([]byte(subOut), &subRes); err != nil {
		t.Fatalf("unmarshal subRes: %v", err)
	}
	if len(subRes.ExecutedLenses) != 1 || subRes.ExecutedLenses[0].Lens != "readability" {
		t.Fatalf("subset executedLenses = %+v, want [readability]", subRes.ExecutedLenses)
	}
	if len(subRes.NativeLensesPending) != 0 || subRes.ReviewStatus.Status != rdd.StatusApprovedPending {
		t.Fatalf("subset unexpected state: %+v", subRes)
	}

	// Ack and verify closed lineage rejects odd_review_run_lenses cleanly.
	if _, isErr := cli.callTool("odd_review_ack", map[string]any{"lineageId": startRes.LineageID}); isErr {
		t.Fatal("odd_review_ack failed")
	}
	if closedOut, closedErr := cli.callTool("odd_review_run_lenses", map[string]any{
		"lineageId": startRes.LineageID,
	}); !closedErr {
		t.Fatalf("expected error on closed lineage, got: %s", closedOut)
	}

	_ = inW.Close()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve error: %v", err)
	}
}
