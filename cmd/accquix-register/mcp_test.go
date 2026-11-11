package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/accquix/register/internal/config"
)

// TestMCPStdio drives the stdio adapter with a real MCP client against a fake instance: every
// tool call becomes a REST call with the key, and the database is never involved.
func TestMCPStdio(t *testing.T) {
	var paths []string
	inst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer adapter-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/capabilities":
			_, _ = io.WriteString(w, `{"apiVersion":"1.0.0","version":"9.9.9","anonymous":false,"taxpayerLookup":true,"search":{"modes":["default"],"limitMax":20},"fuzzy":{"enabled":false,"note":""},"rate":{"perSecond":20,"burst":40},"mcp":{"enabled":false}}`)
		case "/v1/search":
			_, _ = io.WriteString(w, `{"mode":"default","results":[{"ico":"99900001","name":"Zlatá Rybka, s.r.o.","kind":"legal_person","active":true}],"attribution":["Zdroj údajov: Finančná správa SR"]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer inst.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := config.Config{MCPKey: "adapter-key"}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runMCP(ctx, cfg, []string{"--url", inst.URL}, inR, outW, slog.New(slog.NewTextHandler(io.Discard, nil)))
		_ = outW.Close()
	}()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := cs.InitializeResult().ServerInfo.Version; v != "9.9.9" {
		t.Fatalf("server version from the instance: %q", v)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_subjects", Arguments: map[string]any{"q": "zlata"}})
	if err != nil || res.IsError {
		t.Fatalf("search: %v %+v", err, res)
	}
	if got := paths[len(paths)-1]; got != "/v1/search?q=zlata" {
		t.Fatalf("REST call: %q", got)
	}
	_ = cs.Close()
	_ = inW.Close()
	cancel()
	<-done
}

func TestMCPStdioRefusesBadSetup(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := runMCP(ctx, config.Config{}, nil, strings.NewReader(""), io.Discard, log); err == nil {
		t.Fatal("mcp without --url must fail")
	}
	if err := runMCP(ctx, config.Config{}, []string{"--url", "ftp://x"}, strings.NewReader(""), io.Discard, log); err == nil {
		t.Fatal("mcp with a non-http URL must fail")
	}
	inst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer inst.Close()
	err := runMCP(ctx, config.Config{}, []string{"--url", inst.URL}, strings.NewReader(""), io.Discard, log)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a refused key must fail before serving: %v", err)
	}
}
