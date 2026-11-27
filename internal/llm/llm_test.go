package llm

import (
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

func initTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitCmd := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	runGitCmd("init")
	runGitCmd("config", "user.email", "test@example.com")
	runGitCmd("config", "user.name", "Test User")
	writeRepoFile(t, dir, "src/app.go", "package src\n\nfunc Value() int {\n\treturn 1\n}\n")
	writeRepoFile(t, dir, "README.md", "# Demo\n")
	runGitCmd("add", "src/app.go", "README.md")
	runGitCmd("commit", "-m", "initial")
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

func writeRepoFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

func TestDeepSeekThinkingPreservesReasoningContentAndDisablesRequiredToolChoice(t *testing.T) {
	var rawRequests []map[string]any
	var turn atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rawRequests = append(rawRequests, payload)

		n := turn.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    "resp-1",
				"model": "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           "",
						"reasoning_content": "step 1: inspect src/app.go",
						"tool_calls": []map[string]any{{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "read_file",
								"arguments": `{"path":"src/app.go"}`,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
			return
		}

		// Second turn: assert every assistant message has reasoning_content when tools are present.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "resp-2",
			"model": "deepseek-v4-pro",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           "Done reading src/app.go.",
					"reasoning_content": "step 2: summarized result",
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28},
		})
	}))
	defer srv.Close()

	repo := initTestGitRepo(t)
	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	ds := profiles.Providers["deepseek"]
	ds.BaseURL = srv.URL + "/v1"
	profiles.Providers["deepseek"] = ds
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	// Test direct client ToolChoice="required" coercion in deepseek-thinking mode.
	client := NewClient(srv.Client())
	ep, err := config.ResolveProviderEndpoint(ds, func(k string) (string, bool) {
		if k == "DEEPSEEK_API_KEY" {
			return "ds-secret-key", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("ResolveProviderEndpoint: %v", err)
	}
	_, err = client.ChatCompletions(context.Background(), ep, ChatRequest{
		Model:      "deepseek-v4-pro",
		ToolChoice: "required",
		Tools: []ToolDefinition{
			{Type: "function", Function: FunctionDefinition{Name: "z_tool", Description: "z"}},
			{Type: "function", Function: FunctionDefinition{Name: "a_tool", Description: "a"}},
		},
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c0", Type: "function", Function: ToolCallFunction{Name: "a_tool", Arguments: "{}"}}}},
			{Role: "tool", ToolCallID: "c0", Content: "ok"},
		},
	})
	if err != nil {
		t.Fatalf("direct ChatCompletions: %v", err)
	}

	if len(rawRequests) != 1 {
		t.Fatalf("expected 1 direct request, got %d", len(rawRequests))
	}
	if tc, exists := rawRequests[0]["tool_choice"]; exists && tc == "required" {
		t.Fatalf("deepseek-thinking must never send tool_choice=\"required\", got %v", tc)
	}
	// Deterministic tool ordering check: a_tool before z_tool.
	toolsSlice := rawRequests[0]["tools"].([]any)
	firstTool := toolsSlice[0].(map[string]any)["function"].(map[string]any)["name"]
	if firstTool != "a_tool" {
		t.Fatalf("expected tools deterministically sorted (a_tool first), got %v", firstTool)
	}
	// Every assistant message must carry reasoning_content key even if empty.
	msgs0 := rawRequests[0]["messages"].([]any)
	asst0 := msgs0[2].(map[string]any)
	if _, hasRC := asst0["reasoning_content"]; !hasRC {
		t.Fatalf("expected reasoning_content field on assistant message when tools are sent: %+v", asst0)
	}

	// Reset and run full multi-turn agentic loop through Runner.
	rawRequests = nil
	turn.Store(0)

	runner := NewRunner(RunnerConfig{
		RepoRoot: repo,
		Client:   client,
		LookupEnv: func(k string) (string, bool) {
			if k == "DEEPSEEK_API_KEY" {
				return "ds-secret-key", true
			}
			return "", false
		},
	})

	rec, err := runner.Run(context.Background(), RunRequest{
		Role: "explore", // explore in deepseek-hybrid is native; use verify for deepseek-thinking!
	})
	if err != nil {
		t.Fatalf("Runner.Run(explore) unexpected error: %v", err)
	}
	if rec.Status != TaskStatusFallbackNative {
		t.Fatalf("expected explore in deepseek-hybrid to signal fallback_to_native, got %q", rec.Status)
	}

	rawRequests = nil
	turn.Store(0)
	rec, err = runner.Run(context.Background(), RunRequest{
		Role: "verify",
		Task: "Inspect src/app.go",
	})
	if err != nil {
		t.Fatalf("Runner.Run(verify) failed: %v", err)
	}
	if rec.Status != TaskStatusCompleted {
		t.Fatalf("expected completed, got %q (err=%s)", rec.Status, rec.Error)
	}
	if len(rawRequests) != 2 {
		t.Fatalf("expected 2 LLM turns, got %d", len(rawRequests))
	}
	msgsTurn2 := rawRequests[1]["messages"].([]any)
	var foundAssistantWithRC bool
	for _, rawM := range msgsTurn2 {
		m := rawM.(map[string]any)
		if m["role"] == "assistant" {
			rc, ok := m["reasoning_content"]
			if !ok || rc != "step 1: inspect src/app.go" {
				t.Fatalf("turn 2 assistant message lost reasoning_content: %+v", m)
			}
			foundAssistantWithRC = true
		}
	}
	if !foundAssistantWithRC {
		t.Fatal("expected assistant message in turn 2")
	}

	// Continue must preserve reasoning_content from persisted task history.
	turn.Store(0)
	rawRequests = nil
	contRec, err := runner.Continue(context.Background(), ContinueRequest{
		TaskID: rec.TaskID,
		Prompt: "Confirm again",
	})
	if err != nil {
		t.Fatalf("Runner.Continue failed: %v", err)
	}
	if contRec.Status != TaskStatusCompleted {
		t.Fatalf("expected Continue completed, got %q", contRec.Status)
	}
	if len(rawRequests) == 0 {
		t.Fatal("expected at least 1 request on Continue")
	}
	contMsgs := rawRequests[0]["messages"].([]any)
	assistantCount := 0
	for _, rawM := range contMsgs {
		m := rawM.(map[string]any)
		if m["role"] == "assistant" {
			assistantCount++
			if _, ok := m["reasoning_content"]; !ok {
				t.Fatalf("Continue dropped reasoning_content on assistant message: %+v", m)
			}
		}
	}
	if assistantCount < 2 {
		t.Fatalf("expected at least 2 prior assistant messages on Continue, got %d", assistantCount)
	}
}

