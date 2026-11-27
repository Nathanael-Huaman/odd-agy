package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
)

const (
	defaultHTTPTimeout  = 120 * time.Second
	defaultProbeTimeout = 5 * time.Second
	maxResponseBytes    = 8 << 20 // 8 MiB
	maxErrorBodyBytes   = 512
)

// ErrMissingAPIKey indicates an external provider requires an API key env var that is not set.
var ErrMissingAPIKey = errors.New("missing provider API key")

// FunctionDefinition describes a callable function tool in OpenAI format.
type FunctionDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// ToolDefinition describes a tool exposed to the chat completions API.
type ToolDefinition struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

// ToolCallFunction holds the invoked function name and JSON arguments string.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall represents a single model-initiated tool invocation.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// Message represents a chat message, preserving ReasoningContent for deepseek-thinking.
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
}

// ChatRequest configures a chat completions API call.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Tools       []ToolDefinition
	ToolChoice  any
	MaxTokens   int
	Temperature *float64
}

// Usage tracks token consumption reported by a provider.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Choice represents one completion choice returned by the API.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ChatResponse is the parsed OpenAI-compatible chat completion response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// APIError represents an HTTP error returned by an upstream provider.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

// Retryable reports whether this HTTP error warrants fallback to another provider.
func (e *APIError) Retryable() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// ProbeResult describes live connectivity to a configured provider without leaking secrets.
type ProbeResult struct {
	Provider      string `json:"provider"`
	Reachable     bool   `json:"reachable"`
	StatusCode    int    `json:"statusCode,omitempty"`
	LatencyMS     int64  `json:"latencyMs"`
	RedactedURL   string `json:"redactedUrl"`
	MissingAPIKey bool   `json:"missingApiKey,omitempty"`
	KeySource     string `json:"keySource,omitempty"`
	Error         string `json:"error,omitempty"`
}

// ResolvedCandidate represents one resolved provider/model entry in a role fallback chain.
type ResolvedCandidate struct {
	Target         config.RoleTarget
	ProviderConfig config.ProviderConfig
	Endpoint       config.ResolvedEndpoint
	Skipped        bool
	SkipReason     string
}

// NativeFallback is returned when role execution falls back to native Antigravity OAuth.
type NativeFallback struct {
	Role   string `json:"role"`
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// Client speaks OpenAI-compatible chat completions (including deepseek-thinking).
type Client struct {
	httpClient *http.Client
}

// NewClient creates a new OpenAI-compatible HTTP client.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{httpClient: httpClient}
}

type wireMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
}

type wireChatRequest struct {
	Model       string           `json:"model"`
	Messages    []wireMessage    `json:"messages"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	ToolChoice  any              `json:"tool_choice,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	Stream      bool             `json:"stream"`
}

// ChatCompletions invokes POST <BaseURL>/chat/completions with compatibility handling.
func (c *Client) ChatCompletions(ctx context.Context, ep config.ResolvedEndpoint, req ChatRequest) (ChatResponse, error) {
	if ep.MissingAPIKey {
		return ChatResponse{}, fmt.Errorf("%w for %s", ErrMissingAPIKey, RedactURL(ep.BaseURL))
	}

	isDeepSeekThinking := ep.Compatibility == config.CompatibilityDeepSeekThinking
	hasTools := len(req.Tools) > 0

	// Deterministic tool ordering keeps system + tools byte-stable for KV caching.
	var sortedTools []ToolDefinition
	if hasTools {
		sortedTools = append([]ToolDefinition(nil), req.Tools...)
		sort.SliceStable(sortedTools, func(i, j int) bool {
			return sortedTools[i].Function.Name < sortedTools[j].Function.Name
		})
	}

	wireMsgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		wm := wireMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if isDeepSeekThinking && hasTools && m.Role == "assistant" {
			rc := m.ReasoningContent
			wm.ReasoningContent = &rc
		} else if m.ReasoningContent != "" {
			rc := m.ReasoningContent
			wm.ReasoningContent = &rc
		}
		wireMsgs = append(wireMsgs, wm)
	}

	toolChoice := req.ToolChoice
	if isDeepSeekThinking {
		if s, ok := toolChoice.(string); ok && strings.EqualFold(strings.TrimSpace(s), "required") {
			toolChoice = "auto"
		}
	}

	payload := wireChatRequest{
		Model:       req.Model,
		Messages:    wireMsgs,
		Tools:       sortedTools,
		ToolChoice:  toolChoice,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      false,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal chat request: %w", err)
	}

	endpointURL := strings.TrimRight(ep.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("build request for %s: %s", RedactURL(endpointURL), redactError(err, ep.APIKey))
	}
	for k, v := range ep.Headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if ep.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+ep.APIKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("request %s failed: %s", RedactURL(endpointURL), redactError(err, ep.APIKey))
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read response from %s: %s", RedactURL(endpointURL), redactError(err, ep.APIKey))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(respBytes))
		if len(msg) > maxErrorBodyBytes {
			msg = msg[:maxErrorBodyBytes] + "..."
		}
		return ChatResponse{}, &APIError{
			StatusCode: resp.StatusCode,
			Message:    redactText(msg, ep.APIKey),
		}
	}

	return decodeChatResponse(respBytes, resp.Header.Get("Content-Type"), endpointURL, ep.APIKey)
}

