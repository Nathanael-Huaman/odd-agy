package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	EventSessionStart  = "SessionStart"
	EventPreToolUse    = "PreToolUse"
	EventPostToolUse   = "PostToolUse"
	EventPreInvocation = "PreInvocation"
	EventStop          = "Stop"

	DecisionAllow    = "allow"
	DecisionAuto     = "auto_approve"
	DecisionDeny     = "deny"
	DecisionAsk      = "ask"
	DecisionForceAsk = "force_ask"
	DecisionContinue = "continue"
)

// CommonInput holds the shared Antigravity hook stdin fields.
type CommonInput struct {
	ConversationID        string   `json:"conversationId"`
	WorkspacePaths        []string `json:"workspacePaths"`
	TranscriptPath        string   `json:"transcriptPath"`
	ArtifactDirectoryPath string   `json:"artifactDirectoryPath"`
	ModelName             string   `json:"modelName"`
	LastUserInput         string   `json:"lastUserInput"`
}

// ToolCall represents an Antigravity tool invocation payload.
type ToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// PreToolUseInput is the stdin schema for the PreToolUse hook event.
type PreToolUseInput struct {
	CommonInput
	ToolCall ToolCall `json:"toolCall"`
	StepIdx  int      `json:"stepIdx"`
}

// PreToolUseOutput is the stdout schema for the PreToolUse hook event.
type PreToolUseOutput struct {
	Decision  string         `json:"decision"`
	Reason    string         `json:"reason,omitempty"`
	Overwrite map[string]any `json:"overwrite,omitempty"`
}

// PostToolUseInput is the stdin schema for the PostToolUse hook event.
type PostToolUseInput struct {
	CommonInput
	ToolCall ToolCall `json:"toolCall"`
	StepIdx  int      `json:"stepIdx"`
	Error    any      `json:"error"`
}

// PostToolUseOutput is the stdout schema for the PostToolUse hook event.
type PostToolUseOutput struct{}

// InvocationInput is the stdin schema for SessionStart and PreInvocation hook events.
type InvocationInput struct {
	CommonInput
	InvocationNum   int `json:"invocationNum"`
	InitialNumSteps int `json:"initialNumSteps"`
}

// SystemMessagePayload carries an injected system message step.
type SystemMessagePayload struct {
	SystemMessage string         `json:"systemMessage"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// InjectStep wraps an injected step in SessionStart / PreInvocation stdout.
type InjectStep struct {
	SystemMessage *SystemMessagePayload `json:"systemMessage,omitempty"`
}

// InvocationOutput is the stdout schema for SessionStart and PreInvocation hook events.
type InvocationOutput struct {
	InjectSteps         []InjectStep `json:"injectSteps,omitempty"`
	TerminationBehavior string       `json:"terminationBehavior,omitempty"`
}

// StopInput is the stdin schema for the Stop hook event.
type StopInput struct {
	CommonInput
	ExecutionNum      int    `json:"executionNum"`
	TerminationReason string `json:"terminationReason"`
	Error             any    `json:"error"`
	FullyIdle         bool   `json:"fullyIdle"`
}

// StopOutput is the stdout schema for the Stop hook event.
type StopOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// HookState stores persisted per-workspace hook runtime state.
type HookState struct {
	DirtyMutations          bool                `json:"dirtyMutations"`
	LastAssessedDigest      string              `json:"lastAssessedDigest,omitempty"`
	StopPreflightSentDigest string              `json:"stopPreflightSentDigest,omitempty"`
	ActiveWriterSurfaces    map[string][]string `json:"activeWriterSurfaces,omitempty"`
	LastTodoReminderHash    string              `json:"lastTodoReminderHash,omitempty"`
}

var hookStateMu sync.Mutex

func resolveWorkspace(common CommonInput) (string, error) {
	for _, p := range common.WorkspacePaths {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			abs, err := filepath.Abs(trimmed)
			if err != nil {
				return filepath.Clean(trimmed), nil
			}
			return abs, nil
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return cwd, nil
}

func resolveHookStatePath(workspace string) string {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = workspace
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		gitDir := strings.TrimSpace(stdout.String())
		if gitDir != "" {
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(workspace, gitDir)
			}
			return filepath.Join(filepath.Clean(gitDir), "odd-agy", "hook-state.json")
		}
	}
	return filepath.Join(workspace, ".agents", "odd", ".state", "hook-state.json")
}

func loadHookState(workspace string) (HookState, error) {
	hookStateMu.Lock()
	defer hookStateMu.Unlock()
	return loadHookStateLocked(workspace)
}

func loadHookStateLocked(workspace string) (HookState, error) {
	path := resolveHookStatePath(workspace)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HookState{
				ActiveWriterSurfaces: make(map[string][]string),
			}, nil
		}
		return HookState{}, fmt.Errorf("read hook state %s: %w", path, err)
	}

	var st HookState
	if err := json.Unmarshal(data, &st); err != nil {
		return HookState{}, fmt.Errorf("parse hook state %s: %w", path, err)
	}
	if st.ActiveWriterSurfaces == nil {
		st.ActiveWriterSurfaces = make(map[string][]string)
	}
	return st, nil
}

func saveHookStateLocked(workspace string, st HookState) error {
	if st.ActiveWriterSurfaces == nil {
		st.ActiveWriterSurfaces = make(map[string][]string)
	}
	path := resolveHookStatePath(workspace)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal hook state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create hook state directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-hook-state-*.json")
	if err != nil {
		return fmt.Errorf("create temp hook state file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp hook state file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp hook state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp hook state file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename hook state file: %w", err)
	}
	cleanup = false
	return nil
}

func updateHookState(workspace string, fn func(st *HookState) error) (HookState, error) {
	hookStateMu.Lock()
	defer hookStateMu.Unlock()

	st, err := loadHookStateLocked(workspace)
	if err != nil {
		return HookState{}, err
	}
	if err := fn(&st); err != nil {
		return HookState{}, err
	}
	if err := saveHookStateLocked(workspace, st); err != nil {
		return HookState{}, err
	}
	return st, nil
}

func hasToolError(errVal any) bool {
	if errVal == nil {
		return false
	}
	switch v := errVal.(type) {
	case bool:
		return v
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}