func TestNineRouterAndOmniRouteLoopbackAndBaseURLEnv(t *testing.T) {
	var seenAuths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuths = append(seenAuths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "r1",
			"model": "cc/claude-opus-4-6",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	repo := initTestGitRepo(t)
	profiles := config.DefaultProfilesConfig()

	// 1) OmniRoute with OMNIROUTE_BASE_URL override pointing to loopback httptest server and NO OMNIROUTE_API_KEY -> placeholder "omniroute-local".
	profiles.ActiveProfile = "omniroute-hybrid"
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}
	runnerOmni := NewRunner(RunnerConfig{
		RepoRoot: repo,
		Client:   NewClient(srv.Client()),
		LookupEnv: func(k string) (string, bool) {
			if k == "OMNIROUTE_BASE_URL" {
				return srv.URL + "/v1", true
			}
			return "", false
		},
	})
	recOmni, err := runnerOmni.Run(context.Background(), RunRequest{
		Role: "verify",
		Task: "check status",
	})
	if err != nil {
		t.Fatalf("omniroute Run failed: %v", err)
	}
	if recOmni.Status != TaskStatusCompleted {
		t.Fatalf("omniroute status = %q, want completed (%s)", recOmni.Status, recOmni.Error)
	}
	if len(seenAuths) != 1 || seenAuths[0] != "Bearer omniroute-local" {
		t.Fatalf("omniroute auth header = %v, want [Bearer omniroute-local]", seenAuths)
	}

	// 2) 9Router with NINEROUTER_BASE_URL override pointing to loopback httptest server and NO NINEROUTER_API_KEY -> empty Authorization header.
	seenAuths = nil
	profiles.ActiveProfile = "9router-hybrid"
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}
	runner9 := NewRunner(RunnerConfig{
		RepoRoot: repo,
		Client:   NewClient(srv.Client()),
		LookupEnv: func(k string) (string, bool) {
			if k == "NINEROUTER_BASE_URL" {
				return srv.URL + "/v1", true
			}
			return "", false
		},
	})
	rec9, err := runner9.Run(context.Background(), RunRequest{
		Role: "verify",
		Task: "check status",
	})
	if err != nil {
		t.Fatalf("9router Run failed: %v", err)
	}
	if rec9.Status != TaskStatusCompleted {
		t.Fatalf("9router status = %q, want completed (%s)", rec9.Status, rec9.Error)
	}
	if len(seenAuths) != 1 || seenAuths[0] != "" {
		t.Fatalf("9router loopback without key should omit Authorization header, got %v", seenAuths)
	}
}

