package e2e_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("Abs repoRoot: %v", err)
	}
	return root
}

func buildBinaryOnce(t *testing.T, root string) string {
	t.Helper()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	binPath := filepath.Join(t.TempDir(), "odd-agy"+ext)
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/odd-agy")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/odd-agy failed: %v\n%s", err, string(out))
	}
	return binPath
}

func runCmd(t *testing.T, dir string, env []string, stdin string, bin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("exec %s %v failed: %v", bin, args, err)
		}
	}
	return code, stdout.String(), stderr.String()
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func initGitRepo(t *testing.T) string {
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
	runGit("config", "user.email", "e2e@example.com")
	runGit("config", "user.name", "E2E Runner")
	writeFile(t, filepath.Join(dir, "src", "app.go"), "package src\n\nfunc Value() int {\n\treturn 1\n}\n", 0o644)
	writeFile(t, filepath.Join(dir, "README.md"), "# E2E Demo\n", 0o644)
	runGit("add", "src/app.go", "README.md")
	runGit("commit", "-m", "initial commit")
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

type mcpClient struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	stderr bytes.Buffer
	nextID int
}

func startMCPClient(t *testing.T, bin, cwd string, extraEnv []string) *mcpClient {
	t.Helper()
	cmd := exec.Command(bin, "--cwd", cwd, "mcp")
	cmd.Dir = cwd
	if len(extraEnv) > 0 {
		cmd.Env = extraEnv
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	c := &mcpClient{
		t:      t,
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReader(stdout),
	}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mcp server: %v", err)
	}
	t.Cleanup(func() {
		_ = c.stdin.Close()
		_ = c.cmd.Wait()
	})
	return c
}

func (c *mcpClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.nextID++
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID,
		"method":  method,
		"params":  params,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		c.t.Fatalf("marshal mcp request: %v", err)
	}
	if _, err := c.stdin.Write(append(raw, '\n')); err != nil {
		c.t.Fatalf("write mcp request: %v", err)
	}
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("read mcp response for %s: %v (stderr=%s)", method, err, c.stderr.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		c.t.Fatalf("unmarshal mcp response %q: %v", string(line), err)
	}
	if errObj, ok := resp["error"]; ok && errObj != nil {
		c.t.Fatalf("mcp %s returned RPC error: %v", method, errObj)
	}
	return resp
}

func (c *mcpClient) callTool(name string, args any) map[string]any {
	c.t.Helper()
	resp := c.call("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	res, ok := resp["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("tool %s missing result object: %+v", name, resp)
	}
	if isErr, _ := res["isError"].(bool); isErr {
		c.t.Fatalf("tool %s returned isError=true: %+v", name, res)
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		c.t.Fatalf("tool %s missing content: %+v", name, res)
	}
	first := content[0].(map[string]any)
	text, _ := first["text"].(string)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		c.t.Fatalf("tool %s text is not JSON (%q): %v", name, text, err)
	}
	return decoded
}

