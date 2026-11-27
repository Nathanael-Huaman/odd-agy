package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/Nathanael-Huaman/odd-agy/internal/llm"
	"github.com/Nathanael-Huaman/odd-agy/internal/rdd"
	"github.com/Nathanael-Huaman/odd-agy/internal/todo"
	"github.com/Nathanael-Huaman/odd-agy/internal/version"
)

const (
	protocolVersion = "2024-11-05"

	errCodeParse          = -32700
	errCodeInvalidRequest = -32600
	errCodeMethodNotFound = -32601
	errCodeInvalidParams  = -32602
)

// ServerConfig configures the unified odd-agy stdio MCP server.
type ServerConfig struct {
	Workspace string
	LLMClient *llm.Client
	LookupEnv func(string) (string, bool)
}

// Server exposes todo, RDD (odd_review_*), and external runner (odd_external_*) tools
// over JSON-RPC 2.0 newline-delimited stdio.
type Server struct {
	workspace string
	todos     *todo.Store
	rdd       *rdd.Engine
	runner    *llm.Runner
	writeMu   sync.Mutex
}

// NewServer creates a Server rooted at cfg.Workspace.
func NewServer(cfg ServerConfig) *Server {
	ws := cfg.Workspace
	if strings.TrimSpace(ws) == "" {
		ws, _ = os.Getwd()
	}
	return &Server{
		workspace: ws,
		todos:     todo.NewStore(ws),
		rdd:       rdd.NewEngine(ws),
		runner: llm.NewRunner(llm.RunnerConfig{
			RepoRoot:  ws,
			Client:    cfg.LLMClient,
			LookupEnv: cfg.LookupEnv,
		}),
	}
}

// ServeStdio runs the MCP server on os.Stdin and os.Stdout until EOF or ctx cancellation.
func (s *Server) ServeStdio(ctx context.Context) error {
	return s.Serve(ctx, os.Stdin, os.Stdout)
}

// Serve runs the JSON-RPC 2.0 loop over r and w until r reaches EOF or ctx is canceled.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	reader := bufio.NewReader(r)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				if resp := s.handleRawMessage(ctx, trimmed); resp != nil {
					if writeErr := s.writeResponse(w, resp); writeErr != nil {
						return writeErr
					}
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read mcp input: %w", err)
		}
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callToolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

func (s *Server) handleRawMessage(ctx context.Context, raw []byte) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &rpcError{Code: errCodeParse, Message: "parse error: " + err.Error()},
		}
	}

	isNotification := len(bytes.TrimSpace(req.ID)) == 0 || string(bytes.TrimSpace(req.ID)) == "null"
	if req.JSONRPC != "2.0" || strings.TrimSpace(req.Method) == "" {
		if isNotification {
			return nil
		}
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: errCodeInvalidRequest, Message: "invalid JSON-RPC 2.0 request"},
		}
	}

	// Notifications emit no response.
	if isNotification || strings.HasPrefix(req.Method, "notifications/") {
		return nil
	}

	switch req.Method {
	case "initialize":
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "odd-agy",
					"version": version.Version,
				},
			},
		}

	case "ping":
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}

	case "tools/list":
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": s.toolDefinitions(),
			},
		}

	case "tools/call":
		var params callToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: errCodeInvalidParams, Message: "invalid tools/call params: " + err.Error()},
			}
		}
		if strings.TrimSpace(params.Name) == "" {
			return &rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: errCodeInvalidParams, Message: "tool name must not be empty"},
			}
		}
		args := params.Arguments
		if len(bytes.TrimSpace(args)) == 0 {
			args = json.RawMessage("{}")
		}
		res, rpcErr := s.dispatchTool(ctx, params.Name, args)
		if rpcErr != nil {
			return &rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   rpcErr,
			}
		}
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  res,
		}

	default:
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: errCodeMethodNotFound, Message: fmt.Sprintf("method %q not found", req.Method)},
		}
	}
}

func (s *Server) writeResponse(w io.Writer, resp *rpcResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal mcp response: %w", err)
	}
	data = append(data, '\n')

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write mcp response: %w", err)
	}
	return nil
}