func TestFallbackChain429ToSecondaryToAgyNative(t *testing.T) {
	primarySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer primarySrv.Close()

	secondaryOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "sec-1",
			"model": "gpt-4.1",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "secondary succeeded"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer secondaryOK.Close()

	repo := initTestGitRepo(t)
	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	ds := profiles.Providers["deepseek"]
	ds.BaseURL = primarySrv.URL + "/v1"
	profiles.Providers["deepseek"] = ds
	oai := profiles.Providers["openai-compatible"]
	oai.BaseURL = secondaryOK.URL + "/v1"
	profiles.Providers["openai-compatible"] = oai

	// Configure verify: primary deepseek (429) -> fallback[0] openai-compatible (200) -> fallback[1] agy-native.
	prof := profiles.Profiles["deepseek-hybrid"]
	prof.Roles["verify"] = config.RoleRoute{
		Runtime:  config.RuntimeExternalMCP,
		Provider: "deepseek",
		Model:    "deepseek-v4-pro",
		Fallback: []config.RoleTarget{
			{Runtime: config.RuntimeExternalMCP, Provider: "openai-compatible", Model: "gpt-4.1"},
			{Runtime: config.RuntimeAgyNative, Model: "pro"},
		},
	}
	profiles.Profiles["deepseek-hybrid"] = prof
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	env := func(k string) (string, bool) {
		if k == "DEEPSEEK_API_KEY" || k == "OPENAI_API_KEY" {
			return "test-key", true
		}
		return "", false
	}

	runner := NewRunner(RunnerConfig{RepoRoot: repo, LookupEnv: env})
	rec, err := runner.Run(context.Background(), RunRequest{
		Role: "verify",
		Task: "run verify",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if rec.Status != TaskStatusCompleted || rec.Provider != "openai-compatible" || rec.FinalResponse != "secondary succeeded" {
		t.Fatalf("expected fallback to secondary provider, got %+v", rec)
	}

	// Now make secondary unreachable (missing key) -> falls all the way through to agy-native signal!
	recNative, err := NewRunner(RunnerConfig{
		RepoRoot: repo,
		LookupEnv: func(k string) (string, bool) {
			if k == "DEEPSEEK_API_KEY" {
				return "test-key", true
			}
			return "", false // OPENAI_API_KEY missing
		},
	}).Run(context.Background(), RunRequest{
		Role: "verify",
		Task: "run verify",
	})
	if err != nil {
		t.Fatalf("Run fallback-to-native failed: %v", err)
	}
	if recNative.Status != TaskStatusFallbackNative || recNative.NativeFallback == nil || recNative.NativeFallback.Model != "pro" {
		t.Fatalf("expected fallback_to_native signal with model=pro, got %+v", recNative)
	}
}

func TestHardSandboxRejectsOutOfSurfaceSymlinkEnvAndCommitPush(t *testing.T) {
	repo := initTestGitRepo(t)
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outsideTarget, []byte("outside"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	// Symlink inside allowed surface pointing outside repo!
	if err := os.Symlink(outsideTarget, filepath.Join(repo, "src", "escape.go")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	var step atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := step.Add(1)
		w.Header().Set("Content-Type", "application/json")
		var tc ToolCall
		switch n {
		case 1:
			tc = ToolCall{ID: "t1", Type: "function", Function: ToolCallFunction{
				Name: "write_file", Arguments: `{"path":"README.md","content":"hijack"}`,
			}}
		case 2:
			tc = ToolCall{ID: "t2", Type: "function", Function: ToolCallFunction{
				Name: "edit_file", Arguments: `{"path":"src/escape.go","oldText":"outside","newText":"pwn"}`,
			}}
		case 3:
			tc = ToolCall{ID: "t3", Type: "function", Function: ToolCallFunction{
				Name: "write_file", Arguments: `{"path":".env","content":"SECRET=1"}`,
			}}
		case 4:
			tc = ToolCall{ID: "t4", Type: "function", Function: ToolCallFunction{
				Name: "run_shell", Arguments: `{"command":"git commit -m nope"}`,
			}}
		case 5:
			tc = ToolCall{ID: "t5", Type: "function", Function: ToolCallFunction{
				Name: "run_shell", Arguments: `{"command":"git push origin main"}`,
			}}
		case 6:
			tc = ToolCall{ID: "t6", Type: "function", Function: ToolCallFunction{
				Name: "write_file", Arguments: `{"path":"src/app.go","content":"package src\n\nfunc Value() int {\n\treturn 42\n}\n"}`,
			}}
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "done", "model": "gpt-4.1",
				"choices": []map[string]any{{
					"index": 0, "message": map[string]any{"role": "assistant", "content": "updated src/app.go"}, "finish_reason": "stop",
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "step", "model": "gpt-4.1",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":       "assistant",
					"content":    "",
					"tool_calls": []ToolCall{tc},
				},
				"finish_reason": "tool_calls",
			}},
		})
	}))
	defer srv.Close()

	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	oai := profiles.Providers["openai-compatible"]
	oai.BaseURL = srv.URL + "/v1"
	profiles.Providers["openai-compatible"] = oai
	prof := profiles.Profiles["deepseek-hybrid"]
	prof.Roles["worker"] = config.RoleRoute{
		Runtime:  config.RuntimeExternalMCP,
		Provider: "openai-compatible",
		Model:    "gpt-4.1",
	}
	profiles.Profiles["deepseek-hybrid"] = prof
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	runner := NewRunner(RunnerConfig{
		RepoRoot: repo,
		LookupEnv: func(k string) (string, bool) {
			if k == "OPENAI_API_KEY" {
				return "k", true
			}
			return "", false
		},
	})

	// 1) Reject writer without valid Allowed edit surfaces.
	if _, err := runner.Run(context.Background(), RunRequest{
		Role: "worker",
		Task: "Edit src/app.go without surfaces",
	}); err == nil {
		t.Fatal("expected writer without Allowed edit surfaces to fail")
	}

	// 2) Reject wildcard/root surfaces.
	for _, badSurface := range []string{".", "*", "**", "../outside", "/etc/passwd"} {
		if _, err := runner.Run(context.Background(), RunRequest{
			Role:            "worker",
			Task:            "bad surface",
			AllowedSurfaces: []string{badSurface},
		}); err == nil {
			t.Fatalf("expected invalid surface %q to be rejected", badSurface)
		}
	}

	// 3) Run with valid Allowed edit surfaces in task body.
	rec, err := runner.Run(context.Background(), RunRequest{
		Role: "worker",
		Task: "Update Value()\n\n## Allowed edit surfaces\n- src/app.go\n- src/escape.go\n",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if rec.Status != TaskStatusCompleted {
		t.Fatalf("expected completed, got %q (%s)", rec.Status, rec.Error)
	}

	// Verify README.md, outside.txt, .env were untouched and src/app.go was updated.
	readmeBytes, _ := os.ReadFile(filepath.Join(repo, "README.md"))
	if string(readmeBytes) != "# Demo\n" {
		t.Fatalf("README.md was unexpectedly modified: %q", string(readmeBytes))
	}
	outsideBytes, _ := os.ReadFile(outsideTarget)
	if string(outsideBytes) != "outside" {
		t.Fatalf("symlink escape modified outside file: %q", string(outsideBytes))
	}
	if _, err := os.Stat(filepath.Join(repo, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env should never be created")
	}
	appBytes, _ := os.ReadFile(filepath.Join(repo, "src", "app.go"))
	if !strings.Contains(string(appBytes), "42") {
		t.Fatalf("expected src/app.go to be updated to 42, got %q", string(appBytes))
	}

	// Verify tool error responses were fed back to model for blocked actions.
	var toolErrors int
	for _, m := range rec.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "error") {
			toolErrors++
		}
	}
	if toolErrors < 5 {
		t.Fatalf("expected at least 5 sandbox tool rejections recorded in history, got %d", toolErrors)
	}

	// Verify persisted task file has 0600 permissions.
	taskFile := filepath.Join(repo, ".git", "odd-agy", "tasks", rec.TaskID+".json")
	info, err := os.Stat(taskFile)
	if err != nil {
		t.Fatalf("stat task file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("task file perm = %04o, want 0600", perm)
	}

	// Verify Continue inherits AllowedSurfaces when replacementSurfaces is nil.
	step.Store(0)
	// Reuse server: step 1 writes README.md (must still be rejected!).
	contRec, err := runner.Continue(context.Background(), ContinueRequest{
		TaskID: rec.TaskID,
		Prompt: "Try editing README.md again",
	})
	if err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	readmeAfter, _ := os.ReadFile(filepath.Join(repo, "README.md"))
	if string(readmeAfter) != "# Demo\n" {
		t.Fatalf("Continue failed to inherit AllowedSurfaces: README.md modified to %q", string(readmeAfter))
	}
	if len(contRec.AllowedSurfaces) != 2 {
		t.Fatalf("expected inherited AllowedSurfaces, got %v", contRec.AllowedSurfaces)
	}
}

func TestProbeProviderRedactsURLAndCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-v4-pro"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// Inject userinfo and query secret into URL; ProbeProvider must redact both.
	injectedURL := strings.Replace(srv.URL, "http://", "http://user:supersecret@", 1) + "/v1?token=leakme"
	res := ProbeProvider(context.Background(), "custom", config.ProviderConfig{
		BaseURL:       injectedURL,
		APIKeyEnv:     "CUSTOM_KEY",
		Compatibility: config.CompatibilityStandard,
		DefaultModel:  "m1",
	}, func(k string) (string, bool) {
		if k == "CUSTOM_KEY" {
			return "actual-api-key-value", true
		}
		return "", false
	})

	if !res.Reachable {
		t.Fatalf("expected Reachable=true, got %+v", res)
	}
	if strings.Contains(res.RedactedURL, "supersecret") || strings.Contains(res.RedactedURL, "user") || strings.Contains(res.RedactedURL, "leakme") {
		t.Fatalf("RedactedURL leaked credentials or query params: %q", res.RedactedURL)
	}
	if strings.Contains(res.Error, "actual-api-key-value") {
		t.Fatalf("Error leaked API key")
	}

	// Unreachable provider with secret in URL must also redact URL in Error.
	badRes := ProbeProvider(context.Background(), "bad", config.ProviderConfig{
		BaseURL:       "http://admin:topsecret@127.0.0.1:1/v1?api_key=leak",
		APIKeyEnv:     "CUSTOM_KEY",
		Compatibility: config.CompatibilityStandard,
		DefaultModel:  "m1",
	}, func(k string) (string, bool) {
		return "secret-key-xyz", true
	})
	if badRes.Reachable {
		t.Fatal("expected unreachable for closed port")
	}
	if strings.Contains(badRes.RedactedURL, "topsecret") || strings.Contains(badRes.Error, "topsecret") || strings.Contains(badRes.Error, "secret-key-xyz") || strings.Contains(badRes.Error, "leak") {
		t.Fatalf("ProbeProvider leaked secret in unreachable result: %+v", badRes)
	}
}

