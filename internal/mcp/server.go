package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/llm"
	"github.com/Nathanael-Huaman/odd-agy/internal/rdd"
	"github.com/Nathanael-Huaman/odd-agy/internal/todo"
	"github.com/Nathanael-Huaman/odd-agy/internal/version"
)

const (
	protocolVersion       = "2024-11-05"
	maxConcurrentRequests = 8

	defaultMCPWaitTimeout = 100 * time.Second
	maxMCPWaitTimeout     = 120 * time.Second

	errCodeParse          = -32700
	errCodeInvalidRequest = -32600
	errCodeMethodNotFound = -32601
	errCodeInvalidParams  = -32602

	rootCwdErrorMsg = `MCP server started in filesystem root "/"; pass workspaceRoot (absolute path to the active project directory) in the tool arguments.`
)

// ServerConfig configures the unified odd-agy stdio MCP server.
type ServerConfig struct {
	Workspace string
	LLMClient *llm.Client
	LookupEnv func(string) (string, bool)
}

type lensBatchEntry struct {
	lens     string
	role     string
	taskID   string
	done     <-chan struct{}
	startErr error
}

type inFlightLensBatch struct {
	revision     int
	targetLenses []string
	trackedSet   map[string]bool
	nativeSet    map[string]bool
	entries      []lensBatchEntry
	allDone      chan struct{}
}

type workspaceServices struct {
	root        string
	todos       *todo.Store
	rdd         *rdd.Engine
	runner      *llm.Runner
	batchMu     sync.Mutex
	lensBatches map[string]*inFlightLensBatch
}

// Server exposes todo, RDD (odd_review_*), and external runner (odd_external_*) tools
// over JSON-RPC 2.0 newline-delimited stdio.
type Server struct {
	cfg         ServerConfig
	defaultRoot string
	wsMu        sync.Mutex
	workspaces  map[string]*workspaceServices
	writeMu     sync.Mutex
	serveCtxMu  sync.RWMutex
	serveCtx    context.Context
	waitTimeout time.Duration
	bgWG        sync.WaitGroup
}

// SetWaitTimeout overrides the default bounded-wait window per MCP tool call (primarily for tests).
func (s *Server) SetWaitTimeout(d time.Duration) {
	s.serveCtxMu.Lock()
	defer s.serveCtxMu.Unlock()
	s.waitTimeout = d
}

func (s *Server) effectiveDefaultWaitTimeout() time.Duration {
	s.serveCtxMu.RLock()
	d := s.waitTimeout
	s.serveCtxMu.RUnlock()
	if d > 0 {
		return d
	}
	return defaultMCPWaitTimeout
}

func (s *Server) backgroundCtx() context.Context {
	s.serveCtxMu.RLock()
	ctx := s.serveCtx
	s.serveCtxMu.RUnlock()
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

func (s *Server) trackBackground(done <-chan struct{}) {
	if done == nil {
		return
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		<-done
	}()
}

type serverReviewHost struct {
	rdd *rdd.Engine
}

func (h *serverReviewHost) Scope(lineageID string, cursor int) (any, error) {
	return h.rdd.Scope(lineageID, cursor, 0)
}

func (h *serverReviewHost) Submit(lineageID, role string, findings []rdd.Finding, verdicts []rdd.RefuterVerdict) (any, error) {
	lens := strings.TrimPrefix(strings.TrimSpace(role), "review-")
	return h.rdd.Submit(rdd.SubmitRequest{
		LineageID: lineageID,
		Lens:      lens,
		Findings:  findings,
		Verdicts:  verdicts,
	})
}

// NewServer creates a Server rooted at cfg.Workspace.
func NewServer(cfg ServerConfig) *Server {
	ws := strings.TrimSpace(cfg.Workspace)
	if ws == "" {
		ws, _ = os.Getwd()
	}
	ws = strings.TrimSpace(ws)
	if ws != "" {
		if abs, err := filepath.Abs(ws); err == nil {
			ws = abs
		}
		ws = filepath.Clean(ws)
	}
	s := &Server{
		cfg:         cfg,
		defaultRoot: ws,
		workspaces:  make(map[string]*workspaceServices),
	}
	if ws != "" && !isFilesystemRoot(ws) {
		if canon, err := s.validateWorkspaceDir(ws); err == nil {
			s.defaultRoot = canon
		}
	}
	return s
}

func isFilesystemRoot(p string) bool {
	cleaned := filepath.Clean(p)
	return cleaned == "" || filepath.Dir(cleaned) == cleaned
}

func isCaseInsensitiveOS() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}

