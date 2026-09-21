package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lloupp/alvus-core/internal/config"
	"github.com/lloupp/alvus-core/internal/credentials"
	"github.com/lloupp/alvus-core/internal/provider"
	"github.com/lloupp/alvus-core/internal/router"
)

const maxErrorBody = 1 << 20

type Metrics struct {
	Requests  atomic.Uint64
	Attempts  atomic.Uint64
	Fallbacks atomic.Uint64
	Errors    atomic.Uint64
	Reloads   atomic.Uint64
}

type runtimeState struct {
	cfg      config.Config
	router   *router.Router
	pools    map[string]*credentials.Pool
	adapters map[string]provider.Adapter
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}

type Server struct {
	state        atomic.Pointer[runtimeState]
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
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	s := &Server{
		client:       &http.Client{Transport: transport, CheckRedirect: noRedirect},
		streamClient: &http.Client{Transport: transport.Clone(), CheckRedirect: noRedirect},
		log:          logger,
		mux:          http.NewServeMux(),
	}
	st, err := buildState(cfg)
	if err != nil {
		panic(err)
	}
	s.state.Store(st)
	s.routes()
	return s
}

func buildState(cfg config.Config) (*runtimeState, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	pools := make(map[string]*credentials.Pool, len(cfg.Providers))
	adapters := make(map[string]provider.Adapter, len(cfg.Providers))
	for name, p := range cfg.Providers {
		pools[name] = credentials.New(p.APIKeys)
		adapters[name] = provider.New(p.Kind)
	}
	return &runtimeState{cfg: cfg, router: router.New(cfg), pools: pools, adapters: adapters}, nil
}

func (s *Server) Reload(cfg config.Config) error {
	old := s.state.Load()
	if old != nil && cfg.Listen != old.cfg.Listen {
		return fmt.Errorf("listen address change requires restart: %s -> %s", old.cfg.Listen, cfg.Listen)
	}
	next, err := buildState(cfg)
	if err != nil {
		return err
	}
	s.state.Store(next)
	s.metrics.Reloads.Add(1)
	s.log.Info("configuration reloaded", "providers", len(cfg.Providers), "models", len(cfg.Models))
	return nil
}

func (s *Server) Config() config.Config { return s.state.Load().cfg }

var noRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.ready)
	s.mux.HandleFunc("GET /metrics", s.metricsHandler)
	s.mux.HandleFunc("GET /v1/models", s.proxyAuth(s.models))
	s.mux.HandleFunc("POST /v1/messages", s.proxyAuth(s.anthropicMessages))
	s.mux.HandleFunc("POST /v1/messages/count_tokens", s.proxyAuth(s.anthropicCountTokens))
	s.mux.Handle("/v1/", s.proxyAuth(http.HandlerFunc(s.proxy)))
}