type sseToolCallDelta struct {
	Index    *int             `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type sseDelta struct {
	Role             string             `json:"role"`
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content"`
	ToolCalls        []sseToolCallDelta `json:"tool_calls"`
}

type sseChoice struct {
	Index        int       `json:"index"`
	Delta        *sseDelta `json:"delta"`
	Message      *Message  `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

type sseChunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices []sseChoice     `json:"choices"`
	Usage   *Usage          `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

func isSSEResponse(respBytes []byte, contentType string) bool {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return true
	}
	trimmed := bytes.TrimSpace(respBytes)
	return bytes.HasPrefix(trimmed, []byte("data:")) ||
		bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte(":"))
}

func decodeChatResponse(respBytes []byte, contentType string, endpointURL string, apiKey string) (ChatResponse, error) {
	if !isSSEResponse(respBytes, contentType) {
		var parsed ChatResponse
		if err := json.Unmarshal(respBytes, &parsed); err != nil {
			return ChatResponse{}, fmt.Errorf("decode response from %s: %s", RedactURL(endpointURL), redactError(err, apiKey))
		}
		if len(parsed.Choices) == 0 {
			return ChatResponse{}, fmt.Errorf("empty choices returned by %s", RedactURL(endpointURL))
		}
		return parsed, nil
	}

	type choiceAcc struct {
		choice    Choice
		toolCalls map[int]*ToolCall
	}

	var resp ChatResponse
	choicesByIdx := make(map[int]*choiceAcc)
	var dataLines []string
	stopped := false

	flushEvent := func() (bool, error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]

		trimmed := strings.TrimSpace(payload)
		if trimmed == "" {
			return false, nil
		}
		if trimmed == "[DONE]" {
			return true, nil
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
			return false, fmt.Errorf("decode response from %s: %s", RedactURL(endpointURL), redactError(err, apiKey))
		}

		errBytes := bytes.TrimSpace(chunk.Error)
		if len(errBytes) > 0 && string(errBytes) != "null" {
			msg := string(errBytes)
			if len(msg) > maxErrorBodyBytes {
				msg = msg[:maxErrorBodyBytes] + "..."
			}
			return false, &APIError{
				StatusCode: http.StatusBadGateway,
				Message:    redactText(msg, apiKey),
			}
		}

		if chunk.ID != "" {
			resp.ID = chunk.ID
		}
		if chunk.Model != "" {
			resp.Model = chunk.Model
		}
		if chunk.Usage != nil {
			resp.Usage = *chunk.Usage
		}

		for _, sc := range chunk.Choices {
			acc, exists := choicesByIdx[sc.Index]
			if !exists {
				acc = &choiceAcc{
					choice:    Choice{Index: sc.Index},
					toolCalls: make(map[int]*ToolCall),
				}
				choicesByIdx[sc.Index] = acc
			}
			if sc.FinishReason != "" {
				acc.choice.FinishReason = sc.FinishReason
			}
			if sc.Delta != nil {
				d := sc.Delta
				if d.Role != "" {
					acc.choice.Message.Role = d.Role
				}
				if d.Content != "" {
					acc.choice.Message.Content += d.Content
				}
				if d.ReasoningContent != "" {
					acc.choice.Message.ReasoningContent += d.ReasoningContent
				}
				if acc.choice.Message.Role == "" && (d.Content != "" || d.ReasoningContent != "" || len(d.ToolCalls) > 0) {
					acc.choice.Message.Role = "assistant"
				}
				for i, tc := range d.ToolCalls {
					tcIdx := i
					if tc.Index != nil {
						tcIdx = *tc.Index
					}
					tca, ok := acc.toolCalls[tcIdx]
					if !ok {
						tca = &ToolCall{}
						acc.toolCalls[tcIdx] = tca
					}
					if tc.ID != "" {
						tca.ID = tc.ID
					}
					if tc.Type != "" {
						tca.Type = tc.Type
					}
					tca.Function.Name += tc.Function.Name
					tca.Function.Arguments += tc.Function.Arguments
				}
			}
			if sc.Message != nil {
				m := sc.Message
				if m.Role != "" {
					acc.choice.Message.Role = m.Role
				}
				if m.Content != "" {
					acc.choice.Message.Content = m.Content
				}
				if m.ReasoningContent != "" {
					acc.choice.Message.ReasoningContent = m.ReasoningContent
				}
				if len(m.ToolCalls) > 0 {
					acc.toolCalls = make(map[int]*ToolCall, len(m.ToolCalls))
					for i, tc := range m.ToolCalls {
						cp := tc
						if cp.Type == "" {
							cp.Type = "function"
						}
						acc.toolCalls[i] = &cp
					}
				}
				if acc.choice.Message.Role == "" && (m.Content != "" || m.ReasoningContent != "" || len(m.ToolCalls) > 0) {
					acc.choice.Message.Role = "assistant"
				}
			}
		}

		return false, nil
	}

	raw := strings.ReplaceAll(string(respBytes), "\r\n", "\n")
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			done, err := flushEvent()
			if err != nil {
				return ChatResponse{}, err
			}
			if done {
				stopped = true
				break
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			val := strings.TrimPrefix(line, "data:")
			val = strings.TrimPrefix(val, " ")
			dataLines = append(dataLines, val)
			continue
		}
		if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
	}
	if !stopped && len(dataLines) > 0 {
		if _, err := flushEvent(); err != nil {
			return ChatResponse{}, err
		}
	}

	if len(choicesByIdx) == 0 {
		return ChatResponse{}, fmt.Errorf("empty choices returned by %s", RedactURL(endpointURL))
	}

	indices := make([]int, 0, len(choicesByIdx))
	for idx := range choicesByIdx {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	resp.Choices = make([]Choice, 0, len(indices))
	for _, idx := range indices {
		acc := choicesByIdx[idx]
		if len(acc.toolCalls) > 0 {
			tcIndices := make([]int, 0, len(acc.toolCalls))
			for tcIdx := range acc.toolCalls {
				tcIndices = append(tcIndices, tcIdx)
			}
			sort.Ints(tcIndices)
			acc.choice.Message.ToolCalls = make([]ToolCall, 0, len(tcIndices))
			for _, tcIdx := range tcIndices {
				tc := *acc.toolCalls[tcIdx]
				if tc.Type == "" {
					tc.Type = "function"
				}
				acc.choice.Message.ToolCalls = append(acc.choice.Message.ToolCalls, tc)
			}
		}
		resp.Choices = append(resp.Choices, acc.choice)
	}

	return resp, nil
}