func workspaceCacheKey(canonicalRoot string) string {
	cleaned := filepath.Clean(canonicalRoot)
	if isCaseInsensitiveOS() {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

func isSameOrSubpath(target, root string) bool {
	target = filepath.Clean(target)
	root = filepath.Clean(root)
	if isCaseInsensitiveOS() {
		target = strings.ToLower(target)
		root = strings.ToLower(root)
	}
	if target == root {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func collectDirStats(path string) []os.FileInfo {
	var stats []os.FileInfo
	cur := filepath.Clean(path)
	for cur != "" && !isFilesystemRoot(cur) {
		if info, err := os.Stat(cur); err == nil {
			stats = append(stats, info)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return stats
}

func (s *Server) isSensitiveWorkspaceRoot(cleaned, resolved string) (string, bool) {
	roots := []string{"/etc", "/private/etc", "/System", "/Library"}
	var homes []string
	if h, err := os.UserHomeDir(); err == nil && strings.TrimSpace(h) != "" {
		homes = append(homes, filepath.Clean(h))
	}
	if s.cfg.LookupEnv != nil {
		if h, ok := s.cfg.LookupEnv("HOME"); ok && strings.TrimSpace(h) != "" {
			homes = append(homes, filepath.Clean(h))
		}
	}
	for _, h := range homes {
		roots = append(roots,
			filepath.Join(h, ".ssh"),
			filepath.Join(h, ".gemini"),
			filepath.Join(h, ".config", "odd-agy"),
		)
		if canonH, err := filepath.EvalSymlinks(h); err == nil && canonH != h {
			roots = append(roots,
				filepath.Join(canonH, ".ssh"),
				filepath.Join(canonH, ".gemini"),
				filepath.Join(canonH, ".config", "odd-agy"),
			)
		}
	}
	candidateStats := collectDirStats(resolved)
	for _, root := range roots {
		if isSameOrSubpath(cleaned, root) || isSameOrSubpath(resolved, root) {
			return root, true
		}
		if canonRoot, err := filepath.EvalSymlinks(root); err == nil && canonRoot != root {
			if isSameOrSubpath(cleaned, canonRoot) || isSameOrSubpath(resolved, canonRoot) {
				return root, true
			}
		}
		if rootInfo, err := os.Stat(root); err == nil {
			for _, candInfo := range candidateStats {
				if os.SameFile(candInfo, rootInfo) {
					return root, true
				}
			}
		}
	}
	return "", false
}

func (s *Server) validateWorkspaceDir(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", errors.New("invalid workspaceRoot: contains null byte")
	}
	cleaned := filepath.Clean(strings.TrimSpace(raw))
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("invalid workspaceRoot %q: must be an absolute path", raw)
	}
	if isFilesystemRoot(cleaned) {
		return "", fmt.Errorf("invalid workspaceRoot %q: filesystem root is not allowed; pass the absolute path to the active project directory", raw)
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", fmt.Errorf("invalid workspaceRoot %q: %w", raw, err)
	}
	resolved = filepath.Clean(resolved)
	if isFilesystemRoot(resolved) {
		return "", fmt.Errorf("invalid workspaceRoot %q: resolves to filesystem root", raw)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("invalid workspaceRoot %q: %w", raw, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("invalid workspaceRoot %q: path is not a directory", raw)
	}
	if blockedRoot, blocked := s.isSensitiveWorkspaceRoot(cleaned, resolved); blocked {
		return "", fmt.Errorf("invalid workspaceRoot %q: targets restricted directory %q", raw, blockedRoot)
	}
	return resolved, nil
}

func (s *Server) getOrCreateWorkspace(canonicalRoot string) *workspaceServices {
	key := workspaceCacheKey(canonicalRoot)
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if ws, ok := s.workspaces[key]; ok {
		return ws
	}
	eng := rdd.NewEngine(canonicalRoot)
	ws := &workspaceServices{
		root:        canonicalRoot,
		todos:       todo.NewStore(canonicalRoot),
		rdd:         eng,
		lensBatches: make(map[string]*inFlightLensBatch),
		runner: llm.NewRunner(llm.RunnerConfig{
			RepoRoot:   canonicalRoot,
			Client:     s.cfg.LLMClient,
			LookupEnv:  s.cfg.LookupEnv,
			ReviewHost: &serverReviewHost{rdd: eng},
		}),
	}
	s.workspaces[key] = ws
	return ws
}

func (s *Server) resolveWorkspace(requested string) (*workspaceServices, error) {
	req := strings.TrimSpace(requested)
	if req == "" {
		s.wsMu.Lock()
		eff := s.defaultRoot
		s.wsMu.Unlock()
		if eff == "" || isFilesystemRoot(eff) {
			return nil, errors.New(rootCwdErrorMsg)
		}
		canon, err := s.validateWorkspaceDir(eff)
		if err != nil {
			return nil, err
		}
		return s.getOrCreateWorkspace(canon), nil
	}
	canon, err := s.validateWorkspaceDir(req)
	if err != nil {
		return nil, err
	}
	return s.getOrCreateWorkspace(canon), nil
}

func parseWorkspaceCandidate(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Scheme, "file") {
			return "", false
		}
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			return "", false
		}
		if u.Path == "" {
			return "", false
		}
		return u.Path, true
	}
	return raw, true
}

// ServeStdio runs the MCP server on os.Stdin and os.Stdout until EOF or ctx cancellation.
func (s *Server) ServeStdio(ctx context.Context) error {
	return s.Serve(ctx, os.Stdin, os.Stdout)
}

// Serve runs the JSON-RPC 2.0 loop over r and w until r reaches EOF or ctx is canceled.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	serveCtx, cancelServe := context.WithCancel(ctx)
	s.serveCtxMu.Lock()
	s.serveCtx = serveCtx
	s.serveCtxMu.Unlock()
	defer func() {
		s.serveCtxMu.Lock()
		if s.serveCtx == serveCtx {
			s.serveCtx = nil
		}
		s.serveCtxMu.Unlock()
		cancelServe()
		s.bgWG.Wait()
	}()

	type readItem struct {
		line []byte
		err  error
	}

	reader := bufio.NewReader(r)
	lines := make(chan readItem)
	go func() {
		defer close(lines)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				trimmed := bytes.TrimSpace(line)
				if len(trimmed) > 0 {
					cp := append([]byte(nil), trimmed...)
					select {
					case lines <- readItem{line: cp}:
					case <-serveCtx.Done():
						return
					}
				}
			}
			if err != nil {
				select {
				case lines <- readItem{err: err}:
				case <-serveCtx.Done():
				}
				return
			}
		}
	}()

	sem := make(chan struct{}, maxConcurrentRequests)
	var wg sync.WaitGroup
	var writeErrMu sync.Mutex
	var firstWriteErr error

	recordWriteErr := func(err error) {
		if err == nil {
			return
		}
		writeErrMu.Lock()
		if firstWriteErr == nil {
			firstWriteErr = err
			cancelServe()
		}
		writeErrMu.Unlock()
	}

	getWriteErr := func() error {
		writeErrMu.Lock()
		defer writeErrMu.Unlock()
		return firstWriteErr
	}

	for {
		select {
		case <-serveCtx.Done():
			wg.Wait()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if wErr := getWriteErr(); wErr != nil {
				return wErr
			}
			return serveCtx.Err()

		case item, ok := <-lines:
			if !ok {
				wg.Wait()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return getWriteErr()
			}
			if item.err != nil {
				wg.Wait()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if wErr := getWriteErr(); wErr != nil {
					return wErr
				}
				if errors.Is(item.err, io.EOF) {
					return nil
				}
				return fmt.Errorf("read mcp input: %w", item.err)
			}

			if isConcurrentMethod(item.line) {
				select {
				case sem <- struct{}{}:
				case <-serveCtx.Done():
					wg.Wait()
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if wErr := getWriteErr(); wErr != nil {
						return wErr
					}
					return serveCtx.Err()
				}
				wg.Add(1)
				go func(raw []byte) {
					defer func() {
						<-sem
						wg.Done()
					}()
					if resp := s.handleRawMessage(serveCtx, raw); resp != nil {
						if writeErr := s.writeResponse(w, resp); writeErr != nil {
							recordWriteErr(writeErr)
						}
					}
				}(item.line)
				continue
			}

			if resp := s.handleRawMessage(serveCtx, item.line); resp != nil {
				if writeErr := s.writeResponse(w, resp); writeErr != nil {
					recordWriteErr(writeErr)
					wg.Wait()
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return getWriteErr()
				}
			}
		}
	}
}

