package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadPrecedenceAndProviderNormalization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alvus.json")
	data := `{
		"listen":"127.0.0.1:1111",
		"request_body_limit_bytes":1024,
		"request_timeout":"10s",
		"circuit_breaker":{"failure_threshold":2,"cooldown":"5s"},
		"cache":{"responses":{"enabled":true,"ttl":"30m","max_entries":64,"max_body_bytes":1048576,"max_bytes":8388608,"provider_kinds":["nvidia"]}},
		"providers":{"p":{"base_url":"https://example.test/v1","api_key_env":"TEST_KEYS"}},
		"models":{"m":{"provider":"p","upstream_model":"real-m","attempt_timeout":"2s"}},
		"routes":{"default":["m"]}
	}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_KEYS", "k1, k2")
	t.Setenv("ALVUS_LISTEN", "127.0.0.1:2222")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:2222" {
		t.Fatalf("env did not win: %s", cfg.Listen)
	}
	if cfg.Providers["p"].Kind != "openai" {
		t.Fatalf("kind=%q", cfg.Providers["p"].Kind)
	}
	if got := cfg.Providers["p"].APIKeys; len(got) != 2 {
		t.Fatalf("keys=%#v", got)
	}
	if !cfg.Cache.Responses.Enabled || cfg.Cache.Responses.MaxEntries != 64 || cfg.Cache.Responses.MaxBodyBytes != 1048576 || cfg.Cache.Responses.MaxBytes != 8388608 {
		t.Fatalf("response cache config=%+v", cfg.Cache.Responses)
	}
	if got := cfg.Models["m"].AttemptTimeout.Duration; got != 2*time.Second {
		t.Fatalf("attempt timeout=%s", got)
	}
}

func TestModelAttemptTimeoutValidation(t *testing.T) {
	cfg := Defaults()
	cfg.Providers = map[string]Provider{"p": {BaseURL: "https://example.test/v1", APIKeys: []string{"k"}}}
	cfg.Models = map[string]Model{"m": {Provider: "p", UpstreamModel: "real-m", AttemptTimeout: Duration{Duration: -time.Second}}}
	cfg.Routes = map[string][]string{"default": {"m"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative model attempt timeout accepted")
	}
}
