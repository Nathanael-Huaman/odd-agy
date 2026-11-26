package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/todo"
)

const todoReminderTurnThreshold = 4

// Dispatch reads a single Antigravity hook JSON payload from stdin, executes
// the handler for event, and writes the camelCase JSON response to stdout.
func Dispatch(event string, stdin io.Reader, stdout io.Writer) error {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read %s hook stdin: %w", event, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte("{}")
	}

	var resp any
	switch event {
	case EventSessionStart:
		var in InvocationInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return fmt.Errorf("parse %s stdin: %w", event, err)
		}
		resp, err = handleSessionStart(in)
	case EventPreToolUse:
		var in PreToolUseInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return fmt.Errorf("parse %s stdin: %w", event, err)
		}
		resp, err = handlePreToolUse(in)
	case EventPostToolUse:
		var in PostToolUseInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return fmt.Errorf("parse %s stdin: %w", event, err)
		}
		resp, err = handlePostToolUse(in)
	case EventPreInvocation:
		var in InvocationInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return fmt.Errorf("parse %s stdin: %w", event, err)
		}
		resp, err = handlePreInvocation(in)
	case EventStop:
		var in StopInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return fmt.Errorf("parse %s stdin: %w", event, err)
		}
		resp, err = handleStop(in)
	default:
		return fmt.Errorf("unknown hook event %q", event)
	}

	if err != nil {
		return err
	}

	outBytes, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal %s stdout: %w", event, err)
	}
	outBytes = append(outBytes, '\n')
	if _, err := stdout.Write(outBytes); err != nil {
		return fmt.Errorf("write %s stdout: %w", event, err)
	}
	return nil
}

func handleSessionStart(in InvocationInput) (InvocationOutput, error) {
	workspace, err := resolveWorkspace(in.CommonInput)
	if err != nil {
		return InvocationOutput{}, err
	}
	cfg, err := config.LoadConfig(workspace)
	if err != nil {
		return InvocationOutput{}, err
	}
	profiles, err := config.LoadProfiles(workspace)
	if err != nil {
		return InvocationOutput{}, err
	}

	openTaskFiles, err := scanOpenTaskFiles(workspace)
	if err != nil {
		return InvocationOutput{}, err
	}

	todoState, err := todo.NewStore(workspace).List()
	if err != nil {
		return InvocationOutput{}, err
	}
	openTodos := countOpenTodos(todoState.Tasks)

	tasksSummary := "none"
	if len(openTaskFiles) > 0 {
		tasksSummary = strings.Join(openTaskFiles, ", ")
	}

	msg := fmt.Sprintf(
		"ODD harness active: delegationMode=%s | rddEnabled=%t | activeProfile=%s | openTasks=[%s] | openTodos=%d",
		cfg.DelegationMode,
		cfg.RDDEnabled,
		profiles.ActiveProfile,
		tasksSummary,
		openTodos,
	)

	return InvocationOutput{
		InjectSteps: []InjectStep{{
			SystemMessage: &SystemMessagePayload{
				SystemMessage: msg,
			},
		}},
	}, nil
}

func handlePostToolUse(in PostToolUseInput) (PostToolUseOutput, error) {
	workspace, err := resolveWorkspace(in.CommonInput)
	if err != nil {
		return PostToolUseOutput{}, err
	}

	toolName := strings.ToLower(strings.TrimSpace(in.ToolCall.Name))
	errored := hasToolError(in.Error)

	isMutating := false
	switch toolName {
	case "write_to_file", "replace_file_content", "multi_replace_file_content":
		isMutating = true
	case "invoke_subagent":
		for _, entry := range parseSubagents(in.ToolCall.Args) {
			if isWriterRole(entry.effectiveRole()) {
				isMutating = true
				break
			}
		}
	}

	if _, err := updateHookState(workspace, func(st *HookState) error {
		if isMutating && !errored {
			st.DirtyMutations = true
		}
		if toolName == "invoke_subagent" {
			key := admissionKey(in.ConversationID, in.StepIdx)
			if _, exists := st.ActiveWriterSurfaces[key]; exists {
				delete(st.ActiveWriterSurfaces, key)
			} else if in.StepIdx == 0 {
				conv := strings.TrimSpace(in.ConversationID)
				if conv == "" {
					conv = "default"
				}
				prefix := conv + "#"
				for k := range st.ActiveWriterSurfaces {
					if strings.HasPrefix(k, prefix) {
						delete(st.ActiveWriterSurfaces, k)
					}
				}
			}
		}
		return nil
	}); err != nil {
		return PostToolUseOutput{}, err
	}

	if _, err := todo.NewStore(workspace).IncrementToolTurns(); err != nil {
		return PostToolUseOutput{}, err
	}

	return PostToolUseOutput{}, nil
}