func isConcurrentMethod(raw []byte) bool {
	var peek struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return false
	}
	trimmedID := bytes.TrimSpace(peek.ID)
	if len(trimmedID) == 0 || string(trimmedID) == "null" {
		return false
	}
	if strings.HasPrefix(peek.Method, "notifications/") {
		return false
	}
	switch peek.Method {
	case "", "initialize", "ping", "tools/list":
		return false
	default:
		return true
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
		return s.handleInitialize(req)

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
		if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
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

func (s *Server) handleInitialize(req rpcRequest) *rpcResponse {
	if len(bytes.TrimSpace(req.Params)) > 0 && string(bytes.TrimSpace(req.Params)) != "null" {
		var params struct {
			RootURI          string `json:"rootUri"`
			RootPath         string `json:"rootPath"`
			WorkspaceFolders []struct {
				URI string `json:"uri"`
			} `json:"workspaceFolders"`
		}
		if err := json.Unmarshal(req.Params, &params); err == nil {
			s.wsMu.Lock()
			shouldAdopt := s.defaultRoot == "" || isFilesystemRoot(s.defaultRoot)
			s.wsMu.Unlock()

			if shouldAdopt {
				var candidates []string
				if params.RootURI != "" {
					candidates = append(candidates, params.RootURI)
				}
				if params.RootPath != "" {
					candidates = append(candidates, params.RootPath)
				}
				if len(params.WorkspaceFolders) > 0 && params.WorkspaceFolders[0].URI != "" {
					candidates = append(candidates, params.WorkspaceFolders[0].URI)
				}
				for _, cand := range candidates {
					if path, ok := parseWorkspaceCandidate(cand); ok {
						if canon, err := s.validateWorkspaceDir(path); err == nil {
							s.wsMu.Lock()
							if s.defaultRoot == "" || isFilesystemRoot(s.defaultRoot) {
								s.defaultRoot = canon
							}
							s.wsMu.Unlock()
							break
						}
					}
				}
			}
		}
	}

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
}

func isKnownTool(name string) bool {
	switch name {
	case "todo",
		"odd_review_mode",
		"odd_review_assess",
		"odd_review_start",
		"odd_review_consent",
		"odd_review_scope",
		"odd_review_submit",
		"odd_review_run_lenses",
		"odd_review_apply_correction",
		"odd_review_status",
		"odd_review_ack",
		"odd_external_profiles",
		"odd_external_run",
		"odd_external_continue",
		"odd_external_status":
		return true
	default:
		return false
	}
}

func (s *Server) dispatchTool(ctx context.Context, name string, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	if !isKnownTool(name) {
		return callToolResult{}, &rpcError{
			Code:    errCodeInvalidParams,
			Message: fmt.Sprintf("unknown tool %q", name),
		}
	}

	var wsArgs struct {
		WorkspaceRoot string `json:"workspaceRoot"`
	}
	if err := json.Unmarshal(rawArgs, &wsArgs); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}

	ws, err := s.resolveWorkspace(wsArgs.WorkspaceRoot)
	if err != nil {
		return toolErr(err), nil
	}

	switch name {
	case "todo":
		return s.handleTodo(ws, rawArgs)
	case "odd_review_mode":
		return s.handleReviewMode(ws, rawArgs)
	case "odd_review_assess":
		return s.handleReviewAssess(ws, rawArgs)
	case "odd_review_start":
		return s.handleReviewStart(ws, rawArgs)
	case "odd_review_consent":
		return s.handleReviewConsent(ws, rawArgs)
	case "odd_review_scope":
		return s.handleReviewScope(ws, rawArgs)
	case "odd_review_submit":
		return s.handleReviewSubmit(ws, rawArgs)
	case "odd_review_run_lenses":
		return s.handleReviewRunLenses(ctx, ws, rawArgs)
	case "odd_review_apply_correction":
		return s.handleReviewApplyCorrection(ws, rawArgs)
	case "odd_review_status":
		return s.handleReviewStatus(ws, rawArgs)
	case "odd_review_ack":
		return s.handleReviewAck(ws, rawArgs)
	case "odd_external_profiles":
		return s.handleExternalProfiles(ws)
	case "odd_external_run":
		return s.handleExternalRun(ctx, ws, rawArgs)
	case "odd_external_continue":
		return s.handleExternalContinue(ctx, ws, rawArgs)
	case "odd_external_status":
		return s.handleExternalStatus(ctx, ws, rawArgs)
	default:
		return callToolResult{}, &rpcError{
			Code:    errCodeInvalidParams,
			Message: fmt.Sprintf("unknown tool %q", name),
		}
	}
}

