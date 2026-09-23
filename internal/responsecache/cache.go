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
	Set(key string, value Response, ttl time.Duration, now time.Time) bool
	Len(now time.Time) int
	Bytes(now time.Time) int64
}

type entry struct {
	value     Response
	expiresAt time.Time
	size      int64
}

type Memory struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int64
	bytes      int64
	entries    map[string]entry
}

func NewMemory(maxEntries int, maxBytes int64) *Memory {
	return &Memory{
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
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
		m.deleteLocked(key, item)
		return Response{}, false
	}
	return cloneResponse(item.value), true
}

func (m *Memory) Set(key string, value Response, ttl time.Duration, now time.Time) bool {
	if ttl <= 0 || m.maxEntries <= 0 || m.maxBytes <= 0 {
		return false
	}

	cloned := cloneResponse(value)
	size := int64(len(cloned.Body))
	if size > m.maxBytes {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.pruneExpiredLocked(now)
	if existing, ok := m.entries[key]; ok {
		m.deleteLocked(key, existing)
	}
	for len(m.entries) >= m.maxEntries || m.bytes+size > m.maxBytes {
		if !m.evictEarliestLocked() {
			return false
		}
	}
	m.entries[key] = entry{
		value:     cloned,
		expiresAt: now.Add(ttl),
		size:      size,
	}
	m.bytes += size
	return true
}

func (m *Memory) Len(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredLocked(now)
	return len(m.entries)
}

func (m *Memory) Bytes(now time.Time) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredLocked(now)
	return m.bytes
}

func (m *Memory) pruneExpiredLocked(now time.Time) {
	for key, item := range m.entries {
		if !item.expiresAt.After(now) {
			m.deleteLocked(key, item)
		}
	}
}

func (m *Memory) evictEarliestLocked() bool {
	var oldestKey string
	var oldest entry
	first := true
	for key, item := range m.entries {
		if first || item.expiresAt.Before(oldest.expiresAt) {
			oldestKey = key
			oldest = item
			first = false
		}
	}
	if first {
		return false
	}
	m.deleteLocked(oldestKey, oldest)
	return true
}

func (m *Memory) deleteLocked(key string, item entry) {
	delete(m.entries, key)
	m.bytes -= item.size
	if m.bytes < 0 {
		m.bytes = 0
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