func (s *Server) dispatchTool(ctx context.Context, name string, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	switch name {
	case "todo":
		return s.handleTodo(rawArgs)
	case "odd_review_mode":
		return s.handleReviewMode(rawArgs)
	case "odd_review_assess":
		return s.handleReviewAssess(rawArgs)
	case "odd_review_start":
		return s.handleReviewStart(rawArgs)
	case "odd_review_consent":
		return s.handleReviewConsent(rawArgs)
	case "odd_review_scope":
		return s.handleReviewScope(rawArgs)
	case "odd_review_submit":
		return s.handleReviewSubmit(rawArgs)
	case "odd_review_apply_correction":
		return s.handleReviewApplyCorrection(rawArgs)
	case "odd_review_status":
		return s.handleReviewStatus(rawArgs)
	case "odd_review_ack":
		return s.handleReviewAck(rawArgs)
	case "odd_external_profiles":
		return s.handleExternalProfiles()
	case "odd_external_run":
		return s.handleExternalRun(ctx, rawArgs)
	case "odd_external_continue":
		return s.handleExternalContinue(ctx, rawArgs)
	case "odd_external_status":
		return s.handleExternalStatus(rawArgs)
	default:
		return callToolResult{}, &rpcError{
			Code:    errCodeInvalidParams,
			Message: fmt.Sprintf("unknown tool %q", name),
		}
	}
}

