package llm

import (
	"context"
	"encoding/json"
	"errors"
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

func TestProbeProviderAndRoleChainUseStoredSecretsWhenLookupEnvNil(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("DEEPSEEK_API_KEY", "")

	const storedKey = "sk-stored-deepseek-test"
	if err := config.SetSecret(fakeHome, "DEEPSEEK_API_KEY", storedKey); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-v4-pro"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// nil lookupEnv picks up stored key from HOME/.config/odd-agy/secrets.json.
	res := ProbeProvider(context.Background(), "deepseek", config.ProviderConfig{
		BaseURL:       srv.URL + "/v1",
		APIKeyEnv:     "DEEPSEEK_API_KEY",
		Compatibility: config.CompatibilityDeepSeekThinking,
		DefaultModel:  "deepseek-v4-pro",
	}, nil)
	if !res.Reachable {
		t.Fatalf("ProbeProvider with nil lookupEnv and stored secret failed: %+v", res)
	}
	if gotAuth != "Bearer "+storedKey {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer "+storedKey)
	}
	if res.KeySource != "stored" {
		t.Fatalf("res.KeySource = %q, want stored", res.KeySource)
	}

	// Explicit lookupEnv stays hermetic (does NOT fall back to stored secret).
	hermeticRes := ProbeProvider(context.Background(), "deepseek", config.ProviderConfig{
		BaseURL:       srv.URL + "/v1",
		APIKeyEnv:     "DEEPSEEK_API_KEY",
		Compatibility: config.CompatibilityDeepSeekThinking,
		DefaultModel:  "deepseek-v4-pro",
	}, func(string) (string, bool) { return "", false })
	if hermeticRes.Reachable || !hermeticRes.MissingAPIKey {
		t.Fatalf("expected explicit empty lookupEnv to remain hermetic and report missing key, got %+v", hermeticRes)
	}

	// ResolveRoleChain with nil lookupEnv also resolves stored secret.
	profs := config.DefaultProfilesConfig()
	profs.ActiveProfile = "deepseek-hybrid"
	cands, _, err := ResolveRoleChain("verify", profs, "", "", nil)
	if err != nil {
		t.Fatalf("ResolveRoleChain: %v", err)
	}
	if len(cands) == 0 || cands[0].Skipped || cands[0].Endpoint.APIKey != storedKey {
		t.Fatalf("ResolveRoleChain did not resolve stored secret: %+v", cands)
	}

	// Explicit lookupEnv in ResolveRoleChain remains hermetic (skips missing key instead of reading stored secret).
	hermeticCands, _, err := ResolveRoleChain("verify", profs, "", "", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("ResolveRoleChain hermetic: %v", err)
	}
	if len(hermeticCands) == 0 || !hermeticCands[0].Skipped {
		t.Fatalf("expected explicit empty lookupEnv in ResolveRoleChain to skip candidate, got %+v", hermeticCands)
	}
}

func TestNewRunnerUsesStoredSecretsWhenLookupEnvNilAndHermeticWhenInjected(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("DEEPSEEK_API_KEY", "")

	const storedKey = "sk-runner-stored-secret"
	if err := config.SetSecret(fakeHome, "DEEPSEEK_API_KEY", storedKey); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	var gotAuths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuths = append(gotAuths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "r1", "model": "deepseek-v4-pro",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok", "reasoning_content": "done"},
				"finish_reason": "stop",
			}},
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

	// Default runner (LookupEnv == nil) reads stored secret and reports keySource="stored".
	defaultRunner := NewRunner(RunnerConfig{RepoRoot: repo})
	sum, err := defaultRunner.ProfilesSummary()
	if err != nil {
		t.Fatalf("ProfilesSummary: %v", err)
	}
	dsSum := sum.Providers["deepseek"]
	if !dsSum.APIKeyPresent || dsSum.KeySource != "stored" {
		t.Fatalf("deepseek summary = %+v, want apiKeyPresent=true keySource=stored", dsSum)
	}
	if sum.Providers["9router"].KeySource != "loopback-optional" || sum.Providers["9router"].APIKeyPresent {
		t.Fatalf("9router summary = %+v, want keySource=loopback-optional apiKeyPresent=false", sum.Providers["9router"])
	}
	if sum.Providers["openai-compatible"].KeySource != "missing" || sum.Providers["openai-compatible"].APIKeyPresent {
		t.Fatalf("openai-compatible summary = %+v, want keySource=missing apiKeyPresent=false", sum.Providers["openai-compatible"])
	}

	rec, err := defaultRunner.Run(context.Background(), RunRequest{Role: "verify", Task: "Inspect"})
	if err != nil || rec.Status != TaskStatusCompleted {
		t.Fatalf("defaultRunner.Run = (%+v, %v), want completed", rec, err)
	}
	if len(gotAuths) != 1 || gotAuths[0] != "Bearer "+storedKey {
		t.Fatalf("gotAuths = %v, want [Bearer %s]", gotAuths, storedKey)
	}

	// Env var takes precedence over stored secret.
	t.Setenv("DEEPSEEK_API_KEY", "sk-runner-env-secret")
	sumEnv, err := defaultRunner.ProfilesSummary()
	if err != nil {
		t.Fatalf("ProfilesSummary(env): %v", err)
	}
	if !sumEnv.Providers["deepseek"].APIKeyPresent || sumEnv.Providers["deepseek"].KeySource != "env" {
		t.Fatalf("deepseek summary(env) = %+v, want apiKeyPresent=true keySource=env", sumEnv.Providers["deepseek"])
	}

	// Explicit LookupEnv != nil stays 100% hermetic.
	hermeticRunner := NewRunner(RunnerConfig{
		RepoRoot:  repo,
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	hSum, err := hermeticRunner.ProfilesSummary()
	if err != nil {
		t.Fatalf("hermetic ProfilesSummary: %v", err)
	}
	if hSum.Providers["deepseek"].APIKeyPresent || hSum.Providers["deepseek"].KeySource != "missing" {
		t.Fatalf("hermeticRunner leaked stored secret: %+v", hSum.Providers["deepseek"])
	}
}

func TestRunShellIsolatesHomeAndBlocksSecretsJSON(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(fakeHome, ".config"))

	const secretVal = "sk-isolated-home-must-not-read"
	if err := config.SetSecret(fakeHome, "DEEPSEEK_API_KEY", secretVal); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	repo := initTestGitRepo(t)
	writeRepoFile(t, repo, "go.mod", "module example.com/demo\n\ngo 1.22\n")
	writeRepoFile(t, repo, "src/app_test.go", `package src

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestValueAndIsolatedEnv(t *testing.T) {
	if Value() != 1 {
		t.Fatal("bad")
	}
	home := os.Getenv("HOME")
	xdg := os.Getenv("XDG_CONFIG_HOME")
	fmt.Printf("ISOLATED_HOME=%s\nISOLATED_XDG=%s\n", home, xdg)
	if _, err := os.Stat(filepath.Join(home, ".config", "odd-agy", "secrets.json")); !os.IsNotExist(err) {
		t.Fatalf("secrets.json unexpectedly present in child HOME %s", home)
	}
}
`)

	// Also plant an in-workspace secrets.json and .config/odd-agy/data.json and a symlink named secrets.json.
	writeRepoFile(t, repo, "secrets.json", `{"leak":"direct"}`)
	writeRepoFile(t, repo, ".config/odd-agy/token.txt", "leak-dir")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fakeHome, ".config", "odd-agy", "secrets.json"), filepath.Join(repo, "sub", "secrets.json")); err != nil {
		t.Fatalf("symlink secrets.json: %v", err)
	}

	runner := NewRunner(RunnerConfig{RepoRoot: repo})

	// 1) sanitizedShellEnv must never forward the user's real HOME or XDG_CONFIG_HOME.
	for _, entry := range sanitizedShellEnv() {
		k, v, _ := strings.Cut(entry, "=")
		if k == "HOME" && v == fakeHome {
			t.Fatalf("sanitizedShellEnv forwarded real HOME %q", fakeHome)
		}
		if k == "XDG_CONFIG_HOME" && v == filepath.Join(fakeHome, ".config") {
			t.Fatalf("sanitizedShellEnv forwarded real XDG_CONFIG_HOME %q", v)
		}
	}

	// 2) go env preserves GOPATH, GOMODCACHE, GOCACHE; go test works cleanly and receives isolated HOME/XDG_CONFIG_HOME.
	goEnvOut := runner.toolRunShell("verify", nil, "go env GOPATH GOMODCACHE GOCACHE")
	if strings.Contains(goEnvOut, `"error"`) {
		t.Fatalf("go env failed: %s", goEnvOut)
	}
	var goEnvRes struct {
		ExitCode int    `json:"exitCode"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal([]byte(goEnvOut), &goEnvRes); err != nil || goEnvRes.ExitCode != 0 {
		t.Fatalf("go env unexpected: %s", goEnvOut)
	}
	goEnvLines := strings.Split(strings.TrimSpace(goEnvRes.Output), "\n")
	if len(goEnvLines) != 3 || strings.TrimSpace(goEnvLines[0]) == "" || strings.TrimSpace(goEnvLines[1]) == "" || strings.TrimSpace(goEnvLines[2]) == "" {
		t.Fatalf("expected non-empty GOPATH, GOMODCACHE, GOCACHE from go env, got %q", goEnvRes.Output)
	}

	goTestOut := runner.toolRunShell("verify", nil, "go test -v ./src")
	var goTestRes struct {
		ExitCode int    `json:"exitCode"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal([]byte(goTestOut), &goTestRes); err != nil {
		t.Fatalf("unmarshal goTestOut: %v (%s)", err, goTestOut)
	}
	if goTestRes.ExitCode != 0 {
		t.Fatalf("go test -v ./src inside toolRunShell failed: exit=%d out=%s", goTestRes.ExitCode, goTestRes.Output)
	}
	var childHome, childXDG string
	for _, line := range strings.Split(goTestRes.Output, "\n") {
		if strings.HasPrefix(line, "ISOLATED_HOME=") {
			childHome = strings.TrimPrefix(line, "ISOLATED_HOME=")
		}
		if strings.HasPrefix(line, "ISOLATED_XDG=") {
			childXDG = strings.TrimPrefix(line, "ISOLATED_XDG=")
		}
	}
	if childHome == "" || childHome == fakeHome || !strings.Contains(filepath.Base(childHome), "odd-agy-shell-home-") {
		t.Fatalf("child HOME = %q, must be isolated odd-agy-shell-home-* != %q", childHome, fakeHome)
	}
	if childXDG != filepath.Join(childHome, ".config") {
		t.Fatalf("child XDG_CONFIG_HOME = %q, want %q", childXDG, filepath.Join(childHome, ".config"))
	}
	// Temp shell HOME directory must be cleaned up after toolRunShell returns.
	if _, err := os.Stat(childHome); !os.IsNotExist(err) {
		t.Fatalf("expected shellHome %q to be removed after toolRunShell, stat err=%v", childHome, err)
	}

	if pyPath, err := exec.LookPath("python3"); err == nil && pyPath != "" {
		pyOut := runner.toolRunShell("verify", nil, `python3 -c "import os, pathlib; p = pathlib.Path(os.environ['HOME']) / '.config' / 'odd-agy' / 'secrets.json'; print(os.environ['HOME'], p.exists())"`)
		if strings.Contains(pyOut, `"error"`) || strings.Contains(pyOut, secretVal) || strings.Contains(pyOut, "True") || strings.Contains(pyOut, fakeHome) {
			t.Fatalf("python3 saw host HOME or secrets.json: %s", pyOut)
		}
	}

	// 3) secrets.json and .config/odd-agy are blocked by isSensitiveRelPath in shell & file tools.
	for _, blockedCmd := range []string{
		"cat secrets.json",
		"cat sub/secrets.json",
		"cat .config/odd-agy/token.txt",
	} {
		if out := runner.toolRunShell("verify", nil, blockedCmd); !strings.Contains(out, `"error"`) {
			t.Fatalf("expected %q to be blocked, got: %s", blockedCmd, out)
		}
	}
	for _, blockedPath := range []string{"secrets.json", "sub/secrets.json", ".config/odd-agy/token.txt"} {
		if out := runner.toolReadFile(blockedPath, 0, 0); !strings.Contains(out, `"error"`) {
			t.Fatalf("expected read_file(%q) to be blocked, got: %s", blockedPath, out)
		}
	}
}

