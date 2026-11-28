package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/rdd"
	"github.com/Nathanael-Huaman/odd-agy/internal/todo"
)

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
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
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")

	writeFile(t, filepath.Join(dir, "README.md"), "# Test Repo\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial commit")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func dispatchJSON(t *testing.T, event string, payload any, out any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	var stdout bytes.Buffer
	if err := Dispatch(event, bytes.NewReader(raw), &stdout); err != nil {
		t.Fatalf("Dispatch(%s) error: %v", event, err)
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		t.Fatalf("unmarshal %s stdout %q: %v", event, stdout.String(), err)
	}
}

func TestSessionStart_SummaryInjection(t *testing.T) {
	repo := initTestRepo(t)

	cfg := config.DefaultConfig()
	cfg.DelegationMode = config.DelegationModeFull
	cfg.RDDEnabled = true
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	profs := config.DefaultProfilesConfig()
	profs.ActiveProfile = "deepseek-hybrid"
	if err := config.SaveProfiles(repo, profs); err != nil {
		t.Fatalf("SaveProfiles: %v", err)
	}

	writeFile(t, filepath.Join(repo, "odd", "tasks", "feat.md"), "# Feature\n- [ ] **T1** work\n")
	writeFile(t, filepath.Join(repo, "odd", "tasks", "done.md"), "# Done\n- [x] **T1** finished\n")

	store := todo.NewStore(repo)
	if _, err := store.Add("First open task", todo.StatusInProgress, ""); err != nil {
		t.Fatalf("todo.Add: %v", err)
	}
	if _, err := store.Add("Second open task", todo.StatusPending, ""); err != nil {
		t.Fatalf("todo.Add: %v", err)
	}
	if _, err := store.Add("Completed task", todo.StatusDone, ""); err != nil {
		t.Fatalf("todo.Add: %v", err)
	}

	var out InvocationOutput
	dispatchJSON(t, "SessionStart", InvocationInput{
		CommonInput: CommonInput{
			ConversationID: "conv-session-1",
			WorkspacePaths: []string{repo},
		},
		InvocationNum:   1,
		InitialNumSteps: 0,
	}, &out)

	if len(out.InjectSteps) != 1 || out.InjectSteps[0].SystemMessage == nil {
		t.Fatalf("expected 1 injected systemMessage, got %+v", out)
	}
	msg := out.InjectSteps[0].SystemMessage.SystemMessage
	for _, want := range []string{"full-delegation", "true", "deepseek-hybrid", "odd/tasks/feat.md", "2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("SessionStart summary %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "odd/tasks/done.md") {
		t.Errorf("SessionStart summary %q should not list closed task file odd/tasks/done.md", msg)
	}
}

func TestPreToolUse_SensitivePaths(t *testing.T) {
	repo := initTestRepo(t)

	// Create .npmrc with auth token and a clean .env.example.
	writeFile(t, filepath.Join(repo, ".npmrc"), "//registry.npmjs.org/:_authToken=secret\n")
	writeFile(t, filepath.Join(repo, ".env.example"), "API_KEY=\n")

	deniedCases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"env_read", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, ".env")}},
		{"env_local_write", "write_to_file", map[string]any{"TargetFile": filepath.Join(repo, ".env.local"), "CodeContent": "A=1"}},
		{"ssh_id_rsa", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, ".ssh", "id_rsa")}},
		{"ssh_home", "view_file", map[string]any{"AbsolutePath": "~/.ssh/id_ed25519"}},
		{"aws_creds", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, ".aws", "credentials")}},
		{"pem_file", "write_to_file", map[string]any{"TargetFile": filepath.Join(repo, "certs", "server.pem"), "CodeContent": "x"}},
		{"key_file", "replace_file_content", map[string]any{"TargetFile": filepath.Join(repo, "server.key")}},
		{"npmrc_with_auth", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, ".npmrc")}},
		{"escape_parent", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, "..", "escape.txt")}},
		{"escape_relative", "view_file", map[string]any{"path": "../escape.txt"}},
		{"cmd_cat_env", "run_command", map[string]any{"CommandLine": "cat .env"}},
		{"cmd_cat_ssh", "run_command", map[string]any{"CommandLine": "cat ~/.ssh/id_rsa"}},
		{"cmd_escape", "run_command", map[string]any{"CommandLine": "cat ../escape.txt"}},
	}

	// Switch to organic so Rule B doesn't mask Rule A on write tools.
	cfg := config.DefaultConfig()
	cfg.DelegationMode = config.DelegationModeOrganic
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	for _, tc := range deniedCases {
		t.Run("deny_"+tc.name, func(t *testing.T) {
			var out PreToolUseOutput
			dispatchJSON(t, "PreToolUse", PreToolUseInput{
				CommonInput: CommonInput{
					ConversationID: "conv-sens",
					WorkspacePaths: []string{repo},
				},
				ToolCall: ToolCall{Name: tc.tool, Args: tc.args},
			}, &out)
			if out.Decision != "deny" {
				t.Fatalf("expected deny for %s, got %+v", tc.name, out)
			}
		})
	}

	tmpDir := t.TempDir()
	allowedCases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"env_example", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, ".env.example")}},
		{"env_sample", "write_to_file", map[string]any{"TargetFile": filepath.Join(repo, ".env.sample"), "CodeContent": "X="}},
		{"env_template", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, ".env.template")}},
		{"regular_file", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, "README.md")}},
		{"temp_dir_file", "write_to_file", map[string]any{"TargetFile": filepath.Join(tmpDir, "scratch.txt"), "CodeContent": "ok"}},
		{"go_test_ellipsis", "run_command", map[string]any{"CommandLine": "go test -v ./internal/hooks/..."}},
	}

	for _, tc := range allowedCases {
		t.Run("allow_"+tc.name, func(t *testing.T) {
			var out PreToolUseOutput
			dispatchJSON(t, "PreToolUse", PreToolUseInput{
				CommonInput: CommonInput{
					ConversationID: "conv-sens",
					WorkspacePaths: []string{repo},
				},
				ToolCall: ToolCall{Name: tc.tool, Args: tc.args},
			}, &out)
			if out.Decision != "allow" {
				t.Fatalf("expected allow for %s, got %+v", tc.name, out)
			}
		})
	}
}

func TestPreToolUse_FullDelegationWriteGuard(t *testing.T) {
	repo := initTestRepo(t)
	cfg := config.DefaultConfig()
	cfg.DelegationMode = config.DelegationModeFull
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// Allows odd/tasks/*.md.
	var out PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c1", WorkspacePaths: []string{repo}},
		ToolCall: ToolCall{
			Name: "write_to_file",
			Args: map[string]any{"TargetFile": filepath.Join(repo, "odd", "tasks", "feat.md"), "CodeContent": "# Feat"},
		},
	}, &out)
	if out.Decision != "allow" {
		t.Fatalf("expected allow for odd/tasks/feat.md in full-delegation, got %+v", out)
	}

	// Denies src/app.go across write/replace tools.
	for _, tool := range []string{"write_to_file", "replace_file_content", "multi_replace_file_content"} {
		var denyOut PreToolUseOutput
		dispatchJSON(t, "PreToolUse", PreToolUseInput{
			CommonInput: CommonInput{ConversationID: "c1", WorkspacePaths: []string{repo}},
			ToolCall: ToolCall{
				Name: tool,
				Args: map[string]any{"TargetFile": filepath.Join(repo, "src", "app.go")},
			},
		}, &denyOut)
		if denyOut.Decision != "deny" {
			t.Fatalf("expected deny for %s on src/app.go in full-delegation, got %+v", tool, denyOut)
		}
		if !strings.Contains(denyOut.Reason, "gentle-ai-worker") || !strings.Contains(denyOut.Reason, "Allowed edit surfaces") {
			t.Errorf("deny reason %q should mention gentle-ai-worker and Allowed edit surfaces", denyOut.Reason)
		}
	}

	// Denies shell redirection mutations targeting workspace files outside odd/tasks/.
	for _, cmd := range []string{
		"echo 'package main' > src/app.go",
		"cat foo >> src/app.go",
		"echo hi | tee src/app.go",
		"sed -i 's/a/b/' src/app.go",
		"perl -i -pe 's/a/b/' src/app.go",
	} {
		var cmdOut PreToolUseOutput
		dispatchJSON(t, "PreToolUse", PreToolUseInput{
			CommonInput: CommonInput{ConversationID: "c1", WorkspacePaths: []string{repo}},
			ToolCall:    ToolCall{Name: "run_command", Args: map[string]any{"CommandLine": cmd}},
		}, &cmdOut)
		if cmdOut.Decision != "deny" {
			t.Fatalf("expected deny for shell mutation %q in full-delegation, got %+v", cmd, cmdOut)
		}
	}

	// Allows ordinary commands, odd/tasks/*.md edits, and temp-dir redirections.
	for _, cmd := range []string{
		"go test ./...",
		"pnpm test",
		"git status --short",
		"echo ok > /tmp/odd-agy-probe.txt",
		"echo ok > /dev/null 2>&1",
		"sed -i '' 's/a/b/' odd/tasks/feat.md",
	} {
		var cmdOut PreToolUseOutput
		dispatchJSON(t, "PreToolUse", PreToolUseInput{
			CommonInput: CommonInput{ConversationID: "c1", WorkspacePaths: []string{repo}},
			ToolCall:    ToolCall{Name: "run_command", Args: map[string]any{"CommandLine": cmd}},
		}, &cmdOut)
		if cmdOut.Decision != "allow" {
			t.Fatalf("expected allow for %q in full-delegation, got %+v", cmd, cmdOut)
		}
	}

	// Switch to organic -> src/app.go allowed.
	cfg.DelegationMode = config.DelegationModeOrganic
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	var organicOut PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c1", WorkspacePaths: []string{repo}},
		ToolCall: ToolCall{
			Name: "write_to_file",
			Args: map[string]any{"TargetFile": filepath.Join(repo, "src", "app.go")},
		},
	}, &organicOut)
	if organicOut.Decision != "allow" {
		t.Fatalf("expected allow for src/app.go in organic mode, got %+v", organicOut)
	}
}