func TestE2EFullProjectLifecycleViaBinaryAndPnpmWrapper(t *testing.T) {
	root := repoRoot(t)
	compiledBin := buildBinaryOnce(t, root)
	wrapperScript := filepath.Join(root, "bin", "odd-agy.mjs")

	repo := initGitRepo(t)
	fakeHome := t.TempDir()
	fakeBinDir := t.TempDir()
	// Provide deterministic agy + engram stubs on PATH so doctor passes cleanly offline.
	writeFile(t, filepath.Join(fakeBinDir, "agy"), "#!/bin/sh\necho 1.5.18\n", 0o755)
	writeFile(t, filepath.Join(fakeBinDir, "engram"), "#!/bin/sh\necho engram\n", 0o755)

	baseEnv := append(os.Environ(),
		"HOME="+fakeHome,
		"PATH="+fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ODD_AGY_BIN="+compiledBin,
	)

	// 1. Pre-seed user hook & user MCP server before init.
	writeFile(t, filepath.Join(repo, ".agents", "hooks.json"), `{
  "user-custom-hook": {
    "PreToolUse": [
      { "command": "echo user-hook" }
    ]
  }
}
`, 0o644)
	writeFile(t, filepath.Join(repo, ".agents", "mcp_config.json"), `{
  "mcpServers": {
    "user-custom-mcp": {
      "command": "echo",
      "args": ["user-mcp"]
    }
  }
}
`, 0o644)

	// 2. Run odd-agy init via node bin/odd-agy.mjs wrapper.
	code, out, errOut := runCmd(t, root, baseEnv, "", "node", wrapperScript,
		"--cwd", repo, "init", "--mode=standalone", "--binary="+compiledBin)
	if code != 0 || errOut != "" {
		t.Fatalf("wrapper init failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// Verify 5 rules (<24,000 bytes), 12 agents, 15 skills.
	rulesEntries, err := os.ReadDir(filepath.Join(repo, ".agents", "rules"))
	if err != nil || len(rulesEntries) != 5 {
		t.Fatalf("expected 5 rules, got %d (err=%v)", len(rulesEntries), err)
	}
	for _, entry := range rulesEntries {
		info, err := entry.Info()
		if err != nil || info.Size() >= 24000 {
			t.Fatalf("rule %s exceeds 24000 bytes or stat err: size=%d err=%v", entry.Name(), info.Size(), err)
		}
	}

	agentEntries, err := os.ReadDir(filepath.Join(repo, ".agents", "agents"))
	if err != nil || len(agentEntries) != 12 {
		t.Fatalf("expected 12 agents, got %d (err=%v)", len(agentEntries), err)
	}

	skillEntries, err := os.ReadDir(filepath.Join(repo, ".agents", "skills"))
	if err != nil || len(skillEntries) != 15 {
		t.Fatalf("expected 15 skills, got %d (err=%v)", len(skillEntries), err)
	}

	// Verify config.json & profiles.json.
	cfgBytes, err := os.ReadFile(filepath.Join(repo, ".agents", "odd", "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfgDoc map[string]any
	if err := json.Unmarshal(cfgBytes, &cfgDoc); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	if cfgDoc["delegationMode"] != "full-delegation" || cfgDoc["rddEnabled"] != true {
		t.Fatalf("unexpected config.json: %+v", cfgDoc)
	}

	profBytes, err := os.ReadFile(filepath.Join(repo, ".agents", "odd", "profiles.json"))
	if err != nil {
		t.Fatalf("read profiles.json: %v", err)
	}
	var profDoc map[string]any
	if err := json.Unmarshal(profBytes, &profDoc); err != nil {
		t.Fatalf("parse profiles.json: %v", err)
	}
	if profDoc["activeProfile"] != "agy-oauth" {
		t.Fatalf("expected activeProfile agy-oauth, got %v", profDoc["activeProfile"])
	}
	providers := profDoc["providers"].(map[string]any)
	for _, pName := range []string{"deepseek", "9router", "omniroute", "openai-compatible"} {
		if _, ok := providers[pName]; !ok {
			t.Fatalf("missing provider template %q in profiles.json", pName)
		}
	}

	// Verify user hook & user MCP server survived init.
	hooksRaw, _ := os.ReadFile(filepath.Join(repo, ".agents", "hooks.json"))
	if !strings.Contains(string(hooksRaw), "user-custom-hook") || !strings.Contains(string(hooksRaw), "odd-agy") {
		t.Fatalf("hooks.json missing user or managed hook:\n%s", string(hooksRaw))
	}
	mcpRaw, _ := os.ReadFile(filepath.Join(repo, ".agents", "mcp_config.json"))
	if !strings.Contains(string(mcpRaw), "user-custom-mcp") || !strings.Contains(string(mcpRaw), "odd-agy") {
		t.Fatalf("mcp_config.json missing user or managed mcp:\n%s", string(mcpRaw))
	}

	// 3. Mode & Profile switches + doctor.
	for _, args := range [][]string{
		{"--cwd", repo, "mode", "organic"},
		{"--cwd", repo, "mode", "full-delegation"},
		{"--cwd", repo, "profile", "use", "9router-hybrid"},
		{"--cwd", repo, "profile", "use", "agy-oauth"},
	} {
		code, _, errOut = runCmd(t, repo, baseEnv, "", compiledBin, args...)
		if code != 0 || errOut != "" {
			t.Fatalf("cmd %v failed: code=%d err=%q", args, code, errOut)
		}
	}

	code, docOut, docErr := runCmd(t, repo, baseEnv, "", compiledBin, "--cwd", repo, "doctor")
	if code != 0 || docErr != "" || !strings.Contains(docOut, "[ok]") || !strings.Contains(docOut, "managed-files") {
		t.Fatalf("doctor failed: code=%d out=%q err=%q", code, docOut, docErr)
	}

	// 4. PreToolUse hooks via compiled binary stdin/stdout.
	assertHookDecision := func(name string, payload map[string]any, wantDecision string) {
		t.Helper()
		b, _ := json.Marshal(payload)
		c, o, e := runCmd(t, repo, baseEnv, string(b), compiledBin, "hook", "PreToolUse")
		if c != 0 || e != "" {
			t.Fatalf("hook %s failed: code=%d err=%q", name, c, e)
		}
		var outDoc map[string]any
		if err := json.Unmarshal([]byte(o), &outDoc); err != nil {
			t.Fatalf("hook %s invalid JSON %q: %v", name, o, err)
		}
		if outDoc["decision"] != wantDecision {
			t.Fatalf("hook %s decision = %v, want %v (full=%s)", name, outDoc["decision"], wantDecision, o)
		}
	}

	assertHookDecision("deny-parent-source-write", map[string]any{
		"conversationId": "e2e-conv",
		"workspacePaths": []string{repo},
		"stepIdx":        1,
		"toolCall": map[string]any{
			"name": "write_to_file",
			"args": map[string]any{"TargetFile": filepath.Join(repo, "src", "app.go")},
		},
	}, "deny")

	assertHookDecision("allow-parent-odd-task-write", map[string]any{
		"conversationId": "e2e-conv",
		"workspacePaths": []string{repo},
		"stepIdx":        2,
		"toolCall": map[string]any{
			"name": "write_to_file",
			"args": map[string]any{"TargetFile": filepath.Join(repo, "odd", "tasks", "feature.md")},
		},
	}, "allow")

	assertHookDecision("deny-env-access", map[string]any{
		"conversationId": "e2e-conv",
		"workspacePaths": []string{repo},
		"stepIdx":        3,
		"toolCall": map[string]any{
			"name": "view_file",
			"args": map[string]any{"AbsolutePath": filepath.Join(repo, ".env")},
		},
	}, "deny")

	assertHookDecision("deny-worker-without-surfaces", map[string]any{
		"conversationId": "e2e-conv",
		"workspacePaths": []string{repo},
		"stepIdx":        4,
		"toolCall": map[string]any{
			"name": "invoke_subagent",
			"args": map[string]any{
				"Subagents": []map[string]any{{
					"Role":   "gentle-ai-worker",
					"Prompt": "Edit src/app.go without declaring surfaces",
				}},
			},
		},
	}, "deny")

	assertHookDecision("allow-worker-with-surfaces", map[string]any{
		"conversationId": "e2e-conv",
		"workspacePaths": []string{repo},
		"stepIdx":        5,
		"toolCall": map[string]any{
			"name": "invoke_subagent",
			"args": map[string]any{
				"Subagents": []map[string]any{{
					"Role":   "gentle-ai-worker",
					"Prompt": "## Allowed edit surfaces\n- src/app.go\n\nUpdate Value() to return 2.",
				}},
			},
		},
	}, "allow")

	// 5. Real MCP stdio child process + DeepSeek mock server.
	var dsTurn atomic.Int32
	var sawReasoningTurn2 atomic.Bool
	var sawReasoningContinue atomic.Bool

	dsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n := dsTurn.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    "ds-1",
				"model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "",
						"reasoning_content": "run thinking 1",
						"tool_calls": []map[string]any{{
							"id":   "call_write",
							"type": "function",
							"function": map[string]any{
								"name":      "write_file",
								"arguments": `{"path":"src/e2e_generated.go","content":"package src\n\nfunc Generated() int { return 1 }\n"}`,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
		case 2:
			for _, rawM := range req["messages"].([]any) {
				m := rawM.(map[string]any)
				if m["role"] == "assistant" && m["reasoning_content"] == "run thinking 1" {
					sawReasoningTurn2.Store(true)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    "ds-2",
				"model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "Created src/e2e_generated.go",
						"reasoning_content": "run thinking 2",
					},
					"finish_reason": "stop",
				}},
			})
		case 3:
			for _, rawM := range req["messages"].([]any) {
				m := rawM.(map[string]any)
				if m["role"] == "assistant" && m["reasoning_content"] == "run thinking 2" {
					sawReasoningContinue.Store(true)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    "ds-3",
				"model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "",
						"reasoning_content": "continue thinking 3",
						"tool_calls": []map[string]any{{
							"id":   "call_edit",
							"type": "function",
							"function": map[string]any{
								"name":      "edit_file",
								"arguments": `{"path":"src/e2e_generated.go","oldText":"return 1","newText":"return 42"}`,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    "ds-4",
				"model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "Updated src/e2e_generated.go to return 42",
						"reasoning_content": "continue thinking 4",
					},
					"finish_reason": "stop",
				}},
			})
		}
	}))
	defer dsServer.Close()

	// Create a real worktree modification for RDD frozen snapshot review.
	writeFile(t, filepath.Join(repo, "src", "app.go"), "package src\n\nfunc Value() int {\n\treturn 2\n}\n", 0o644)

	mcpEnv := append(append([]string(nil), baseEnv...),
		"DEEPSEEK_BASE_URL="+dsServer.URL+"/v1",
		"DEEPSEEK_API_KEY=ds-e2e-test-key",
	)
	client := startMCPClient(t, compiledBin, repo, mcpEnv)

	initResp := client.call("initialize", map[string]any{})
	if initResp["result"] == nil {
		t.Fatalf("expected initialize result")
	}
	listResp := client.call("tools/list", map[string]any{})
	toolsRaw, _ := json.Marshal(listResp)
	if !strings.Contains(string(toolsRaw), "odd_review_start") || !strings.Contains(string(toolsRaw), "odd_external_run") {
		t.Fatalf("tools/list missing expected tools: %s", string(toolsRaw))
	}

	todoWrite := client.callTool("todo", map[string]any{
		"action": "write",
		"tasks": []map[string]any{
			{"title": "Implement E2E change", "status": "in_progress"},
		},
	})
	if len(todoWrite["tasks"].([]any)) != 1 {
		t.Fatalf("unexpected todo write result: %+v", todoWrite)
	}
	todoList := client.callTool("todo", map[string]any{"action": "list"})
	if len(todoList["tasks"].([]any)) != 1 {
		t.Fatalf("unexpected todo list result: %+v", todoList)
	}

	assess := client.callTool("odd_review_assess", map[string]any{})
	if assess["rddEnabled"] != true || assess["reviewDue"] != true {
		t.Fatalf("unexpected odd_review_assess result: %+v", assess)
	}

	started := client.callTool("odd_review_start", map[string]any{
		"lenses":       []string{"risk"},
		"lensesReason": "E2E smoke check on src/app.go change",
	})
	lineageID, _ := started["lineageId"].(string)
	if lineageID == "" || started["consentRequired"] != true {
		t.Fatalf("unexpected odd_review_start result: %+v", started)
	}

	consent := client.callTool("odd_review_consent", map[string]any{
		"lineageId": lineageID,
		"answer":    "granted",
	})
	if consent["status"] != "collecting_lenses" {
		t.Fatalf("unexpected consent status: %+v", consent)
	}

	var scopeRawBuf strings.Builder
	cursor := 0
	for {
		scope := client.callTool("odd_review_scope", map[string]any{
			"lineageId": lineageID,
			"cursor":    cursor,
			"limit":     10,
		})
		b, _ := json.Marshal(scope)
		scopeRawBuf.Write(b)
		hasMore, _ := scope["hasMore"].(bool)
		if !hasMore {
			break
		}
		nextCur, _ := scope["nextCursor"].(float64)
		cursor = int(nextCur)
	}
	scopeAll := scopeRawBuf.String()
	if !strings.Contains(scopeAll, "src/app.go") || !strings.Contains(scopeAll, "return 2") {
		t.Fatalf("odd_review_scope missing frozen diff: %s", scopeAll)
	}

	submit := client.callTool("odd_review_submit", map[string]any{
		"lineageId": lineageID,
		"role":      "review-risk",
		"findings":  []any{},
	})
	if submit["status"] != "approved_pending_ack" {
		t.Fatalf("unexpected odd_review_submit status: %+v", submit)
	}

	ack := client.callTool("odd_review_ack", map[string]any{
		"lineageId": lineageID,
	})
	if ack["authority"] != "burned" {
		t.Fatalf("unexpected odd_review_ack result: %+v", ack)
	}

	extRun := client.callTool("odd_external_run", map[string]any{
		"role":            "worker",
		"provider":        "deepseek",
		"model":           "deepseek-v4-pro",
		"task":            "Create src/e2e_generated.go returning 1",
		"allowedSurfaces": []string{"src/e2e_generated.go"},
	})
	taskID, _ := extRun["taskId"].(string)
	if taskID == "" || extRun["status"] != "completed" {
		t.Fatalf("unexpected odd_external_run result: %+v", extRun)
	}

	extCont := client.callTool("odd_external_continue", map[string]any{
		"taskId":          taskID,
		"prompt":          "Update Generated() to return 42",
		"allowedSurfaces": []string{"src/e2e_generated.go"},
	})
	if extCont["status"] != "completed" {
		t.Fatalf("unexpected odd_external_continue result: %+v", extCont)
	}
	if !sawReasoningTurn2.Load() || !sawReasoningContinue.Load() {
		t.Fatalf("mock server did not observe reasoning_content round-trip: turn2=%t continue=%t",
			sawReasoningTurn2.Load(), sawReasoningContinue.Load())
	}

	genBytes, err := os.ReadFile(filepath.Join(repo, "src", "e2e_generated.go"))
	if err != nil || !strings.Contains(string(genBytes), "return 42") {
		t.Fatalf("expected sandboxed generated file with return 42, got %q (err=%v)", string(genBytes), err)
	}

	// Give child process clean exit before uninstalling.
	_ = client.stdin.Close()
	waitDone := make(chan error, 1)
	go func() { waitDone <- client.cmd.Wait() }()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		_ = client.cmd.Process.Kill()
	}

	// 6. Uninstall removes managed files/keys while keeping user hooks and user MCP servers intact.
	code, unOut, unErr := runCmd(t, repo, baseEnv, "", compiledBin, "--cwd", repo, "uninstall")
	if code != 0 || unErr != "" || !strings.Contains(unOut, "removed") {
		t.Fatalf("uninstall failed: code=%d out=%q err=%q", code, unOut, unErr)
	}

	if _, err := os.Stat(filepath.Join(repo, ".agents", "odd", ".managed-manifest.json")); !os.IsNotExist(err) {
		t.Fatal("managed manifest still exists after uninstall")
	}

	hooksAfter, err := os.ReadFile(filepath.Join(repo, ".agents", "hooks.json"))
	if err != nil || !strings.Contains(string(hooksAfter), "user-custom-hook") || strings.Contains(string(hooksAfter), `"odd-agy"`) {
		t.Fatalf("hooks.json after uninstall unexpected (err=%v):\n%s", err, string(hooksAfter))
	}
	mcpAfter, err := os.ReadFile(filepath.Join(repo, ".agents", "mcp_config.json"))
	if err != nil || !strings.Contains(string(mcpAfter), "user-custom-mcp") || strings.Contains(string(mcpAfter), `"odd-agy"`) {
		t.Fatalf("mcp_config.json after uninstall unexpected (err=%v):\n%s", err, string(mcpAfter))
	}
}
