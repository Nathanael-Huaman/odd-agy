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

func TestDispatch_Errors(t *testing.T) {
	var out bytes.Buffer
	if err := Dispatch("UnknownEvent", strings.NewReader("{}"), &out); err == nil {
		t.Fatal("expected error for unknown hook event")
	}
	if err := Dispatch("PreToolUse", strings.NewReader("{invalid"), &out); err == nil {
		t.Fatal("expected error for invalid JSON input")
	}
}
