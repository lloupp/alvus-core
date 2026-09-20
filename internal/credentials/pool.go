package credentials

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

type keyState struct {
	value    string
	disabled bool
	until    time.Time
}

type Pool struct {
	mu     sync.Mutex
	cursor atomic.Uint64
	keys   []keyState
}

func New(keys []string) *Pool {
	states := make([]keyState, 0, len(keys))
	for _, k := range keys {
		if k != "" {
			states = append(states, keyState{value: k})
		}
	}
	return &Pool{keys: states}
}

func (p *Pool) Len() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.keys) }

func (p *Pool) Next(now time.Time) (int, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) == 0 {
		return -1, "", errors.New("empty credential pool")
	}
	start := int(p.cursor.Add(1)-1) % len(p.keys)
	for i := 0; i < len(p.keys); i++ {
		idx := (start + i) % len(p.keys)
		k := p.keys[idx]
		if !k.disabled && !now.Before(k.until) {
			return idx, k.value, nil
		}
	}
	return -1, "", errors.New("no credential currently available")
}

func (p *Pool) Cooldown(idx int, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.keys) {
		return
	}
	if until.After(p.keys[idx].until) {
		p.keys[idx].until = until
	}
}

func (p *Pool) Disable(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx >= 0 && idx < len(p.keys) {
		p.keys[idx].disabled = true
	}
}

func (p *Pool) Available(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, k := range p.keys {
		if !k.disabled && !now.Before(k.until) {
			n++
		}
	}
	return n
}