func TestPreToolUse_InvokeSubagentAdmission(t *testing.T) {
	repo := initTestRepo(t)
	writeFile(t, filepath.Join(repo, "odd", "tasks", "feat.md"), "# Feature\n- [ ] **T1** task\n")

	cfg := config.DefaultConfig()
	cfg.DelegationMode = config.DelegationModeFull
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// 1. Missing Allowed edit surfaces -> deny.
	var out PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-adm", WorkspacePaths: []string{repo}},
		StepIdx:     1,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Prompt": "Implement T1"},
				},
			},
		},
	}, &out)
	if out.Decision != "deny" {
		t.Fatalf("expected deny when Allowed edit surfaces missing, got %+v", out)
	}

	// 2. Broad/invalid surfaces rejected.
	for _, badSurface := range []string{".", "./", "/", "..", "*", "**", "/abs/path.go", "../escape.go"} {
		var badOut PreToolUseOutput
		dispatchJSON(t, "PreToolUse", PreToolUseInput{
			CommonInput: CommonInput{ConversationID: "c-adm", WorkspacePaths: []string{repo}},
			StepIdx:     2,
			ToolCall: ToolCall{
				Name: "invoke_subagent",
				Args: map[string]any{
					"Subagents": []map[string]any{
						{
							"Role":   "gentle-ai-worker",
							"Prompt": "## Allowed edit surfaces\n- " + badSurface + "\n",
						},
					},
				},
			},
		}, &badOut)
		if badOut.Decision != "deny" {
			t.Fatalf("expected deny for broad/invalid surface %q, got %+v", badSurface, badOut)
		}
	}

	// 3. Batch overlap without Workspace: "branch" -> deny.
	var batchDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-adm", WorkspacePaths: []string{repo}},
		StepIdx:     3,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Prompt": "## Allowed edit surfaces\n- internal/hooks/**\n"},
					{"Role": "jd-fix-agent", "Prompt": "## Allowed edit surfaces\n- internal/hooks/pretool.go\n"},
				},
			},
		},
	}, &batchDeny)
	if batchDeny.Decision != "deny" {
		t.Fatalf("expected deny for overlapping batch surfaces, got %+v", batchDeny)
	}

	// 4. Batch overlap WITH Workspace: "branch" -> allow.
	var batchBranchAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-adm", WorkspacePaths: []string{repo}},
		StepIdx:     4,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Workspace": "branch", "Prompt": "## Allowed edit surfaces\n- internal/hooks/**\n"},
					{"Role": "jd-fix-agent", "Workspace": "branch", "Prompt": "## Allowed edit surfaces\n- internal/hooks/pretool.go\n"},
				},
			},
		},
	}, &batchBranchAllow)
	if batchBranchAllow.Decision != "allow" {
		t.Fatalf("expected allow when Workspace=branch, got %+v", batchBranchAllow)
	}

	// 5. Active writer surface lock across invocations -> deny, then branch -> allow, then PostToolUse releases -> allow.
	var active1Allow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-lock", WorkspacePaths: []string{repo}},
		StepIdx:     10,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Prompt": "## Allowed edit surfaces\n- internal/hooks/**\n"},
				},
			},
		},
	}, &active1Allow)
	if active1Allow.Decision != "allow" {
		t.Fatalf("expected allow for first non-branch writer, got %+v", active1Allow)
	}

	var active2Deny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-lock", WorkspacePaths: []string{repo}},
		StepIdx:     11,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Prompt": "## Allowed edit surfaces\n- internal/hooks/pretool.go\n"},
				},
			},
		},
	}, &active2Deny)
	if active2Deny.Decision != "deny" {
		t.Fatalf("expected deny for overlapping active writer surface, got %+v", active2Deny)
	}

	var active2BranchAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-lock", WorkspacePaths: []string{repo}},
		StepIdx:     11,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Workspace": "branch", "Prompt": "## Allowed edit surfaces\n- internal/hooks/pretool.go\n"},
				},
			},
		},
	}, &active2BranchAllow)
	if active2BranchAllow.Decision != "allow" {
		t.Fatalf("expected allow when overlapping writer uses Workspace=branch, got %+v", active2BranchAllow)
	}

	// PostToolUse releases c-lock#10.
	var postOut map[string]any
	dispatchJSON(t, "PostToolUse", PostToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-lock", WorkspacePaths: []string{repo}},
		StepIdx:     10,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Prompt": "## Allowed edit surfaces\n- internal/hooks/**\n"},
				},
			},
		},
	}, &postOut)

	var afterReleaseAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-lock", WorkspacePaths: []string{repo}},
		StepIdx:     12,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Prompt": "## Allowed edit surfaces\n- internal/hooks/pretool.go\n"},
				},
			},
		},
	}, &afterReleaseAllow)
	if afterReleaseAllow.Decision != "allow" {
		t.Fatalf("expected allow after PostToolUse released surface lock, got %+v", afterReleaseAllow)
	}

	// Release c-lock#12 cleanly.
	dispatchJSON(t, "PostToolUse", PostToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-lock", WorkspacePaths: []string{repo}},
		StepIdx:     12,
		ToolCall:    ToolCall{Name: "invoke_subagent"},
	}, &postOut)

	// 6. gentle-ai-verify rules.
	var verifyDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-ver", WorkspacePaths: []string{repo}},
		StepIdx:     1,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-verify", "Prompt": "Verify everything"},
				},
			},
		},
	}, &verifyDeny)
	if verifyDeny.Decision != "deny" {
		t.Fatalf("expected deny for gentle-ai-verify without Risk: or Spec:, got %+v", verifyDeny)
	}

	var verifyAllowRisk PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-ver", WorkspacePaths: []string{repo}},
		StepIdx:     2,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-verify", "Prompt": "Risk: item 2\nVerify auth path"},
				},
			},
		},
	}, &verifyAllowRisk)
	if verifyAllowRisk.Decision != "allow" {
		t.Fatalf("expected allow for gentle-ai-verify with Risk: item 2, got %+v", verifyAllowRisk)
	}

	var verifyAllowSpec PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-ver", WorkspacePaths: []string{repo}},
		StepIdx:     3,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-verify", "Prompt": "Spec: odd/tasks/feat.md, T1\nVerify S1"},
				},
			},
		},
	}, &verifyAllowSpec)
	if verifyAllowSpec.Decision != "allow" {
		t.Fatalf("expected allow for gentle-ai-verify with existing Spec:, got %+v", verifyAllowSpec)
	}

	// 7. Organic mode single writer vs 2+ disjoint writers.
	cfg.DelegationMode = config.DelegationModeOrganic
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	var organicSingleDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-org", WorkspacePaths: []string{repo}},
		StepIdx:     1,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Prompt": "## Allowed edit surfaces\n- pkg/a.go\n"},
				},
			},
		},
	}, &organicSingleDeny)
	if organicSingleDeny.Decision != "deny" {
		t.Fatalf("expected deny for single generic writer in organic mode without Spec:, got %+v", organicSingleDeny)
	}

	var organicSingleAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-org", WorkspacePaths: []string{repo}},
		StepIdx:     2,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Prompt": "Spec: odd/tasks/feat.md, T1\n## Allowed edit surfaces\n- pkg/a.go\n"},
				},
			},
		},
	}, &organicSingleAllow)
	if organicSingleAllow.Decision != "allow" {
		t.Fatalf("expected allow for single writer with Spec: in organic mode, got %+v", organicSingleAllow)
	}
	dispatchJSON(t, "PostToolUse", PostToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-org", WorkspacePaths: []string{repo}},
		StepIdx:     2,
		ToolCall:    ToolCall{Name: "invoke_subagent"},
	}, &postOut)

	var organicParallelAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-org", WorkspacePaths: []string{repo}},
		StepIdx:     3,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-worker", "Prompt": "## Allowed edit surfaces\n- pkg/a.go\n"},
					{"Role": "gentle-ai-worker", "Prompt": "## Allowed edit surfaces\n- pkg/b.go\n"},
				},
			},
		},
	}, &organicParallelAllow)
	if organicParallelAllow.Decision != "allow" {
		t.Fatalf("expected allow for 2 disjoint writers in organic mode without Spec:, got %+v", organicParallelAllow)
	}
}