func TestChatCompletionsSendsStreamFalse(t *testing.T) {
	var rawBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &rawBody); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "json-1",
			"model": "gpt-4.1",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	client := NewClient(srv.Client())
	ep := config.ResolvedEndpoint{
		BaseURL: srv.URL + "/v1",
	}
	resp, err := client.ChatCompletions(context.Background(), ep, ChatRequest{
		Model:    "gpt-4.1",
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Choices[0].Message.Content)
	}
	streamVal, exists := rawBody["stream"]
	if !exists {
		t.Fatalf("expected request JSON to include explicit \"stream\": false, got body: %+v", rawBody)
	}
	streamBool, ok := streamVal.(bool)
	if !ok || streamBool {
		t.Fatalf("expected request JSON \"stream\" to be false, got %#v", streamVal)
	}
}

func TestChatCompletionsSSEReasoningAndTextChunks(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		sseBody     string
	}{
		{
			name:        "text/event-stream with CRLF, comments, multi-line data, implicit assistant role",
			contentType: "text/event-stream; charset=utf-8",
			sseBody: ": keepalive\r\n" +
				"event: message\r\n" +
				"id: 1\r\n" +
				"retry: 1000\r\n\r\n" +
				"data: {\"id\":\"chatcmpl-sse-1\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"first thought \"}}]}\r\n\r\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"second thought\",\"content\":\"Hello \"}}]}\r\n\r\n" +
				// Multi-line data event joined by \n
				"data: {\"choices\":[{\"index\":0,\n" +
				"data: \"delta\":{\"content\":\"world!\"},\"finish_reason\":\"stop\"}],\n" +
				"data: \"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7,\"total_tokens\":19}}\r\n\r\n" +
				"data: [DONE]\r\n\r\n",
		},
		{
			name:        "mislabelled application/json starting with data:",
			contentType: "application/json",
			sseBody: "data: {\"id\":\"chatcmpl-sse-1\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"first thought second thought\",\"content\":\"Hello \"}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world!\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7,\"total_tokens\":19}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:        "mislabelled application/json starting with SSE comment and final message chunk",
			contentType: "application/json",
			sseBody: ": ping\n\n" +
				"data: {\"id\":\"chatcmpl-sse-1\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"first thought second thought\"}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"Hello world!\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7,\"total_tokens\":19}}\n\n" +
				"data: [DONE]\n\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.sseBody)
			}))
			defer srv.Close()

			client := NewClient(srv.Client())
			resp, err := client.ChatCompletions(context.Background(), config.ResolvedEndpoint{
				BaseURL: srv.URL + "/v1",
			}, ChatRequest{
				Model:    "gemini-2.5-pro",
				Messages: []Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("ChatCompletions SSE failed: %v", err)
			}
			if resp.ID != "chatcmpl-sse-1" {
				t.Fatalf("resp.ID = %q, want chatcmpl-sse-1", resp.ID)
			}
			if resp.Model != "gemini-2.5-pro" {
				t.Fatalf("resp.Model = %q, want gemini-2.5-pro", resp.Model)
			}
			if resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 7 || resp.Usage.TotalTokens != 19 {
				t.Fatalf("resp.Usage = %+v, want {12 7 19}", resp.Usage)
			}
			if len(resp.Choices) != 1 {
				t.Fatalf("len(resp.Choices) = %d, want 1", len(resp.Choices))
			}
			ch := resp.Choices[0]
			if ch.Message.Role != "assistant" {
				t.Fatalf("Message.Role = %q, want assistant", ch.Message.Role)
			}
			if ch.Message.Content != "Hello world!" {
				t.Fatalf("Message.Content = %q, want %q", ch.Message.Content, "Hello world!")
			}
			if ch.Message.ReasoningContent != "first thought second thought" {
				t.Fatalf("Message.ReasoningContent = %q, want %q", ch.Message.ReasoningContent, "first thought second thought")
			}
			if ch.FinishReason != "stop" {
				t.Fatalf("FinishReason = %q, want stop", ch.FinishReason)
			}
		})
	}
}