func (s *Server) proxyAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := s.state.Load()
		if st.cfg.ProxyToken == "" {
			next(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(got) != len(st.cfg.ProxyToken) || subtle.ConstantTimeCompare([]byte(got), []byte(st.cfg.ProxyToken)) != 1 {
			http.Error(w, "alvus-core: unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) adminAuth(r *http.Request) bool {
	st := s.state.Load()
	if st.cfg.AdminToken == "" {
		return true
	}
	got := r.Header.Get("X-Alvus-Admin-Token")
	return len(got) == len(st.cfg.AdminToken) && subtle.ConstantTimeCompare([]byte(got), []byte(st.cfg.AdminToken)) == 1
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	st := s.state.Load()
	now := time.Now()
	providers := map[string]int{}
	ready := true
	for name, p := range st.pools {
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
	st := s.state.Load()
	writeJSON(w, http.StatusOK, map[string]any{
		"requests":          s.metrics.Requests.Load(),
		"upstream_attempts": s.metrics.Attempts.Load(),
		"fallbacks":         s.metrics.Fallbacks.Load(),
		"errors":            s.metrics.Errors.Load(),
		"reloads":           s.metrics.Reloads.Load(),
		"circuits":          st.router.Snapshot(time.Now()),
	})
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	st := s.state.Load()
	data := make([]map[string]any, 0, len(st.cfg.Models)+len(st.cfg.Routes))
	for name := range st.cfg.Models {
		data = append(data, map[string]any{"id": name, "object": "model", "owned_by": "alvus-core"})
	}
	for name := range st.cfg.Routes {
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
	st := s.state.Load()
	s.metrics.Requests.Add(1)
	body, err := readLimitedBody(w, r, st.cfg.RequestBodyLimitBytes)
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
	resp, err := s.routeRequest(st, r, r.URL.Path, body, probe.Stream, probe.Model)
	if err != nil {
		s.metrics.Errors.Add(1)
		http.Error(w, "alvus-core: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.copyResponse(w, resp, probe.Stream)
}

func (s *Server) routeRequest(st *runtimeState, original *http.Request, targetPath string, body []byte, stream bool, requestedModel string) (*http.Response, error) {
	candidates, err := st.router.Candidates(requestedModel, time.Now())
	if err != nil || len(candidates) == 0 {
		return nil, fmt.Errorf("%s", errString(err, "no healthy route"))
	}

	for ci, candidate := range candidates {
		if ci > 0 {
			s.metrics.Fallbacks.Add(1)
		}
		pool := st.pools[candidate.Provider]
		adapter := st.adapters[candidate.Provider]
		if pool == nil || adapter == nil {
			continue
		}
		maxKeyAttempts := pool.Len()
		for keyAttempt := 0; keyAttempt < maxKeyAttempts; keyAttempt++ {
			idx, key, err := pool.Next(time.Now())
			if err != nil {
				break
			}
			s.metrics.Attempts.Add(1)
			resp, err := s.doAttempt(st, original, targetPath, body, stream, candidate, key, adapter)
			if err != nil {
				pool.Cooldown(idx, time.Now().Add(5*time.Second))
				s.log.Warn("upstream request failed", "provider", candidate.Provider, "model", candidate.Alias, "error", err)
				continue
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				st.router.Success(candidate.Alias)
				return resp, nil
			}

			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
			resp.Body.Close()
			decision := adapter.Classify(resp.StatusCode, errBody, resp.Header)
			s.log.Warn("upstream rejected request", "provider", candidate.Provider, "kind", adapter.Kind(), "model", candidate.Alias, "status", resp.StatusCode, "action", decision.Action)
			switch decision.Action {
			case provider.DisableKey:
				pool.Disable(idx)
				continue
			case provider.RetryKey:
				pool.Cooldown(idx, time.Now().Add(maxDuration(decision.RetryAfter, 10*time.Second)))
				continue
			case provider.FallbackModel:
				pool.Cooldown(idx, time.Now().Add(minPositive(decision.RetryAfter, 10*time.Second)))
				st.router.Failure(candidate.Alias, strconv.Itoa(resp.StatusCode), time.Now(), decision.RetryAfter)
				keyAttempt = maxKeyAttempts
				continue
			case provider.SkipModel:
				keyAttempt = maxKeyAttempts
				continue
			default:
				resp.Body = io.NopCloser(bytes.NewReader(errBody))
				return resp, nil
			}
		}
	}
	return nil, errors.New("all routes exhausted")
}

func (s *Server) doAttempt(st *runtimeState, original *http.Request, targetPath string, body []byte, stream bool, c router.Candidate, key string, adapter provider.Adapter) (*http.Response, error) {
	p := st.cfg.Providers[c.Provider]
	target, err := targetURL(p.BaseURL, targetPath, original.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	patched, err := patchModelRequest(body, c.UpstreamModel, st.cfg.Models[c.Alias].Params)
	if err != nil {
		return nil, err
	}

	ctx := original.Context()
	var cancel context.CancelFunc
	if !stream {
		ctx, cancel = context.WithTimeout(ctx, st.cfg.RequestTimeout.Duration)
	}
	req, err := http.NewRequestWithContext(ctx, original.Method, target, bytes.NewReader(patched))
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	copyRequestHeaders(req.Header, original.Header)
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	adapter.Authorize(req, key)
	req.Header.Set("Content-Length", strconv.Itoa(len(patched)))
	client := s.client
	if stream {
		client = s.streamClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	if cancel != nil {
		resp.Body = &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
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

func patchModelRequest(body []byte, model string, defaults map[string]any) ([]byte, error) {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	v["model"] = model
	for key, value := range defaults {
		if _, exists := v[key]; !exists {
			v[key] = value
		}
	}
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

func copyRequestHeaders(dst, src http.Header) {
	for k, vals := range src {
		if hopByHop(k) || strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Content-Length") || strings.HasPrefix(strings.ToLower(k), "x-api-key") {
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