func TestExternalReviewRoleHasFrozenScopeToolAndNoWriteTools(t *testing.T) {
	repo := initTestGitRepo(t)
	writeRepoFile(t, repo, "src/app.go", "package src\n\nfunc Value() int {\n\treturn 99\n}\n")

	eng := rdd.NewEngine(repo)
	startRes, err := eng.Start(rdd.StartRequest{
		Lenses:       []string{"risk"},
		LensesReason: "check Value change",
	})
	if err != nil {
		t.Fatalf("rdd.Start: %v", err)
	}
	if _, err := eng.AnswerConsent(startRes.LineageID, "granted"); err != nil {
		t.Fatalf("rdd.AnswerConsent: %v", err)
	}

	var step atomic.Int32
	var seenToolNames []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if step.Load() == 0 {
			for _, item := range payload["tools"].([]any) {
				seenToolNames = append(seenToolNames, item.(map[string]any)["function"].(map[string]any)["name"].(string))
			}
		}
		n := step.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "rev1", "model": "deepseek-v4-pro",
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
								"name":      "odd_review_scope",
								"arguments": `{"lineageId":"` + startRes.LineageID + `","cursor":0,"limit":10}`,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "rev2", "model": "deepseek-v4-pro",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           "reviewed frozen snapshot cleanly",
					"reasoning_content": "no blockers",
				},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "deepseek-hybrid"
	ds := profiles.Providers["deepseek"]
	ds.BaseURL = srv.URL + "/v1"
	profiles.Providers["deepseek"] = ds
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	runner := NewRunner(RunnerConfig{
		RepoRoot: repo,
		LookupEnv: func(k string) (string, bool) {
			if k == "DEEPSEEK_API_KEY" {
				return "ds-key", true
			}
			return "", false
		},
	})
	rec, err := runner.Run(context.Background(), RunRequest{
		Role: "review-risk",
		Task: "Review lineage " + startRes.LineageID,
	})
	if err != nil {
		t.Fatalf("Runner.Run(review-risk) failed: %v", err)
	}
	if rec.Status != TaskStatusCompleted {
		t.Fatalf("status = %q, want completed (%s)", rec.Status, rec.Error)
	}

	hasScope := false
	for _, name := range seenToolNames {
		if name == "odd_review_scope" {
			hasScope = true
		}
		if name == "write_file" || name == "edit_file" || name == "run_shell" {
			t.Fatalf("read-only review-risk must not receive tool %q", name)
		}
	}
	if !hasScope {
		t.Fatalf("review-risk must receive odd_review_scope tool, got %v", seenToolNames)
	}
	// Confirm odd_review_scope tool output actually contained the frozen candidate file.
	var sawScopeOutput bool
	for _, m := range rec.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "src/app.go") {
			sawScopeOutput = true
		}
	}
	if !sawScopeOutput {
		t.Fatalf("expected odd_review_scope tool message to contain src/app.go")
	}
}