// ProbeProvider checks live reachability of a provider using <baseUrl>/models
// (falling back to a lightweight check on 404/405) with credential/query redaction.
func ProbeProvider(
	ctx context.Context,
	providerName string,
	providerCfg config.ProviderConfig,
	lookupEnv func(string) (string, bool),
) ProbeResult {
	return NewClient(&http.Client{Timeout: defaultProbeTimeout}).ProbeProvider(ctx, providerName, providerCfg, lookupEnv)
}

// ProbeProvider checks live reachability of a provider using <baseUrl>/models
// (falling back to a lightweight check on 404/405) with credential/query redaction.
func (c *Client) ProbeProvider(
	ctx context.Context,
	providerName string,
	providerCfg config.ProviderConfig,
	lookupEnv func(string) (string, bool),
) ProbeResult {
	usedDefaultLookup := lookupEnv == nil
	if usedDefaultLookup {
		lookupEnv = config.NewSecretsLookupEnv("", os.LookupEnv)
	}
	ep, err := config.ResolveProviderEndpoint(providerCfg, lookupEnv)
	if err != nil {
		return ProbeResult{
			Provider:    providerName,
			Reachable:   false,
			RedactedURL: RedactURL(providerCfg.BaseURL),
			Error:       redactError(err),
		}
	}
	if usedDefaultLookup && ep.KeySource == "env" {
		if _, src := config.LookupSecret("", providerCfg.APIKeyEnv, os.Getenv); src == "stored" {
			ep.KeySource = "stored"
		}
	}

	redacted := RedactURL(ep.BaseURL)
	if ep.MissingAPIKey {
		return ProbeResult{
			Provider:      providerName,
			Reachable:     false,
			RedactedURL:   redacted,
			MissingAPIKey: true,
			KeySource:     ep.KeySource,
			Error:         fmt.Sprintf("missing required API key environment variable %q", providerCfg.APIKeyEnv),
		}
	}

	probeCtx := ctx
	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > defaultProbeTimeout {
		probeCtx, cancel = context.WithTimeout(ctx, defaultProbeTimeout)
		defer cancel()
	}

	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultProbeTimeout}
	}
	start := time.Now()
	modelsURL := strings.TrimRight(ep.BaseURL, "/") + "/models"

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return ProbeResult{
			Provider:    providerName,
			Reachable:   false,
			LatencyMS:   time.Since(start).Milliseconds(),
			RedactedURL: redacted,
			KeySource:   ep.KeySource,
			Error:       redactError(err, ep.APIKey),
		}
	}
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}
	if ep.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+ep.APIKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return ProbeResult{
			Provider:    providerName,
			Reachable:   false,
			LatencyMS:   time.Since(start).Milliseconds(),
			RedactedURL: redacted,
			KeySource:   ep.KeySource,
			Error:       redactError(err, ep.APIKey),
		}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ProbeResult{
			Provider:    providerName,
			Reachable:   true,
			StatusCode:  resp.StatusCode,
			LatencyMS:   time.Since(start).Milliseconds(),
			RedactedURL: redacted,
			KeySource:   ep.KeySource,
		}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ProbeResult{
			Provider:    providerName,
			Reachable:   false,
			StatusCode:  resp.StatusCode,
			LatencyMS:   time.Since(start).Milliseconds(),
			RedactedURL: redacted,
			KeySource:   ep.KeySource,
			Error:       fmt.Sprintf("authentication failed (HTTP %d)", resp.StatusCode),
		}
	}

	// Lightweight fallback check when /models is unimplemented (404/405).
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		fbURL := strings.TrimRight(ep.BaseURL, "/") + "/chat/completions"
		fbReq, fbErr := http.NewRequestWithContext(probeCtx, http.MethodPost, fbURL, bytes.NewReader([]byte("{}")))
		if fbErr == nil {
			for k, v := range ep.Headers {
				fbReq.Header.Set(k, v)
			}
			fbReq.Header.Set("Content-Type", "application/json")
			if ep.APIKey != "" {
				fbReq.Header.Set("Authorization", "Bearer "+ep.APIKey)
			}
			fbResp, doErr := httpClient.Do(fbReq)
			if doErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(fbResp.Body, 4096))
				_ = fbResp.Body.Close()
				reachable := fbResp.StatusCode >= 200 && fbResp.StatusCode < 500 &&
					fbResp.StatusCode != http.StatusUnauthorized && fbResp.StatusCode != http.StatusForbidden
				errMsg := ""
				if !reachable {
					errMsg = fmt.Sprintf("probe fallback returned HTTP %d", fbResp.StatusCode)
				}
				return ProbeResult{
					Provider:    providerName,
					Reachable:   reachable,
					StatusCode:  fbResp.StatusCode,
					LatencyMS:   time.Since(start).Milliseconds(),
					RedactedURL: redacted,
					KeySource:   ep.KeySource,
					Error:       errMsg,
				}
			}
		}
	}

	return ProbeResult{
		Provider:    providerName,
		Reachable:   false,
		StatusCode:  resp.StatusCode,
		LatencyMS:   time.Since(start).Milliseconds(),
		RedactedURL: redacted,
		KeySource:   ep.KeySource,
		Error:       fmt.Sprintf("probe returned HTTP %d", resp.StatusCode),
	}
}

