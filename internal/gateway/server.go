package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lloupp/alvus-core/internal/config"
	"github.com/lloupp/alvus-core/internal/credentials"
	"github.com/lloupp/alvus-core/internal/router"
)

const maxErrorBody = 1 << 20

type Metrics struct {
	Requests  atomic.Uint64
	Attempts  atomic.Uint64
	Fallbacks atomic.Uint64
	Errors    atomic.Uint64
}

type Server struct {
	cfg          config.Config
	router       *router.Router
	pools        map[string]*credentials.Pool
	client       *http.Client
	streamClient *http.Client
	log          *slog.Logger
	metrics      Metrics
	mux          *http.ServeMux
}

func New(cfg config.Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	pools := make(map[string]*credentials.Pool, len(cfg.Providers))
	for name, p := range cfg.Providers {
		pools[name] = credentials.New(p.APIKeys)
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	s := &Server{
		cfg:          cfg,
		router:       router.New(cfg),
		pools:        pools,
		client:       &http.Client{Transport: transport, CheckRedirect: noRedirect},
		streamClient: &http.Client{Transport: transport.Clone(), CheckRedirect: noRedirect},
		log:          logger,
		mux:          http.NewServeMux(),
	}
	s.routes()
	return s
}

var noRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.ready)
	s.mux.HandleFunc("GET /metrics", s.metricsHandler)
	s.mux.HandleFunc("GET /v1/models", s.proxyAuth(s.models))
	s.mux.Handle("/v1/", s.proxyAuth(http.HandlerFunc(s.proxy)))
}

func (s *Server) proxyAuth(next http.HandlerFunc) http.HandlerFunc {
	if s.cfg.ProxyToken == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(got) != len(s.cfg.ProxyToken) || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.ProxyToken)) != 1 {
			http.Error(w, "alvus-core: unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) adminAuth(r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		return true
	}
	got := r.Header.Get("X-Alvus-Admin-Token")
	return len(got) == len(s.cfg.AdminToken) && subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) == 1
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	providers := map[string]int{}
	ready := true
	for name, p := range s.pools {
		providers[name] = p.Available(now)
		if p.Available(now) == 0 {
			ready = false
		}
	}
	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "degraded"
	}
	writeJSON(w, status, map[string]any{"status": state, "providers": providers})
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuth(r) {
		http.Error(w, "alvus-core: unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requests":          s.metrics.Requests.Load(),
		"upstream_attempts": s.metrics.Attempts.Load(),
		"fallbacks":         s.metrics.Fallbacks.Load(),
		"errors":            s.metrics.Errors.Load(),
		"circuits":          s.router.Snapshot(time.Now()),
	})
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	data := make([]map[string]any, 0, len(s.cfg.Models)+len(s.cfg.Routes))
	for name := range s.cfg.Models {
		data = append(data, map[string]any{"id": name, "object": "model", "owned_by": "alvus-core"})
	}
	for name := range s.cfg.Routes {
		if name != "default" {
			data = append(data, map[string]any{"id": name, "object": "model", "owned_by": "alvus-core-router"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

type modelProbe struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	s.metrics.Requests.Add(1)
	body, err := readLimitedBody(w, r, s.cfg.RequestBodyLimitBytes)
	if err != nil {
		s.metrics.Errors.Add(1)
		return
	}
	var probe modelProbe
	if err := json.Unmarshal(body, &probe); err != nil || probe.Model == "" {
		http.Error(w, "alvus-core: request must contain a model", http.StatusBadRequest)
		s.metrics.Errors.Add(1)
		return
	}
	candidates, err := s.router.Candidates(probe.Model, time.Now())
	if err != nil || len(candidates) == 0 {
		http.Error(w, "alvus-core: "+errString(err, "no healthy route"), http.StatusServiceUnavailable)
		s.metrics.Errors.Add(1)
		return
	}

	for ci, candidate := range candidates {
		if ci > 0 {
			s.metrics.Fallbacks.Add(1)
		}
		pool := s.pools[candidate.Provider]
		if pool == nil {
			continue
		}
		maxKeyAttempts := pool.Len()
		if maxKeyAttempts < 1 {
			continue
		}
		for keyAttempt := 0; keyAttempt < maxKeyAttempts; keyAttempt++ {
			idx, key, err := pool.Next(time.Now())
			if err != nil {
				break
			}
			s.metrics.Attempts.Add(1)
			resp, err := s.doAttempt(r, body, probe.Stream, candidate, key)
			if err != nil {
				pool.Cooldown(idx, time.Now().Add(5*time.Second))
				s.log.Warn("upstream request failed", "provider", candidate.Provider, "model", candidate.Alias, "error", err)
				continue
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				s.router.Success(candidate.Alias)
				s.copyResponse(w, resp, probe.Stream)
				return
			}

			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
			resp.Body.Close()
			disposition, retryAfter := classify(resp.StatusCode, errBody, resp.Header)
			s.log.Warn("upstream rejected request", "provider", candidate.Provider, "model", candidate.Alias, "status", resp.StatusCode, "disposition", disposition)
			switch disposition {
			case "disable-key":
				pool.Disable(idx)
				continue
			case "retry-key":
				pool.Cooldown(idx, time.Now().Add(maxDuration(retryAfter, 10*time.Second)))
				continue
			case "fallback-model":
				pool.Cooldown(idx, time.Now().Add(minPositive(retryAfter, 10*time.Second)))
				s.router.Failure(candidate.Alias, strconv.Itoa(resp.StatusCode), time.Now(), retryAfter)
				keyAttempt = maxKeyAttempts
				continue
			case "skip-model":
				keyAttempt = maxKeyAttempts
				continue
			default:
				copyBufferedResponse(w, resp, errBody)
				return
			}
		}
	}
	s.metrics.Errors.Add(1)
	http.Error(w, "alvus-core: all routes exhausted", http.StatusServiceUnavailable)
}

func (s *Server) doAttempt(original *http.Request, body []byte, stream bool, c router.Candidate, key string) (*http.Response, error) {
	p := s.cfg.Providers[c.Provider]
	target, err := targetURL(p.BaseURL, original.URL.Path, original.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	patched, err := swapModel(body, c.UpstreamModel)
	if err != nil {
		return nil, err
	}

	ctx := original.Context()
	cancel := func() {}
	if !stream {
		ctx, cancel = context.WithTimeout(ctx, s.cfg.RequestTimeout.Duration)
	}
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, original.Method, target, bytes.NewReader(patched))
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req.Header, original.Header)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Length", strconv.Itoa(len(patched)))
	client := s.client
	if stream {
		client = s.streamClient
	}
	return client.Do(req)
}

func (s *Server) copyResponse(w http.ResponseWriter, resp *http.Response, stream bool) {
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if stream {
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}
	_, _ = io.Copy(w, resp.Body)
}

func copyBufferedResponse(w http.ResponseWriter, resp *http.Response, body []byte) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func readLimitedBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "alvus-core: request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "alvus-core: invalid request body", http.StatusBadRequest)
		}
		return nil, err
	}
	return body, nil
}

