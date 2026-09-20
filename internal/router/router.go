package router

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/lloupp/alvus-core/internal/config"
)

type Candidate struct {
	Alias         string
	Provider      string
	UpstreamModel string
}

type healthState struct {
	failures   int
	openUntil  time.Time
	lastReason string
}

type Router struct {
	mu     sync.Mutex
	cfg    config.Config
	health map[string]healthState
}

func New(cfg config.Config) *Router { return &Router{cfg: cfg, health: map[string]healthState{}} }

func (r *Router) Candidates(requested string, now time.Time) ([]Candidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	aliases, ok := r.cfg.Routes[requested]
	if !ok {
		if _, direct := r.cfg.Models[requested]; direct {
			aliases = []string{requested}
			ok = true
		}
	}
	if !ok && requested == "" {
		aliases, ok = r.cfg.Routes["default"]
	}
	if !ok {
		return nil, fmt.Errorf("unknown model or route %q", requested)
	}

	out := make([]Candidate, 0, len(aliases))
	for _, alias := range aliases {
		h := r.health[alias]
		if now.Before(h.openUntil) {
			continue
		}
		m := r.cfg.Models[alias]
		out = append(out, Candidate{Alias: alias, Provider: m.Provider, UpstreamModel: m.UpstreamModel})
	}
	if len(out) == 0 {
		// Deterministic half-open behavior: try the candidate whose circuit recovers first.
		type pair struct {
			alias string
			until time.Time
		}
		ps := make([]pair, 0, len(aliases))
		for _, a := range aliases {
			ps = append(ps, pair{a, r.health[a].openUntil})
		}
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].until.Before(ps[j].until) })
		if len(ps) > 0 && !now.Before(ps[0].until) {
			m := r.cfg.Models[ps[0].alias]
			out = append(out, Candidate{Alias: ps[0].alias, Provider: m.Provider, UpstreamModel: m.UpstreamModel})
		}
	}
	return out, nil
}

func (r *Router) Success(alias string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health[alias] = healthState{}
}

func (r *Router) Failure(alias, reason string, now time.Time, retryAfter time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.health[alias]
	h.failures++
	h.lastReason = reason
	if h.failures >= r.cfg.CircuitBreaker.FailureThreshold || retryAfter > 0 {
		d := r.cfg.CircuitBreaker.Cooldown.Duration
		if retryAfter > d {
			d = retryAfter
		}
		h.openUntil = now.Add(d)
		h.failures = 0
	}
	r.health[alias] = h
}

func (r *Router) Snapshot(now time.Time) map[string]map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]map[string]any, len(r.cfg.Models))
	for alias := range r.cfg.Models {
		h := r.health[alias]
		state := "closed"
		if now.Before(h.openUntil) {
			state = "open"
		}
		out[alias] = map[string]any{"state": state, "open_until": h.openUntil, "last_reason": h.lastReason}
	}
	return out
}
