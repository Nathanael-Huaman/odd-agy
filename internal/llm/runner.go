package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/rdd"
)

const (
	TaskStatusRunning        = "running"
	TaskStatusCompleted      = "completed"
	TaskStatusFailed         = "failed"
	TaskStatusFallbackNative = "fallback_to_native"
	TaskStatusMaxTurns       = "max_turns"

	defaultMaxTurns      = 20
	defaultTaskTimeout   = 5 * time.Minute
	defaultShellTimeout  = 60 * time.Second
	maxToolOutputBytes   = 64 * 1024
	maxReadFileBytes     = 128 * 1024
	maxGrepMatches       = 100
	maxGlobMatches       = 200
	maxDirEntries        = 200
)

// AttemptInfo records one provider attempt during fallback resolution.
type AttemptInfo struct {
	Runtime  string `json:"runtime"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Skipped  bool   `json:"skipped,omitempty"`
	Error    string `json:"error,omitempty"`
}

// TaskRecord persists full external task state (including reasoning_content) on disk.
type TaskRecord struct {
	TaskID          string          `json:"taskId"`
	Role            string          `json:"role"`
	Provider        string          `json:"provider,omitempty"`
	Model           string          `json:"model,omitempty"`
	Compatibility   string          `json:"compatibility,omitempty"`
	AllowedSurfaces []string        `json:"allowedSurfaces,omitempty"`
	Messages        []Message       `json:"messages"`
	Turns           int             `json:"turns"`
	Usage           Usage           `json:"usage"`
	Status          string          `json:"status"`
	FinalResponse   string          `json:"finalResponse,omitempty"`
	NativeFallback  *NativeFallback `json:"nativeFallback,omitempty"`
	Attempts        []AttemptInfo   `json:"attempts,omitempty"`
	Error           string          `json:"error,omitempty"`
	CreatedAt       string          `json:"createdAt"`
	UpdatedAt       string          `json:"updatedAt"`
}

// RunRequest starts a new bounded external agentic task.
type RunRequest struct {
	Role            string        `json:"role"`
	Task            string        `json:"task"`
	Provider        string        `json:"provider,omitempty"`
	Model           string        `json:"model,omitempty"`
	MaxTurns        int           `json:"maxTurns,omitempty"`
	MaxTokens       int           `json:"maxTokens,omitempty"`
	Timeout         time.Duration `json:"timeout,omitempty"`
	AllowedSurfaces []string      `json:"allowedSurfaces,omitempty"`
}

// ContinueRequest resumes a persisted task with a follow-up prompt.
type ContinueRequest struct {
	TaskID          string   `json:"taskId"`
	Prompt          string   `json:"prompt"`
	AllowedSurfaces []string `json:"allowedSurfaces,omitempty"`
	MaxTurns        int      `json:"maxTurns,omitempty"`
}

// ProviderSummary exposes provider config & env key presence without revealing secrets.
type ProviderSummary struct {
	Name          string `json:"name"`
	BaseURL       string `json:"baseUrl"`
	BaseURLEnv    string `json:"baseUrlEnv,omitempty"`
	APIKeyEnv     string `json:"apiKeyEnv,omitempty"`
	APIKeyPresent bool   `json:"apiKeyPresent"`
	KeySource     string `json:"keySource"`
	IsLoopback    bool   `json:"isLoopback"`
	Compatibility string `json:"compatibility"`
	DefaultModel  string `json:"defaultModel"`
	FastModel     string `json:"fastModel,omitempty"`
}

// ProfilesSummary is the redacted profile overview returned by odd_external_profiles.
type ProfilesSummary struct {
	ActiveProfile string                          `json:"activeProfile"`
	Providers     map[string]ProviderSummary      `json:"providers"`
	Profiles      map[string]config.ProfileConfig `json:"profiles"`
}

// RunnerConfig configures a sandboxed external agentic Runner.
type RunnerConfig struct {
	RepoRoot        string
	Client          *Client
	LookupEnv       func(string) (string, bool)
	DefaultMaxTurns int
	DefaultTimeout  time.Duration
}

// Runner executes sandboxed multi-turn agentic tasks against OpenAI-compatible providers.
type Runner struct {
	mu              sync.Mutex
	repoRoot        string
	client          *Client
	lookupEnv       func(string) (string, bool)
	defaultMaxTurns int
	defaultTimeout  time.Duration
}

// NewRunner constructs a Runner rooted at cfg.RepoRoot.
func NewRunner(cfg RunnerConfig) *Runner {
	client := cfg.Client
	if client == nil {
		client = NewClient(nil)
	}
	lookupEnv := cfg.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	maxTurns := cfg.DefaultMaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}
	timeout := cfg.DefaultTimeout
	if timeout <= 0 {
		timeout = defaultTaskTimeout
	}
	return &Runner{
		repoRoot:        cfg.RepoRoot,
		client:          client,
		lookupEnv:       lookupEnv,
		defaultMaxTurns: maxTurns,
		defaultTimeout:  timeout,
	}
}

// ProfilesSummary loads .agents/odd/profiles.json and returns redacted provider status.
func (r *Runner) ProfilesSummary() (ProfilesSummary, error) {
	profiles, err := config.LoadProfiles(r.repoRoot)
	if err != nil {
		return ProfilesSummary{}, err
	}

	provMap := make(map[string]ProviderSummary, len(profiles.Providers))
	for name, p := range profiles.Providers {
		ep, resolveErr := config.ResolveProviderEndpoint(p, r.lookupEnv)
		summary := ProviderSummary{
			Name:          name,
			BaseURL:       RedactURL(p.BaseURL),
			BaseURLEnv:    p.BaseURLEnv,
			APIKeyEnv:     p.APIKeyEnv,
			Compatibility: p.Compatibility,
			DefaultModel:  p.DefaultModel,
			FastModel:     p.FastModel,
		}
		if resolveErr == nil {
			summary.BaseURL = RedactURL(ep.BaseURL)
			summary.IsLoopback = ep.IsLoopback
			summary.KeySource = ep.KeySource
			summary.APIKeyPresent = !ep.MissingAPIKey && ep.KeySource == "env"
		} else {
			summary.KeySource = "invalid"
		}
		provMap[name] = summary
	}

	return ProfilesSummary{
		ActiveProfile: profiles.ActiveProfile,
		Providers:     provMap,
		Profiles:      profiles.Profiles,
	}, nil
}

// Run resolves the role's routing/fallback chain and runs a bounded sandboxed agentic loop.
func (r *Runner) Run(ctx context.Context, req RunRequest) (TaskRecord, error) {
	role := strings.TrimSpace(req.Role)
	if role == "" {
		return TaskRecord{}, errors.New("role must not be empty")
	}

	var surfaces []string
	if isWriterRole(role) {
		rawSurfaces := req.AllowedSurfaces
		if len(rawSurfaces) == 0 {
			rawSurfaces = ParseAllowedSurfaces(req.Task)
		}
		validated, err := ValidateAllowedSurfaces(rawSurfaces)
		if err != nil {
			return TaskRecord{}, fmt.Errorf("writer role %q requires valid Allowed edit surfaces: %w", role, err)
		}
		surfaces = validated
	} else if len(req.AllowedSurfaces) > 0 {
		validated, err := ValidateAllowedSurfaces(req.AllowedSurfaces)
		if err != nil {
			return TaskRecord{}, fmt.Errorf("invalid allowedSurfaces: %w", err)
		}
		surfaces = validated
	}

	profiles, err := config.LoadProfiles(r.repoRoot)
	if err != nil {
		return TaskRecord{}, err
	}

	candidates, nativeFB, err := ResolveRoleChain(role, profiles, req.Provider, req.Model, r.lookupEnv)
	if err != nil {
		return TaskRecord{}, err
	}

	taskID, err := newTaskID()
	if err != nil {
		return TaskRecord{}, err
	}

	now := nowUTC()
	rec := TaskRecord{
		TaskID:          taskID,
		Role:            role,
		AllowedSurfaces: surfaces,
		Messages: []Message{
			{Role: "system", Content: buildSystemPrompt(role, surfaces)},
			{Role: "user", Content: req.Task},
		},
		Status:    TaskStatusRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}

	maxTurns := req.MaxTurns
	if maxTurns <= 0 {
		maxTurns = r.defaultMaxTurns
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.defaultTimeout
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	return r.executeWithCandidates(runCtx, rec, candidates, nativeFB, maxTurns, req.MaxTokens)
}

// Continue reloads a persisted TaskRecord (keeping reasoning_content and surfaces intact)
// and resumes the conversation with prompt.
func (r *Runner) Continue(ctx context.Context, req ContinueRequest) (TaskRecord, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return TaskRecord{}, errors.New("prompt must not be empty")
	}
	rec, err := r.Status(req.TaskID)
	if err != nil {
		return TaskRecord{}, err
	}

	if len(req.AllowedSurfaces) > 0 {
		validated, err := ValidateAllowedSurfaces(req.AllowedSurfaces)
		if err != nil {
			return TaskRecord{}, fmt.Errorf("invalid allowedSurfaces: %w", err)
		}
		rec.AllowedSurfaces = validated
	} else if isWriterRole(rec.Role) && len(rec.AllowedSurfaces) == 0 {
		return TaskRecord{}, fmt.Errorf("persisted writer task %s has no AllowedSurfaces", rec.TaskID)
	}

	profiles, err := config.LoadProfiles(r.repoRoot)
	if err != nil {
		return TaskRecord{}, err
	}

	candidates, nativeFB, err := ResolveRoleChain(rec.Role, profiles, rec.Provider, rec.Model, r.lookupEnv)
	if err != nil {
		return TaskRecord{}, err
	}

	rec.Messages = append(rec.Messages, Message{
		Role:    "user",
		Content: req.Prompt,
	})
	rec.Status = TaskStatusRunning
	rec.Error = ""
	rec.FinalResponse = ""
	rec.NativeFallback = nil

	maxTurns := req.MaxTurns
	if maxTurns <= 0 {
		maxTurns = r.defaultMaxTurns
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		runCtx, cancel = context.WithTimeout(ctx, r.defaultTimeout)
		defer cancel()
	}

	return r.executeWithCandidates(runCtx, rec, candidates, nativeFB, maxTurns, 0)
}

// Status loads a persisted task record by ID.
func (r *Runner) Status(taskID string) (TaskRecord, error) {
	if err := validateTaskID(taskID); err != nil {
		return TaskRecord{}, err
	}
	path := r.taskFilePath(taskID)
	data, err := os.ReadFile(path)
	if err != nil {
		return TaskRecord{}, fmt.Errorf("load task %s: %w", taskID, err)
	}
	var rec TaskRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return TaskRecord{}, fmt.Errorf("parse task %s: %w", taskID, err)
	}
	return rec, nil
}

func (r *Runner) executeWithCandidates(
	ctx context.Context,
	rec TaskRecord,
	candidates []ResolvedCandidate,
	nativeFB *NativeFallback,
	maxTurns int,
	maxTokens int,
) (TaskRecord, error) {
	tools := r.toolsForRole(rec.Role)
	baseMsgsLen := len(rec.Messages)

	for _, cand := range candidates {
		if cand.Skipped {
			rec.Attempts = append(rec.Attempts, AttemptInfo{
				Runtime:  cand.Target.Runtime,
				Provider: cand.Target.Provider,
				Model:    cand.Target.Model,
				Skipped:  true,
				Error:    cand.SkipReason,
			})
			continue
		}

		// Reset turn-specific messages when retrying a fresh fallback candidate.
		attemptMsgs := append([]Message(nil), rec.Messages[:baseMsgsLen]...)
		var attemptUsage Usage
		var turnsTaken int
		var finalResp string
		var loopErr error
		var retryOnNext bool
		hitMaxTurns := false

		for turn := 0; turn < maxTurns; turn++ {
			turnsTaken++
			resp, err := r.client.ChatCompletions(ctx, cand.Endpoint, ChatRequest{
				Model:     cand.Target.Model,
				Messages:  attemptMsgs,
				Tools:     tools,
				MaxTokens: maxTokens,
			})
			if err != nil {
				loopErr = err
				// Only fall back to next provider if first turn failed with retryable error.
				if turn == 0 && isRetryableError(err) {
					retryOnNext = true
				}
				break
			}

			attemptUsage.PromptTokens += resp.Usage.PromptTokens
			attemptUsage.CompletionTokens += resp.Usage.CompletionTokens
			attemptUsage.TotalTokens += resp.Usage.TotalTokens

			choice := resp.Choices[0]
			asstMsg := choice.Message
			if asstMsg.Role == "" {
				asstMsg.Role = "assistant"
			}
			attemptMsgs = append(attemptMsgs, asstMsg)

			if len(asstMsg.ToolCalls) == 0 {
				finalResp = asstMsg.Content
				break
			}

			for _, tc := range asstMsg.ToolCalls {
				toolOut := r.executeToolCall(rec.Role, rec.AllowedSurfaces, tc)
				attemptMsgs = append(attemptMsgs, Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
					Content:    toolOut,
				})
			}

			if turn == maxTurns-1 {
				hitMaxTurns = true
			}
		}

		if loopErr != nil {
			rec.Attempts = append(rec.Attempts, AttemptInfo{
				Runtime:  cand.Target.Runtime,
				Provider: cand.Target.Provider,
				Model:    cand.Target.Model,
				Error:    redactError(loopErr, cand.Endpoint.APIKey),
			})
			if retryOnNext {
				continue
			}
			rec.Provider = cand.Target.Provider
			rec.Model = cand.Target.Model
			rec.Compatibility = cand.Endpoint.Compatibility
			rec.Messages = attemptMsgs
			rec.Turns += turnsTaken
			rec.Status = TaskStatusFailed
			rec.Error = redactError(loopErr, cand.Endpoint.APIKey)
			rec.UpdatedAt = nowUTC()
			if err := r.saveTask(rec); err != nil {
				return TaskRecord{}, err
			}
			return rec, nil
		}

		rec.Provider = cand.Target.Provider
		rec.Model = cand.Target.Model
		rec.Compatibility = cand.Endpoint.Compatibility
		rec.Messages = attemptMsgs
		rec.Turns += turnsTaken
		rec.Usage.PromptTokens += attemptUsage.PromptTokens
		rec.Usage.CompletionTokens += attemptUsage.CompletionTokens
		rec.Usage.TotalTokens += attemptUsage.TotalTokens
		rec.FinalResponse = finalResp
		if hitMaxTurns {
			rec.Status = TaskStatusMaxTurns
			rec.Error = fmt.Sprintf("reached maxTurns limit (%d)", maxTurns)
		} else {
			rec.Status = TaskStatusCompleted
		}
		rec.UpdatedAt = nowUTC()
		if err := r.saveTask(rec); err != nil {
			return TaskRecord{}, err
		}
		return rec, nil
	}

	if nativeFB != nil {
		rec.Status = TaskStatusFallbackNative
		rec.NativeFallback = nativeFB
		rec.UpdatedAt = nowUTC()
		if err := r.saveTask(rec); err != nil {
			return TaskRecord{}, err
		}
		return rec, nil
	}

	rec.Status = TaskStatusFailed
	rec.Error = "all configured external providers failed or were skipped with no fallback remaining"
	rec.UpdatedAt = nowUTC()
	if err := r.saveTask(rec); err != nil {
		return TaskRecord{}, err
	}
	return rec, nil
}

func (r *Runner) taskFilePath(taskID string) string {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = r.repoRoot
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err == nil {
		gitDir := strings.TrimSpace(stdout.String())
		if gitDir != "" {
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(r.repoRoot, gitDir)
			}
			return filepath.Join(filepath.Clean(gitDir), "odd-agy", "tasks", taskID+".json")
		}
	}
	return filepath.Join(r.repoRoot, ".agents", "odd", ".state", "tasks", taskID+".json")
}

func (r *Runner) saveTask(rec TaskRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	path := r.taskFilePath(rec.TaskID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create task dir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal task %s: %w", rec.TaskID, err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".tmp-task-*.json")
	if err != nil {
		return fmt.Errorf("create temp task file: %w", err)
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
		return fmt.Errorf("write temp task file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp task file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp task file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename task file: %w", err)
	}
	cleanup = false
	return nil
}

func (r *Runner) toolsForRole(role string) []ToolDefinition {
	tools := []ToolDefinition{
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "read_file",
				Description: "Read a file inside the repository.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":   map[string]any{"type": "string"},
						"offset": map[string]any{"type": "integer"},
						"limit":  map[string]any{"type": "integer"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "list_dir",
				Description: "List directory contents inside the repository.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "grep_search",
				Description: "Search text inside repository files.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern": map[string]any{"type": "string"},
						"path":    map[string]any{"type": "string"},
					},
					"required": []string{"pattern"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "glob_files",
				Description: "Find repository files matching a glob pattern.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern": map[string]any{"type": "string"},
						"path":    map[string]any{"type": "string"},
					},
					"required": []string{"pattern"},
				},
			},
		},
	}

	if isReviewRole(role) {
		tools = append(tools, ToolDefinition{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "odd_review_scope",
				Description: "Read paginated frozen candidate snapshot for an RDD lineage.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"lineageId": map[string]any{"type": "string"},
						"cursor":    map[string]any{"type": "integer"},
						"limit":     map[string]any{"type": "integer"},
					},
					"required": []string{"lineageId"},
				},
			},
		})
	}

	if isShellRole(role) {
		tools = append(tools, ToolDefinition{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "run_shell",
				Description: "Run a non-destructive shell command inside the repository.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
					"required": []string{"command"},
				},
			},
		})
	}

	if isWriterRole(role) {
		tools = append(tools,
			ToolDefinition{
				Type: "function",
				Function: FunctionDefinition{
					Name:        "write_file",
					Description: "Write a file within the Allowed edit surfaces.",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]any{"type": "string"},
							"content": map[string]any{"type": "string"},
						},
						"required": []string{"path", "content"},
					},
				},
			},
			ToolDefinition{
				Type: "function",
				Function: FunctionDefinition{
					Name:        "edit_file",
					Description: "Replace an exact unique substring in a file within Allowed edit surfaces.",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]any{"type": "string"},
							"oldText": map[string]any{"type": "string"},
							"newText": map[string]any{"type": "string"},
						},
						"required": []string{"path", "oldText", "newText"},
					},
				},
			},
		)
	}

	return tools
}

func (r *Runner) executeToolCall(role string, allowedSurfaces []string, tc ToolCall) string {
	allowed := make(map[string]bool)
	for _, t := range r.toolsForRole(role) {
		allowed[t.Function.Name] = true
	}
	if !allowed[tc.Function.Name] {
		return toolErrJSON(fmt.Sprintf("tool %q is not permitted for role %q", tc.Function.Name, role))
	}

	switch tc.Function.Name {
	case "read_file":
		var args struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return toolErrJSON("invalid arguments: " + err.Error())
		}
		return r.toolReadFile(args.Path, args.Offset, args.Limit)

	case "list_dir":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return toolErrJSON("invalid arguments: " + err.Error())
		}
		return r.toolListDir(args.Path)

	case "grep_search":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return toolErrJSON("invalid arguments: " + err.Error())
		}
		return r.toolGrep(args.Pattern, args.Path)

	case "glob_files":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return toolErrJSON("invalid arguments: " + err.Error())
		}
		return r.toolGlob(args.Pattern, args.Path)

	case "odd_review_scope":
		var args struct {
			LineageID string `json:"lineageId"`
			Cursor    int    `json:"cursor"`
			Limit     int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return toolErrJSON("invalid arguments: " + err.Error())
		}
		page, err := rdd.NewEngine(r.repoRoot).Scope(args.LineageID, args.Cursor, args.Limit)
		if err != nil {
			return toolErrJSON(err.Error())
		}
		return mustJSON(page)

	case "run_shell":
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return toolErrJSON("invalid arguments: " + err.Error())
		}
		return r.toolRunShell(role, allowedSurfaces, args.Command)

	case "write_file":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return toolErrJSON("invalid arguments: " + err.Error())
		}
		return r.toolWriteFile(allowedSurfaces, args.Path, args.Content)

	case "edit_file":
		var args struct {
			Path    string `json:"path"`
			OldText string `json:"oldText"`
			NewText string `json:"newText"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return toolErrJSON("invalid arguments: " + err.Error())
		}
		return r.toolEditFile(allowedSurfaces, args.Path, args.OldText, args.NewText)

	default:
		return toolErrJSON("unknown tool " + tc.Function.Name)
	}
}

