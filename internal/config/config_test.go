package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPrecedenceAndProviderKeyEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alvus.json")
	data := `{
		"listen":"127.0.0.1:1111",
		"request_body_limit_bytes":1024,
		"request_timeout":"10s",
		"circuit_breaker":{"failure_threshold":2,"cooldown":"5s"},
		"providers":{"p":{"base_url":"https://example.test/v1","api_key_env":"TEST_KEYS"}},
		"models":{"m":{"provider":"p","upstream_model":"real-m"}},
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
	if got := cfg.Providers["p"].APIKeys; len(got) != 2 || got[0] != "k1" || got[1] != "k2" {
		t.Fatalf("keys = %#v", got)
	}
}