func TestChatCompletionsSSEToolCallsReassemblyAndRunnerEndToEnd(t *testing.T) {
	var turn atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := turn.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// Interleaved tool_calls index 0 and index 1 across multiple chunks; index 1 omits "type".
			stream := strings.Join([]string{
				`data: {"id":"sse-tc-1","model":"cc/claude-opus-4-6","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"need two files","tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"read_","arguments":"{\"path\":"}}]}}]}`,
				``,
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"READ"}}]}}]}`,
				``,
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"file","arguments":"\"src/app.go\"}"}}]}}]}`,
				``,
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"ME.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":15,"completion_tokens":10,"total_tokens":25}}`,
				``,
				`data: [DONE]`,
				``,
			}, "\n")
			_, _ = io.WriteString(w, stream)
			return
		}

		// Turn 2: verify runner sent the reassembled tool call results back and return final SSE text.
		bodyBytes, _ := io.ReadAll(r.Body)
		bodyStr := string(bodyBytes)
		if !strings.Contains(bodyStr, "func Value() int") || !strings.Contains(bodyStr, "# Demo") {
			t.Fatalf("turn 2 did not receive executed tool outputs from reassembled SSE tool_calls: %s", bodyStr)
		}
		stream := strings.Join([]string{
			`data: {"id":"sse-tc-2","model":"cc/claude-opus-4-6","choices":[{"index":0,"delta":{"role":"assistant","content":"Read both "}}]}`,
			``,
			`data: {"choices":[{"index":0,"delta":{"content":"files cleanly."},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5,"total_tokens":35}}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n")
		_, _ = io.WriteString(w, stream)
	}))
	defer srv.Close()

	repo := initTestGitRepo(t)
	profiles := config.DefaultProfilesConfig()
	profiles.ActiveProfile = "9router-hybrid"
	if err := config.SaveProfiles(repo, profiles); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	runner := NewRunner(RunnerConfig{
		RepoRoot: repo,
		Client:   NewClient(srv.Client()),
		LookupEnv: func(k string) (string, bool) {
			if k == "NINEROUTER_BASE_URL" {
				return srv.URL + "/v1", true
			}
			return "", false
		},
	})

	rec, err := runner.Run(context.Background(), RunRequest{
		Role: "verify",
		Task: "Read src/app.go and README.md",
	})
	if err != nil {
		t.Fatalf("Runner.Run failed: %v", err)
	}
	if rec.Status != TaskStatusCompleted {
		t.Fatalf("status = %q, want completed (err=%s)", rec.Status, rec.Error)
	}
	if rec.FinalResponse != "Read both files cleanly." {
		t.Fatalf("FinalResponse = %q, want %q", rec.FinalResponse, "Read both files cleanly.")
	}
	if rec.Usage.TotalTokens != 60 {
		t.Fatalf("Usage.TotalTokens = %d, want 60", rec.Usage.TotalTokens)
	}

	// Check the assistant message from turn 1 has both reassembled tool calls with type defaulted to "function".
	var foundToolMsg bool
	for _, m := range rec.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 2 {
			foundToolMsg = true
			if m.ToolCalls[0].ID != "call_0" || m.ToolCalls[0].Type != "function" ||
				m.ToolCalls[0].Function.Name != "read_file" || m.ToolCalls[0].Function.Arguments != `{"path":"src/app.go"}` {
				t.Fatalf("reassembled ToolCalls[0] = %+v", m.ToolCalls[0])
			}
			if m.ToolCalls[1].ID != "call_1" || m.ToolCalls[1].Type != "function" ||
				m.ToolCalls[1].Function.Name != "read_file" || m.ToolCalls[1].Function.Arguments != `{"path":"README.md"}` {
				t.Fatalf("reassembled ToolCalls[1] = %+v", m.ToolCalls[1])
			}
		}
	}
	if !foundToolMsg {
		t.Fatalf("expected assistant message with 2 reassembled tool calls in rec.Messages: %+v", rec.Messages)
	}
}