func swapModel(body []byte, model string) ([]byte, error) {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	v["model"] = model
	return json.Marshal(v)
}

func targetURL(base, path, rawQuery string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	bp := strings.TrimRight(u.Path, "/")
	p := path
	if strings.HasSuffix(bp, "/v1") && strings.HasPrefix(p, "/v1/") {
		p = strings.TrimPrefix(p, "/v1")
	}
	u.Path = bp + "/" + strings.TrimLeft(p, "/")
	u.RawQuery = rawQuery
	return u.String(), nil
}

func classify(status int, body []byte, h http.Header) (string, time.Duration) {
	retryAfter := parseRetryAfter(h.Get("Retry-After"), time.Now())
	lower := strings.ToLower(string(body))
	switch status {
	case http.StatusUnauthorized:
		return "disable-key", retryAfter
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, 529:
		return "fallback-model", retryAfter
	case http.StatusForbidden:
		return "fallback-model", retryAfter
	case http.StatusBadRequest:
		if strings.Contains(lower, "context length") || strings.Contains(lower, "too many tokens") {
			return "skip-model", 0
		}
		return "terminal", 0
	default:
		if status >= 500 {
			return "retry-key", retryAfter
		}
		return "terminal", 0
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

func copyRequestHeaders(dst, src http.Header) {
	for k, vals := range src {
		if hopByHop(k) || strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}
func copyResponseHeaders(dst, src http.Header) {
	for k, vals := range src {
		if !hopByHop(k) {
			for _, v := range vals {
				dst.Add(k, v)
			}
		}
	}
}
func hopByHop(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func errString(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
func minPositive(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}