func (s *Server) handleTodo(ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
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
		st, err := ws.todos.List()
		return toolJSONOrErr(st, err), nil
	case "write":
		st, err := ws.todos.Write(args.Tasks)
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
		st, err := ws.todos.Add(title, status, note)
		return toolJSONOrErr(st, err), nil
	case "update":
		st, err := ws.todos.Update(args.ID, args.Title, args.Status, args.Note)
		return toolJSONOrErr(st, err), nil
	case "clear":
		st, err := ws.todos.Clear()
		return toolJSONOrErr(st, err), nil
	default:
		return toolErr(fmt.Errorf("invalid todo action %q: must be write, add, update, list, or clear", args.Action)), nil
	}
}

func (s *Server) handleReviewMode(ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	if strings.TrimSpace(args.Action) == "" {
		args.Action = "status"
	}
	res, err := ws.rdd.Mode(args.Action)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewAssess(ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var req rdd.AssessRequest
	if err := json.Unmarshal(rawArgs, &req); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := ws.rdd.Assess(req)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewStart(ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var req rdd.StartRequest
	if err := json.Unmarshal(rawArgs, &req); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := ws.rdd.Start(req)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewConsent(ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID string `json:"lineageId"`
		Answer    string `json:"answer"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := ws.rdd.AnswerConsent(args.LineageID, args.Answer)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewScope(ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID string `json:"lineageId"`
		Cursor    int    `json:"cursor"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := ws.rdd.Scope(args.LineageID, args.Cursor, args.Limit)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewSubmit(ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
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
		if sev == "BLOCKER" {
			sev = rdd.SeveritySEVERE
		}
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

	res, err := ws.rdd.Submit(rdd.SubmitRequest{
		LineageID: args.LineageID,
		Lens:      lens,
		Findings:  findings,
		Verdicts:  verdicts,
	})
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewApplyCorrection(ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID string `json:"lineageId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := ws.rdd.ApplyCorrection(args.LineageID)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewStatus(ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID string `json:"lineageId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := ws.rdd.Status(args.LineageID)
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleReviewAck(ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID string `json:"lineageId"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	res, err := ws.rdd.Ack(args.LineageID)
	return toolJSONOrErr(res, err), nil
}

type lensExecResult struct {
	Lens               string `json:"lens"`
	Role               string `json:"role"`
	Provider           string `json:"provider,omitempty"`
	Model              string `json:"model,omitempty"`
	Status             string `json:"status"`
	SubmittedToLineage bool   `json:"submittedToLineage"`
	TaskID             string `json:"taskId,omitempty"`
	Error              string `json:"error,omitempty"`
	FallbackToNative   bool   `json:"fallbackToNative,omitempty"`
}

type reviewRunLensesResult struct {
	LineageID           string            `json:"lineageId"`
	Status              string            `json:"status,omitempty"`
	RunningLenses       []string          `json:"runningLenses,omitempty"`
	ExecutedLenses      []lensExecResult  `json:"executedLenses"`
	NativeLensesPending []string          `json:"nativeLensesPending"`
	NextAction          string            `json:"nextAction"`
	ReviewStatus        rdd.LineageStatus `json:"reviewStatus"`
}

func defaultMCPWaitSeconds() int {
	return int(defaultMCPWaitTimeout / time.Second)
}

func formatRunningTaskNextAction(taskID string, turns int) string {
	return fmt.Sprintf(
		"External task %s is still running in the background (turns: %d). Do NOT fall back to native while running. Call odd_external_status with taskId %q and waitSeconds: %d to wait for completion.",
		taskID, turns, taskID, defaultMCPWaitSeconds(),
	)
}

func formatRunningLensesNextAction(runningLenses []string, lineageID string) string {
	return fmt.Sprintf(
		"External review lenses ([%s]) are still running for lineage %s. Do NOT fall back to native while running. Call odd_review_run_lenses with lineageId %q (and waitSeconds: %d) to attach to the in-flight batch without duplicating reviews and wait for completion.",
		strings.Join(runningLenses, ", "),
		lineageID,
		lineageID,
		defaultMCPWaitSeconds(),
	)
}

func formatNativeLensesNextAction(nativePending, fallbackRoles []string, lineageID string) string {
	nextAction := fmt.Sprintf(
		"Launch all pending native review lenses ([%s]) together in a single parallel invoke_subagent call for lineage %s.",
		strings.Join(nativePending, ", "),
		lineageID,
	)
	if len(fallbackRoles) > 0 {
		nextAction += fmt.Sprintf(
			" Include fallback_to_native in the prompt for failed external-mcp role(s) ([%s]) so PreToolUse admits the invoke_subagent batch.",
			strings.Join(fallbackRoles, ", "),
		)
	}
	return nextAction
}

func resolveWaitTimeout(waitSeconds *int, defaultDuration time.Duration) (time.Duration, error) {
	if waitSeconds == nil {
		return defaultDuration, nil
	}
	if *waitSeconds < 0 {
		return 0, errors.New("waitSeconds must be >= 0")
	}
	d := time.Duration(*waitSeconds) * time.Second
	if d > maxMCPWaitTimeout {
		d = maxMCPWaitTimeout
	}
	return d, nil
}

func parseRequestedLenses(rawLenses []string) ([]string, error) {
	if len(rawLenses) == 0 {
		return nil, nil
	}
	requestedLenses := make([]string, 0, len(rawLenses))
	requestedSet := make(map[string]bool, len(rawLenses))
	for _, raw := range rawLenses {
		norm := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "review-")
		switch norm {
		case rdd.LensRisk, rdd.LensResilience, rdd.LensReadability, rdd.LensReliability:
		default:
			return nil, fmt.Errorf("invalid lens %q: must be one of risk, resilience, readability, reliability", raw)
		}
		if requestedSet[norm] {
			return nil, fmt.Errorf("duplicate lens %q", norm)
		}
		requestedSet[norm] = true
		requestedLenses = append(requestedLenses, norm)
	}
	return requestedLenses, nil
}

func resolveTargetLenses(st rdd.LineageStatus, requestedLenses []string) ([]string, error) {
	if st.Status != rdd.StatusCollectingLenses {
		return nil, fmt.Errorf("lineage %s is in status %q, expected %q", st.LineageID, st.Status, rdd.StatusCollectingLenses)
	}

	pendingLenses := make([]string, 0, len(st.Lenses))
	if len(st.PendingLenses) > 0 {
		pendingLenses = append(pendingLenses, st.PendingLenses...)
	} else {
		completed := make(map[string]bool, len(st.CompletedLenses))
		for _, c := range st.CompletedLenses {
			completed[c] = true
		}
		for _, l := range st.Lenses {
			if !completed[l] {
				pendingLenses = append(pendingLenses, l)
			}
		}
	}
	if len(pendingLenses) == 0 {
		return nil, fmt.Errorf("lineage %s has no pending lenses", st.LineageID)
	}

	if len(requestedLenses) == 0 {
		return append([]string(nil), pendingLenses...), nil
	}

	pendingSet := make(map[string]bool, len(pendingLenses))
	for _, l := range pendingLenses {
		pendingSet[l] = true
	}
	requestedSet := make(map[string]bool, len(requestedLenses))
	for _, norm := range requestedLenses {
		if !pendingSet[norm] {
			return nil, fmt.Errorf("lens %q is not pending for lineage %s", norm, st.LineageID)
		}
		requestedSet[norm] = true
	}

	targetLenses := make([]string, 0, len(requestedLenses))
	for _, l := range pendingLenses {
		if requestedSet[l] {
			targetLenses = append(targetLenses, l)
		}
	}
	return targetLenses, nil
}

func partitionTargetLenses(profs config.ProfilesConfig, targetLenses []string) (externalLenses []string, nativeSet map[string]bool) {
	pf, hasPF := profs.Profiles[profs.ActiveProfile]
	nativeSet = make(map[string]bool, len(targetLenses))
	for _, lens := range targetLenses {
		role := "review-" + lens
		isExternal := false
		if hasPF {
			if ra, ok := pf.Roles[role]; ok && ra.Runtime == config.RuntimeExternalMCP {
				isExternal = true
			}
		}
		if isExternal {
			externalLenses = append(externalLenses, lens)
		} else {
			nativeSet[lens] = true
		}
	}
	return externalLenses, nativeSet
}

func (s *Server) attachOrStartLensBatch(
	ws *workspaceServices,
	lineageID string,
	requestedLenses []string,
) (batch *inFlightLensBatch, st rdd.LineageStatus, nativeOnly *reviewRunLensesResult, err error) {
	ws.batchMu.Lock()
	defer ws.batchMu.Unlock()

	if ws.lensBatches == nil {
		ws.lensBatches = make(map[string]*inFlightLensBatch)
	}
	st, err = ws.rdd.Status(lineageID)
	if err != nil {
		return nil, rdd.LineageStatus{}, nil, err
	}

	batch = ws.lensBatches[lineageID]
	if batch != nil {
		if st.Revision == batch.revision {
			for _, rl := range requestedLenses {
				if !batch.trackedSet[rl] {
					return nil, rdd.LineageStatus{}, nil, fmt.Errorf(
						"in-flight lens batch for lineage %s is already running lenses %v; lens %q is not part of the active batch",
						lineageID, batch.targetLenses, rl,
					)
				}
			}
			return batch, st, nil, nil
		}
		delete(ws.lensBatches, lineageID)
	}

	targetLenses, err := resolveTargetLenses(st, requestedLenses)
	if err != nil {
		return nil, rdd.LineageStatus{}, nil, err
	}

	profs, err := config.LoadProfiles(ws.root)
	if err != nil {
		return nil, rdd.LineageStatus{}, nil, err
	}

	externalLenses, nativeSet := partitionTargetLenses(profs, targetLenses)
	if len(externalLenses) == 0 {
		res := buildNativeOnlyLensesResult(lineageID, st, targetLenses, nativeSet)
		return nil, st, &res, nil
	}

	entries := make([]lensBatchEntry, 0, len(externalLenses))
	bgCtx := s.backgroundCtx()
	for _, lens := range externalLenses {
		role := "review-" + lens
		rec, done, startErr := ws.runner.StartRun(bgCtx, llm.RunRequest{
			Role:      role,
			LineageID: lineageID,
			Task:      buildReviewLensPrompt(lineageID, lens, role),
		})
		if startErr != nil {
			entries = append(entries, lensBatchEntry{
				lens:     lens,
				role:     role,
				startErr: startErr,
			})
			continue
		}
		s.trackBackground(done)
		entries = append(entries, lensBatchEntry{
			lens:   lens,
			role:   role,
			taskID: rec.TaskID,
			done:   done,
		})
	}

	trackedSet := make(map[string]bool, len(targetLenses))
	for _, l := range targetLenses {
		trackedSet[l] = true
	}
	batch = &inFlightLensBatch{
		revision:     st.Revision,
		targetLenses: targetLenses,
		trackedSet:   trackedSet,
		nativeSet:    nativeSet,
		entries:      entries,
		allDone:      make(chan struct{}),
	}

	startedCount := 0
	for _, e := range batch.entries {
		if e.done != nil {
			startedCount++
		}
	}
	if startedCount == 0 {
		close(batch.allDone)
	} else {
		s.trackBackground(batch.allDone)
		go func(b *inFlightLensBatch) {
			for _, e := range b.entries {
				if e.done != nil {
					<-e.done
				}
			}
			close(b.allDone)
		}(batch)
		ws.lensBatches[lineageID] = batch
	}

	return batch, st, nil, nil
}

func releaseLensBatch(ws *workspaceServices, lineageID string, batch *inFlightLensBatch) {
	ws.batchMu.Lock()
	defer ws.batchMu.Unlock()
	if ws.lensBatches != nil && ws.lensBatches[lineageID] == batch {
		delete(ws.lensBatches, lineageID)
	}
}

func awaitLensBatch(ctx context.Context, allDone <-chan struct{}, waitDur time.Duration) (bool, error) {
	if waitDur <= 0 {
		select {
		case <-allDone:
			return true, nil
		default:
			return false, nil
		}
	}
	timer := time.NewTimer(waitDur)
	defer timer.Stop()
	select {
	case <-allDone:
		return true, nil
	case <-timer.C:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func collectLensExecResults(ws *workspaceServices, entries []lensBatchEntry, batchCompleted bool) ([]lensExecResult, []string) {
	executed := make([]lensExecResult, 0, len(entries))
	var runningLenses []string
	for _, e := range entries {
		if e.startErr != nil {
			executed = append(executed, lensExecResult{
				Lens:             e.lens,
				Role:             e.role,
				Status:           llm.TaskStatusFailed,
				Error:            e.startErr.Error(),
				FallbackToNative: true,
			})
			continue
		}
		rec, statusErr := ws.runner.Status(e.taskID)
		if statusErr != nil {
			if !batchCompleted {
				runningLenses = append(runningLenses, e.lens)
				executed = append(executed, lensExecResult{
					Lens:   e.lens,
					Role:   e.role,
					Status: llm.TaskStatusRunning,
					TaskID: e.taskID,
				})
			} else {
				executed = append(executed, lensExecResult{
					Lens:             e.lens,
					Role:             e.role,
					Status:           llm.TaskStatusFailed,
					TaskID:           e.taskID,
					Error:            statusErr.Error(),
					FallbackToNative: true,
				})
			}
			continue
		}
		res := lensExecResult{
			Lens:               e.lens,
			Role:               e.role,
			Provider:           rec.Provider,
			Model:              rec.Model,
			Status:             rec.Status,
			SubmittedToLineage: rec.SubmittedToLineage,
			TaskID:             e.taskID,
			Error:              rec.Error,
		}
		if !batchCompleted && rec.Status == llm.TaskStatusRunning {
			runningLenses = append(runningLenses, e.lens)
		} else if !rec.SubmittedToLineage {
			res.FallbackToNative = true
			if res.Error == "" && rec.Status != llm.TaskStatusFallbackNative {
				res.Error = "external lens did not submit findings to lineage"
			}
		}
		executed = append(executed, res)
	}
	return executed, runningLenses
}

func buildNativeOnlyLensesResult(lineageID string, st rdd.LineageStatus, targetLenses []string, nativeSet map[string]bool) reviewRunLensesResult {
	nativePending := make([]string, 0, len(targetLenses))
	for _, lens := range targetLenses {
		if nativeSet[lens] {
			nativePending = append(nativePending, "review-"+lens)
		}
	}
	nextAction := st.NextAction
	if len(nativePending) > 0 {
		nextAction = formatNativeLensesNextAction(nativePending, nil, lineageID)
	}
	return reviewRunLensesResult{
		LineageID:           lineageID,
		Status:              llm.TaskStatusCompleted,
		ExecutedLenses:      []lensExecResult{},
		NativeLensesPending: nativePending,
		NextAction:          nextAction,
		ReviewStatus:        st,
	}
}

func buildRunningLensesResult(ws *workspaceServices, lineageID string, fallbackStatus rdd.LineageStatus, runningLenses []string, executed []lensExecResult) reviewRunLensesResult {
	postStatus, postErr := ws.rdd.Status(lineageID)
	if postErr != nil {
		postStatus = fallbackStatus
	}
	return reviewRunLensesResult{
		LineageID:           lineageID,
		Status:              llm.TaskStatusRunning,
		RunningLenses:       runningLenses,
		ExecutedLenses:      executed,
		NativeLensesPending: []string{},
		NextAction:          formatRunningLensesNextAction(runningLenses, lineageID),
		ReviewStatus:        postStatus,
	}
}

func buildCompletedLensesResult(ws *workspaceServices, lineageID string, fallbackStatus rdd.LineageStatus, batch *inFlightLensBatch, executed []lensExecResult) reviewRunLensesResult {
	postStatus, err := ws.rdd.Status(lineageID)
	if err != nil {
		postStatus = fallbackStatus
	}

	postPendingSet := make(map[string]bool, len(postStatus.PendingLenses))
	for _, l := range postStatus.PendingLenses {
		postPendingSet[l] = true
	}

	fallbackSet := make(map[string]bool)
	for _, ex := range executed {
		if ex.FallbackToNative && !ex.SubmittedToLineage && postPendingSet[ex.Lens] {
			fallbackSet[ex.Lens] = true
		}
	}

	nativePending := make([]string, 0, len(batch.targetLenses))
	var fallbackRoles []string
	for _, lens := range batch.targetLenses {
		if !postPendingSet[lens] {
			continue
		}
		if batch.nativeSet[lens] || fallbackSet[lens] {
			role := "review-" + lens
			nativePending = append(nativePending, role)
			if fallbackSet[lens] {
				fallbackRoles = append(fallbackRoles, role)
			}
		}
	}

	nextAction := postStatus.NextAction
	if len(nativePending) > 0 {
		nextAction = formatNativeLensesNextAction(nativePending, fallbackRoles, lineageID)
	}

	return reviewRunLensesResult{
		LineageID:           lineageID,
		Status:              llm.TaskStatusCompleted,
		ExecutedLenses:      executed,
		NativeLensesPending: nativePending,
		NextAction:          nextAction,
		ReviewStatus:        postStatus,
	}
}

func (s *Server) handleReviewRunLenses(ctx context.Context, ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		LineageID   string   `json:"lineageId"`
		Lenses      []string `json:"lenses"`
		WaitSeconds *int     `json:"waitSeconds"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	waitDur, err := resolveWaitTimeout(args.WaitSeconds, s.effectiveDefaultWaitTimeout())
	if err != nil {
		return toolErr(err), nil
	}
	lineageID := strings.TrimSpace(args.LineageID)
	if lineageID == "" {
		return toolErr(errors.New("lineageId is required")), nil
	}
	requestedLenses, err := parseRequestedLenses(args.Lenses)
	if err != nil {
		return toolErr(err), nil
	}

	batch, st, nativeOnly, err := s.attachOrStartLensBatch(ws, lineageID, requestedLenses)
	if err != nil {
		return toolErr(err), nil
	}
	if nativeOnly != nil {
		return toolJSONOrErr(*nativeOnly, nil), nil
	}

	allDone, err := awaitLensBatch(ctx, batch.allDone, waitDur)
	if err != nil {
		return toolErr(err), nil
	}

	if !allDone {
		executed, runningLenses := collectLensExecResults(ws, batch.entries, false)
		if len(runningLenses) > 0 {
			return toolJSONOrErr(buildRunningLensesResult(ws, lineageID, st, runningLenses, executed), nil), nil
		}
		select {
		case <-batch.allDone:
		case <-ctx.Done():
			return toolErr(ctx.Err()), nil
		}
	}

	releaseLensBatch(ws, lineageID, batch)
	executed, _ := collectLensExecResults(ws, batch.entries, true)
	return toolJSONOrErr(buildCompletedLensesResult(ws, lineageID, st, batch, executed), nil), nil
}

func buildReviewLensPrompt(lineageID, lens, role string) string {
	return fmt.Sprintf(
		"You are %s evaluating the %s lens for frozen RDD review lineage %s.\n"+
			"1. Read the frozen candidate snapshot exclusively using read_review_scope (never inspect live worktree files).\n"+
			"2. Submit your structured findings directly using submit_review_findings (with severity BLOCKER, SEVERE, WARNING, or SUGGESTION; causality candidate, pre_existing, or unknown; location file:line; and concise claim).\n"+
			"3. After submitting, provide a brief summary.",
		role, lens, lineageID,
	)
}

func (s *Server) handleExternalProfiles(ws *workspaceServices) (callToolResult, *rpcError) {
	res, err := ws.runner.ProfilesSummary()
	return toolJSONOrErr(res, err), nil
}

func (s *Server) handleExternalRun(ctx context.Context, ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		Role            string   `json:"role"`
		Task            string   `json:"task"`
		LineageID       string   `json:"lineageId"`
		Provider        string   `json:"provider"`
		Model           string   `json:"model"`
		MaxTurns        int      `json:"maxTurns"`
		AllowedSurfaces []string `json:"allowedSurfaces"`
		WaitSeconds     *int     `json:"waitSeconds"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	waitDur, err := resolveWaitTimeout(args.WaitSeconds, s.effectiveDefaultWaitTimeout())
	if err != nil {
		return toolErr(err), nil
	}
	rec, done, err := ws.runner.StartRun(s.backgroundCtx(), llm.RunRequest{
		Role:            args.Role,
		Task:            args.Task,
		LineageID:       args.LineageID,
		Provider:        args.Provider,
		Model:           args.Model,
		MaxTurns:        args.MaxTurns,
		AllowedSurfaces: args.AllowedSurfaces,
	})
	if err != nil {
		return toolErr(err), nil
	}
	s.trackBackground(done)

	if waitDur > 0 {
		rec, err = ws.runner.Wait(ctx, rec.TaskID, waitDur)
		if err != nil {
			return toolErr(err), nil
		}
	}
	if rec.Status == llm.TaskStatusRunning {
		rec.NextAction = formatRunningTaskNextAction(rec.TaskID, rec.Turns)
	}
	return toolJSONOrErr(rec, nil), nil
}

func (s *Server) handleExternalContinue(ctx context.Context, ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		TaskID          string   `json:"taskId"`
		Prompt          string   `json:"prompt"`
		AllowedSurfaces []string `json:"allowedSurfaces"`
		MaxTurns        int      `json:"maxTurns"`
		WaitSeconds     *int     `json:"waitSeconds"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	waitDur, err := resolveWaitTimeout(args.WaitSeconds, s.effectiveDefaultWaitTimeout())
	if err != nil {
		return toolErr(err), nil
	}
	rec, done, err := ws.runner.StartContinue(s.backgroundCtx(), llm.ContinueRequest{
		TaskID:          args.TaskID,
		Prompt:          args.Prompt,
		AllowedSurfaces: args.AllowedSurfaces,
		MaxTurns:        args.MaxTurns,
	})
	if err != nil {
		return toolErr(err), nil
	}
	s.trackBackground(done)

	if waitDur > 0 {
		rec, err = ws.runner.Wait(ctx, rec.TaskID, waitDur)
		if err != nil {
			return toolErr(err), nil
		}
	}
	if rec.Status == llm.TaskStatusRunning {
		rec.NextAction = formatRunningTaskNextAction(rec.TaskID, rec.Turns)
	}
	return toolJSONOrErr(rec, nil), nil
}

func (s *Server) handleExternalStatus(ctx context.Context, ws *workspaceServices, rawArgs json.RawMessage) (callToolResult, *rpcError) {
	var args struct {
		TaskID      string `json:"taskId"`
		WaitSeconds *int   `json:"waitSeconds"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return toolErr(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	waitDur, err := resolveWaitTimeout(args.WaitSeconds, 0)
	if err != nil {
		return toolErr(err), nil
	}
	rec, err := ws.runner.Wait(ctx, args.TaskID, waitDur)
	if err != nil {
		return toolErr(err), nil
	}
	if rec.Status == llm.TaskStatusRunning {
		rec.NextAction = formatRunningTaskNextAction(rec.TaskID, rec.Turns)
	}
	return toolJSONOrErr(rec, nil), nil
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
	specs := []toolSpec{
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
					"lineageId": map[string]any{"type": "string", "description": "RDD review lineage ID"},
					"role":      map[string]any{"type": "string", "description": "Reviewer role or lens (risk, resilience, readability, reliability, refuter)"},
					"findings": map[string]any{
						"type":        "array",
						"description": "Structured findings for a 4R lens (empty array if none)",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":        map[string]any{"type": "string", "description": "Unique finding identifier"},
								"severity":  map[string]any{"type": "string", "enum": []string{"BLOCKER", "SEVERE", "WARNING", "SUGGESTION"}},
								"causality": map[string]any{"type": "string", "enum": []string{"candidate", "pre_existing", "unknown"}},
								"location":  map[string]any{"type": "string", "description": "file:line"},
								"claim":     map[string]any{"type": "string", "description": "Concise evidence-backed finding"},
							},
							"required": []string{"id", "severity", "causality", "location", "claim"},
						},
					},
					"refuterVerdicts": map[string]any{
						"type":        "array",
						"description": "Refuter verdicts for severe blockers when role is refuter",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"findingId": map[string]any{"type": "string"},
								"verdict":   map[string]any{"type": "string", "enum": []string{"confirmed", "refuted", "unknown"}},
								"rationale": map[string]any{"type": "string"},
							},
							"required": []string{"findingId", "verdict"},
						},
					},
				},
				"required": []string{"lineageId", "role"},
			},
		},
		{
			Name:        "odd_review_run_lenses",
			Description: "Run all pending external-mcp 4R review lenses in parallel for an RDD lineage with bounded-wait chunked polling (returns status=\"running\" when still active so callers can re-call odd_review_run_lenses to attach without duplicating reviews) and report any agy-native lenses to launch together.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"lineageId": map[string]any{
						"type":        "string",
						"description": "Active RDD review lineage ID (in collecting_lenses status, or re-polling an in-flight batch)",
					},
					"lenses": map[string]any{
						"type":        "array",
						"description": "Optional subset of pending 4R lenses; defaults to all currently pending lenses for lineageId",
						"items":       map[string]any{"type": "string"},
					},
					"waitSeconds": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"maximum":     int(maxMCPWaitTimeout / time.Second),
						"description": fmt.Sprintf("Optional bounded wait window in seconds (0-%d, default %ds) before returning status=\"running\" for chunked re-attachment", int(maxMCPWaitTimeout/time.Second), defaultMCPWaitSeconds()),
					},
				},
				"required": []string{"lineageId"},
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
			Description: "Run a sandboxed agentic task for a role on configured external OpenAI-compatible providers with bounded-wait chunked polling (returns status=\"running\" + taskId if still active so callers can poll with odd_external_status).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"role":            map[string]any{"type": "string"},
					"task":            map[string]any{"type": "string"},
					"lineageId":       map[string]any{"type": "string", "description": "Optional RDD review lineage ID (recommended for review-* roles to bind frozen-snapshot inspection and direct finding submission)"},
					"provider":        map[string]any{"type": "string"},
					"model":           map[string]any{"type": "string"},
					"maxTurns":        map[string]any{"type": "integer"},
					"allowedSurfaces": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"waitSeconds":     map[string]any{"type": "integer", "minimum": 0, "maximum": int(maxMCPWaitTimeout / time.Second), "description": fmt.Sprintf("Optional bounded wait window in seconds (0-%d, default %ds) before returning status=\"running\"", int(maxMCPWaitTimeout/time.Second), defaultMCPWaitSeconds())},
				},
				"required": []string{"role", "task"},
			},
		},
		{
			Name:        "odd_external_continue",
			Description: "Resume a persisted external task preserving full history and reasoning_content with bounded-wait chunked polling (returns status=\"running\" + taskId if still active so callers can poll with odd_external_status).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskId":          map[string]any{"type": "string"},
					"prompt":          map[string]any{"type": "string"},
					"allowedSurfaces": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"maxTurns":        map[string]any{"type": "integer"},
					"waitSeconds":     map[string]any{"type": "integer", "minimum": 0, "maximum": int(maxMCPWaitTimeout / time.Second), "description": fmt.Sprintf("Optional bounded wait window in seconds (0-%d, default %ds) before returning status=\"running\"", int(maxMCPWaitTimeout/time.Second), defaultMCPWaitSeconds())},
				},
				"required": []string{"taskId", "prompt"},
			},
		},
		{
			Name:        "odd_external_status",
			Description: "Inspect status and history metadata of a persisted external task, optionally blocking up to waitSeconds for completion.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskId":      map[string]any{"type": "string"},
					"waitSeconds": map[string]any{"type": "integer", "minimum": 0, "maximum": int(maxMCPWaitTimeout / time.Second), "description": fmt.Sprintf("Optional bounded wait window in seconds (0-%d, default 0 for instant status check)", int(maxMCPWaitTimeout/time.Second))},
				},
				"required": []string{"taskId"},
			},
		},
	}

	wsProp := map[string]any{
		"type":        "string",
		"description": "Optional absolute path to the active project directory (required when the MCP server starts in '/', e.g. in Antigravity Desktop App).",
	}
	for i := range specs {
		props, ok := specs[i].InputSchema["properties"].(map[string]any)
		if !ok || props == nil {
			props = map[string]any{}
			specs[i].InputSchema["properties"] = props
		}
		props["workspaceRoot"] = wsProp
	}
	return specs
}
