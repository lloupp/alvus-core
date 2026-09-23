package gateway

import (
	"encoding/json"
	"github.com/lloupp/alvus-core/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func baseConfig(url string) config.Config {
	c := config.Defaults()
	c.Providers = map[string]config.Provider{"p": {Kind: "openai", BaseURL: url, APIKeys: []string{"k1", "k2"}}}
	c.Models = map[string]config.Model{"a": {Provider: "p", UpstreamModel: "real-a"}, "b": {Provider: "p", UpstreamModel: "real-b"}}
	c.Routes = map[string][]string{"auto": {"a", "b"}, "default": {"a", "b"}}
	c.CircuitBreaker.FailureThreshold = 1
	c.CircuitBreaker.Cooldown = config.Duration{Duration: time.Minute}
	return c
}

func TestFallbackOn429AndModelRewrite(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		if v["model"] != "real-b" {
			t.Errorf("model=%v", v["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()
	s := New(baseConfig(up.URL+"/v1"), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestProviderCapacityFallbackDoesNotCooldownCredential(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		calls.Add(1)
		switch v["model"] {
		case "real-a":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("capacity"))
		case "real-b":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"fallback-ok"},"finish_reason":"stop"}]}`))
		default:
			t.Fatalf("unexpected model=%v", v["model"])
		}
	}))
	defer up.Close()

	cfg := baseConfig(up.URL + "/v1")
	cfg.Providers["p"] = config.Provider{Kind: "nvidia", BaseURL: up.URL + "/v1", APIKeys: []string{"only-key"}}

	s := New(cfg, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fallback-ok") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want 2", calls.Load())
	}
	if got := s.state.Load().pools["p"].Available(time.Now()); got != 1 {
		t.Fatalf("available credentials=%d, want 1 after model-capacity fallback", got)
	}
	snapshot := s.state.Load().router.Snapshot(time.Now())
	if snapshot["a"]["last_reason"] != "provider capacity" {
		t.Fatalf("circuit reason=%#v", snapshot["a"])
	}
}

func TestPerModelTimeoutFallsBackWithoutRetryingSecondKey(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		if v["model"] == "real-a" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"fallback-ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	cfg := baseConfig(up.URL + "/v1")
	a := cfg.Models["a"]
	a.AttemptTimeout = config.Duration{Duration: 25 * time.Millisecond}
	cfg.Models["a"] = a

	s := New(cfg, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fallback-ok") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want exactly 2 (one timeout + one fallback)", calls.Load())
	}
	stats := s.modelMetricsSnapshot()
	if stats["a"]["timeouts"] != uint64(1) {
		t.Fatalf("a metrics=%#v", stats["a"])
	}
	if stats["a"]["fallbacks"] != uint64(1) {
		t.Fatalf("a fallback metrics=%#v", stats["a"])
	}
}

func TestStreamingHeaderTimeoutFallsBack(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		if v["model"] == "real-a" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer up.Close()

	cfg := baseConfig(up.URL + "/v1")
	a := cfg.Models["a"]
	a.AttemptTimeout = config.Duration{Duration: 25 * time.Millisecond}
	cfg.Models["a"] = a

	s := New(cfg, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want exactly 2", calls.Load())
	}
	stats := s.modelMetricsSnapshot()
	if stats["a"]["timeouts"] != uint64(1) {
		t.Fatalf("a metrics=%#v", stats["a"])
	}
}

func TestRequestTimeoutBoundsEntireFallbackChain(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(250 * time.Millisecond):
		}
	}))
	defer up.Close()

	cfg := baseConfig(up.URL + "/v1")
	cfg.RequestTimeout = config.Duration{Duration: 150 * time.Millisecond}
	for _, alias := range []string{"a", "b"} {
		m := cfg.Models[alias]
		m.AttemptTimeout = config.Duration{Duration: 120 * time.Millisecond}
		cfg.Models[alias] = m
	}

	s := New(cfg, nil)
	started := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	elapsed := time.Since(started)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want 2", calls.Load())
	}
	if elapsed >= 220*time.Millisecond {
		t.Fatalf("fallback chain exceeded total request budget: %s", elapsed)
	}
	stats := s.modelMetricsSnapshot()
	if stats["a"]["timeouts"] != uint64(1) || stats["b"]["timeouts"] != uint64(1) {
		t.Fatalf("metrics=%#v", stats)
	}
}

func TestFallbackOnEmptySuccessfulCompletion(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			if v["model"] != "real-a" {
				t.Errorf("first model=%v", v["model"])
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`))
			return
		}
		if v["model"] != "real-b" {
			t.Errorf("fallback model=%v", v["model"])
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"fallback-ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	s := New(baseConfig(up.URL+"/v1"), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "fallback-ok") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if s.metrics.Fallbacks.Load() != 1 {
		t.Fatalf("fallbacks=%d", s.metrics.Fallbacks.Load())
	}
}

func TestRequiredToolChoiceFallsBackWhenModelReturnsText(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			if v["model"] != "real-a" {
				t.Errorf("first model=%v", v["model"])
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"I will add 19 and 23 myself."},"finish_reason":"stop"}]}`))
			return
		}
		if v["model"] != "real-b" {
			t.Errorf("fallback model=%v", v["model"])
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"add","arguments":"{\"a\":19,\"b\":23}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer up.Close()

	s := New(baseConfig(up.URL+"/v1"), nil)
	body := `{"model":"auto","messages":[{"role":"user","content":"Use add for 19+23"}],"tools":[{"type":"function","function":{"name":"add","parameters":{"type":"object"}}}],"tool_choice":"required"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "tool_calls") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	stats := s.modelMetricsSnapshot()
	if stats["a"]["last_reason"] != "route_fallback" {
		t.Fatalf("a metrics=%#v", stats["a"])
	}
}