func (r *Runner) toolReadFile(rel string, offset, limit int) string {
	full, _, err := resolveReadPath(r.repoRoot, rel)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	if len(data) > maxReadFileBytes && offset <= 0 && limit <= 0 {
		data = data[:maxReadFileBytes]
	}
	if offset > 0 || limit > 0 {
		lines := strings.Split(string(data), "\n")
		start := 0
		if offset > 1 {
			start = offset - 1
		}
		if start > len(lines) {
			start = len(lines)
		}
		end := len(lines)
		if limit > 0 && start+limit < end {
			end = start + limit
		}
		return strings.Join(lines[start:end], "\n")
	}
	return string(data)
}

func (r *Runner) toolListDir(rel string) string {
	if strings.TrimSpace(rel) == "" {
		rel = "."
	}
	full, _, err := resolveReadPath(r.repoRoot, rel)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if name == ".git" || isSensitiveRelPath(name) {
			continue
		}
		kind := "file"
		if e.IsDir() {
			kind = "dir"
		}
		out = append(out, fmt.Sprintf("[%s] %s", kind, name))
		if len(out) >= maxDirEntries {
			break
		}
	}
	return strings.Join(out, "\n")
}

func (r *Runner) toolGrep(pattern, rel string) string {
	if strings.TrimSpace(pattern) == "" {
		return toolErrJSON("pattern must not be empty")
	}
	if strings.TrimSpace(rel) == "" {
		rel = "."
	}
	startFull, _, err := resolveReadPath(r.repoRoot, rel)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	realRoot, err := filepath.EvalSymlinks(r.repoRoot)
	if err != nil {
		return toolErrJSON(err.Error())
	}

	var matches []string
	_ = filepath.WalkDir(startFull, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == ".ssh" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relPath, err := filepath.Rel(realRoot, p)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(relPath)
		if isSensitiveRelPath(relSlash) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 512*1024 {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if strings.Contains(line, pattern) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", relSlash, lineNum, line))
				if len(matches) >= maxGrepMatches {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	return strings.Join(matches, "\n")
}

func (r *Runner) toolGlob(pattern, rel string) string {
	if strings.TrimSpace(pattern) == "" {
		return toolErrJSON("pattern must not be empty")
	}
	if strings.TrimSpace(rel) == "" {
		rel = "."
	}
	startFull, _, err := resolveReadPath(r.repoRoot, rel)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	realRoot, err := filepath.EvalSymlinks(r.repoRoot)
	if err != nil {
		return toolErrJSON(err.Error())
	}

	var matches []string
	_ = filepath.WalkDir(startFull, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == ".ssh" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relPath, err := filepath.Rel(realRoot, p)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(relPath)
		if isSensitiveRelPath(relSlash) {
			return nil
		}
		if matchGlob(pattern, relSlash) {
			matches = append(matches, relSlash)
			if len(matches) >= maxGlobMatches {
				return fs.SkipAll
			}
		}
		return nil
	})
	return strings.Join(matches, "\n")
}

func (r *Runner) toolRunShell(role string, allowedSurfaces []string, command string) string {
	if err := validateShellCommand(role, allowedSurfaces, r.repoRoot, command); err != nil {
		return toolErrJSON(err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultShellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = r.repoRoot
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := buf.String()
	if len(out) > maxToolOutputBytes {
		out = out[:maxToolOutputBytes] + "\n...[truncated]"
	}

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return toolErrJSON(err.Error())
		}
	}
	return mustJSON(map[string]any{
		"exitCode": exitCode,
		"output":   out,
	})
}

func (r *Runner) toolWriteFile(allowedSurfaces []string, rel, content string) string {
	full, relSlash, err := resolveWritePath(r.repoRoot, allowedSurfaces, rel)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return toolErrJSON(err.Error())
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return toolErrJSON(err.Error())
	}
	return mustJSON(map[string]any{"written": relSlash, "bytes": len(content)})
}

func (r *Runner) toolEditFile(allowedSurfaces []string, rel, oldText, newText string) string {
	if oldText == "" {
		return toolErrJSON("oldText must not be empty")
	}
	full, relSlash, err := resolveWritePath(r.repoRoot, allowedSurfaces, rel)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	text := string(data)
	count := strings.Count(text, oldText)
	if count == 0 {
		return toolErrJSON("oldText not found in " + relSlash)
	}
	if count > 1 {
		return toolErrJSON(fmt.Sprintf("oldText matched %d locations in %s; must be unique", count, relSlash))
	}
	updated := strings.Replace(text, oldText, newText, 1)
	if err := os.WriteFile(full, []byte(updated), 0o644); err != nil {
		return toolErrJSON(err.Error())
	}
	return mustJSON(map[string]any{"edited": relSlash})
}

// ParseAllowedSurfaces extracts paths/globs listed under `## Allowed edit surfaces`.
func ParseAllowedSurfaces(task string) []string {
	lines := strings.Split(task, "\n")
	inSection := false
	seenEntries := false
	var out []string
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "## ") {
			heading := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")))
			if heading == "allowed edit surfaces" {
				inSection = true
				continue
			}
			if inSection {
				break
			}
		}
		if !inSection {
			continue
		}
		if trimmed == "" {
			if seenEntries {
				break
			}
			continue
		}
		isBullet := strings.HasPrefix(trimmed, "- ") ||
			strings.HasPrefix(trimmed, "* ") ||
			strings.HasPrefix(trimmed, "+ ")
		item := strings.TrimPrefix(trimmed, "- ")
		item = strings.TrimPrefix(item, "* ")
		item = strings.TrimPrefix(item, "+ ")
		item = strings.TrimSpace(item)
		item = strings.Trim(item, "`")
		item = strings.TrimSpace(item)
		if !isBullet && strings.ContainsAny(item, " \t") {
			if seenEntries {
				break
			}
			continue
		}
		if item == "" || strings.EqualFold(item, "none") {
			continue
		}
		seenEntries = true
		out = append(out, item)
	}
	return out
}

