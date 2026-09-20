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
		_, _ = w.Write([]byte(`{"ok":true}`))
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

func TestTransactionalReloadKeepsOldStateOnInvalidConfig(t *testing.T) {
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"source":1}`)) }))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"source":2}`)) }))
	defer up2.Close()
	c1 := baseConfig(up1.URL + "/v1")
	s := New(c1, nil)
	bad := c1
	bad.Providers = map[string]config.Provider{}
	if err := s.Reload(bad); err == nil {
		t.Fatal("invalid reload accepted")
	}
	assertSource(t, s, "1")
	c2 := baseConfig(up2.URL + "/v1")
	if err := s.Reload(c2); err != nil {
		t.Fatal(err)
	}
	assertSource(t, s, "2")
}

func assertSource(t *testing.T, s *Server, want string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"a","messages":[]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"source":`+want) {
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
	patched, err := patchModelRequest(body, "z-ai/glm-5-3", map[string]any{
		"reasoning_effort": "max",
		"chat_template_kwargs": map[string]any{"clear_thinking": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(patched, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "z-ai/glm-5-3" {
		t.Fatalf("model=%v", got["model"])
	}
	if got["reasoning_effort"] != "low" {
		t.Fatalf("client reasoning override lost: %v", got["reasoning_effort"])
	}
	if _, ok := got["chat_template_kwargs"]; !ok {
		t.Fatal("model defaults were not applied")
	}
}
