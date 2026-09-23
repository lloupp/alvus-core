package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultListen                = "127.0.0.1:3000"
	DefaultBodyLimit             = int64(64 << 20)
	DefaultRequestTimeout        = 300 * time.Second
	DefaultFailureLimit          = 3
	DefaultCircuitCooldown       = 60 * time.Second
	DefaultResponseCacheTTL      = time.Hour
	DefaultResponseCacheSize     = 256
	DefaultResponseCacheBodySize = int64(1 << 20)
	DefaultResponseCacheBytes    = int64(64 << 20)
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

type Provider struct {
	Kind      string            `json:"kind,omitempty"`
	BaseURL   string            `json:"base_url"`
	APIKeyEnv string            `json:"api_key_env,omitempty"`
	APIKeys   []string          `json:"api_keys,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

type Model struct {
	Provider      string         `json:"provider"`
	UpstreamModel string         `json:"upstream_model"`
	Params        map[string]any `json:"params,omitempty"`
}

type CircuitBreaker struct {
	FailureThreshold int      `json:"failure_threshold"`
	Cooldown         Duration `json:"cooldown"`
}

type ResponseCache struct {
	Enabled       bool     `json:"enabled"`
	TTL           Duration `json:"ttl"`
	MaxEntries    int      `json:"max_entries"`
	MaxBodyBytes  int64    `json:"max_body_bytes"`
	MaxBytes      int64    `json:"max_bytes"`
	ProviderKinds []string `json:"provider_kinds,omitempty"`
}

type Cache struct {
	Responses ResponseCache `json:"responses"`
}

type Config struct {
	Listen                string              `json:"listen"`
	ProxyToken            string              `json:"proxy_token,omitempty"`
	AdminToken            string              `json:"admin_token,omitempty"`
	RequestBodyLimitBytes int64               `json:"request_body_limit_bytes"`
	RequestTimeout        Duration            `json:"request_timeout"`
	CircuitBreaker        CircuitBreaker      `json:"circuit_breaker"`
	Cache                 Cache               `json:"cache"`
	Providers             map[string]Provider `json:"providers"`
	Models                map[string]Model    `json:"models"`
	Routes                map[string][]string `json:"routes"`
}

func Defaults() Config {
	return Config{
		Listen:                DefaultListen,
		RequestBodyLimitBytes: DefaultBodyLimit,
		RequestTimeout:        Duration{DefaultRequestTimeout},
		CircuitBreaker: CircuitBreaker{
			FailureThreshold: DefaultFailureLimit,
			Cooldown:         Duration{DefaultCircuitCooldown},
		},
		Cache: Cache{
			Responses: ResponseCache{
				TTL:           Duration{DefaultResponseCacheTTL},
				MaxEntries:    DefaultResponseCacheSize,
				MaxBodyBytes:  DefaultResponseCacheBodySize,
				MaxBytes:      DefaultResponseCacheBytes,
				ProviderKinds: []string{"nvidia"},
			},
		},
		Providers: map[string]Provider{},
		Models:    map[string]Model{},
		Routes:    map[string][]string{},
	}
}

// Load applies precedence without mutating the process environment:
// explicit environment > JSON config file > defaults.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(&cfg)
	normalizeProviders(&cfg)
	resolveProviderKeys(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("ALVUS_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("ALVUS_PROXY_TOKEN"); v != "" {
		cfg.ProxyToken = v
	}
	if v := os.Getenv("ALVUS_ADMIN_TOKEN"); v != "" {
		cfg.AdminToken = v
	}
	if v := os.Getenv("ALVUS_BODY_LIMIT_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.RequestBodyLimitBytes = n
		}
	}
	if v := os.Getenv("ALVUS_REQUEST_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.RequestTimeout = Duration{d}
		}
	}
}

func normalizeProviders(cfg *Config) {
	for name, p := range cfg.Providers {
		p.Kind = strings.ToLower(strings.TrimSpace(p.Kind))
		if p.Kind == "" {
			p.Kind = "openai"
		}
		if p.Headers == nil {
			p.Headers = map[string]string{}
		}
		cfg.Providers[name] = p
	}
}

func resolveProviderKeys(cfg *Config) {
	for name, p := range cfg.Providers {
		if p.APIKeyEnv != "" {
			if raw := os.Getenv(p.APIKeyEnv); raw != "" {
				p.APIKeys = splitCSV(raw)
			}
		}
		cfg.Providers[name] = p
	}
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen address is required")
	}
	if c.RequestBodyLimitBytes <= 0 {
		return errors.New("request_body_limit_bytes must be > 0")
	}
	if c.RequestTimeout.Duration <= 0 {
		return errors.New("request_timeout must be > 0")
	}
	if c.CircuitBreaker.FailureThreshold <= 0 {
		return errors.New("circuit_breaker.failure_threshold must be > 0")
	}
	if c.CircuitBreaker.Cooldown.Duration <= 0 {
		return errors.New("circuit_breaker.cooldown must be > 0")
	}
	if c.Cache.Responses.Enabled {
		if c.Cache.Responses.TTL.Duration <= 0 {
			return errors.New("cache.responses.ttl must be > 0")
		}
		if c.Cache.Responses.MaxEntries <= 0 {
			return errors.New("cache.responses.max_entries must be > 0")
		}
		if c.Cache.Responses.MaxBodyBytes <= 0 {
			return errors.New("cache.responses.max_body_bytes must be > 0")
		}
		if c.Cache.Responses.MaxBytes <= 0 {
			return errors.New("cache.responses.max_bytes must be > 0")
		}
		if c.Cache.Responses.MaxBodyBytes > c.Cache.Responses.MaxBytes {
			return errors.New("cache.responses.max_body_bytes must be <= cache.responses.max_bytes")
		}
		if len(c.Cache.Responses.ProviderKinds) == 0 {
			return errors.New("cache.responses.provider_kinds must not be empty")
		}
		for _, kind := range c.Cache.Responses.ProviderKinds {
			if strings.TrimSpace(kind) == "" {
				return errors.New("cache.responses.provider_kinds must not contain empty values")
			}
		}
	}
	if len(c.Providers) == 0 {
		return errors.New("at least one provider is required")
	}
	for name, p := range c.Providers {
		if strings.TrimSpace(p.BaseURL) == "" {
			return fmt.Errorf("provider %q: base_url is required", name)
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("provider %q: invalid base_url", name)
		}
		switch p.Kind {
		case "openai", "nvidia", "openrouter", "groq", "together":
		default:
			return fmt.Errorf("provider %q: unsupported kind %q", name, p.Kind)
		}
		if len(p.APIKeys) == 0 {
			return fmt.Errorf("provider %q: no API keys resolved", name)
		}
	}
	if len(c.Models) == 0 {
		return errors.New("at least one model is required")
	}
	for name, m := range c.Models {
		if _, ok := c.Providers[m.Provider]; !ok {
			return fmt.Errorf("model %q references unknown provider %q", name, m.Provider)
		}
		if strings.TrimSpace(m.UpstreamModel) == "" {
			return fmt.Errorf("model %q: upstream_model is required", name)
		}
	}
	for route, models := range c.Routes {
		if len(models) == 0 {
			return fmt.Errorf("route %q is empty", route)
		}
		for _, model := range models {
			if _, ok := c.Models[model]; !ok {
				return fmt.Errorf("route %q references unknown model %q", route, model)
			}
		}
	}
	return nil
}