// ValidateAllowedSurfaces validates repo-relative paths/globs and rejects overly broad or escaping patterns.
func ValidateAllowedSurfaces(surfaces []string) ([]string, error) {
	if len(surfaces) == 0 {
		return nil, errors.New("at least one allowed edit surface is required")
	}
	out := make([]string, 0, len(surfaces))
	for _, raw := range surfaces {
		s := strings.TrimSpace(raw)
		s = strings.Trim(s, "`")
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, errors.New("empty surface pattern")
		}
		if filepath.IsAbs(s) || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "\\") || (len(s) >= 2 && s[1] == ':') {
			return nil, fmt.Errorf("surface %q must be repo-relative, not absolute", raw)
		}
		slash := filepath.ToSlash(s)
		slash = strings.TrimPrefix(slash, "./")
		if slash == "" || slash == "." || slash == "*" || slash == "**" || slash == "**/*" {
			return nil, fmt.Errorf("surface %q is too broad", raw)
		}
		for _, seg := range strings.Split(slash, "/") {
			if seg == ".." {
				return nil, fmt.Errorf("surface %q must not contain '..'", raw)
			}
		}
		if isSensitiveRelPath(slash) {
			return nil, fmt.Errorf("surface %q targets a restricted path", raw)
		}
		out = append(out, slash)
	}
	return out, nil
}