func TestPreToolUse_RunCommandGuardsAndRDDGate(t *testing.T) {
	repo := initTestRepo(t)
	cfg := config.DefaultConfig()
	cfg.RDDEnabled = true
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// 1. Destructive commands denied.
	for _, cmd := range []string{
		"rm -rf /",
		"rm -rf ~",
		"git reset --hard",
		"git push --force",
		"git push -f origin main",
		"git clean -fd",
		"git clean -fdx",
	} {
		var out PreToolUseOutput
		dispatchJSON(t, "PreToolUse", PreToolUseInput{
			CommonInput: CommonInput{ConversationID: "c-cmd", WorkspacePaths: []string{repo}},
			ToolCall:    ToolCall{Name: "run_command", Args: map[string]any{"CommandLine": cmd}},
		}, &out)
		if out.Decision != "deny" {
			t.Fatalf("expected deny for destructive command %q, got %+v", cmd, out)
		}
	}

	// 2. Conflict markers before git push / gh pr create denied.
	writeFile(t, filepath.Join(repo, "app.go"), "package main\n<<<<<<< HEAD\nvar x = 1\n=======\nvar x = 2\n>>>>>>> feat\n")
	runGit(t, repo, "add", "app.go")
	for _, deliveryCmd := range []string{"git push origin feat", "gh pr create --title test"} {
		var out PreToolUseOutput
		dispatchJSON(t, "PreToolUse", PreToolUseInput{
			CommonInput: CommonInput{ConversationID: "c-cmd", WorkspacePaths: []string{repo}},
			ToolCall:    ToolCall{Name: "run_command", Args: map[string]any{"CommandLine": deliveryCmd}},
		}, &out)
		if out.Decision != "deny" {
			t.Fatalf("expected deny for conflict markers on %q, got %+v", deliveryCmd, out)
		}
	}

	// 3. Clean conflict markers, introduce unreviewed high-risk change -> git push denied by RDD gate.
	writeFile(t, filepath.Join(repo, "app.go"), "package main\nvar x = 2\n")
	writeFile(t, filepath.Join(repo, "security", "auth.go"), "package security\nfunc Check() bool { return true }\n")
	runGit(t, repo, "add", "app.go", "security/auth.go")

	var rddDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-cmd", WorkspacePaths: []string{repo}},
		ToolCall:    ToolCall{Name: "run_command", Args: map[string]any{"CommandLine": "git push origin feat"}},
	}, &rddDeny)
	if rddDeny.Decision != "deny" {
		t.Fatalf("expected deny for unreviewed high-risk RDD candidate before git push, got %+v", rddDeny)
	}
	if !strings.Contains(rddDeny.Reason, "odd-agy rdd disable") {
		t.Errorf("RDD deny reason %q should mention 'odd-agy rdd disable'", rddDeny.Reason)
	}

	// 4. Allowed when RDD is disabled.
	cfg.RDDEnabled = false
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	var rddDisabledAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-cmd", WorkspacePaths: []string{repo}},
		ToolCall:    ToolCall{Name: "run_command", Args: map[string]any{"CommandLine": "git push origin feat"}},
	}, &rddDisabledAllow)
	if rddDisabledAllow.Decision != "allow" {
		t.Fatalf("expected allow when RDD disabled, got %+v", rddDisabledAllow)
	}

	// 5. Re-enable RDD, approve & ack candidate -> allowed.
	cfg.RDDEnabled = true
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	eng := rdd.NewEngine(repo)
	started, err := eng.Start(rdd.StartRequest{
		Lenses:       []string{rdd.LensRisk},
		LensesReason: "security auth change",
	})
	if err != nil {
		t.Fatalf("rdd.Start: %v", err)
	}
	if _, err := eng.AnswerConsent(started.LineageID, "granted"); err != nil {
		t.Fatalf("rdd.AnswerConsent: %v", err)
	}
	if _, err := eng.Submit(rdd.SubmitRequest{
		LineageID: started.LineageID,
		Lens:      rdd.LensRisk,
		Findings:  nil,
	}); err != nil {
		t.Fatalf("rdd.Submit: %v", err)
	}
	if _, err := eng.Ack(started.LineageID); err != nil {
		t.Fatalf("rdd.Ack: %v", err)
	}

	var rddApprovedAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-cmd", WorkspacePaths: []string{repo}},
		ToolCall:    ToolCall{Name: "run_command", Args: map[string]any{"CommandLine": "git push origin feat"}},
	}, &rddApprovedAllow)
	if rddApprovedAllow.Decision != "allow" {
		t.Fatalf("expected allow after RDD approval & ack, got %+v", rddApprovedAllow)
	}
}

func TestPostToolUse_And_PreInvocation_TodoReminderDedup(t *testing.T) {
	repo := initTestRepo(t)
	store := todo.NewStore(repo)
	if _, err := store.Add("Implement hooks", todo.StatusInProgress, ""); err != nil {
		t.Fatalf("todo.Add: %v", err)
	}

	// 3 PostToolUse calls -> <4 turns -> no reminder.
	for i := 1; i <= 3; i++ {
		var post map[string]any
		dispatchJSON(t, "PostToolUse", PostToolUseInput{
			CommonInput: CommonInput{ConversationID: "c-todo", WorkspacePaths: []string{repo}},
			StepIdx:     i,
			ToolCall:    ToolCall{Name: "view_file"},
		}, &post)
	}

	var preBefore InvocationOutput
	dispatchJSON(t, "PreInvocation", InvocationInput{
		CommonInput:   CommonInput{ConversationID: "c-todo", WorkspacePaths: []string{repo}},
		InvocationNum: 1,
	}, &preBefore)
	if len(preBefore.InjectSteps) != 0 {
		t.Fatalf("expected no reminder before 4 turns, got %+v", preBefore)
	}

	// 4th PostToolUse (write_to_file with error should NOT mark DirtyMutations, but still counts turn).
	var post4 map[string]any
	dispatchJSON(t, "PostToolUse", PostToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-todo", WorkspacePaths: []string{repo}},
		StepIdx:     4,
		ToolCall:    ToolCall{Name: "write_to_file"},
		Error:       "permission denied",
	}, &post4)

	st, err := loadHookState(repo)
	if err != nil {
		t.Fatalf("loadHookState: %v", err)
	}
	if st.DirtyMutations {
		t.Errorf("errored write_to_file should not mark DirtyMutations=true")
	}

	// Now PreInvocation should inject reminder once and reset turns.
	var preInjected InvocationOutput
	dispatchJSON(t, "PreInvocation", InvocationInput{
		CommonInput:   CommonInput{ConversationID: "c-todo", WorkspacePaths: []string{repo}},
		InvocationNum: 2,
	}, &preInjected)
	if len(preInjected.InjectSteps) != 1 || preInjected.InjectSteps[0].SystemMessage == nil {
		t.Fatalf("expected 1 todo reminder on 4th turn, got %+v", preInjected)
	}
	if !strings.Contains(strings.ToLower(preInjected.InjectSteps[0].SystemMessage.SystemMessage), "todo") {
		t.Errorf("reminder %q should mention todo", preInjected.InjectSteps[0].SystemMessage.SystemMessage)
	}

	todoState, err := store.List()
	if err != nil {
		t.Fatalf("todo.List: %v", err)
	}
	if todoState.ToolTurnsSinceUpdate != 0 {
		t.Errorf("expected ToolTurnsSinceUpdate reset to 0, got %d", todoState.ToolTurnsSinceUpdate)
	}

	// Consecutive turn AND even 4 more turns with identical todo state must NOT duplicate reminder.
	for i := 5; i <= 9; i++ {
		var post map[string]any
		dispatchJSON(t, "PostToolUse", PostToolUseInput{
			CommonInput: CommonInput{ConversationID: "c-todo", WorkspacePaths: []string{repo}},
			StepIdx:     i,
			ToolCall:    ToolCall{Name: "view_file"},
		}, &post)
	}
	var preDup InvocationOutput
	dispatchJSON(t, "PreInvocation", InvocationInput{
		CommonInput:   CommonInput{ConversationID: "c-todo", WorkspacePaths: []string{repo}},
		InvocationNum: 3,
	}, &preDup)
	if len(preDup.InjectSteps) != 0 {
		t.Fatalf("expected no duplicate reminder for unchanged todo list, got %+v", preDup)
	}

	// Successful write_to_file marks DirtyMutations=true.
	var postMut map[string]any
	dispatchJSON(t, "PostToolUse", PostToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-todo", WorkspacePaths: []string{repo}},
		StepIdx:     10,
		ToolCall:    ToolCall{Name: "write_to_file"},
	}, &postMut)
	st, err = loadHookState(repo)
	if err != nil {
		t.Fatalf("loadHookState: %v", err)
	}
	if !st.DirtyMutations {
		t.Errorf("successful write_to_file should mark DirtyMutations=true")
	}
}

func TestStop_RDDPreflightNoInfiniteLoop(t *testing.T) {
	repo := initTestRepo(t)
	cfg := config.DefaultConfig()
	cfg.RDDEnabled = true
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// Create a high-risk mutation and mark DirtyMutations via PostToolUse.
	writeFile(t, filepath.Join(repo, "security", "policy.go"), "package security\nfunc Allow() bool { return false }\n")
	var post map[string]any
	dispatchJSON(t, "PostToolUse", PostToolUseInput{
		CommonInput: CommonInput{ConversationID: "c-stop", WorkspacePaths: []string{repo}},
		StepIdx:     1,
		ToolCall:    ToolCall{Name: "write_to_file"},
	}, &post)

	// When fullyIdle == false -> allow.
	var notIdle StopOutput
	dispatchJSON(t, "Stop", StopInput{
		CommonInput: CommonInput{ConversationID: "c-stop", WorkspacePaths: []string{repo}},
		FullyIdle:   false,
	}, &notIdle)
	if notIdle.Decision != "allow" {
		t.Fatalf("expected allow when fullyIdle=false, got %+v", notIdle)
	}

	// 1st fullyIdle Stop -> continue once with reason.
	var stop1 StopOutput
	dispatchJSON(t, "Stop", StopInput{
		CommonInput: CommonInput{ConversationID: "c-stop", WorkspacePaths: []string{repo}},
		FullyIdle:   true,
	}, &stop1)
	if stop1.Decision != "continue" {
		t.Fatalf("expected continue on first Stop with unreviewed high-risk mutations, got %+v", stop1)
	}
	if !strings.Contains(stop1.Reason, "odd_review_assess") {
		t.Errorf("Stop continue reason %q should mention odd_review_assess", stop1.Reason)
	}

	// 2nd fullyIdle Stop on same mutation state -> allow (no infinite loop).
	var stop2 StopOutput
	dispatchJSON(t, "Stop", StopInput{
		CommonInput: CommonInput{ConversationID: "c-stop", WorkspacePaths: []string{repo}},
		FullyIdle:   true,
	}, &stop2)
	if stop2.Decision != "allow" {
		t.Fatalf("expected allow on second Stop call (no infinite loop), got %+v", stop2)
	}
}

