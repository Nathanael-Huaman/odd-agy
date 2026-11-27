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
	"sync/atomic"
	"testing"

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
		"odd_review_scope", "odd_review_submit", "odd_review_apply_correction", "odd_review_status", "odd_review_ack",
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