func resolveReadPath(repoRoot, raw string) (string, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", errors.New("path must not be empty")
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") {
		return "", "", fmt.Errorf("path %q must be repo-relative", raw)
	}
	slash := filepath.ToSlash(trimmed)
	for _, seg := range strings.Split(slash, "/") {
		if seg == ".." {
			return "", "", fmt.Errorf("path %q escapes repository", raw)
		}
	}

	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve repo root: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(filepath.Join(realRoot, filepath.FromSlash(slash)))
	if err != nil {
		return "", "", fmt.Errorf("resolve path %q: %w", raw, err)
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q escapes repository root", raw)
	}
	relSlash := filepath.ToSlash(rel)
	if isSensitiveRelPath(relSlash) || isSensitiveRelPath(slash) {
		return "", "", fmt.Errorf("access to sensitive path %q is blocked", raw)
	}
	return realTarget, relSlash, nil
}

func resolveWritePath(repoRoot string, allowedSurfaces []string, raw string) (string, string, error) {
	if len(allowedSurfaces) == 0 {
		return "", "", errors.New("no allowed edit surfaces configured")
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", errors.New("path must not be empty")
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") || (len(trimmed) >= 2 && trimmed[1] == ':') {
		return "", "", fmt.Errorf("path %q must be repo-relative", raw)
	}
	reqSlash := strings.TrimPrefix(filepath.ToSlash(trimmed), "./")
	for _, seg := range strings.Split(reqSlash, "/") {
		if seg == ".." {
			return "", "", fmt.Errorf("path %q escapes repository", raw)
		}
	}
	if isSensitiveRelPath(reqSlash) {
		return "", "", fmt.Errorf("writing to sensitive path %q is blocked", raw)
	}
	if !matchesAnySurface(allowedSurfaces, reqSlash) {
		return "", "", fmt.Errorf("path %q is outside Allowed edit surfaces %v", reqSlash, allowedSurfaces)
	}

	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve repo root: %w", err)
	}

	joined := filepath.Join(realRoot, filepath.FromSlash(reqSlash))
	// Check if target exists (including dangling symlinks via Lstat).
	if info, lerr := os.Lstat(joined); lerr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			realTarget, err := filepath.EvalSymlinks(joined)
			if err != nil {
				return "", "", fmt.Errorf("blocked symlink %q: %w", raw, err)
			}
			rel, err := filepath.Rel(realRoot, realTarget)
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", "", fmt.Errorf("symlink %q escapes repository root", raw)
			}
			realRel := filepath.ToSlash(rel)
			if isSensitiveRelPath(realRel) || !matchesAnySurface(allowedSurfaces, realRel) {
				return "", "", fmt.Errorf("symlink target %q is outside Allowed edit surfaces", realRel)
			}
			return realTarget, realRel, nil
		}
	}

	// Walk up to nearest existing ancestor to prevent parent-directory symlink escapes.
	ancestor := filepath.Dir(joined)
	var tail []string
	tail = append(tail, filepath.Base(joined))
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			break
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", "", fmt.Errorf("cannot resolve ancestor for %q", raw)
		}
		tail = append([]string{filepath.Base(ancestor)}, tail...)
		ancestor = parent
	}

	realAncestor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", "", fmt.Errorf("resolve parent of %q: %w", raw, err)
	}
	relAnc, err := filepath.Rel(realRoot, realAncestor)
	if err != nil || relAnc == ".." || strings.HasPrefix(relAnc, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q parent symlink escapes repository root", raw)
	}

	parts := append([]string{realAncestor}, tail...)
	finalPath := filepath.Join(parts...)
	finalRel, err := filepath.Rel(realRoot, finalPath)
	if err != nil || finalRel == "." || finalRel == ".." || strings.HasPrefix(finalRel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q escapes repository root", raw)
	}
	finalRelSlash := filepath.ToSlash(finalRel)
	if isSensitiveRelPath(finalRelSlash) || !matchesAnySurface(allowedSurfaces, finalRelSlash) {
		return "", "", fmt.Errorf("resolved path %q is outside Allowed edit surfaces", finalRelSlash)
	}
	return finalPath, finalRelSlash, nil
}

