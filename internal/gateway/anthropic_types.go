package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type anthropicRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []anthMessage   `json:"messages"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []anthTool      `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	s.metrics.Requests.Add(1)
	body, err := readLimitedBody(w, r, st.cfg.RequestBodyLimitBytes)
	if err != nil {
		s.metrics.Errors.Add(1)
		return
	}
	var in anthropicRequest
	if err := json.Unmarshal(body, &in); err != nil || in.Model == "" || in.MaxTokens <= 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model and max_tokens are required")
		s.metrics.Errors.Add(1)
		return
	}
	translated, err := translateAnthropicRequest(in)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		s.metrics.Errors.Add(1)
		return
	}
	resp, err := s.routeRequest(st, r, "/v1/chat/completions", translated, in.Stream, in.Model)
	if err != nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", err.Error())
		s.metrics.Errors.Add(1)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		writeAnthropicError(w, resp.StatusCode, "api_error", strings.TrimSpace(string(errBody)))
		return
	}
	if in.Stream {
		translateOpenAIStream(w, resp, in.Model)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read upstream response")
		return
	}
	out, err := translateOpenAIResponse(respBody, in.Model)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	body, err := readLimitedBody(w, r, st.cfg.RequestBodyLimitBytes)
	if err != nil {
		return
	}
	// Providers behind Alvus Core do not expose one common tokenizer. This
	// endpoint intentionally returns a conservative transport-level estimate so
	// Anthropic-compatible clients can budget context without a false claim of
	// tokenizer-exact accounting.
	n := len(bytes.TrimSpace(body))
	tokens := (n + 2) / 3
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}