func TestPreToolUseDeniesInWorkspaceSymlinkToSensitiveFile(t *testing.T) {
	repo := initTestRepo(t)
	cfg := config.DefaultConfig()
	cfg.DelegationMode = config.DelegationModeOrganic
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	writeFile(t, filepath.Join(repo, "secret.pem"), "PRIVATE KEY")
	writeFile(t, filepath.Join(repo, ".env"), "SECRET=1")
	writeFile(t, filepath.Join(repo, ".ssh", "id_rsa"), "SSH KEY")

	if err := os.Symlink(filepath.Join(repo, "secret.pem"), filepath.Join(repo, "innocent.txt")); err != nil {
		t.Fatalf("symlink innocent.txt: %v", err)
	}
	if err := os.Symlink(".env", filepath.Join(repo, "public.txt")); err != nil {
		t.Fatalf("symlink public.txt: %v", err)
	}
	if err := os.Symlink(filepath.Join(".ssh", "id_rsa"), filepath.Join(repo, "keylink")); err != nil {
		t.Fatalf("symlink keylink: %v", err)
	}
	if err := os.Symlink("README.md", filepath.Join(repo, "readme_link.md")); err != nil {
		t.Fatalf("symlink readme_link.md: %v", err)
	}

	deniedCases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"view_symlink_pem", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, "innocent.txt")}},
		{"view_symlink_env", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, "public.txt")}},
		{"view_symlink_ssh", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, "keylink")}},
		{"cmd_cat_symlink_pem", "run_command", map[string]any{"CommandLine": "cat innocent.txt"}},
		{"cmd_cat_rel_symlink_pem", "run_command", map[string]any{"CommandLine": "cat ./innocent.txt"}},
		{"cmd_cat_symlink_env", "run_command", map[string]any{"CommandLine": "cat public.txt"}},
		{"cmd_cat_symlink_ssh", "run_command", map[string]any{"CommandLine": "cat keylink"}},
	}

	for _, tc := range deniedCases {
		t.Run("deny_"+tc.name, func(t *testing.T) {
			var out PreToolUseOutput
			dispatchJSON(t, "PreToolUse", PreToolUseInput{
				CommonInput: CommonInput{
					ConversationID: "conv-sym",
					WorkspacePaths: []string{repo},
				},
				ToolCall: ToolCall{Name: tc.tool, Args: tc.args},
			}, &out)
			if out.Decision != DecisionDeny {
				t.Fatalf("expected deny for %s, got %+v", tc.name, out)
			}
		})
	}

	// Preserve: benign in-workspace symlink remains allowed.
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"view_benign_symlink", "view_file", map[string]any{"AbsolutePath": filepath.Join(repo, "readme_link.md")}},
		{"cmd_cat_benign_symlink", "run_command", map[string]any{"CommandLine": "cat readme_link.md"}},
	} {
		t.Run("allow_"+tc.name, func(t *testing.T) {
			var out PreToolUseOutput
			dispatchJSON(t, "PreToolUse", PreToolUseInput{
				CommonInput: CommonInput{
					ConversationID: "conv-sym",
					WorkspacePaths: []string{repo},
				},
				ToolCall: ToolCall{Name: tc.tool, Args: tc.args},
			}, &out)
			if out.Decision != DecisionAllow {
				t.Fatalf("expected allow for %s, got %+v", tc.name, out)
			}
		})
	}
}

func TestPreToolUseRDDCorrectionSecurityFixes(t *testing.T) {
	repo, outsideDir := initTestRepo(t), t.TempDir()
	writeFile(t, filepath.Join(repo, ".env"), "SECRET=1\n")
	writeFile(t, filepath.Join(repo, "sub", ".npmrc"), "//registry.npmjs.org/:_authToken=secret\n")
	writeFile(t, filepath.Join(repo, "security", "auth.go"), "package security\nfunc OK() bool { return true }\n")
	runGit(t, repo, "add", "security/auth.go")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink("../.env", filepath.Join(repo, "sub", "link_env")))
	must(os.Symlink(outsideDir, filepath.Join(repo, "escape-dir")))
	for _, tc := range []struct{ name, mode, cmd, cwd, want string }{
		{"r1_1_cwd_aws", config.DelegationModeOrganic, "ls", filepath.Join(repo, ".aws"), DecisionDeny},
		{"r1_1_cwd_ssh", config.DelegationModeOrganic, "ls", ".ssh", DecisionDeny},
		{"r1_1_cwd_sub_symlink", config.DelegationModeOrganic, "cat link_env", "sub", DecisionDeny},
		{"r1_1_cwd_sub_npmrc", config.DelegationModeOrganic, "cat .npmrc", filepath.Join(repo, "sub"), DecisionDeny},
		{"r1_1_cwd_sub_readme_allow", config.DelegationModeOrganic, "cat ../README.md", "sub", DecisionAllow},
		{"r1_1_cwd_full_task_allow", config.DelegationModeFull, "echo hi > feat.md", filepath.Join(repo, "odd", "tasks"), DecisionAllow},
		{"r1_2_attached_in_redir", config.DelegationModeOrganic, "cat<.env", "", DecisionDeny},
		{"r1_2_attached_out_redir", config.DelegationModeFull, "echo hi>src/app.go", "", DecisionDeny},
		{"r1_2_flag_equals_env", config.DelegationModeOrganic, "cmd --file=.env", "", DecisionDeny},
		{"r1_2_short_flag_env", config.DelegationModeOrganic, "cmd -f.env", "", DecisionDeny},
		{"r1_2_subshell_env", config.DelegationModeOrganic, "(cat .env)", "", DecisionDeny},
		{"r1_3_env_git_reset", config.DelegationModeOrganic, "GIT_TERMINAL_PROMPT=0 git reset --hard", "", DecisionDeny},
		{"r1_3_env_gh_pr", config.DelegationModeOrganic, "GH_TOKEN=x gh pr create --title t", "", DecisionDeny},
		{"r1_3_env_sed_inplace", config.DelegationModeFull, "LC_ALL=C sed -i 's/a/b/' src/app.go", "", DecisionDeny},
		{"r1_3_wrapper_env_push", config.DelegationModeOrganic, "env git push origin main", "", DecisionDeny},
		{"r1_4_newfile_symlink_dir", config.DelegationModeOrganic, "touch escape-dir/newfile.txt", "", DecisionDeny},
	} {
		cfg := config.DefaultConfig()
		cfg.DelegationMode, cfg.RDDEnabled = tc.mode, true
		must(config.SaveConfig(repo, cfg))
		var out PreToolUseOutput
		dispatchJSON(t, "PreToolUse", PreToolUseInput{CommonInput: CommonInput{ConversationID: "conv-rdd-sec", WorkspacePaths: []string{repo}}, ToolCall: ToolCall{Name: "run_command", Args: map[string]any{"CommandLine": tc.cmd, "Cwd": tc.cwd}}}, &out)
		if out.Decision != tc.want {
			t.Errorf("%s: got %q (%s), want %q", tc.name, out.Decision, out.Reason, tc.want)
		}
	}
}