func isSensitiveRelPath(slashPath string) bool {
	lower := strings.ToLower(strings.TrimPrefix(filepath.ToSlash(slashPath), "./"))
	base := filepath.Base(lower)
	if lower == ".git" || strings.HasPrefix(lower, ".git/") || strings.Contains(lower, "/.git/") {
		return true
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	if lower == ".ssh" || strings.HasPrefix(lower, ".ssh/") || strings.Contains(lower, "/.ssh/") {
		return true
	}
	if lower == ".aws" || strings.HasPrefix(lower, ".aws/") || strings.Contains(lower, "/.aws/") {
		return true
	}
	switch base {
	case "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa", ".netrc", ".npmrc", ".pypirc":
		return true
	}
	ext := filepath.Ext(base)
	if ext == ".pem" || ext == ".key" || ext == ".p12" || ext == ".pfx" {
		return true
	}
	return false
}

func matchesAnySurface(surfaces []string, relSlash string) bool {
	for _, pat := range surfaces {
		if matchGlob(pat, relSlash) {
			return true
		}
	}
	return false
}

// matchGlob matches slash-separated paths supporting `**` recursive globs and `*` segment globs.
func matchGlob(pattern, target string) bool {
	patSegs := strings.Split(strings.Trim(filepath.ToSlash(pattern), "/"), "/")
	tgtSegs := strings.Split(strings.Trim(filepath.ToSlash(target), "/"), "/")
	return matchSegments(patSegs, tgtSegs)
}

func matchSegments(pat, tgt []string) bool {
	if len(pat) == 0 {
		return len(tgt) == 0
	}
	if pat[0] == "**" {
		// ** matches zero or more segments.
		for i := 0; i <= len(tgt); i++ {
			if matchSegments(pat[1:], tgt[i:]) {
				return true
			}
		}
		return false
	}
	if len(tgt) == 0 {
		return false
	}
	ok, err := filepath.Match(pat[0], tgt[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], tgt[1:])
}

func validateShellCommand(role string, allowedSurfaces []string, repoRoot, command string) error {
	return validateShellCommandDepth(role, allowedSurfaces, repoRoot, command, 0)
}

func validateShellCommandDepth(role string, allowedSurfaces []string, repoRoot, command string, depth int) error {
	if depth > 4 {
		return errors.New("blocked shell command: nesting depth exceeded")
	}
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return errors.New("command must not be empty")
	}

	// Quick block for well-known catastrophic/obfuscated forms before parsing.
	collapsed := strings.Join(strings.Fields(strings.ToLower(trimmed)), " ")
	for _, b := range []string{"mkfs", "dd if=", ":(){:|:&};:"} {
		if strings.Contains(collapsed, b) {
			return fmt.Errorf("blocked shell command containing %q", b)
		}
	}

	// Recursively validate command substitutions `...` and $(...).
	subs, err := extractCommandSubstitutions(trimmed)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if err := validateShellCommandDepth(role, allowedSurfaces, repoRoot, sub, depth+1); err != nil {
			return err
		}
	}

	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}

	segments, err := parseShellSegments(trimmed)
	if err != nil {
		return err
	}

	for _, seg := range segments {
		if err := validateShellSegment(role, allowedSurfaces, repoRoot, realRoot, seg, depth); err != nil {
			return err
		}
	}
	return nil
}

type shellTokenKind int

const (
	tokWord shellTokenKind = iota
	tokSep
	tokRedirWrite
	tokRedirRead
	tokRedirDup
)

type shellToken struct {
	kind   shellTokenKind
	val    string
	quoted bool
}

type shellRedir struct {
	op     string
	target string
}

type shellSegment struct {
	words  []string
	redirs []shellRedir
}

func extractCommandSubstitutions(s string) ([]string, error) {
	var subs []string
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\\' && !inSingle && i+1 < len(s) {
			i++
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if inSingle {
			continue
		}
		if ch == '`' {
			end := i + 1
			for end < len(s) {
				if s[end] == '\\' && end+1 < len(s) {
					end += 2
					continue
				}
				if s[end] == '`' {
					break
				}
				end++
			}
			if end >= len(s) {
				return nil, errors.New("unterminated backtick substitution in shell command")
			}
			subs = append(subs, s[i+1:end])
			i = end
			continue
		}
		if ch == '$' && i+1 < len(s) && s[i+1] == '(' {
			// Skip $(( arithmetic ))? Still harmless to parse, or handle paren depth.
			depth := 1
			j := i + 2
			subSingle := false
			subDouble := false
			for j < len(s) && depth > 0 {
				c := s[j]
				if c == '\\' && !subSingle && j+1 < len(s) {
					j += 2
					continue
				}
				if c == '\'' && !subDouble {
					subSingle = !subSingle
				} else if c == '"' && !subSingle {
					subDouble = !subDouble
				} else if !subSingle && !subDouble {
					if c == '(' {
						depth++
					} else if c == ')' {
						depth--
						if depth == 0 {
							break
						}
					}
				}
				j++
			}
			if depth != 0 {
				return nil, errors.New("unterminated $(...) substitution in shell command")
			}
			subs = append(subs, s[i+2:j])
			i = j
		}
	}
	return subs, nil
}

