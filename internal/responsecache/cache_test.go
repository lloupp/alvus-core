package responsecache

import (
	"net/http"
	"testing"
	"time"
)

func TestMemorySetGetExpiryAndCloning(t *testing.T) {
	now := time.Unix(100, 0)
	cache := NewMemory(2, 1024)
	value := Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       []byte("one"),
	}
	if !cache.Set("a", value, time.Minute, now) {
		t.Fatal("expected cache store")
	}

	value.Header.Set("Content-Type", "mutated")
	value.Body[0] = 'x'

	got, ok := cache.Get("a", now.Add(time.Second))
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.StatusCode != http.StatusOK || got.Header.Get("Content-Type") != "application/json" || string(got.Body) != "one" {
		t.Fatalf("unexpected cached value: %#v body=%q", got, got.Body)
	}

	got.Header.Set("Content-Type", "changed")
	got.Body[0] = 'z'
	again, ok := cache.Get("a", now.Add(2*time.Second))
	if !ok || again.Header.Get("Content-Type") != "application/json" || string(again.Body) != "one" {
		t.Fatal("cache returned mutable shared state")
	}

	if _, ok := cache.Get("a", now.Add(time.Minute)); ok {
		t.Fatal("expired entry returned as hit")
	}
	if got := cache.Bytes(now.Add(time.Minute)); got != 0 {
		t.Fatalf("bytes=%d, want 0 after expiry", got)
	}
}

func TestMemoryEvictsForEntryAndByteBudgets(t *testing.T) {
	now := time.Unix(200, 0)
	cache := NewMemory(2, 5)
	if !cache.Set("a", Response{Body: []byte("aa")}, time.Minute, now) {
		t.Fatal("store a failed")
	}
	if !cache.Set("b", Response{Body: []byte("bb")}, 2*time.Minute, now) {
		t.Fatal("store b failed")
	}
	if !cache.Set("c", Response{Body: []byte("ccc")}, 3*time.Minute, now) {
		t.Fatal("store c failed")
	}

	if _, ok := cache.Get("a", now); ok {
		t.Fatal("earliest entry was not evicted")
	}
	if _, ok := cache.Get("b", now); !ok {
		t.Fatal("expected b to remain")
	}
	if _, ok := cache.Get("c", now); !ok {
		t.Fatal("expected c to remain")
	}
	if got := cache.Bytes(now); got != 5 {
		t.Fatalf("bytes=%d, want 5", got)
	}

	if cache.Set("too-large", Response{Body: []byte("123456")}, time.Minute, now) {
		t.Fatal("oversized entry should not be stored")
	}
}

func TestKeyIsStableAndSeparatesInputs(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	a := Key("nvidia", "m1", "POST", "/v1/chat/completions", "", body)
	b := Key("nvidia", "m1", "POST", "/v1/chat/completions", "", body)
	c := Key("nvidia", "m2", "POST", "/v1/chat/completions", "", body)

	if a != b {
		t.Fatal("same request produced different keys")
	}
	if a == c {
		t.Fatal("different model produced same key")
	}
}
