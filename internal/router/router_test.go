package router

import (
	"github.com/lloupp/alvus-core/internal/config"
	"testing"
	"time"
)

func testConfig() config.Config {
	c := config.Defaults()
	c.CircuitBreaker.FailureThreshold = 1
	c.Providers = map[string]config.Provider{"p": {BaseURL: "https://x", APIKeys: []string{"k"}}}
	c.Models = map[string]config.Model{"a": {Provider: "p", UpstreamModel: "ua"}, "b": {Provider: "p", UpstreamModel: "ub"}}
	c.Routes = map[string][]string{"auto": {"a", "b"}}
	return c
}

func TestCircuitBreakerSkipsFailedModel(t *testing.T) {
	r := New(testConfig())
	now := time.Now()
	cs, err := r.Candidates("auto", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("want 2, got %d", len(cs))
	}
	r.Failure("a", "429", now, time.Minute)
	cs, err = r.Candidates("auto", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Alias != "b" {
		t.Fatalf("candidates = %#v", cs)
	}
}