func tokenizeShell(s string) ([]shellToken, error) {
	var tokens []shellToken
	i := 0
	for i < len(s) {
		// Skip whitespace (except newline which separates commands).
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r') {
			i++
		}
		if i >= len(s) {
			break
		}

		// Operators.
		if s[i] == '\n' || s[i] == ';' {
			tokens = append(tokens, shellToken{kind: tokSep, val: string(s[i])})
			i++
			continue
		}
		if s[i] == '|' {
			if i+1 < len(s) && s[i+1] == '|' {
				tokens = append(tokens, shellToken{kind: tokSep, val: "||"})
				i += 2
			} else {
				tokens = append(tokens, shellToken{kind: tokSep, val: "|"})
				i++
			}
			continue
		}
		if s[i] == '&' {
			if i+1 < len(s) && s[i+1] == '&' {
				tokens = append(tokens, shellToken{kind: tokSep, val: "&&"})
				i += 2
				continue
			}
			if i+1 < len(s) && s[i+1] == '>' {
				if i+2 < len(s) && s[i+2] == '>' {
					tokens = append(tokens, shellToken{kind: tokRedirWrite, val: "&>>"})
					i += 3
				} else {
					tokens = append(tokens, shellToken{kind: tokRedirWrite, val: "&>"})
					i += 2
				}
				continue
			}
			tokens = append(tokens, shellToken{kind: tokSep, val: "&"})
			i++
			continue
		}

		// FD-prefixed redirections: 1>, 1>>, 2>, 2>>, 2>&1, 1>&2, 0<
		if (s[i] == '1' || s[i] == '2' || s[i] == '0') && i+1 < len(s) && (s[i+1] == '>' || s[i+1] == '<') {
			if s[i+1] == '>' {
				if i+3 < len(s) && s[i+2] == '&' && (s[i+3] >= '0' && s[i+3] <= '9') {
					tokens = append(tokens, shellToken{kind: tokRedirDup, val: s[i : i+4]})
					i += 4
					continue
				}
				if i+2 < len(s) && s[i+2] == '>' {
					tokens = append(tokens, shellToken{kind: tokRedirWrite, val: s[i : i+3]})
					i += 3
				} else {
					tokens = append(tokens, shellToken{kind: tokRedirWrite, val: s[i : i+2]})
					i += 2
				}
				continue
			}
			tokens = append(tokens, shellToken{kind: tokRedirRead, val: s[i : i+2]})
			i += 2
			continue
		}

		if s[i] == '>' {
			if i+2 < len(s) && s[i+1] == '&' && (s[i+2] >= '0' && s[i+2] <= '9') {
				tokens = append(tokens, shellToken{kind: tokRedirDup, val: s[i : i+3]})
				i += 3
				continue
			}
			if i+1 < len(s) && s[i+1] == '>' {
				tokens = append(tokens, shellToken{kind: tokRedirWrite, val: ">>"})
				i += 2
			} else {
				tokens = append(tokens, shellToken{kind: tokRedirWrite, val: ">"})
				i++
			}
			continue
		}
		if s[i] == '<' {
			if i+1 < len(s) && s[i+1] == '<' {
				tokens = append(tokens, shellToken{kind: tokRedirRead, val: "<<"})
				i += 2
			} else {
				tokens = append(tokens, shellToken{kind: tokRedirRead, val: "<"})
				i++
			}
			continue
		}

		// Read word.
		var buf strings.Builder
		inSingle := false
		inDouble := false
		quoted := false
		for i < len(s) {
			ch := s[i]
			if !inSingle && !inDouble {
				if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' ||
					ch == ';' || ch == '|' || ch == '&' || ch == '>' || ch == '<' {
					break
				}
			}
			if ch == '\\' && !inSingle {
				if i+1 < len(s) {
					buf.WriteByte(s[i+1])
					i += 2
					continue
				}
			}
			if ch == '\'' && !inDouble {
				inSingle = !inSingle
				quoted = true
				i++
				continue
			}
			if ch == '"' && !inSingle {
				inDouble = !inDouble
				quoted = true
				i++
				continue
			}
			buf.WriteByte(ch)
			i++
		}
		if inSingle || inDouble {
			return nil, errors.New("unterminated quote in shell command")
		}
		tokens = append(tokens, shellToken{kind: tokWord, val: buf.String(), quoted: quoted})
	}
	return tokens, nil
}

func parseShellSegments(command string) ([]shellSegment, error) {
	tokens, err := tokenizeShell(command)
	if err != nil {
		return nil, err
	}
	var segments []shellSegment
	var cur shellSegment
	for i := 0; i < len(tokens); i++ {
		tk := tokens[i]
		switch tk.kind {
		case tokSep:
			if len(cur.words) > 0 || len(cur.redirs) > 0 {
				segments = append(segments, cur)
				cur = shellSegment{}
			}
		case tokRedirDup:
			// Safe fd duplication like 2>&1.
		case tokRedirWrite:
			if i+1 >= len(tokens) || tokens[i+1].kind != tokWord {
				return nil, fmt.Errorf("redirection %q missing target", tk.val)
			}
			cur.redirs = append(cur.redirs, shellRedir{op: tk.val, target: tokens[i+1].val})
			i++
		case tokRedirRead:
			if i+1 < len(tokens) && tokens[i+1].kind == tokWord {
				// Validate read redirection targets unless heredoc delimiter (<<).
				if tk.val != "<<" {
					cur.words = append(cur.words, tokens[i+1].val)
				}
				i++
			}
		case tokWord:
			cur.words = append(cur.words, tk.val)
		}
	}
	if len(cur.words) > 0 || len(cur.redirs) > 0 {
		segments = append(segments, cur)
	}
	return segments, nil
}

func validateShellSegment(role string, allowedSurfaces []string, repoRoot, realRoot string, seg shellSegment, depth int) error {
	// 1. Validate redirection write targets.
	for _, rd := range seg.redirs {
		if err := validateShellWriteTarget(role, allowedSurfaces, repoRoot, realRoot, rd.target); err != nil {
			return err
		}
	}

	if len(seg.words) == 0 {
		return nil
	}

	// 2. Skip leading VAR=VALUE assignments (while still validating path-like RHS values).
	idx := 0
	for idx < len(seg.words) && isEnvAssignment(seg.words[idx]) {
		if eq := strings.IndexByte(seg.words[idx], '='); eq >= 0 {
			varName := seg.words[idx][:eq]
			rhs := seg.words[idx][eq+1:]
			allowEnvTemp := varName == "TMPDIR" || varName == "TMP" || varName == "TEMP"
			if err := validateShellArgPath(repoRoot, realRoot, rhs, allowEnvTemp); err != nil {
				return err
			}
		}
		idx++
	}
	if idx >= len(seg.words) {
		return nil
	}

	// Unwrap `command` / `env` prefixes.
	for idx < len(seg.words) {
		base := filepath.Base(seg.words[idx])
		if base == "command" {
			idx++
			continue
		}
		if base == "env" {
			idx++
			for idx < len(seg.words) && (strings.HasPrefix(seg.words[idx], "-") || isEnvAssignment(seg.words[idx])) {
				if isEnvAssignment(seg.words[idx]) {
					if eq := strings.IndexByte(seg.words[idx], '='); eq >= 0 {
						varName := seg.words[idx][:eq]
						rhs := seg.words[idx][eq+1:]
						allowEnvTemp := varName == "TMPDIR" || varName == "TMP" || varName == "TEMP"
						if err := validateShellArgPath(repoRoot, realRoot, rhs, allowEnvTemp); err != nil {
							return err
						}
					}
				}
				idx++
			}
			continue
		}
		break
	}
	if idx >= len(seg.words) {
		return nil
	}

	cmdWord := seg.words[idx]
	cmdBase := filepath.Base(cmdWord)
	args := seg.words[idx+1:]

	if cmdBase == "eval" {
		return validateShellCommandDepth(role, allowedSurfaces, repoRoot, strings.Join(args, " "), depth+1)
	}

	// Nested shell invocation (`sh -c ...`, `bash -c ...`, `zsh -c ...`).
	if cmdBase == "sh" || cmdBase == "bash" || cmdBase == "zsh" || cmdBase == "dash" {
		for i := 0; i < len(args); i++ {
			if args[i] == "-c" && i+1 < len(args) {
				if err := validateShellCommandDepth(role, allowedSurfaces, repoRoot, args[i+1], depth+1); err != nil {
					return err
				}
			}
		}
	}

	// 3. Git subcommand & global-flag enforcement.
	if cmdBase == "git" {
		if err := validateGitInvocation(repoRoot, realRoot, args); err != nil {
			return err
		}
	}

	// 4. Write/mutation commands.
	if err := validateMutatingCommand(role, allowedSurfaces, repoRoot, realRoot, cmdBase, args); err != nil {
		return err
	}

	// 5. Inspect all arguments (and command word itself) for outside-root paths or sensitive files.
	if err := validateShellArgPath(repoRoot, realRoot, cmdWord, false); err != nil {
		return err
	}
	if cmdBase == "echo" || cmdBase == "printf" {
		return nil
	}
	for i, a := range args {
		if isPatternOrScriptArg(cmdBase, args, i) {
			continue
		}
		if err := validateShellArgPath(repoRoot, realRoot, a, false); err != nil {
			return err
		}
	}
	return nil
}