func TestPreToolUseRunCommandCwdAwareSymlinkAndBounds(t *testing.T) {
	repo := initTestRepo(t)
	cfg := config.DefaultConfig()
	cfg.DelegationMode = config.DelegationModeOrganic
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	writeFile(t, outsideFile, "OUTSIDE SECRET")
	writeFile(t, filepath.Join(repo, "secret.pem"), "PRIVATE KEY")
	writeFile(t, filepath.Join(repo, "sub", "ok.txt"), "safe")
	writeFile(t, filepath.Join(repo, "sub", ".npmrc"), "//registry.npmjs.org/:_authToken=secret\n")

	// Symlink inside sub/ pointing outside workspace and pointing to ../secret.pem.
	if err := os.Symlink(outsideFile, filepath.Join(repo, "sub", "sub_escape_link.txt")); err != nil {
		t.Fatalf("symlink sub_escape_link.txt: %v", err)
	}
	if err := os.Symlink("../secret.pem", filepath.Join(repo, "sub", "sub_secret_link.txt")); err != nil {
		t.Fatalf("symlink sub_secret_link.txt: %v", err)
	}
	if err := os.Symlink(filepath.Join(outsideDir, "missing_outside.txt"), filepath.Join(repo, "sub", "sub_dangling_escape.txt")); err != nil {
		t.Fatalf("symlink sub_dangling_escape.txt: %v", err)
	}
	if err := os.Symlink("ok.txt", filepath.Join(repo, "sub", "sub_ok_link.txt")); err != nil {
		t.Fatalf("symlink sub_ok_link.txt: %v", err)
	}

	subDir := filepath.Join(repo, "sub")

	deniedCases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "cwd_sub_escape_symlink",
			args: map[string]any{"Cwd": subDir, "CommandLine": "cat sub_escape_link.txt"},
		},
		{
			name: "cwd_sub_secret_symlink",
			args: map[string]any{"Cwd": subDir, "CommandLine": "cat sub_secret_link.txt"},
		},
		{
			name: "cwd_sub_dangling_escape_symlink",
			args: map[string]any{"Cwd": subDir, "CommandLine": "cat sub_dangling_escape.txt"},
		},
		{
			name: "cwd_sub_npmrc_auth",
			args: map[string]any{"Cwd": subDir, "CommandLine": "cat .npmrc"},
		},
		{
			name: "cwd_outside_workspace",
			args: map[string]any{"Cwd": outsideDir, "CommandLine": "cat outside.txt"},
		},
		{
			name: "cwd_temp_root_outside_workspace",
			args: map[string]any{"Cwd": os.TempDir(), "CommandLine": "ls"},
		},
		{
			name: "cwd_relative_escape",
			args: map[string]any{"Cwd": "../outside", "CommandLine": "ls"},
		},
		{
			name: "cwd_sub_relative_double_dot_escape",
			args: map[string]any{"Cwd": subDir, "CommandLine": "cat ../../outside.txt"},
		},
	}

	for _, tc := range deniedCases {
		t.Run("deny_"+tc.name, func(t *testing.T) {
			var out PreToolUseOutput
			dispatchJSON(t, "PreToolUse", PreToolUseInput{
				CommonInput: CommonInput{
					ConversationID: "conv-cwd-sym",
					WorkspacePaths: []string{repo},
				},
				ToolCall: ToolCall{Name: "run_command", Args: tc.args},
			}, &out)
			if out.Decision != DecisionDeny {
				t.Fatalf("expected deny for %s, got %+v", tc.name, out)
			}
		})
	}

	// Preserve: benign symlink and in-workspace parent ref from Cwd=sub remain allowed.
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{
			name: "cwd_sub_benign_symlink",
			args: map[string]any{"Cwd": subDir, "CommandLine": "cat sub_ok_link.txt"},
		},
		{
			name: "cwd_sub_in_workspace_parent",
			args: map[string]any{"Cwd": subDir, "CommandLine": "cat ../README.md"},
		},
	} {
		t.Run("allow_"+tc.name, func(t *testing.T) {
			var out PreToolUseOutput
			dispatchJSON(t, "PreToolUse", PreToolUseInput{
				CommonInput: CommonInput{
					ConversationID: "conv-cwd-sym",
					WorkspacePaths: []string{repo},
				},
				ToolCall: ToolCall{Name: "run_command", Args: tc.args},
			}, &out)
			if out.Decision != DecisionAllow {
				t.Fatalf("expected allow for %s, got %+v", tc.name, out)
			}
		})
	}

	// Preserve & verify full-delegation Cwd-aware mutation checks.
	cfg.DelegationMode = config.DelegationModeFull
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig full-delegation: %v", err)
	}
	var allowOddTask PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-cwd-full", WorkspacePaths: []string{repo}},
		ToolCall: ToolCall{
			Name: "run_command",
			Args: map[string]any{"Cwd": filepath.Join(repo, "odd", "tasks"), "CommandLine": "echo '# feat' > feat.md"},
		},
	}, &allowOddTask)
	if allowOddTask.Decision != DecisionAllow {
		t.Fatalf("expected allow for odd/tasks/feat.md with Cwd=odd/tasks, got %+v", allowOddTask)
	}

	var denySubMutation PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-cwd-full", WorkspacePaths: []string{repo}},
		ToolCall: ToolCall{
			Name: "run_command",
			Args: map[string]any{"Cwd": subDir, "CommandLine": "echo x > ok.txt"},
		},
	}, &denySubMutation)
	if denySubMutation.Decision != DecisionDeny {
		t.Fatalf("expected deny for shell mutation in subDir under full-delegation, got %+v", denySubMutation)
	}
}

func TestPreToolUseDeniesDanglingSymlinkInsideSymlinkedExternalDir(t *testing.T) {
	repo := initTestRepo(t)
	cfg := config.DefaultConfig()
	cfg.DelegationMode = config.DelegationModeOrganic
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	extParent := t.TempDir()
	extDir := filepath.Join(extParent, "ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink("../outside_new.txt", filepath.Join(extDir, "escape")); err != nil {
		t.Fatalf("symlink escape: %v", err)
	}
	if err := os.Symlink(extDir, filepath.Join(repo, "extdir")); err != nil {
		t.Fatalf("symlink extdir: %v", err)
	}

	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"write_to_file", "write_to_file", map[string]any{"TargetFile": filepath.Join(repo, "extdir", "escape"), "CodeContent": "hi"}},
		{"run_command", "run_command", map[string]any{"CommandLine": "echo hi > extdir/escape"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out PreToolUseOutput
			dispatchJSON(t, "PreToolUse", PreToolUseInput{
				CommonInput: CommonInput{ConversationID: "conv-dangling", WorkspacePaths: []string{repo}},
				ToolCall:    ToolCall{Name: tc.tool, Args: tc.args},
			}, &out)
			if out.Decision != DecisionDeny {
				t.Fatalf("expected deny for %s, got %+v", tc.name, out)
			}
		})
	}
}

func TestPreToolUse_MCPAutoApproveAndConsentForceAsk(t *testing.T) {
	repo := initTestRepo(t)

	autoApproveCases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"slash_todo", "odd-agy/todo", map[string]any{"action": "list"}},
		{"mcp_prefix_assess", "mcp_odd-agy_odd_review_assess", map[string]any{}},
		{"call_mcp_tool_scope", "call_mcp_tool", map[string]any{"ServerName": "odd-agy", "ToolName": "odd_review_scope"}},
		{"slash_status", "odd-agy/odd_review_status", nil},
		{"mcp_prefix_ext_profiles", "mcp_odd-agy_odd_external_profiles", nil},
		{"call_mcp_tool_ext_status", "call_mcp_tool", map[string]any{"ServerName": "odd-agy", "ToolName": "odd_external_status"}},
		{"slash_mode_omitted", "odd-agy/odd_review_mode", map[string]any{}},
		{"slash_mode_status", "odd-agy/odd_review_mode", map[string]any{"action": "status"}},
		{"call_mcp_tool_mode_status_in_arguments", "call_mcp_tool", map[string]any{
			"ServerName": "odd-agy",
			"ToolName":   "odd_review_mode",
			"Arguments":  map[string]any{"action": "status"},
		}},
		{"call_mcp_tool_mode_status_json_arguments", "call_mcp_tool", map[string]any{
			"ServerName": "odd-agy",
			"ToolName":   "odd_review_mode",
			"Arguments":  `{"action":"status"}`,
		}},
		{"call_mcp_tool_mode_omitted", "call_mcp_tool", map[string]any{
			"ServerName": "odd-agy",
			"ToolName":   "odd_review_mode",
		}},
		{"pascal_todo", "McpOdd-agyTodo", map[string]any{"action": "list"}},
		{"pascal_assess", "McpOdd-agyOddReviewAssess", map[string]any{}},
		{"pascal_scope", "McpOdd-agyOddReviewScope", map[string]any{}},
		{"pascal_status", "McpOdd-agyOddReviewStatus", nil},
		{"pascal_ext_profiles", "McpOdd-agyOddExternalProfiles", nil},
		{"pascal_ext_status", "McpOdd-agyOddExternalStatus", nil},
		{"pascal_mode_omitted", "McpOdd-agyOddReviewMode", map[string]any{}},
		{"pascal_mode_status", "McpOdd-agyOddReviewMode", map[string]any{"action": "status"}},
		{"call_mcp_tool_snake_case_scope", "call_mcp_tool", map[string]any{"server_name": "odd-agy", "tool_name": "odd_review_scope"}},
		{"call_mcp_tool_snake_case_mode_status", "call_mcp_tool", map[string]any{
			"server_name": "odd-agy",
			"tool_name":   "odd_review_mode",
			"arguments":   map[string]any{"action": "status"},
		}},
	}

	for _, tc := range autoApproveCases {
		t.Run("auto_approve_"+tc.name, func(t *testing.T) {
			var out PreToolUseOutput
			dispatchJSON(t, "PreToolUse", PreToolUseInput{
				CommonInput: CommonInput{ConversationID: "conv-mcp", WorkspacePaths: []string{repo}},
				ToolCall:    ToolCall{Name: tc.tool, Args: tc.args},
			}, &out)
			if out.Decision != DecisionAutoApprove {
				t.Fatalf("expected %q for %s, got %+v", DecisionAutoApprove, tc.name, out)
			}
		})
	}

	forceAskCases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"slash_consent", "odd-agy/odd_review_consent", map[string]any{"consent": "granted"}},
		{"mcp_prefix_consent", "mcp_odd-agy_odd_review_consent", map[string]any{"consent": "granted"}},
		{"call_mcp_tool_consent", "call_mcp_tool", map[string]any{"ServerName": "odd-agy", "ToolName": "odd_review_consent"}},
		{"pascal_consent", "McpOdd-agyOddReviewConsent", map[string]any{"consent": "granted"}},
	}

	for _, tc := range forceAskCases {
		t.Run("force_ask_"+tc.name, func(t *testing.T) {
			var out PreToolUseOutput
			dispatchJSON(t, "PreToolUse", PreToolUseInput{
				CommonInput: CommonInput{ConversationID: "conv-mcp", WorkspacePaths: []string{repo}},
				ToolCall:    ToolCall{Name: tc.tool, Args: tc.args},
			}, &out)
			if out.Decision != DecisionForceAsk {
				t.Fatalf("expected %q for %s, got %+v", DecisionForceAsk, tc.name, out)
			}
			if out.Reason != "RDD review consent requires explicit human approval (granted or declined)." {
				t.Fatalf("unexpected force_ask reason %q", out.Reason)
			}
		})
	}

	allowCases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"mode_disable_slash", "odd-agy/odd_review_mode", map[string]any{"action": "disable"}},
		{"mode_enable_mcp_prefix", "mcp_odd-agy_odd_review_mode", map[string]any{"action": "enable"}},
		{"mode_disable_call_mcp_tool_arguments", "call_mcp_tool", map[string]any{
			"ServerName": "odd-agy",
			"ToolName":   "odd_review_mode",
			"Arguments":  map[string]any{"action": "disable"},
		}},
		{"mode_disable_call_mcp_tool_json_arguments", "call_mcp_tool", map[string]any{
			"ServerName": "odd-agy",
			"ToolName":   "odd_review_mode",
			"Arguments":  `{"action":"disable"}`,
		}},
		{"odd_review_start", "odd-agy/odd_review_start", map[string]any{}},
		{"odd_review_submit", "mcp_odd-agy_odd_review_submit", map[string]any{}},
		{"odd_review_ack", "odd-agy/odd_review_ack", map[string]any{}},
		{"odd_external_run", "call_mcp_tool", map[string]any{"ServerName": "odd-agy", "ToolName": "odd_external_run"}},
		{"engram_slash", "engram/mem_save", map[string]any{"title": "test"}},
		{"engram_mcp_prefix", "mcp_engram_mem_save", map[string]any{"title": "test"}},
		{"engram_call_mcp_tool", "call_mcp_tool", map[string]any{"ServerName": "engram", "ToolName": "mem_save"}},
		{"pascal_mode_disable", "McpOdd-agyOddReviewMode", map[string]any{"action": "disable"}},
		{"pascal_mode_nonstring_int", "McpOdd-agyOddReviewMode", map[string]any{"action": 123}},
		{"pascal_mode_nonstring_bool", "McpOdd-agyOddReviewMode", map[string]any{"action": true}},
		{"pascal_mode_nonstring_slice", "McpOdd-agyOddReviewMode", map[string]any{"action": []any{"status"}}},
		{"slash_mode_nonstring_int", "odd-agy/odd_review_mode", map[string]any{"action": 123}},
		{"pascal_spoofed_evil_todo", "McpOdd-agyEvilTodo", map[string]any{}},
		{"pascal_engram_todo", "McpEngramTodo", map[string]any{}},
		{"pascal_lowercase_after_prefix", "McpOdd-agytodo", map[string]any{}},
		{"pascal_odd_review_start", "McpOdd-agyOddReviewStart", map[string]any{}},
		{"call_mcp_tool_mode_malformed_json_arguments", "call_mcp_tool", map[string]any{
			"ServerName": "odd-agy",
			"ToolName":   "odd_review_mode",
			"Arguments":  "{not-json",
		}},
		{"call_mcp_tool_mode_non_object_arguments_int", "call_mcp_tool", map[string]any{
			"ServerName": "odd-agy",
			"ToolName":   "odd_review_mode",
			"Arguments":  123,
		}},
		{"call_mcp_tool_mode_non_object_arguments_array", "call_mcp_tool", map[string]any{
			"ServerName": "odd-agy",
			"ToolName":   "odd_review_mode",
			"Arguments":  []any{"status"},
		}},
		{"call_mcp_tool_mode_non_object_arguments_json_null", "call_mcp_tool", map[string]any{
			"ServerName": "odd-agy",
			"ToolName":   "odd_review_mode",
			"Arguments":  "null",
		}},
		{"call_mcp_tool_mode_nonstring_action_in_arguments", "call_mcp_tool", map[string]any{
			"ServerName": "odd-agy",
			"ToolName":   "odd_review_mode",
			"Arguments":  `{"action": 123}`,
		}},
	}

	for _, tc := range allowCases {
		t.Run("allow_"+tc.name, func(t *testing.T) {
			var out PreToolUseOutput
			dispatchJSON(t, "PreToolUse", PreToolUseInput{
				CommonInput: CommonInput{ConversationID: "conv-mcp", WorkspacePaths: []string{repo}},
				ToolCall:    ToolCall{Name: tc.tool, Args: tc.args},
			}, &out)
			if out.Decision != DecisionAllow {
				t.Fatalf("expected %q for %s, got %+v", DecisionAllow, tc.name, out)
			}
		})
	}
}

