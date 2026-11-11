package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestAdmission: a full instance refuses the excess at once with 503 overloaded and Retry-After,
// keeps serving the admitted requests, and frees the slot when a request ends (D39).
func TestAdmission(t *testing.T) {
	s := &Server{cfg: Config{AdmitWait: 50 * time.Millisecond}, slots: make(chan struct{}, 2)}
	release := make(chan struct{})
	entered := make(chan struct{}, 10)
	h := s.admission(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/search" {
			entered <- struct{}{}
			<-release
		}
		_, _ = w.Write([]byte("ok"))
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	get := func(path string) (*http.Response, error) { return http.Get(srv.URL + path) }

	// two slow requests take both slots
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := get("/v1/search")
			if err != nil {
				t.Error(err)
				return
			}
			codes[i] = resp.StatusCode
			_ = resp.Body.Close()
		}()
	}
	<-entered
	<-entered

	// the third waits AdmitWait, then is refused, quickly and with Retry-After
	t0 := time.Now()
	resp, err := get("/v1/lookup/99900001")
	if err != nil {
		t.Fatal(err)
	}
	var p Problem
	_ = json.NewDecoder(resp.Body).Decode(&p)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || p.Reason != "overloaded" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("want 503 overloaded with Retry-After, got %d %q %q", resp.StatusCode, p.Reason, resp.Header.Get("Retry-After"))
	}
	if d := time.Since(t0); d < 40*time.Millisecond || d > time.Second {
		t.Fatalf("refused after %v, want about AdmitWait", d)
	}

	// paths that do not query the database are not limited
	for _, path := range []string{"/v1/capabilities", "/readyz", "/mcp"} {
		resp, err := get(path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d while the instance is full", path, resp.StatusCode)
		}
	}

	// the admitted requests finish normally, and their slots come back
	close(release)
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("admitted requests: %v", codes)
	}
	resp, err = get("/v1/lookup/99900001")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after release: %d", resp.StatusCode)
	}
}

// TestAdmissionAbsorbsAShortBurst: a request that finds the instance full gets a slot when one
// frees within AdmitWait, instead of being refused.
func TestAdmissionAbsorbsAShortBurst(t *testing.T) {
	s := &Server{cfg: Config{AdmitWait: 500 * time.Millisecond}, slots: make(chan struct{}, 1)}
	h := s.admission(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/v1/search")
			if err != nil {
				t.Error(err)
				return
			}
			codes[i] = resp.StatusCode
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: %d, want 200 (it fits in AdmitWait)", i, c)
		}
	}
}