func handlePreInvocation(in InvocationInput) (InvocationOutput, error) {
	workspace, err := resolveWorkspace(in.CommonInput)
	if err != nil {
		return InvocationOutput{}, err
	}

	store := todo.NewStore(workspace)
	st, err := store.List()
	if err != nil {
		return InvocationOutput{}, err
	}

	openTasks := filterOpenTodos(st.Tasks)
	if len(openTasks) == 0 || st.ToolTurnsSinceUpdate < todoReminderTurnThreshold {
		return InvocationOutput{}, nil
	}

	hash := computeTodoReminderHash(openTasks)
	hookSt, err := loadHookState(workspace)
	if err != nil {
		return InvocationOutput{}, err
	}
	if hookSt.LastTodoReminderHash == hash {
		return InvocationOutput{}, nil
	}

	if _, err := updateHookState(workspace, func(hs *HookState) error {
		hs.LastTodoReminderHash = hash
		return nil
	}); err != nil {
		return InvocationOutput{}, err
	}

	if err := store.ResetToolTurns(); err != nil {
		return InvocationOutput{}, err
	}

	msg := fmt.Sprintf("Reminder: keep the project todo list updated (%d open task(s); %d tool turns since last update).", len(openTasks), st.ToolTurnsSinceUpdate)
	return InvocationOutput{
		InjectSteps: []InjectStep{{
			SystemMessage: &SystemMessagePayload{
				SystemMessage: msg,
			},
		}},
	}, nil
}

func handleStop(in StopInput) (StopOutput, error) {
	if !in.FullyIdle {
		return StopOutput{Decision: DecisionAllow}, nil
	}

	workspace, err := resolveWorkspace(in.CommonInput)
	if err != nil {
		return StopOutput{}, err
	}
	cfg, err := config.LoadConfig(workspace)
	if err != nil {
		return StopOutput{}, err
	}
	if !cfg.RDDEnabled {
		return StopOutput{Decision: DecisionAllow}, nil
	}

	hookSt, err := loadHookState(workspace)
	if err != nil {
		return StopOutput{}, err
	}
	if !hookSt.DirtyMutations {
		return StopOutput{Decision: DecisionAllow}, nil
	}

	assess, err := assessWorkspaceForHook(workspace)
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return StopOutput{Decision: DecisionAllow}, nil
		}
		return StopOutput{}, err
	}

	if assess.ReviewDue && !assess.AlreadyApproved && hookSt.StopPreflightSentDigest != assess.CandidateDigest {
		if _, err := updateHookState(workspace, func(st *HookState) error {
			st.LastAssessedDigest = assess.CandidateDigest
			st.StopPreflightSentDigest = assess.CandidateDigest
			return nil
		}); err != nil {
			return StopOutput{}, err
		}
		return StopOutput{
			Decision: DecisionContinue,
			Reason:   "Unreviewed high-risk mutations detected; run odd_review_assess / odd_review_start before closing.",
		}, nil
	}

	return StopOutput{Decision: DecisionAllow}, nil
}

func scanOpenTaskFiles(workspace string) ([]string, error) {
	tasksDir := filepath.Join(workspace, "odd", "tasks")
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read odd/tasks directory: %w", err)
	}

	var open []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(tasksDir, e.Name()))
		if err != nil {
			continue
		}
		text := string(data)
		hasUnchecked := strings.Contains(text, "- [ ]")
		hasChecked := strings.Contains(text, "- [x]") || strings.Contains(text, "- [X]")
		if hasUnchecked || !hasChecked {
			open = append(open, filepath.ToSlash(filepath.Join("odd", "tasks", e.Name())))
		}
	}
	sort.Strings(open)
	return open, nil
}

func filterOpenTodos(tasks []todo.Task) []todo.Task {
	var open []todo.Task
	for _, t := range tasks {
		switch t.Status {
		case todo.StatusPending, todo.StatusInProgress, todo.StatusBlocked:
			open = append(open, t)
		}
	}
	return open
}

func countOpenTodos(tasks []todo.Task) int {
	return len(filterOpenTodos(tasks))
}

func computeTodoReminderHash(openTasks []todo.Task) string {
	sorted := append([]todo.Task(nil), openTasks...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID < sorted[j].ID
	})
	raw, _ := json.Marshal(sorted)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