func TestDispatch_Errors(t *testing.T) {
	var out bytes.Buffer
	if err := Dispatch("UnknownEvent", strings.NewReader("{}"), &out); err == nil {
		t.Fatal("expected error for unknown hook event")
	}
	if err := Dispatch("PreToolUse", strings.NewReader("{invalid"), &out); err == nil {
		t.Fatal("expected error for invalid JSON input")
	}
}

func TestPreToolUseInvokeSubagentEnforcesExternalMCPRouting(t *testing.T) {
	repo := initTestRepo(t)
	cfg := config.DefaultConfig()
	cfg.DelegationMode = config.DelegationModeFull
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// 1. Active profile 9router-hybrid with worker = external-mcp DENIES invoke_subagent(gentle-ai-worker)
	// and does NOT acquire a surface lock.
	profs := config.DefaultProfilesConfig()
	profs.ActiveProfile = "9router-hybrid"
	nineProf := profs.Profiles["9router-hybrid"]
	nineProf.Roles["worker"] = config.RoleRoute{
		Runtime:  config.RuntimeExternalMCP,
		Provider: "9router",
		Model:    "kr/claude-sonnet-4.5",
		Fallback: []config.RoleTarget{{Runtime: config.RuntimeAgyNative, Model: "pro"}},
	}
	profs.Profiles["9router-hybrid"] = nineProf
	if err := config.SaveProfiles(repo, profs); err != nil {
		t.Fatalf("SaveProfiles 9router-hybrid: %v", err)
	}

	var out9Router PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-ext", WorkspacePaths: []string{repo}},
		StepIdx:     1,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{
						"Role":   "gentle-ai-worker",
						"Prompt": "## Allowed edit surfaces\n- pkg/a.go\n",
					},
				},
			},
		},
	}, &out9Router)
	if out9Router.Decision != DecisionDeny {
		t.Fatalf("9router-hybrid worker=external-mcp: expected deny, got %+v", out9Router)
	}
	for _, want := range []string{"9router-hybrid", "worker", "9router/kr/claude-sonnet-4.5", "odd-agy/odd_external_run", "fallback_to_native"} {
		if !strings.Contains(out9Router.Reason, want) {
			t.Errorf("9router-hybrid deny reason %q missing %q", out9Router.Reason, want)
		}
	}
	st, err := loadHookState(repo)
	if err != nil {
		t.Fatalf("loadHookState: %v", err)
	}
	if len(st.ActiveWriterSurfaces) != 0 {
		t.Fatalf("expected no surface lock acquired on external-mcp deny, got %+v", st.ActiveWriterSurfaces)
	}

	// 2. Active profile deepseek-hybrid with worker = external-mcp DENIES invoke_subagent(gentle-ai-worker)
	// and does NOT acquire a surface lock.
	profs.ActiveProfile = "deepseek-hybrid"
	dsProf := profs.Profiles["deepseek-hybrid"]
	dsProf.Roles["worker"] = config.RoleRoute{
		Runtime:  config.RuntimeExternalMCP,
		Provider: "deepseek",
		Model:    "deepseek-v4-pro",
		Fallback: []config.RoleTarget{{Runtime: config.RuntimeAgyNative, Model: "pro"}},
	}
	profs.Profiles["deepseek-hybrid"] = dsProf
	if err := config.SaveProfiles(repo, profs); err != nil {
		t.Fatalf("SaveProfiles deepseek-hybrid: %v", err)
	}

	var outDeepSeek PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-ext", WorkspacePaths: []string{repo}},
		StepIdx:     2,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{
						"Role":   "gentle-ai-worker",
						"Prompt": "## Allowed edit surfaces\n- pkg/a.go\n",
					},
				},
			},
		},
	}, &outDeepSeek)
	if outDeepSeek.Decision != DecisionDeny {
		t.Fatalf("deepseek-hybrid worker=external-mcp: expected deny, got %+v", outDeepSeek)
	}
	for _, want := range []string{"deepseek-hybrid", "worker", "deepseek/deepseek-v4-pro", "odd-agy/odd_external_run"} {
		if !strings.Contains(outDeepSeek.Reason, want) {
			t.Errorf("deepseek-hybrid deny reason %q missing %q", outDeepSeek.Reason, want)
		}
	}
	st, err = loadHookState(repo)
	if err != nil {
		t.Fatalf("loadHookState: %v", err)
	}
	if len(st.ActiveWriterSurfaces) != 0 {
		t.Fatalf("expected no surface lock acquired on deepseek-hybrid deny, got %+v", st.ActiveWriterSurfaces)
	}

	// 3. Same call WITH fallback_to_native but WITHOUT Allowed edit surfaces -> still DENIED by surface guard.
	var outFallbackNoSurface PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-ext", WorkspacePaths: []string{repo}},
		StepIdx:     3,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{
						"Role":   "gentle-ai-worker",
						"Prompt": "Runtime fallback_to_native from odd_external_run.\nImplement change.",
					},
				},
			},
		},
	}, &outFallbackNoSurface)
	if outFallbackNoSurface.Decision != DecisionDeny {
		t.Fatalf("expected deny when fallback_to_native omits Allowed edit surfaces, got %+v", outFallbackNoSurface)
	}
	if !strings.Contains(outFallbackNoSurface.Reason, "Allowed edit surfaces") {
		t.Errorf("expected Allowed edit surfaces error, got %q", outFallbackNoSurface.Reason)
	}

	// 4. Same call WITH fallback_to_native AND Allowed edit surfaces -> ALLOWED.
	var outFallbackAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-ext", WorkspacePaths: []string{repo}},
		StepIdx:     4,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{
						"Role":   "gentle-ai-worker",
						"Prompt": "Fallback reason: FALLBACK_TO_NATIVE after odd_external_run returned fallback_to_native.\n## Allowed edit surfaces\n- pkg/a.go\n",
					},
				},
			},
		},
	}, &outFallbackAllow)
	if outFallbackAllow.Decision != DecisionAllow {
		t.Fatalf("expected allow with fallback_to_native + Allowed edit surfaces, got %+v", outFallbackAllow)
	}
	var postOut map[string]any
	dispatchJSON(t, "PostToolUse", PostToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-ext", WorkspacePaths: []string{repo}},
		StepIdx:     4,
		ToolCall:    ToolCall{Name: "invoke_subagent"},
	}, &postOut)

	// 5. Active profile agy-oauth (worker = agy-native) ALLOWS invoke_subagent(gentle-ai-worker) normally.
	profs.ActiveProfile = "agy-oauth"
	if err := config.SaveProfiles(repo, profs); err != nil {
		t.Fatalf("SaveProfiles agy-oauth: %v", err)
	}
	var outAgyAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-ext", WorkspacePaths: []string{repo}},
		StepIdx:     5,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{
						"Role":   "gentle-ai-worker",
						"Prompt": "## Allowed edit surfaces\n- pkg/b.go\n",
					},
				},
			},
		},
	}, &outAgyAllow)
	if outAgyAllow.Decision != DecisionAllow {
		t.Fatalf("expected allow under agy-oauth, got %+v", outAgyAllow)
	}
	dispatchJSON(t, "PostToolUse", PostToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-ext", WorkspacePaths: []string{repo}},
		StepIdx:     5,
		ToolCall:    ToolCall{Name: "invoke_subagent"},
	}, &postOut)

	// 6. Active profile with explore = external-mcp DENIES invoke_subagent(gentle-ai-explore).
	profs.ActiveProfile = "9router-hybrid"
	nineProf = profs.Profiles["9router-hybrid"]
	nineProf.Roles["explore"] = config.RoleRoute{
		Runtime:  config.RuntimeExternalMCP,
		Provider: "9router",
		Model:    "kr/claude-sonnet-4.5",
		Fallback: []config.RoleTarget{{Runtime: config.RuntimeAgyNative, Model: "flash"}},
	}
	profs.Profiles["9router-hybrid"] = nineProf
	if err := config.SaveProfiles(repo, profs); err != nil {
		t.Fatalf("SaveProfiles explore=external-mcp: %v", err)
	}

	var outExploreDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-ext", WorkspacePaths: []string{repo}},
		StepIdx:     6,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{
						"TypeName": "GENTLE-AI-EXPLORE",
						"Prompt":   "Map repo structure",
					},
				},
			},
		},
	}, &outExploreDeny)
	if outExploreDeny.Decision != DecisionDeny {
		t.Fatalf("expected deny for gentle-ai-explore when explore=external-mcp, got %+v", outExploreDeny)
	}
	for _, want := range []string{"9router-hybrid", "explore", "odd-agy/odd_external_run"} {
		if !strings.Contains(outExploreDeny.Reason, want) {
			t.Errorf("explore deny reason %q missing %q", outExploreDeny.Reason, want)
		}
	}

	// Same explore call WITH fallback_to_native -> ALLOWED.
	var outExploreFallback PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-ext", WorkspacePaths: []string{repo}},
		StepIdx:     7,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{
						"TypeName": "gentle-ai-explore",
						"Prompt":   "fallback_to_native from odd_external_run. Map repo structure.",
					},
				},
			},
		},
	}, &outExploreFallback)
	if outExploreFallback.Decision != DecisionAllow {
		t.Fatalf("expected allow for gentle-ai-explore with fallback_to_native, got %+v", outExploreFallback)
	}
}

