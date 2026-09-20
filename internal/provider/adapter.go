package provider

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Action string

const (
	Terminal      Action = "terminal"
	DisableKey    Action = "disable-key"
	RetryKey      Action = "retry-key"
	FallbackModel Action = "fallback-model"
	SkipModel     Action = "skip-model"
)

type Decision struct {
	Action     Action
	RetryAfter time.Duration
	Reason     string
}

type Adapter interface {
	Kind() string
	Authorize(*http.Request, string)
	Classify(int, []byte, http.Header) Decision
}

type openAIAdapter struct{ kind string }

func New(kind string) Adapter {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		kind = "openai"
	}
	return openAIAdapter{kind: kind}
}

func (a openAIAdapter) Kind() string { return a.kind }

func (a openAIAdapter) Authorize(r *http.Request, key string) {
	r.Header.Set("Authorization", "Bearer "+key)
}

func (a openAIAdapter) Classify(status int, body []byte, h http.Header) Decision {
	retryAfter := parseRetryAfter(h.Get("Retry-After"), time.Now())
	lower := strings.ToLower(string(body))

	switch status {
	case http.StatusUnauthorized:
		return Decision{Action: DisableKey, RetryAfter: retryAfter, Reason: "unauthorized"}
	case http.StatusForbidden:
		if a.kind == "nvidia" {
			return Decision{Action: FallbackModel, RetryAfter: retryAfter, Reason: "nvidia authorization or entitlement"}
		}
		return Decision{Action: Terminal, RetryAfter: retryAfter, Reason: "forbidden"}
	case http.StatusPaymentRequired:
		return Decision{Action: Terminal, Reason: "billing required"}
	case http.StatusTooManyRequests:
		return Decision{Action: FallbackModel, RetryAfter: retryAfter, Reason: "rate limited"}
	case http.StatusBadGateway, http.StatusServiceUnavailable, 529:
		return Decision{Action: FallbackModel, RetryAfter: retryAfter, Reason: "provider capacity"}
	case http.StatusBadRequest:
		if strings.Contains(lower, "context length") || strings.Contains(lower, "too many tokens") || strings.Contains(lower, "maximum context") {
			return Decision{Action: SkipModel, Reason: "context length"}
		}
		return Decision{Action: Terminal, Reason: "bad request"}
	case http.StatusNotFound:
		if a.kind == "nvidia" && strings.Contains(lower, "not found for account") {
			return Decision{Action: FallbackModel, Reason: "model unavailable for account"}
		}
		return Decision{Action: Terminal, Reason: "not found"}
	default:
		if status >= 500 {
			return Decision{Action: RetryKey, RetryAfter: retryAfter, Reason: "provider server error"}
		}
		return Decision{Action: Terminal}
	}
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