func (s *Server) handleTodo(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		Action string           `json:"action"`
		ID     int              `json:"id"`
		Title  *string          `json:"title"`
		Status *string          `json:"status"`
		Note   *string          `json:"note"`
		Tasks  []todo.TaskInput `json:"tasks"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid todo arguments: %w", err)), nil
	}

	switch args.Action {
	case "list":
		st, err := s.todos.List()
		return toolJSONOrErr(st, err), nil
	case "write":
		st, err := s.todos.Write(args.Tasks)
		return toolJSONOrErr(st, err), nil
	case "add":
		var title, status, note string
		if args.Title != nil {
			title = *args.Title
		}
		if args.Status != nil {
			status = *args.Status
		} else {
			status = todo.StatusPending
		}
		if args.Note != nil {
			note = *args.Note
		}
		st, err := s.todos.Add(title, status, note)
		return toolJSONOrErr(st, err), nil
	case "update":
		st, err := s.todos.Update(args.ID, args.Title, args.Status, args.Note)
		return toolJSONOrErr(st, err), nil
	case "clear":
		st, err := s.todos.Clear()
		return toolJSONOrErr(st, err), nil
	default:
		return toolErr(fmt.Errorf("invalid todo action %q: must be write, add, update, list, or clear", args.Action)), nil
	}
}

func (s *Server) handleReviewMode(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	if strings.TrimSpace(args.Action) == "" {
		args.Action = "status"
	}
	res, err := s.rdd.Mode(args.Action)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewAssess(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var req rdd.AssessRequest
	if err := json.Unmarshal(rawArgs, &req); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := s.rdd.Assess(req)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewStart(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var req rdd.StartRequest
	if err := json.Unmarshal(rawArgs, &req); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := s.rdd.Start(req)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewConsent(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID string `json:"lineageId"`
		Answer    string `json:"answer"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := s.rdd.AnswerConsent(args.LineageID, args.Answer)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewScope(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID string `json:"lineageId"`
		Cursor    int    `json:"cursor"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := s.rdd.Scope(args.LineageID, args.Cursor, args.Limit)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewSubmit(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	type findingInput struct {
		ID        string `json:"id"`
		Severity  string `json:"severity"`
		Causality string `json:"causality"`
		Origin    string `json:"origin"`
		Location  string `json:"location"`
		Claim     string `json:"claim"`
		Lens      string `json:"lens"`
	}
	type verdictInput struct {
		FindingID string `json:"findingId"`
		Verdict   string `json:"verdict"`
		Rationale string `json:"rationale"`
	}
	var args struct {
		LineageID       string         `json:"lineageId"`
		Role            string         `json:"role"`
		Lens            string         `json:"lens"`
		Findings        []findingInput `json:"findings"`
		RefuterVerdicts []verdictInput `json:"refuterVerdicts"`
		Verdicts        []verdictInput `json:"verdicts"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}

	lens := strings.TrimSpace(args.Role)
	if lens == "" {
		lens = strings.TrimSpace(args.Lens)
	}
	lens = strings.TrimPrefix(lens, "review-")

	findings := make([]rdd.Finding, 0, len(args.Findings))
	for _, f := range args.Findings {
		causality := f.Causality
		if causality == "" && f.Origin != "" {
			causality = f.Origin
		}
		if strings.EqualFold(causality, "pre-existing") {
			causality = rdd.CausalityPreExisting
		} else {
			causality = strings.ToLower(causality)
		}

		sev := strings.ToUpper(strings.TrimSpace(f.Severity))
		findings = append(findings, rdd.Finding{
			ID:        f.ID,
			Severity:  sev,
			Causality: causality,
			Location:  f.Location,
			Claim:     f.Claim,
			Lens:      f.Lens,
		})
	}

	rawVerdicts := args.RefuterVerdicts
	if len(rawVerdicts) == 0 {
		rawVerdicts = args.Verdicts
	}
	verdicts := make([]rdd.RefuterVerdict, 0, len(rawVerdicts))
	for _, v := range rawVerdicts {
		verdict := strings.ToLower(strings.TrimSpace(v.Verdict))
		switch verdict {
		case "uphold", "upheld":
			verdict = rdd.VerdictConfirmed
		case "refute":
			verdict = rdd.VerdictRefuted
		}
		verdicts = append(verdicts, rdd.RefuterVerdict{
			FindingID: v.FindingID,
			Verdict:   verdict,
			Rationale: v.Rationale,
		})
	}

	res, err := s.rdd.Submit(rdd.SubmitRequest{
		LineageID: args.LineageID,
		Lens:      lens,
		Findings:  findings,
		Verdicts:  verdicts,
	})
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewApplyCorrection(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID string `json:"lineageId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := s.rdd.ApplyCorrection(args.LineageID)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewStatus(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID string `json:"lineageId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := s.rdd.Status(args.LineageID)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewAck(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID string `json:"lineageId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := s.rdd.Ack(args.LineageID)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleExternalProfiles() (callToolResult, *rpcError) {
	res, err := s.runner.ProfilesSummary()
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleExternalRun(ctx context.Context, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		Role            string   `json:"role"`
		Task            string   `json:"task"`
		Provider        string   `json:"provider"`
		Model           string   `json:"model"`
		MaxTurns        int      `json:"maxTurns"`
		AllowedSurfaces []string `json:"allowedSurfaces"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := s.runner.Run(ctx, llm.RunRequest{
		Role:            args.Role,
		Task:            args.Task,
		Provider:        args.Provider,
		Model:           args.Model,
		MaxTurns:        args.MaxTurns,
		AllowedSurfaces: args.AllowedSurfaces,
	})
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleExternalContinue(ctx context.Context, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		TaskID          string   `json:"taskId"`
		Prompt          string   `json:"prompt"`
		AllowedSurfaces []string `json:"allowedSurfaces"`
		MaxTurns        int      `json:"maxTurns"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := s.runner.Continue(ctx, llm.ContinueRequest{
		TaskID:          args.TaskID,
		Prompt:          args.Prompt,
		AllowedSurfaces: args.AllowedSurfaces,
		MaxTurns:        args.MaxTurns,
	})
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleExternalStatus(rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		TaskID string `json:"taskId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := s.runner.Status(args.TaskID)
	return toolJSONOrErr(res, err), nil
}

func toolJSONOrErr(v any, err error) callToolResult {
	if err != nil {
		return toolErr(err)
	}
	b, merr := json.MarshalIndent(v, "", "  ")
	if merr != nil {
		return toolErr(merr)
	}
	return callToolResult{
		Content: []textContent{{Type: "text", Text: string(b)}},
	}
}

func toolErr(err error) callToolResult {
	return callToolResult{
		Content: []textContent{{Type: "text", Text: err.Error()}},
		IsError: true,
	}
}

func (s *Server) toolDefinitions() []toolSpec {
	return []toolSpec{
		{
			Name:        "todo",
			Description: "Manage project-scoped ODD todo items (write, add, update, list, clear).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "enum": []string{"write", "add", "update", "list", "clear"}},
					"id":     map[string]any{"type": "integer"},
					"title":  map[string]any{"type": "string"},
					"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "blocked", "done", "dropped"}},
					"note":   map[string]any{"type": "string"},
					"tasks": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":     map[string]any{"type": "integer"},
								"title":  map[string]any{"type": "string"},
								"status": map[string]any{"type": "string"},
								"note":   map[string]any{"type": "string"},
							},
							"required": []string{"title", "status"},
						},
					},
				},
				"required": []string{"action"},
			},
		},
		{
			Name:        "odd_review_mode",
			Description: "Inspect or toggle project RDD mode (status, enable, disable).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "enum": []string{"status", "enable", "disable"}},
				},
				"required": []string{"action"},
			},
		},
		{
			Name:        "odd_review_assess",
			Description: "Assess candidate diff risk tier and review readiness without mutating RDD state.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"baseRef":       map[string]any{"type": "string"},
					"committedOnly": map[string]any{"type": "boolean"},
					"escalate": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"item":   map[string]any{"type": "integer", "minimum": 1, "maximum": 6},
							"reason": map[string]any{"type": "string"},
						},
						"required": []string{"item", "reason"},
					},
				},
			},
		},
		{
			Name:        "odd_review_start",
			Description: "Freeze candidate snapshot with SHA-256 manifest and request human consent.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"baseRef":            map[string]any{"type": "string"},
					"committedOnly":      map[string]any{"type": "boolean"},
					"lenses":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"lensesReason":       map[string]any{"type": "string"},
					"requestContextPath": map[string]any{"type": "string"},
				},
				"required": []string{"lenses", "lensesReason"},
			},
		},
		{
			Name:        "odd_review_consent",
			Description: "Record human consent (granted or declined) on a frozen RDD review lineage.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"lineageId": map[string]any{"type": "string"},
					"answer":    map[string]any{"type": "string", "enum": []string{"granted", "declined"}},
				},
				"required": []string{"lineageId", "answer"},
			},
		},
		{
			Name:        "odd_review_scope",
			Description: "Paginated, SHA-256-verified read-only view of a frozen review candidate.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"lineageId": map[string]any{"type": "string"},
					"cursor":    map[string]any{"type": "integer"},
					"limit":     map[string]any{"type": "integer"},
				},
				"required": []string{"lineageId"},
			},
		},
		{
			Name:        "odd_review_submit",
			Description: "Submit 4R lens findings or refuter verdicts for a review lineage.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"lineageId":       map[string]any{"type": "string"},
					"role":            map[string]any{"type": "string"},
					"findings":        map[string]any{"type": "array"},
					"refuterVerdicts": map[string]any{"type": "array"},
				},
				"required": []string{"lineageId", "role"},
			},
		},
		{
			Name:        "odd_review_apply_correction",
			Description: "Re-freeze corrected candidate as revision 2 within the 1-correction budget.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"lineageId": map[string]any{"type": "string"},
				},
				"required": []string{"lineageId"},
			},
		},
		{
			Name:        "odd_review_status",
			Description: "Return deterministic state of an RDD review lineage.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"lineageId": map[string]any{"type": "string"},
				},
				"required": []string{"lineageId"},
			},
		},
		{
			Name:        "odd_review_ack",
			Description: "Burn approval authority for an approved RDD review lineage.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"lineageId": map[string]any{"type": "string"},
				},
				"required": []string{"lineageId"},
			},
		},
		{
			Name:        "odd_external_profiles",
			Description: "List active profile, role routes, and providers with redacted URLs and env key presence.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "odd_external_run",
			Description: "Run a sandboxed agentic task for a role on configured external OpenAI-compatible providers.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"role":            map[string]any{"type": "string"},
					"task":            map[string]any{"type": "string"},
					"provider":        map[string]any{"type": "string"},
					"model":           map[string]any{"type": "string"},
					"maxTurns":        map[string]any{"type": "integer"},
					"allowedSurfaces": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"role", "task"},
			},
		},
		{
			Name:        "odd_external_continue",
			Description: "Resume a persisted external task preserving full history and reasoning_content.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskId":          map[string]any{"type": "string"},
					"prompt":          map[string]any{"type": "string"},
					"allowedSurfaces": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"maxTurns":        map[string]any{"type": "integer"},
				},
				"required": []string{"taskId", "prompt"},
			},
		},
		{
			Name:        "odd_external_status",
			Description: "Inspect status and history metadata of a persisted external task.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskId": map[string]any{"type": "string"},
				},
				"required": []string{"taskId"},
			},
		},
	}
}