func TestPreToolUseInvokeSubagentEnforcesParallelNativeReviewBatch(t *testing.T) {
	repo := initTestRepo(t)
	cfg := config.DefaultConfig()
	cfg.DelegationMode = config.DelegationModeFull
	cfg.RDDEnabled = true
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// Create candidate diff and start 3-lens RDD lineage in collecting_lenses.
	writeFile(t, filepath.Join(repo, "app.go"), "package main\nfunc Add(a, b int) int { return a + b }\n")
	eng := rdd.NewEngine(repo)
	started, err := eng.Start(rdd.StartRequest{
		Lenses:       []string{rdd.LensRisk, rdd.LensReliability, rdd.LensReadability},
		LensesReason: "multi-lens review",
	})
	if err != nil {
		t.Fatalf("rdd.Start: %v", err)
	}

	// While in consent_required, guard does not block (or wait until collecting_lenses).
	if _, err := eng.AnswerConsent(started.LineageID, "granted"); err != nil {
		t.Fatalf("rdd.AnswerConsent: %v", err)
	}

	// Default agy-oauth (even without profiles.json on disk) treats all 3 pending lenses as native.
	// 1. Single-lens invoke_subagent([review-risk]) -> DENY naming lineageId, count, and all 3 pending lenses.
	var singleDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     1,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review lineage " + started.LineageID},
				},
			},
		},
	}, &singleDeny)
	if singleDeny.Decision != DecisionDeny {
		t.Fatalf("single-lens review-risk: expected deny, got %+v", singleDeny)
	}
	for _, want := range []string{started.LineageID, "3", "review-risk", "review-reliability", "review-readability"} {
		if !strings.Contains(singleDeny.Reason, want) {
			t.Errorf("single-lens deny reason %q missing %q", singleDeny.Reason, want)
		}
	}

	// 2. Single-lens without lineageId in prompt -> still resolves active collecting_lenses lineage and DENIES.
	var inferredDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     2,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review the frozen snapshot."},
				},
			},
		},
	}, &inferredDeny)
	if inferredDeny.Decision != DecisionDeny {
		t.Fatalf("inferred lineage single-lens review-risk: expected deny, got %+v", inferredDeny)
	}
	for _, want := range []string{started.LineageID, "review-risk", "review-reliability", "review-readability"} {
		if !strings.Contains(inferredDeny.Reason, want) {
			t.Errorf("inferred deny reason %q missing %q", inferredDeny.Reason, want)
		}
	}

	// 3. Partial 2-of-3 batch invoke_subagent([review-risk, review-reliability]) -> DENY naming missing review-readability.
	var partialDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     3,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review " + started.LineageID},
					{"Role": "review-reliability", "Prompt": "Review " + started.LineageID},
				},
			},
		},
	}, &partialDeny)
	if partialDeny.Decision != DecisionDeny {
		t.Fatalf("partial 2-of-3 batch: expected deny, got %+v", partialDeny)
	}
	if !strings.Contains(partialDeny.Reason, "review-readability") {
		t.Errorf("partial 2-of-3 deny reason %q missing missing lens review-readability", partialDeny.Reason)
	}

	// 4. Duplicate role in batch [review-risk, review-risk, review-reliability, review-readability] -> DENY.
	var dupDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     4,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review " + started.LineageID},
					{"Role": "review-risk", "Prompt": "Duplicate " + started.LineageID},
					{"Role": "review-reliability", "Prompt": "Review " + started.LineageID},
					{"Role": "review-readability", "Prompt": "Review " + started.LineageID},
				},
			},
		},
	}, &dupDeny)
	if dupDeny.Decision != DecisionDeny {
		t.Fatalf("duplicate role in batch: expected deny, got %+v", dupDeny)
	}

	// 5. Complete 3-of-3 batch invoke_subagent([review-risk, review-reliability, review-readability]) -> ALLOW.
	var completeAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     5,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review " + started.LineageID},
					{"Role": "review-reliability", "Prompt": "Review " + started.LineageID},
					{"Role": "review-readability", "Prompt": "Review " + started.LineageID},
				},
			},
		},
	}, &completeAllow)
	if completeAllow.Decision != DecisionAllow {
		t.Fatalf("complete 3-of-3 batch: expected allow, got %+v", completeAllow)
	}

	// 6. Single-lens WITH fallback_to_native in prompt -> ALLOW even with 3 pending native lenses.
	var fallbackAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     6,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-reliability", "Prompt": "fallback_to_native after external error on " + started.LineageID},
				},
			},
		},
	}, &fallbackAllow)
	if fallbackAllow.Decision != DecisionAllow {
		t.Fatalf("single-lens with fallback_to_native: expected allow, got %+v", fallbackAllow)
	}

	// 7. review-refuter alone -> ALLOW.
	var refuterAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     7,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-refuter", "Prompt": "Refute findings for " + started.LineageID},
				},
			},
		},
	}, &refuterAllow)
	if refuterAllow.Decision != DecisionAllow {
		t.Fatalf("review-refuter alone: expected allow, got %+v", refuterAllow)
	}

	// 8. Mixed profile: review-readability=external-mcp, review-risk+review-reliability=agy-native.
	profs := config.DefaultProfilesConfig()
	profs.ActiveProfile = "agy-oauth"
	mixed := profs.Profiles["agy-oauth"]
	mixed.Roles["review-readability"] = config.RoleRoute{
		Runtime:  config.RuntimeExternalMCP,
		Provider: "deepseek",
		Model:    "deepseek-v4-pro",
	}
	profs.Profiles["agy-oauth"] = mixed
	if err := config.SaveProfiles(repo, profs); err != nil {
		t.Fatalf("SaveProfiles mixed: %v", err)
	}

	var mixedSingleDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     8,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review " + started.LineageID},
				},
			},
		},
	}, &mixedSingleDeny)
	if mixedSingleDeny.Decision != DecisionDeny {
		t.Fatalf("mixed profile single native lens: expected deny, got %+v", mixedSingleDeny)
	}
	if !strings.Contains(mixedSingleDeny.Reason, "2") || !strings.Contains(mixedSingleDeny.Reason, "review-reliability") {
		t.Errorf("mixed profile deny reason %q should mention 2 pending native lenses and review-reliability", mixedSingleDeny.Reason)
	}

	var mixedPairAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     9,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review " + started.LineageID},
					{"Role": "review-reliability", "Prompt": "Review " + started.LineageID},
				},
			},
		},
	}, &mixedPairAllow)
	if mixedPairAllow.Decision != DecisionAllow {
		t.Fatalf("mixed profile 2 native lenses together: expected allow, got %+v", mixedPairAllow)
	}

	// Restore native profile and submit 2 lenses so only 1 remains (review-readability) -> single-lens ALLOW.
	if err := config.SaveProfiles(repo, config.DefaultProfilesConfig()); err != nil {
		t.Fatalf("SaveProfiles restore: %v", err)
	}
	if _, err := eng.Submit(rdd.SubmitRequest{LineageID: started.LineageID, Lens: rdd.LensRisk}); err != nil {
		t.Fatalf("Submit risk: %v", err)
	}
	if _, err := eng.Submit(rdd.SubmitRequest{LineageID: started.LineageID, Lens: rdd.LensReliability}); err != nil {
		t.Fatalf("Submit reliability: %v", err)
	}

	var lastLensAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     10,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-readability", "Prompt": "Review " + started.LineageID},
				},
			},
		},
	}, &lastLensAllow)
	if lastLensAllow.Decision != DecisionAllow {
		t.Fatalf("single remaining lens review-readability: expected allow, got %+v", lastLensAllow)
	}

	// PRESERVE: non-review role (gentle-ai-explore) is unaffected even when multiple native lenses are pending.
	started2, err := eng.Start(rdd.StartRequest{
		Lenses:       []string{rdd.LensRisk, rdd.LensResilience, rdd.LensReliability},
		LensesReason: "second lineage",
	})
	if err != nil {
		t.Fatalf("rdd.Start 2: %v", err)
	}
	// While started2 is in consent_required (not collecting_lenses), explicit started2 ID is not blocked by batch guard.
	var preConsentExplore PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-rev", WorkspacePaths: []string{repo}},
		StepIdx:     11,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "gentle-ai-explore", "Prompt": "Explore repo"},
				},
			},
		},
	}, &preConsentExplore)
	if preConsentExplore.Decision != DecisionAllow {
		t.Fatalf("gentle-ai-explore: expected allow, got %+v", preConsentExplore)
	}
	_ = started2
}

