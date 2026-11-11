package api

import (
	"net/http"
	"strings"
	"time"
)

// Admission control (D39). The API admits at most MaxInFlight data requests at once; that is at
// most the size of its connection pool, so an admitted request never waits for a connection.
// Without it, overload made requests queue for a connection for most of their timeout and then
// time out half-done, and the throughput collapsed to near zero (managed PostgreSQL test,
// 2026-11-11). With it, the instance keeps serving at its capacity and refuses the excess at once.

// DefaultAdmitWait is how long a request may wait for a free slot before it is refused.
const DefaultAdmitWait = 250 * time.Millisecond

// admitted reports whether a path takes a slot: the data endpoints, which query the database.
// /v1/capabilities answers from memory; /mcp itself does not, and each tool call it makes comes
// back through the router as a /v1 request and takes its slot there.
func admitted(path string) bool {
	return strings.HasPrefix(path, "/v1/") && path != "/v1/capabilities"
}

// admission runs after authentication and the per-caller rate limit: a caller over its own quota
// gets 429 without taking a slot; a full instance answers 503 reason=overloaded.
func (s *Server) admission(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !admitted(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case s.slots <- struct{}{}:
		default:
			t := time.NewTimer(s.cfg.AdmitWait)
			select {
			case s.slots <- struct{}{}:
				t.Stop()
			case <-t.C:
				p := unavailable("overloaded", "the instance is at its capacity; retry after the Retry-After interval")
				writeProblem(w, p, time.Second)
				return
			case <-r.Context().Done():
				t.Stop()
				return
			}
		}
		defer func() { <-s.slots }()
		next.ServeHTTP(w, r)
	})
}
