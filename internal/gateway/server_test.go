package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lloupp/alvus-core/internal/config"
)

func baseConfig(url string) config.Config {
	c := config.Defaults()
	c.Providers = map[string]config.Provider{"p": {BaseURL: url, APIKeys: []string{"k1", "k2"}}}
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
			t.Errorf("model = %v, want real-b", v["model"])
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

func TestBodyLimit(t *testing.T) {
	c := baseConfig("https://example.invalid/v1")
	c.RequestBodyLimitBytes = 10
	s := New(c, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto"}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", rec.Code)
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
