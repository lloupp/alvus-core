package provider

import (
	"net/http"
	"testing"
)

func TestProviderSpecificClassification(t *testing.T) {
	if got := New("nvidia").Classify(http.StatusForbidden, nil, nil).Action; got != FallbackModel {
		t.Fatalf("nvidia 403=%s", got)
	}
	if got := New("openrouter").Classify(http.StatusForbidden, nil, nil).Action; got != Terminal {
		t.Fatalf("openrouter 403=%s", got)
	}
	body := []byte(`{"detail":"Function x: Not found for account y"}`)
	if got := New("nvidia").Classify(http.StatusNotFound, body, nil).Action; got != FallbackModel {
		t.Fatalf("nvidia 404=%s", got)
	}
	if got := New("groq").Classify(http.StatusTooManyRequests, nil, nil).Action; got != FallbackModel {
		t.Fatalf("groq 429=%s", got)
	}
	if d := New("nvidia").Classify(http.StatusServiceUnavailable, nil, nil); d.Action != FallbackModel || d.CooldownCredential {
		t.Fatalf("nvidia 503=%+v", d)
	}
	if d := New("nvidia").Classify(http.StatusTooManyRequests, nil, nil); d.Action != FallbackModel || !d.CooldownCredential {
		t.Fatalf("nvidia 429=%+v", d)
	}
}