func TestNamedToolChoiceRequiresToolCall(t *testing.T) {
	body := []byte(`{"tool_choice":{"type":"function","function":{"name":"add"}}}`)
	if !requestRequiresToolCall(body) {
		t.Fatal("named tool choice was not treated as required")
	}
}

func TestAutoToolChoiceDoesNotRequireToolCall(t *testing.T) {
	body := []byte(`{"tool_choice":"auto"}`)
	if requestRequiresToolCall(body) {
		t.Fatal("auto tool choice must allow text-only responses")
	}
}

func TestToolCallOnlyCompletionDoesNotFallback(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"add","arguments":"{\\\"a\\\":1}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer up.Close()

	s := New(baseConfig(up.URL+"/v1"), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("unexpected fallback, calls=%d", calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "tool_calls") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestContentFilterCompletionDoesNotFallback(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"content_filter"}]}`))
	}))
	defer up.Close()

	s := New(baseConfig(up.URL+"/v1"), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("content filter must not trigger fallback, calls=%d", calls.Load())
	}
}

func TestTransactionalReloadKeepsOldStateOnInvalidConfig(t *testing.T) {
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"source-1"},"finish_reason":"stop"}]}`))
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"source-2"},"finish_reason":"stop"}]}`))
	}))
	defer up2.Close()
	c1 := baseConfig(up1.URL + "/v1")
	s := New(c1, nil)
	bad := c1
	bad.Providers = map[string]config.Provider{}
	if err := s.Reload(bad); err == nil {
		t.Fatal("invalid reload accepted")
	}
	assertSource(t, s, "source-1")
	c2 := baseConfig(up2.URL + "/v1")
	if err := s.Reload(c2); err != nil {
		t.Fatal(err)
	}
	assertSource(t, s, "source-2")
}

func assertSource(t *testing.T, s *Server, want string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"a","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestTargetURLAvoidsDoubleV1(t *testing.T) {
	got, err := targetURL("https://example.test/v1", "/v1/chat/completions", "x=1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.test/v1/chat/completions?x=1" {
		t.Fatalf("got %s", got)
	}
}

func TestModelDefaultsApplyWithoutOverridingClient(t *testing.T) {
	body := []byte(`{"model":"quality","messages":[],"reasoning_effort":"low"}`)
	patched, err := patchModelRequest(body, "z-ai/glm-5.3", map[string]any{
		"reasoning_effort":     "max",
		"chat_template_kwargs": map[string]any{"clear_thinking": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(patched, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "z-ai/glm-5.3" {
		t.Fatalf("model=%v", got["model"])
	}
	if got["reasoning_effort"] != "low" {
		t.Fatalf("client reasoning override lost: %v", got["reasoning_effort"])
	}
	if _, ok := got["chat_template_kwargs"]; !ok {
		t.Fatal("model defaults were not applied")
	}
}

func TestNVIDIAResponseCacheAvoidsRepeatedUpstreamCall(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"cached"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	cfg := baseConfig(up.URL + "/v1")
	p := cfg.Providers["p"]
	p.Kind = "nvidia"
	cfg.Providers["p"] = p
	cfg.Cache.Responses.Enabled = true
	cfg.Cache.Responses.TTL = config.Duration{Duration: time.Minute}
	cfg.Cache.Responses.MaxEntries = 16
	cfg.Cache.Responses.MaxBodyBytes = 1 << 20
	cfg.Cache.Responses.MaxBytes = 4 << 20

	s := New(cfg, nil)
	body := `{"model":"a","messages":[{"role":"user","content":"same"}]}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls.Load())
	}
	if s.metrics.CacheHits.Load() != 1 || s.metrics.CacheMisses.Load() != 1 || s.metrics.CacheStores.Load() != 1 {
		t.Fatalf("cache metrics hits=%d misses=%d stores=%d", s.metrics.CacheHits.Load(), s.metrics.CacheMisses.Load(), s.metrics.CacheStores.Load())
	}
}

func TestResponseCacheKeyIncludesRequestBody(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	cfg := baseConfig(up.URL + "/v1")
	p := cfg.Providers["p"]
	p.Kind = "nvidia"
	cfg.Providers["p"] = p
	cfg.Cache.Responses.Enabled = true

	s := New(cfg, nil)
	for _, content := range []string{"one", "two"} {
		body := `{"model":"a","messages":[{"role":"user","content":"` + content + `"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("content=%s status=%d body=%s", content, rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want 2", calls.Load())
	}
}

func TestResponseCacheBypassesStreamingAndOtherProviderKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		body string
	}{
		{name: "streaming", kind: "nvidia", body: `{"model":"a","messages":[],"stream":true}`},
		{name: "tool-calling", kind: "nvidia", body: `{"model":"a","messages":[],"tools":[{"type":"function","function":{"name":"ping","parameters":{"type":"object"}}}]}`},
		{name: "other-provider", kind: "openai", body: `{"model":"a","messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if strings.Contains(tc.body, `"stream":true`) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: done\\n\\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer up.Close()

			cfg := baseConfig(up.URL + "/v1")
			p := cfg.Providers["p"]
			p.Kind = tc.kind
			cfg.Providers["p"] = p
			cfg.Cache.Responses.Enabled = true
			s := New(cfg, nil)

			for i := 0; i < 2; i++ {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("request %d status=%d body=%s", i+1, rec.Code, rec.Body.String())
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("upstream calls=%d, want 2", calls.Load())
			}
			if s.metrics.CacheHits.Load() != 0 || s.metrics.CacheMisses.Load() != 0 {
				t.Fatalf("unexpected cache activity hits=%d misses=%d", s.metrics.CacheHits.Load(), s.metrics.CacheMisses.Load())
			}
		})
	}
}