func TestPreToolUseNativeReviewBatchPartialFallbackAndMixedTypeNameDuplicates(t *testing.T) {
	repo := initTestRepo(t)
	cfg := config.DefaultConfig()
	cfg.DelegationMode = config.DelegationModeFull
	cfg.RDDEnabled = true
	if err := config.SaveConfig(repo, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	writeFile(t, filepath.Join(repo, "app.go"), "package main\nfunc Mul(a, b int) int { return a * b }\n")
	eng := rdd.NewEngine(repo)
	started, err := eng.Start(rdd.StartRequest{
		Lenses:       []string{rdd.LensRisk, rdd.LensReliability, rdd.LensReadability},
		LensesReason: "partial fallback and mixed TypeName duplicate checks",
	})
	if err != nil {
		t.Fatalf("rdd.Start: %v", err)
	}
	if _, err := eng.AnswerConsent(started.LineageID, "granted"); err != nil {
		t.Fatalf("rdd.AnswerConsent: %v", err)
	}

	// 1. Mixed Role and TypeName duplicates (e.g. Role=review-risk + TypeName=REVIEW-RISK) -> DENY.
	var mixedDupDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-partial", WorkspacePaths: []string{repo}},
		StepIdx:     1,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review " + started.LineageID},
					{"TypeName": "REVIEW-RISK", "Prompt": "Duplicate via TypeName " + started.LineageID},
					{"Role": "review-reliability", "Prompt": "Review " + started.LineageID},
					{"TypeName": "review-readability", "Prompt": "Review " + started.LineageID},
				},
			},
		},
	}, &mixedDupDeny)
	if mixedDupDeny.Decision != DecisionDeny {
		t.Fatalf("mixed Role/TypeName duplicate: expected deny, got %+v", mixedDupDeny)
	}
	if !strings.Contains(mixedDupDeny.Reason, "duplicate") || !strings.Contains(mixedDupDeny.Reason, "review-risk") {
		t.Errorf("mixed Role/TypeName duplicate deny reason %q should mention duplicate and review-risk", mixedDupDeny.Reason)
	}

	// 2. Valid mixed Role + TypeName (different casing, no duplicates) -> ALLOW.
	var mixedTypeAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-partial", WorkspacePaths: []string{repo}},
		StepIdx:     2,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review " + started.LineageID},
					{"TypeName": "Review-Reliability", "Prompt": "Review " + started.LineageID},
					{"TypeName": "review-readability", "Prompt": "Review " + started.LineageID},
				},
			},
		},
	}, &mixedTypeAllow)
	if mixedTypeAllow.Decision != DecisionAllow {
		t.Fatalf("valid mixed Role/TypeName batch: expected allow, got %+v", mixedTypeAllow)
	}

	// 3. Partial fallback: mixed profile with review-reliability=external-mcp and review-risk+review-readability=agy-native.
	profs := config.DefaultProfilesConfig()
	profs.ActiveProfile = "deepseek-hybrid"
	prof := profs.Profiles["deepseek-hybrid"]
	prof.Roles["review-risk"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "pro"}
	prof.Roles["review-readability"] = config.RoleRoute{Runtime: config.RuntimeAgyNative, Model: "pro"}
	prof.Roles["review-reliability"] = config.RoleRoute{
		Runtime:  config.RuntimeExternalMCP,
		Provider: "deepseek",
		Model:    "deepseek-v4-pro",
		Fallback: []config.RoleTarget{{Runtime: config.RuntimeAgyNative, Model: "pro"}},
	}
	profs.Profiles["deepseek-hybrid"] = prof
	if err := config.SaveProfiles(repo, profs); err != nil {
		t.Fatalf("SaveProfiles partial fallback: %v", err)
	}

	// 3a. Batch containing failed external role WITHOUT fallback_to_native -> DENY.
	var missingFallbackMarkerDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-partial", WorkspacePaths: []string{repo}},
		StepIdx:     3,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review " + started.LineageID},
					{"Role": "review-readability", "Prompt": "Review " + started.LineageID},
					{"TypeName": "review-reliability", "Prompt": "Review " + started.LineageID},
				},
			},
		},
	}, &missingFallbackMarkerDeny)
	if missingFallbackMarkerDeny.Decision != DecisionDeny {
		t.Fatalf("batch with external-mcp role missing fallback_to_native: expected deny, got %+v", missingFallbackMarkerDeny)
	}
	if !strings.Contains(missingFallbackMarkerDeny.Reason, "fallback_to_native") {
		t.Errorf("expected deny reason to mention fallback_to_native, got %q", missingFallbackMarkerDeny.Reason)
	}

	// 3b. Partial batch missing native review-readability even though review-reliability has fallback_to_native -> DENY.
	var partialNativeMissingDeny PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-partial", WorkspacePaths: []string{repo}},
		StepIdx:     4,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review " + started.LineageID},
					{"TypeName": "review-reliability", "Prompt": "fallback_to_native for " + started.LineageID},
				},
			},
		},
	}, &partialNativeMissingDeny)
	if partialNativeMissingDeny.Decision != DecisionDeny {
		t.Fatalf("partial batch missing review-readability: expected deny, got %+v", partialNativeMissingDeny)
	}
	if !strings.Contains(partialNativeMissingDeny.Reason, "review-readability") {
		t.Errorf("expected deny reason to mention missing review-readability, got %q", partialNativeMissingDeny.Reason)
	}

	// 3c. Complete batch including all pending native lenses + failed external role WITH fallback_to_native -> ALLOW.
	var fullBatchWithFallbackAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-partial", WorkspacePaths: []string{repo}},
		StepIdx:     5,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"Role": "review-risk", "Prompt": "Review " + started.LineageID},
					{"Role": "review-readability", "Prompt": "Review " + started.LineageID},
					{"TypeName": "review-reliability", "Prompt": "fallback_to_native after external error on " + started.LineageID},
				},
			},
		},
	}, &fullBatchWithFallbackAllow)
	if fullBatchWithFallbackAllow.Decision != DecisionAllow {
		t.Fatalf("complete batch with fallback_to_native: expected allow, got %+v", fullBatchWithFallbackAllow)
	}

	// 3d. Single fallback lens alone -> ALLOW.
	var singleFallbackAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-partial", WorkspacePaths: []string{repo}},
		StepIdx:     6,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"TypeName": "review-reliability", "Prompt": "fallback_to_native for " + started.LineageID},
				},
			},
		},
	}, &singleFallbackAllow)
	if singleFallbackAllow.Decision != DecisionAllow {
		t.Fatalf("single fallback_to_native lens: expected allow, got %+v", singleFallbackAllow)
	}

	// 3e. After review-risk submits externally, only [review-reliability (fallback), review-readability (native)] remain -> ALLOW together.
	if _, err := eng.Submit(rdd.SubmitRequest{LineageID: started.LineageID, Lens: rdd.LensRisk}); err != nil {
		t.Fatalf("Submit risk: %v", err)
	}
	var remainingPairAllow PreToolUseOutput
	dispatchJSON(t, "PreToolUse", PreToolUseInput{
		CommonInput: CommonInput{ConversationID: "conv-partial", WorkspacePaths: []string{repo}},
		StepIdx:     7,
		ToolCall: ToolCall{
			Name: "invoke_subagent",
			Args: map[string]any{
				"Subagents": []map[string]any{
					{"TypeName": "review-reliability", "Prompt": "fallback_to_native for " + started.LineageID},
					{"Role": "review-readability", "Prompt": "Review " + started.LineageID},
				},
			},
		},
	}, &remainingPairAllow)
	if remainingPairAllow.Decision != DecisionAllow {
		t.Fatalf("remaining pair (1 fallback + 1 native): expected allow, got %+v", remainingPairAllow)
	}
}
