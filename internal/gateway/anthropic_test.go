package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type contextAwareBody struct {
	ctx    context.Context
	reader *strings.Reader
	closed bool
}

func (b *contextAwareBody) Read(p []byte) (int, error) {
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	default:
		return b.reader.Read(p)
	}
}

func (b *contextAwareBody) Close() error {
	b.closed = true
	return nil
}

func TestAnthropicMessagesTranslation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		if v["model"] != "real-a" {
			t.Errorf("model=%v", v["model"])
		}
		msgs := v["messages"].([]any)
		first := msgs[0].(map[string]any)
		if first["role"] != "system" {
			t.Errorf("system missing: %#v", msgs)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-1","model":"real-a","choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer up.Close()
	s := New(baseConfig(up.URL+"/v1"), nil)
	body := `{"model":"a","max_tokens":64,"system":"be useful","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("out=%#v", out)
	}
	content := out["content"].([]any)
	if content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("content=%#v", content)
	}
}

func TestAnthropicResponseContextLivesUntilBodyClose(t *testing.T) {
	s := New(baseConfig("https://example.test/v1"), nil)
	var upstreamBody *contextAwareBody
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamBody = &contextAwareBody{
			ctx:    r.Context(),
			reader: strings.NewReader(`{"id":"chat-1","model":"real-a","choices":[{"message":{"content":"hello"},"finish_reason":"stop"}]}`),
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       upstreamBody,
			Request:    r,
		}, nil
	})

	body := `{"model":"a","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamBody == nil || !upstreamBody.closed {
		t.Fatal("upstream response body was not closed")
	}
	select {
	case <-upstreamBody.ctx.Done():
	default:
		t.Fatal("upstream request context was not canceled after body close")
	}
}

func TestAnthropicTextStreaming(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()
	s := New(baseConfig(up.URL+"/v1"), nil)
	body := `{"model":"a","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "content_block_delta") || !strings.Contains(got, `"text":"Hi"`) || !strings.Contains(got, "message_stop") {
		t.Fatalf("stream=%s", got)
	}
}

func TestAnthropicToolStreaming(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()
	s := New(baseConfig(up.URL+"/v1"), nil)
	body := `{"model":"a","max_tokens":64,"stream":true,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	got := rec.Body.String()
	if !strings.Contains(got, `"type":"tool_use"`) || !strings.Contains(got, `"type":"input_json_delta"`) || !strings.Contains(got, `"stop_reason":"tool_use"`) {
		t.Fatalf("stream=%s", got)
	}
}

func TestAnthropicCountTokensEstimate(t *testing.T) {
	s := New(baseConfig("https://example.invalid/v1"), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"a","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTranslateAnthropicToolChoice(t *testing.T) {
	in := anthropicRequest{Model: "a", MaxTokens: 10, Messages: []anthMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}, Tools: []anthTool{{Name: "lookup", InputSchema: map[string]any{"type": "object"}}}, ToolChoice: json.RawMessage(`{"type":"tool","name":"lookup"}`)}
	b, err := translateAnthropicRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	tc := v["tool_choice"].(map[string]any)
	if tc["type"] != "function" {
		t.Fatalf("tool_choice=%#v", tc)
	}
}