func isPatternOrScriptArg(cmdBase string, args []string, idx int) bool {
	switch cmdBase {
	case "sed", "grep", "egrep", "fgrep", "rg", "awk":
	default:
		return false
	}
	hasExprFlag := false
	firstNonFlag := -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-e" || a == "--expression" || a == "-f" || a == "--file" || a == "--regexp" {
			hasExprFlag = true
			if (a == "-e" || a == "--expression" || a == "--regexp") && i+1 == idx {
				return true
			}
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if firstNonFlag == -1 {
			firstNonFlag = i
		}
	}
	return !hasExprFlag && idx == firstNonFlag
}

func isEnvAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	name := w[:eq]
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

func validateGitInvocation(repoRoot, realRoot string, args []string) error {
	valueFlags := map[string]bool{
		"-C":             true,
		"-c":             true,
		"--git-dir":      true,
		"--work-tree":    true,
		"--namespace":    true,
		"--super-prefix": true,
		"--config-env":   true,
	}

	i := 0
	var subcmd string
	for i < len(args) {
		a := args[i]
		if a == "--" {
			i++
			if i < len(args) {
				subcmd = args[i]
				i++
			}
			break
		}
		if valueFlags[a] {
			if i+1 >= len(args) {
				return fmt.Errorf("git global flag %q missing value", a)
			}
			val := args[i+1]
			if a == "-C" || a == "--git-dir" || a == "--work-tree" {
				if err := checkPathContainment(repoRoot, realRoot, val, false); err != nil {
					return fmt.Errorf("blocked git %s target %q: %w", a, val, err)
				}
			}
			i += 2
			continue
		}
		if strings.HasPrefix(a, "-C") && len(a) > 2 {
			dir := a[2:]
			if err := checkPathContainment(repoRoot, realRoot, dir, false); err != nil {
				return fmt.Errorf("blocked git -C target %q: %w", dir, err)
			}
			i++
			continue
		}
		if strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree=") {
			dir := a[strings.IndexByte(a, '=')+1:]
			if err := checkPathContainment(repoRoot, realRoot, dir, false); err != nil {
				return fmt.Errorf("blocked git directory flag %q: %w", a, err)
			}
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			i++
			continue
		}
		subcmd = a
		i++
		break
	}

	rest := args[i:]
	switch strings.ToLower(subcmd) {
	case "commit", "push", "rebase", "restore":
		return fmt.Errorf("blocked git subcommand %q", subcmd)
	case "reset":
		for _, r := range rest {
			if r == "--hard" {
				return errors.New("blocked git reset --hard")
			}
		}
	case "clean":
		for _, r := range rest {
			if r == "--force" || (strings.HasPrefix(r, "-") && !strings.HasPrefix(r, "--") && strings.Contains(r, "f")) {
				return errors.New("blocked git clean -f")
			}
		}
	case "checkout":
		for _, r := range rest {
			if r == "--" {
				return errors.New("blocked git checkout --")
			}
		}
	}
	return nil
}

func validateMutatingCommand(role string, allowedSurfaces []string, repoRoot, realRoot, cmdBase string, args []string) error {
	switch cmdBase {
	case "tee":
		for _, a := range args {
			if strings.HasPrefix(a, "-") && a != "-" {
				continue
			}
			if err := validateShellWriteTarget(role, allowedSurfaces, repoRoot, realRoot, a); err != nil {
				return err
			}
		}
	case "touch", "rm", "rmdir", "mkdir", "truncate":
		if cmdBase == "rm" {
			hasRecursive := false
			hasForce := false
			for _, a := range args {
				if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
					if strings.ContainsAny(a, "rR") {
						hasRecursive = true
					}
					if strings.Contains(a, "f") {
						hasForce = true
					}
				} else if a == "--recursive" {
					hasRecursive = true
				} else if a == "--force" {
					hasForce = true
				}
			}
			for _, a := range args {
				if strings.HasPrefix(a, "-") {
					continue
				}
				if hasRecursive && hasForce && (a == "/" || a == "~" || a == "*" || a == "/*") {
					return fmt.Errorf("blocked destructive rm -rf %q", a)
				}
			}
		}
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			if err := validateShellWriteTarget(role, allowedSurfaces, repoRoot, realRoot, a); err != nil {
				return err
			}
		}
	case "chmod", "chown":
		// First non-flag arg is mode/owner; remaining non-flag args are targets.
		seenSpec := false
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			if !seenSpec {
				seenSpec = true
				continue
			}
			if err := validateShellWriteTarget(role, allowedSurfaces, repoRoot, realRoot, a); err != nil {
				return err
			}
		}
	case "sed":
		inPlace := false
		var targets []string
		sawScript := false
		for i := 0; i < len(args); i++ {
			a := args[i]
			if a == "--in-place" || strings.HasPrefix(a, "--in-place=") || a == "-i" || strings.HasPrefix(a, "-i") ||
				(strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "i")) {
				inPlace = true
				continue
			}
			if a == "-e" || a == "-f" || a == "--expression" || a == "--file" {
				sawScript = true
				i++
				continue
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			if !sawScript {
				sawScript = true
				continue
			}
			targets = append(targets, a)
		}
		if inPlace {
			for _, tgt := range targets {
				if err := validateShellWriteTarget(role, allowedSurfaces, repoRoot, realRoot, tgt); err != nil {
					return err
				}
			}
		}
	case "perl":
		inPlace := false
		var targets []string
		for i := 0; i < len(args); i++ {
			a := args[i]
			if a == "-e" || a == "-E" {
				i++
				continue
			}
			if strings.HasPrefix(a, "-") {
				if !strings.HasPrefix(a, "--") && strings.Contains(a, "i") {
					inPlace = true
				}
				continue
			}
			targets = append(targets, a)
		}
		if inPlace {
			for _, tgt := range targets {
				if err := validateShellWriteTarget(role, allowedSurfaces, repoRoot, realRoot, tgt); err != nil {
					return err
				}
			}
		}
	case "mv", "cp", "install", "ln":
		var targets []string
		explicitDest := ""
		for i := 0; i < len(args); i++ {
			a := args[i]
			if a == "-t" && i+1 < len(args) {
				explicitDest = args[i+1]
				i++
				continue
			}
			if strings.HasPrefix(a, "--target-directory=") {
				explicitDest = strings.TrimPrefix(a, "--target-directory=")
				continue
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			targets = append(targets, a)
		}
		if explicitDest != "" {
			if err := validateShellWriteTarget(role, allowedSurfaces, repoRoot, realRoot, explicitDest); err != nil {
				return err
			}
		}
		if cmdBase == "mv" {
			// mv mutates both source and destination.
			for _, tgt := range targets {
				if err := validateShellWriteTarget(role, allowedSurfaces, repoRoot, realRoot, tgt); err != nil {
					return err
				}
			}
		} else if len(targets) >= 2 {
			dest := targets[len(targets)-1]
			if err := validateShellWriteTarget(role, allowedSurfaces, repoRoot, realRoot, dest); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateShellWriteTarget(role string, allowedSurfaces []string, repoRoot, realRoot, target string) error {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return errors.New("empty shell write target")
	}
	if trimmed == "/dev/null" {
		return nil
	}
	if err := checkPathContainment(repoRoot, realRoot, trimmed, false); err != nil {
		return err
	}
	resolved, _, err := resolveShellTarget(repoRoot, realRoot, trimmed)
	if err != nil {
		return err
	}
	if isSensitiveResolvedPath(realRoot, resolved) {
		return fmt.Errorf("blocked shell write to sensitive path %q", target)
	}
	if !isWriterRole(role) {
		return fmt.Errorf("shell write/mutation to workspace path %q is not permitted for role %q", target, role)
	}
	rel, err := filepath.Rel(realRoot, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("shell write target %q escapes repository root", target)
	}
	relSlash := filepath.ToSlash(rel)
	if !matchesAnySurface(allowedSurfaces, relSlash) {
		return fmt.Errorf("shell write target %q is outside Allowed edit surfaces %v", relSlash, allowedSurfaces)
	}
	return nil
}

func validateShellArgPath(repoRoot, realRoot, tok string, allowTemp bool) error {
	if tok == "" {
		return nil
	}
	// Block home directory shortcuts and $HOME.
	if tok == "~" || strings.HasPrefix(tok, "~/") || strings.Contains(tok, "$HOME") || strings.Contains(tok, "${HOME}") {
		return fmt.Errorf("blocked shell reference outside repository: %q", tok)
	}
	if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
		return nil
	}
	if strings.HasPrefix(tok, "file://") {
		tok = strings.TrimPrefix(tok, "file://")
	}
	// Inspect flag=value values if the flag carries a path-like value.
	if strings.HasPrefix(tok, "-") {
		if eq := strings.IndexByte(tok, '='); eq >= 0 {
			val := tok[eq+1:]
			return validateShellArgPath(repoRoot, realRoot, val, allowTemp)
		}
		return nil
	}

	if !looksLikePath(repoRoot, tok) {
		if isSensitiveRelPath(tok) {
			return fmt.Errorf("blocked shell command referencing sensitive file %q", tok)
		}
		return nil
	}

	return checkPathContainment(repoRoot, realRoot, tok, allowTemp)
}

