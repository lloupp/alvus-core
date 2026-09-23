package responsecache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

type Store interface {
	Get(key string, now time.Time) (Response, bool)
	Set(key string, value Response, ttl time.Duration, now time.Time)
	Len(now time.Time) int
}

type entry struct {
	value     Response
	expiresAt time.Time
}

type Memory struct {
	mu         sync.Mutex
	maxEntries int
	entries    map[string]entry
}

func NewMemory(maxEntries int) *Memory {
	return &Memory{
		maxEntries: maxEntries,
		entries:    make(map[string]entry),
	}
}

func (m *Memory) Get(key string, now time.Time) (Response, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	item, ok := m.entries[key]
	if !ok {
		return Response{}, false
	}
	if !item.expiresAt.After(now) {
		delete(m.entries, key)
		return Response{}, false
	}
	return cloneResponse(item.value), true
}

func (m *Memory) Set(key string, value Response, ttl time.Duration, now time.Time) {
	if ttl <= 0 || m.maxEntries <= 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.pruneExpiredLocked(now)
	if _, exists := m.entries[key]; !exists && len(m.entries) >= m.maxEntries {
		m.evictEarliestLocked()
	}
	m.entries[key] = entry{
		value:     cloneResponse(value),
		expiresAt: now.Add(ttl),
	}
}

func (m *Memory) Len(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredLocked(now)
	return len(m.entries)
}

func (m *Memory) pruneExpiredLocked(now time.Time) {
	for key, item := range m.entries {
		if !item.expiresAt.After(now) {
			delete(m.entries, key)
		}
	}
}

func (m *Memory) evictEarliestLocked() {
	var oldestKey string
	var oldestExpiry time.Time
	first := true
	for key, item := range m.entries {
		if first || item.expiresAt.Before(oldestExpiry) {
			oldestKey = key
			oldestExpiry = item.expiresAt
			first = false
		}
	}
	if !first {
		delete(m.entries, oldestKey)
	}
}

func cloneResponse(in Response) Response {
	body := append([]byte(nil), in.Body...)
	header := make(http.Header, len(in.Header))
	for key, values := range in.Header {
		header[key] = append([]string(nil), values...)
	}
	return Response{
		StatusCode: in.StatusCode,
		Header:     header,
		Body:       body,
	}
}

func Key(provider, model, method, path, rawQuery string, body []byte) string {
	h := sha256.New()
	writePart := func(part []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(part)
	}
	for _, part := range []string{provider, model, method, path, rawQuery} {
		writePart([]byte(part))
	}
	writePart(body)
	return hex.EncodeToString(h.Sum(nil))
}