func TestRunnerShellSandboxBlocksEscapesAndGitFlags(t *testing.T) {
	repo := initTestGitRepo(t)
	writeRepoFile(t, repo, "src/main.go", "package main\n\nfunc main() {}\n")
	writeRepoFile(t, repo, "server.pem", "PRIVATE CERT\n")
	writeRepoFile(t, repo, "certs/dev.key", "PRIVATE KEY\n")
	writeRepoFile(t, repo, ".aws/credentials", "[default]\n")
	writeRepoFile(t, repo, ".npmrc", "//registry/:_authToken=secret\n")

	// Create ../outside.txt relative to repoRoot.
	parentOutside := filepath.Join(filepath.Dir(repo), "outside.txt")
	if err := os.WriteFile(parentOutside, []byte("secret outside"), 0o644); err != nil {
		t.Fatalf("write parentOutside: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(parentOutside) })

	// Create an in-workspace symlink pointing to server.pem.
	if err := os.Symlink(filepath.Join(repo, "server.pem"), filepath.Join(repo, "innocent.txt")); err != nil {
		t.Fatalf("symlink to pem: %v", err)
	}

	runner := NewRunner(RunnerConfig{RepoRoot: repo})
	writerSurfaces := []string{"src/**"}

	blockedForWriter := []struct {
		name string
		cmd  string
	}{
		{"outside absolute path", "cat /etc/hosts"},
		{"relative parent escape", "cat ../outside.txt"},
		{"git -C commit bypass", "git -C . commit --allow-empty -m bypass"},
		{"git -c push bypass", "git -c user.name=x push origin main"},
		{"git reset hard", "git reset --hard"},
		{"git clean -fd", "git clean -fd"},
		{"read .pem", "cat server.pem"},
		{"read .key", "cat certs/dev.key"},
		{"read .aws/credentials", "cat .aws/credentials"},
		{"read .npmrc", "cat .npmrc"},
		{"symlink to .pem", "cat innocent.txt"},
		{"shell redirect outside surface", "echo pwn > outside_surface.txt"},
		{"shell append outside surface", "echo pwn >> README.md"},
		{"tee outside surface", "echo pwn | tee outside_surface.txt"},
		{"sed -i outside surface", "sed -i.bak 's/Demo/Pwn/' README.md"},
		{"perl -i outside surface", "perl -i -pe 's/Demo/Pwn/' README.md"},
		{"touch outside surface", "touch outside_surface.txt"},
		{"mv outside surface", "mv src/main.go outside_surface.txt"},
		{"cp outside surface", "cp src/main.go outside_surface.txt"},
		{"rm outside surface", "rm README.md"},
		{"nested sh -c bypass", "sh -c 'git -C . commit -m nope'"},
		{"command substitution escape", "echo $(cat /etc/hosts)"},
	}

	for _, tc := range blockedForWriter {
		t.Run("blocked_writer/"+tc.name, func(t *testing.T) {
			out := runner.toolRunShell("worker", writerSurfaces, tc.cmd)
			if !strings.Contains(out, `"error"`) {
				t.Fatalf("expected command %q to be blocked, got: %s", tc.cmd, out)
			}
		})
	}

	// Ensure outside_surface.txt was never created and README.md is intact.
	if _, err := os.Stat(filepath.Join(repo, "outside_surface.txt")); !os.IsNotExist(err) {
		t.Fatal("outside_surface.txt must not be created by blocked shell commands")
	}

	// Read-only verify role must not mutate workspace files even inside src/**.
	for _, cmd := range []string{
		"echo pwn > src/main.go",
		"touch src/new.go",
		"echo pwn | tee src/main.go",
		"sed -i 's/main/pwn/' src/main.go",
	} {
		out := runner.toolRunShell("verify", nil, cmd)
		if !strings.Contains(out, `"error"`) {
			t.Fatalf("expected verify shell write %q to be blocked, got: %s", cmd, out)
		}
	}

	// Valid read-only commands must succeed for verify role.
	for _, cmd := range []string{
		"cat src/main.go",
		"go version",
		"git status --short",
		"echo ok > /dev/null",
		"go test ./... 2>&1 || true",
	} {
		out := runner.toolRunShell("verify", nil, cmd)
		if strings.Contains(out, `"error"`) {
			t.Fatalf("expected valid verify command %q to succeed, got: %s", cmd, out)
		}
	}

	// Verify mktemp -d is allowed and clean up the created temp directory in test teardown.
	mkOut := runner.toolRunShell("verify", nil, "mktemp -d")
	if strings.Contains(mkOut, `"error"`) {
		t.Fatalf("expected mktemp -d to succeed, got: %s", mkOut)
	}
	var mkRes struct {
		ExitCode int    `json:"exitCode"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal([]byte(mkOut), &mkRes); err == nil && mkRes.ExitCode == 0 {
		if created := strings.TrimSpace(mkRes.Output); created != "" {
			t.Cleanup(func() { _ = os.RemoveAll(created) })
		}
	}

	// Valid in-surface shell write must succeed for worker role.
	okOut := runner.toolRunShell("worker", writerSurfaces, "echo '// ok' > src/generated.go")
	if strings.Contains(okOut, `"error"`) {
		t.Fatalf("expected in-surface write to succeed, got: %s", okOut)
	}
	genBytes, err := os.ReadFile(filepath.Join(repo, "src", "generated.go"))
	if err != nil || !strings.Contains(string(genBytes), "// ok") {
		t.Fatalf("expected src/generated.go written, err=%v content=%q", err, string(genBytes))
	}
}

func TestParseAllowedSurfacesIgnoresProseAfterBlankLine(t *testing.T) {
	task := `Implement feature X.

## Allowed edit surfaces

- internal/llm/**
- ` + "`internal/mcp/server.go`" + `

Please keep changes minimal and run go test after editing.
Do not touch unrelated packages.

## Verification
- go test ./internal/llm/...
`
	got := ParseAllowedSurfaces(task)
	if len(got) != 2 || got[0] != "internal/llm/**" || got[1] != "internal/mcp/server.go" {
		t.Fatalf("ParseAllowedSurfaces = %v, want [internal/llm/** internal/mcp/server.go]", got)
	}
}

func TestRunnerShellSandboxBlocksAbsoluteOutsidePathInTempDir(t *testing.T) {
	repo := initTestGitRepo(t)
	siblingDir := t.TempDir()
	outsideRead := filepath.Join(siblingDir, "outside_abs.txt")
	if err := os.WriteFile(outsideRead, []byte("TOP-SECRET-OUTSIDE\n"), 0o600); err != nil {
		t.Fatalf("write outside_abs.txt: %v", err)
	}
	outsideWrite := filepath.Join(siblingDir, "outside_write_bypass.txt")
	tmpWrite := filepath.Join(os.TempDir(), "odd-agy-outside-write-bypass.txt")
	t.Cleanup(func() {
		_ = os.Remove(outsideWrite)
		_ = os.Remove(tmpWrite)
	})

	runner := NewRunner(RunnerConfig{RepoRoot: repo})
	surfaces := []string{"src/**"}

	for _, role := range []string{"worker", "verify"} {
		out := runner.toolRunShell(role, surfaces, "cat "+outsideRead)
		if !strings.Contains(out, `"error"`) {
			t.Fatalf("role %q: expected cat %s to be blocked, got: %s", role, outsideRead, out)
		}
		if strings.Contains(out, "TOP-SECRET-OUTSIDE") {
			t.Fatalf("role %q: leaked outside file content: %s", role, out)
		}
	}

	for _, target := range []string{outsideWrite, tmpWrite, "/tmp/outside_write_bypass.txt"} {
		out := runner.toolRunShell("worker", surfaces, "echo pwn > "+target)
		if !strings.Contains(out, `"error"`) {
			t.Fatalf("expected shell write to %s to be blocked, got: %s", target, out)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("file %s must not be created by blocked shell write", target)
		}
	}
}

func TestRunShellRDDHardeningR1AndR2(t *testing.T) {
	runner := NewRunner(RunnerConfig{RepoRoot: initTestGitRepo(t)})
	for _, cmd := range []string{
		`echo "git push" | sh`, `sh -ec "git push"`, `awk 'BEGIN{system("id")}'`,
		`xargs rm`, `perl -e 'print 1'`, `X=..; cat $X/secret`,
		`git -c alias.x='!id' x`, `env`, `printenv`,
	} {
		if out := runner.toolRunShell("worker", []string{"src/**"}, cmd); !strings.Contains(out, `"error"`) {
			t.Fatalf("%q: expected denial, got: %s", cmd, out)
		}
	}
	for _, k := range []string{"DEEPSEEK_API_KEY", "OPENAI_API_KEY", "GITHUB_TOKEN", "MY_SECRET_TOKEN"} {
		t.Setenv(k, "secret-"+k)
	}
	t.Setenv("GOPATH", "/tmp/gopath")
	envMap := map[string]bool{}
	for _, e := range sanitizedShellEnv() {
		if k, _, ok := strings.Cut(e, "="); ok {
			envMap[k] = true
		}
	}
	for _, secret := range []string{"DEEPSEEK_API_KEY", "OPENAI_API_KEY", "GITHUB_TOKEN", "MY_SECRET_TOKEN"} {
		if envMap[secret] {
			t.Fatalf("sanitizedShellEnv leaked %s", secret)
		}
	}
	for _, safe := range []string{"PATH", "HOME", "GOPATH"} {
		if !envMap[safe] {
			t.Fatalf("sanitizedShellEnv dropped %s", safe)
		}
	}
}