func TestChatCompletionsSSEErrorAndEmptyChoicesRedactAPIKey(t *testing.T) {
	const secretKey = "sk-super-secret-leak-check"

	t.Run("SSE error chunk returns BadGateway APIError with redacted apiKey", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"upstream rejected key "+secretKey+" at https://user:pass@proxy.local/v1?token=abc\",\"type\":\"upstream_error\"}}\n\ndata: [DONE]\n\n")
		}))
		defer srv.Close()

		client := NewClient(srv.Client())
		_, err := client.ChatCompletions(context.Background(), config.ResolvedEndpoint{
			BaseURL: strings.Replace(srv.URL, "http://", "http://admin:pw@", 1) + "/v1?key=" + secretKey,
			APIKey:  secretKey,
		}, ChatRequest{Model: "m1", Messages: []Message{{Role: "user", Content: "hi"}}})
		if err == nil {
			t.Fatal("expected error from SSE error chunk")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected *APIError, got %T: %v", err, err)
		}
		if apiErr.StatusCode != http.StatusBadGateway {
			t.Fatalf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusBadGateway)
		}
		if strings.Contains(err.Error(), secretKey) || strings.Contains(err.Error(), "pass@") || strings.Contains(err.Error(), "token=abc") {
			t.Fatalf("SSE error leaked secret: %v", err)
		}
		if !strings.Contains(err.Error(), "[REDACTED]") {
			t.Fatalf("expected redacted marker in error: %v", err)
		}
	})

	t.Run("SSE with no choices returns empty choices returned by redactedURL", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		client := NewClient(srv.Client())
		_, err := client.ChatCompletions(context.Background(), config.ResolvedEndpoint{
			BaseURL: strings.Replace(srv.URL, "http://", "http://admin:pw@", 1) + "/v1?key=" + secretKey,
			APIKey:  secretKey,
		}, ChatRequest{Model: "m1", Messages: []Message{{Role: "user", Content: "hi"}}})
		if err == nil {
			t.Fatal("expected error for empty SSE choices")
		}
		if !strings.HasPrefix(err.Error(), "empty choices returned by ") {
			t.Fatalf("err = %q, want prefix 'empty choices returned by '", err.Error())
		}
		if strings.Contains(err.Error(), secretKey) || strings.Contains(err.Error(), "admin:pw") {
			t.Fatalf("empty choices error leaked URL credentials or key: %v", err)
		}
	})

	t.Run("PRESERVE: standard JSON empty choices and invalid JSON preserve error contract without leaking apiKey", func(t *testing.T) {
		srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"j1","model":"m1","choices":[]}`)
		}))
		defer srvEmpty.Close()

		client := NewClient(srvEmpty.Client())
		_, err := client.ChatCompletions(context.Background(), config.ResolvedEndpoint{
			BaseURL: strings.Replace(srvEmpty.URL, "http://", "http://admin:pw@", 1) + "/v1?key=" + secretKey,
			APIKey:  secretKey,
		}, ChatRequest{Model: "m1", Messages: []Message{{Role: "user", Content: "hi"}}})
		if err == nil || !strings.HasPrefix(err.Error(), "empty choices returned by ") {
			t.Fatalf("empty JSON choices err = %v, want 'empty choices returned by ...'", err)
		}
		if strings.Contains(err.Error(), secretKey) || strings.Contains(err.Error(), "admin:pw") {
			t.Fatalf("empty JSON choices leaked secret: %v", err)
		}

		srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{not-json}`)
		}))
		defer srvBad.Close()

		_, err = client.ChatCompletions(context.Background(), config.ResolvedEndpoint{
			BaseURL: strings.Replace(srvBad.URL, "http://", "http://admin:pw@", 1) + "/v1?key=" + secretKey,
			APIKey:  secretKey,
		}, ChatRequest{Model: "m1", Messages: []Message{{Role: "user", Content: "hi"}}})
		if err == nil || !strings.HasPrefix(err.Error(), "decode response from ") {
			t.Fatalf("invalid JSON err = %v, want 'decode response from ...'", err)
		}
		if strings.Contains(err.Error(), secretKey) || strings.Contains(err.Error(), "admin:pw") {
			t.Fatalf("invalid JSON error leaked secret: %v", err)
		}
	})
}