func looksLikePath(repoRoot, tok string) bool {
	if tok == "." || tok == ".." || tok == "/dev/null" {
		return true
	}
	if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") || strings.Contains(tok, "/") || strings.Contains(tok, "\\") {
		// Distinguish common Go import paths like github.com/a/b if not starting with /, ./, ../
		if !strings.HasPrefix(tok, "/") && !strings.HasPrefix(tok, "./") && !strings.HasPrefix(tok, "../") && !strings.Contains(tok, "..") {
			if _, err := os.Lstat(filepath.Join(repoRoot, filepath.FromSlash(tok))); err != nil && !isSensitiveRelPath(tok) {
				// If it has a domain-like first segment (e.g. github.com/...) and doesn't exist locally, treat as non-path.
				first := strings.SplitN(tok, "/", 2)[0]
				if strings.Contains(first, ".") && !strings.HasPrefix(first, ".") {
					return false
				}
			}
		}
		return true
	}
	if strings.Contains(tok, "..") {
		return true
	}
	if isSensitiveRelPath(tok) {
		return true
	}
	if _, err := os.Lstat(filepath.Join(repoRoot, tok)); err == nil {
		return true
	}
	return false
}

func checkPathContainment(repoRoot, realRoot, rawPath string, allowTemp bool) error {
	if rawPath == "/dev/null" {
		return nil
	}
	if rawPath == "~" || strings.HasPrefix(rawPath, "~/") {
		return fmt.Errorf("path %q escapes repository root", rawPath)
	}
	cleanInput := rawPath
	if cleanInput == "./..." {
		cleanInput = "."
	} else if strings.HasSuffix(cleanInput, "/...") {
		cleanInput = strings.TrimSuffix(cleanInput, "/...")
	}

	if isSensitiveRelPath(cleanInput) {
		return fmt.Errorf("blocked shell access to sensitive path %q", rawPath)
	}

	resolved, hasTraversal, err := resolveShellTarget(repoRoot, realRoot, cleanInput)
	if err != nil {
		return err
	}
	if isSensitiveResolvedPath(realRoot, resolved) {
		return fmt.Errorf("blocked shell access to sensitive path %q", rawPath)
	}
	if isWithinDir(realRoot, resolved) {
		return nil
	}
	if !hasTraversal && allowTemp && isAllowedTempPath(resolved) {
		return nil
	}
	return fmt.Errorf("blocked shell access outside repository root: %q", rawPath)
}

func resolveShellTarget(repoRoot, realRoot, rawPath string) (string, bool, error) {
	slash := filepath.ToSlash(strings.TrimSpace(rawPath))
	hasDotDot := false
	for _, seg := range strings.Split(slash, "/") {
		if seg == ".." {
			hasDotDot = true
			break
		}
	}

	var candidate string
	if filepath.IsAbs(rawPath) {
		candidate = filepath.Clean(rawPath)
	} else {
		candidate = filepath.Clean(filepath.Join(realRoot, filepath.FromSlash(slash)))
	}

	resolved, err := evalSymlinksBestEffort(candidate, 0)
	if err != nil {
		return "", hasDotDot, fmt.Errorf("resolve path %q: %w", rawPath, err)
	}
	// Traversal escaping realRoot is always flagged even if realRoot is inside os.TempDir().
	escapedByDotDot := hasDotDot && !isWithinDir(realRoot, resolved)
	return resolved, escapedByDotDot, nil
}

func evalSymlinksBestEffort(path string, depth int) (string, error) {
	if depth > 16 {
		return "", errors.New("too many symlinks")
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved), nil
	}

	// If path itself is a dangling symlink, follow its link target manually.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, rerr := os.Readlink(path)
		if rerr != nil {
			return "", rerr
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return evalSymlinksBestEffort(filepath.Clean(target), depth+1)
	}

	// Walk up to nearest existing ancestor.
	cur := filepath.Clean(path)
	var tail []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(path), nil
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}

	resolvedAnc, err := evalSymlinksBestEffort(cur, depth+1)
	if err != nil {
		return "", err
	}
	parts := append([]string{resolvedAnc}, tail...)
	return filepath.Clean(filepath.Join(parts...)), nil
}

func isWithinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func isAllowedTempPath(resolved string) bool {
	for _, base := range []string{"/tmp", "/var/tmp", os.TempDir()} {
		if strings.TrimSpace(base) == "" {
			continue
		}
		resolvedBase, err := evalSymlinksBestEffort(base, 0)
		if err != nil {
			resolvedBase = filepath.Clean(base)
		}
		if isWithinDir(resolvedBase, resolved) {
			return true
		}
	}
	return false
}

func isSensitiveResolvedPath(realRoot, resolved string) bool {
	if isWithinDir(realRoot, resolved) {
		if rel, err := filepath.Rel(realRoot, resolved); err == nil {
			return isSensitiveRelPath(filepath.ToSlash(rel))
		}
	}
	return isSensitiveRelPath(filepath.ToSlash(resolved))
}

func buildSystemPrompt(role string, surfaces []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the external %s agent for odd-agy.\n", role)
	b.WriteString("Use only the provided tools within the repository sandbox. ")
	b.WriteString("Never attempt to access secrets, .env, .ssh, or run git commit/git push.\n")
	if isReviewRole(role) {
		b.WriteString("Inspect frozen candidate snapshots exclusively via odd_review_scope.\n")
	}
	if len(surfaces) > 0 {
		fmt.Fprintf(&b, "Allowed edit surfaces: %s\n", strings.Join(surfaces, ", "))
	}
	return b.String()
}

func isWriterRole(role string) bool {
	return role == "worker" || role == "jd-fix-agent"
}

func isReviewRole(role string) bool {
	return strings.HasPrefix(role, "review-")
}

func isShellRole(role string) bool {
	return role == "verify" || role == "jd-judge-a" || role == "jd-judge-b" || isWriterRole(role)
}

func validateTaskID(taskID string) error {
	trimmed := strings.TrimSpace(taskID)
	if trimmed == "" || strings.Contains(trimmed, "..") || strings.ContainsAny(trimmed, "/\\") {
		return fmt.Errorf("invalid taskId %q", taskID)
	}
	return nil
}

func newTaskID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate task id: %w", err)
	}
	return "task-" + hex.EncodeToString(buf[:]), nil
}

func toolErrJSON(msg string) string {
	return mustJSON(map[string]string{"error": msg})
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"marshal failed"}`
	}
	return string(b)
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}
