package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Keys maps the SHA-256 (hex) of an API key to its name. Only hashes are configured.
type Keys map[string]string

// ParseKeys reads "name:sha256hex,name2:sha256hex".
func ParseKeys(s string) (Keys, error) {
	keys := Keys{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, hash, ok := strings.Cut(part, ":")
		hash = strings.ToLower(strings.TrimSpace(hash))
		if b, err := hex.DecodeString(hash); !ok || name == "" || err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("REGISTER_API_KEYS: entry %q: want name:sha256hex", name)
		}
		keys[hash] = strings.TrimSpace(name)
	}
	return keys, nil
}

// HashKey returns the hex SHA-256 of a key, as configured in REGISTER_API_KEYS.
func HashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

type principalKey struct{}

// holderKey carries a *Principal set by accessLog, so the outer log sees the caller.
type holderKey struct{}

// Principal identifies the caller for limits and policy.
type Principal struct {
	Name          string // the key name, or "anonymous"
	Authenticated bool
	Bucket        string // rate-limit bucket: key name, or client IP for anonymous callers
}

func principal(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

var publicPaths = map[string]bool{"/livez": true, "/readyz": true, "/openapi.json": true, "/openapi.yaml": true, "/openapi-3.0.json": true, "/openapi-3.0.yaml": true}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authenticate checks the Bearer key. A wrong key is always rejected, also in anonymous mode.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPaths[r.URL.Path] || strings.HasPrefix(r.URL.Path, "/schemas/") {
			next.ServeHTTP(w, r)
			return
		}
		// A tool call of the MCP adapter: the /mcp request was already authenticated and charged.
		if p, ok := r.Context().Value(internalKey{}).(Principal); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
			return
		}
		h := r.Header.Get("Authorization")
		var p Principal
		switch {
		case h == "" && s.cfg.AllowAnonymous:
			p = Principal{Name: "anonymous", Bucket: "ip:" + clientIP(r)}
		case strings.HasPrefix(h, "Bearer "):
			sum := HashKey(strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")))
			name := ""
			for hash, n := range s.keys {
				if subtle.ConstantTimeCompare([]byte(hash), []byte(sum)) == 1 {
					name = n
				}
			}
			if name == "" {
				writeProblem(w, problem(http.StatusUnauthorized, "unauthorized", "unknown API key"), 0)
				return
			}
			p = Principal{Name: name, Authenticated: true, Bucket: "key:" + name}
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="accquix-register"`)
			writeProblem(w, problem(http.StatusUnauthorized, "unauthorized", "an API key is required: Authorization: Bearer <key>"), 0)
			return
		}
		if hold, ok := r.Context().Value(holderKey{}).(*Principal); ok {
			*hold = p
		}
		if ok, wait := s.limiter.allow(p.Bucket); !ok {
			writeProblem(w, problem(http.StatusTooManyRequests, "rate_limited", "too many requests"), wait)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// limiter is an in-process token bucket per caller (per instance).
type limiter struct {
	rate  float64 // tokens per second
	burst float64
	mu    sync.Mutex
	b     map[string]*bucket
	now   func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate float64, burst int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), b: map[string]*bucket{}, now: time.Now}
}

// allow takes one token; when none is left it returns the wait until the next one.
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.b[key]
	if b == nil {
		if len(l.b) > 50_000 { // drop full buckets of idle callers
			for k, v := range l.b {
				if v.tokens+now.Sub(v.last).Seconds()*l.rate >= l.burst {
					delete(l.b, k)
				}
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.b[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// statusWriter records the status for the access log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(s int) {
	w.status = s
	w.ResponseWriter.WriteHeader(s)
}

// accessLog logs the route pattern, status, duration and caller name: never the query string,
// the identifier in the path or any data (personal-data minimisation).
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(internalKey{}) != nil { // logged once, as the /mcp request and its tool line
			next.ServeHTTP(w, r)
			return
		}
		t0 := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		hold := &Principal{}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), holderKey{}, hold)))
		route := r.URL.Path
		if strings.HasPrefix(route, "/v1/lookup/") {
			route = "/v1/lookup/{id}"
		}
		s.log.Info("request", "method", r.Method, "route", route, "status", sw.status,
			"duration_ms", time.Since(t0).Milliseconds(), "caller", hold.Name)
	})
}
