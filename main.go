// Command wally-preview is a small, SSRF-hardened Matrix URL-preview service.
//
// It sits in front of a Matrix homeserver and answers the standard
// `/_matrix/.../preview_url` endpoints itself, so the homeserver process never
// fetches attacker-controlled content. It speaks the unmodified client-server
// API, so ANY Matrix client benefits — no client-side feature required.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Listen         string
	Homeserver     string
	UploadToken    string
	MaxHTMLBytes   int64
	MaxImageBytes  int64
	UserAgent      string
	AllowDomains   []string // empty ⇒ allow any public host (SSRF guard is the safety net)
	CacheTTL       time.Duration
	NegativeTTL    time.Duration
	RequestTimeout time.Duration
	MaxConcurrency int
}

type server struct {
	cfg       Config
	fetch     *http.Client // SSRF-guarded — outbound to the open internet
	mx        *matrixClient
	pcache    *ttlCache[*previewResult]
	negcache  *ttlCache[string]
	authcache *ttlCache[string]
	sem       chan struct{}
}

func main() {
	cfg := loadConfig()
	s := &server{
		cfg:   cfg,
		fetch: guardedClient(cfg.RequestTimeout),
		mx: &matrixClient{
			homeserver:  cfg.Homeserver,
			uploadToken: cfg.UploadToken,
			hc:          &http.Client{Timeout: 30 * time.Second},
		},
		pcache:    newTTLCache[*previewResult](),
		negcache:  newTTLCache[string](),
		authcache: newTTLCache[string](),
		sem:       make(chan struct{}, cfg.MaxConcurrency),
	}

	mux := http.NewServeMux()
	for _, p := range []string{
		"/_matrix/media/v3/preview_url",       // legacy media (what matrix-js-sdk uses today)
		"/_matrix/media/r0/preview_url",       // older legacy
		"/_matrix/client/v1/media/preview_url", // authenticated media (Element / newer SDKs)
	} {
		mux.HandleFunc(p, s.handlePreview)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           logging(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	idleClosed := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(idleClosed)
	}()

	log.Printf("wally-preview listening on %s → homeserver %s (allowlist: %s)",
		cfg.Listen, cfg.Homeserver, allowlistDesc(cfg.AllowDomains))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
	<-idleClosed
}

func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpErr(w, http.StatusMethodNotAllowed, "M_UNRECOGNIZED", "Only GET is supported")
		return
	}

	// Authz: a valid Matrix access token is required, so the shim is not an open
	// fetch proxy. whoami results are cached briefly to avoid hammering the HS.
	token := bearer(r)
	if token == "" {
		httpErr(w, http.StatusUnauthorized, "M_MISSING_TOKEN", "Missing access token")
		return
	}
	if _, ok := s.authcache.get(token); !ok {
		uid, err := s.mx.whoami(r.Context(), token)
		if err != nil {
			httpErr(w, http.StatusUnauthorized, "M_UNKNOWN_TOKEN", "Invalid access token")
			return
		}
		s.authcache.put(token, uid, 5*time.Minute)
	}

	target := r.URL.Query().Get("url")
	if target == "" {
		httpErr(w, http.StatusBadRequest, "M_MISSING_PARAM", "Missing 'url' parameter")
		return
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		httpErr(w, http.StatusBadRequest, "M_INVALID_PARAM", "Invalid 'url' parameter")
		return
	}
	if !s.domainAllowed(u.Hostname()) {
		httpErr(w, http.StatusForbidden, "M_FORBIDDEN", "Previews are not allowed for this domain")
		return
	}

	if pr, ok := s.pcache.get(target); ok {
		writeJSON(w, http.StatusOK, pr)
		return
	}
	if _, ok := s.negcache.get(target); ok {
		writeJSON(w, http.StatusOK, &previewResult{}) // recently failed — serve empty, don't refetch
		return
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	}

	pr, err := s.buildPreview(r.Context(), u)
	if errors.Is(err, errNoPreview) {
		empty := &previewResult{}
		s.pcache.put(target, empty, s.cfg.CacheTTL)
		writeJSON(w, http.StatusOK, empty)
		return
	}
	if err != nil {
		s.negcache.put(target, "fetch failed", s.cfg.NegativeTTL)
		httpErr(w, http.StatusBadGateway, "M_UNKNOWN", "Could not generate a preview")
		return
	}
	s.pcache.put(target, pr, s.cfg.CacheTTL)
	writeJSON(w, http.StatusOK, pr)
}

func (s *server) domainAllowed(host string) bool {
	if len(s.cfg.AllowDomains) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range s.cfg.AllowDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// --- helpers ---------------------------------------------------------------

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return r.URL.Query().Get("access_token") // legacy clients
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, errcode, msg string) {
	writeJSON(w, code, map[string]string{"errcode": errcode, "error": msg})
}

// logging records method, path, the target host (never the token) and status.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		host := ""
		if t := r.URL.Query().Get("url"); t != "" {
			if u, err := url.Parse(t); err == nil {
				host = u.Hostname()
			}
		}
		log.Printf("%s %s host=%q -> %d (%s)", r.Method, r.URL.Path, host, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func allowlistDesc(d []string) string {
	if len(d) == 0 {
		return "any public host"
	}
	return strings.Join(d, ",")
}

// --- config ----------------------------------------------------------------

func loadConfig() Config {
	c := Config{
		Listen:         env("WALLY_PREVIEW_LISTEN", "127.0.0.1:8088"),
		Homeserver:     strings.TrimRight(env("WALLY_PREVIEW_HOMESERVER", "http://127.0.0.1:6167"), "/"),
		UploadToken:    os.Getenv("WALLY_PREVIEW_UPLOAD_TOKEN"),
		MaxHTMLBytes:   envInt64("WALLY_PREVIEW_MAX_HTML_BYTES", 256*1024),
		MaxImageBytes:  envInt64("WALLY_PREVIEW_MAX_IMAGE_BYTES", 5*1024*1024),
		UserAgent:      env("WALLY_PREVIEW_USER_AGENT", "wally-preview/0.1 (+https://github.com/LaPingvino/wally-preview)"),
		CacheTTL:       envDur("WALLY_PREVIEW_CACHE_TTL", time.Hour),
		NegativeTTL:    envDur("WALLY_PREVIEW_NEGATIVE_TTL", 5*time.Minute),
		RequestTimeout: envDur("WALLY_PREVIEW_REQUEST_TIMEOUT", 10*time.Second),
		MaxConcurrency: envInt("WALLY_PREVIEW_MAX_CONCURRENCY", 8),
	}
	if d := os.Getenv("WALLY_PREVIEW_ALLOW_DOMAINS"); d != "" {
		for _, p := range strings.Split(d, ",") {
			if p = strings.TrimSpace(strings.ToLower(p)); p != "" {
				c.AllowDomains = append(c.AllowDomains, p)
			}
		}
	}
	if c.UploadToken == "" {
		log.Fatal("WALLY_PREVIEW_UPLOAD_TOKEN is required (use a dedicated low-privilege Matrix account)")
	}
	if c.MaxConcurrency < 1 {
		c.MaxConcurrency = 1
	}
	return c
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt64(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