// ResolveRoleChain resolves the ordered execution chain for a role under the active profile,
// applying optional provider/model overrides and returning any agy-native fallback target.
func ResolveRoleChain(
	role string,
	profiles config.ProfilesConfig,
	providerOverride string,
	modelOverride string,
	lookupEnv func(string) (string, bool),
) ([]ResolvedCandidate, *NativeFallback, error) {
	if lookupEnv == nil {
		lookupEnv = config.NewSecretsLookupEnv("", os.LookupEnv)
	}

	prof, ok := profiles.Profiles[profiles.ActiveProfile]
	if !ok {
		return nil, nil, fmt.Errorf("unknown activeProfile %q", profiles.ActiveProfile)
	}

	var targets []config.RoleTarget
	if strings.TrimSpace(providerOverride) != "" {
		provCfg, exists := profiles.Providers[providerOverride]
		if !exists {
			return nil, nil, fmt.Errorf("unknown provider %q", providerOverride)
		}
		model := strings.TrimSpace(modelOverride)
		if model == "" {
			if route, hasRole := prof.Roles[role]; hasRole && route.Provider == providerOverride && route.Model != "" {
				model = route.Model
			} else {
				model = provCfg.DefaultModel
			}
		}
		targets = []config.RoleTarget{{
			Runtime:  config.RuntimeExternalMCP,
			Provider: providerOverride,
			Model:    model,
		}}
	} else {
		route, exists := prof.Roles[role]
		if !exists {
			return nil, nil, fmt.Errorf("unknown role %q in profile %q", role, profiles.ActiveProfile)
		}
		primaryModel := route.Model
		if strings.TrimSpace(modelOverride) != "" {
			primaryModel = strings.TrimSpace(modelOverride)
		}
		targets = append(targets, config.RoleTarget{
			Runtime:  route.Runtime,
			Provider: route.Provider,
			Model:    primaryModel,
		})
		targets = append(targets, route.Fallback...)
	}

	candidates := make([]ResolvedCandidate, 0, len(targets))
	var nativeFB *NativeFallback

	for _, t := range targets {
		if t.Runtime == config.RuntimeAgyNative {
			nativeFB = &NativeFallback{
				Role:   role,
				Model:  t.Model,
				Reason: "role or fallback resolved to agy-native runtime; invoke native Antigravity subagent",
			}
			break
		}
		if t.Runtime != config.RuntimeExternalMCP {
			return nil, nil, fmt.Errorf("unsupported runtime %q for role %q", t.Runtime, role)
		}

		provCfg, exists := profiles.Providers[t.Provider]
		if !exists {
			candidates = append(candidates, ResolvedCandidate{
				Target:     t,
				Skipped:    true,
				SkipReason: fmt.Sprintf("unknown provider %q", t.Provider),
			})
			continue
		}

		ep, err := config.ResolveProviderEndpoint(provCfg, lookupEnv)
		if err != nil {
			candidates = append(candidates, ResolvedCandidate{
				Target:         t,
				ProviderConfig: provCfg,
				Skipped:        true,
				SkipReason:     redactError(err),
			})
			continue
		}
		if ep.MissingAPIKey {
			candidates = append(candidates, ResolvedCandidate{
				Target:         t,
				ProviderConfig: provCfg,
				Endpoint:       ep,
				Skipped:        true,
				SkipReason:     fmt.Sprintf("missing API key env %q", provCfg.APIKeyEnv),
			})
			continue
		}

		candidates = append(candidates, ResolvedCandidate{
			Target:         t,
			ProviderConfig: provCfg,
			Endpoint:       ep,
		})
	}

	return candidates, nativeFB, nil
}

// RedactURL strips userinfo, query parameters, and fragments from a URL string.
func RedactURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<invalid-url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return strings.TrimRight(u.String(), "/")
}

var (
	reURLUserInfo = regexp.MustCompile(`(https?://)[^/@\s]+@`)
	reURLQuery    = regexp.MustCompile(`\?[^\s"']*`)
)

func redactError(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		safeURL := RedactURL(ue.URL)
		inner := redactText(ue.Err.Error(), secrets...)
		return fmt.Sprintf("%s %q: %s", ue.Op, safeURL, inner)
	}
	return redactText(err.Error(), secrets...)
}

func redactText(s string, secrets ...string) string {
	out := s
	for _, sec := range secrets {
		if sec != "" {
			out = strings.ReplaceAll(out, sec, "[REDACTED]")
		}
	}
	out = reURLUserInfo.ReplaceAllString(out, "${1}[REDACTED]@")
	out = reURLQuery.ReplaceAllString(out, "")
	return out
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	// Transport / connection / timeout errors are retryable across fallback providers
	// unless explicitly missing key or context canceled by caller.
	if errors.Is(err, ErrMissingAPIKey) || errors.Is(err, context.Canceled) {
		return false
	}
	return true
}
