package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/accquix/register/internal/api"
	"github.com/accquix/register/internal/testkit"
)

// bearer adds an API key to every request of the MCP client.
type bearer struct{ key string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.key != "" {
		r.Header.Set("Authorization", "Bearer "+b.key)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, e env, apiKey string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: e.srv.URL + api.MCPPath, HTTPClient: &http.Client{Transport: bearer{apiKey}}, MaxRetries: -1,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text + "\n")
		}
	}
	return sb.String()
}

// restBody fetches the REST body for comparison, as raw JSON.
func restBody(t *testing.T, e env, p string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+p, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

func sameJSON(t *testing.T, what string, a, b []byte) {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	if !bytes.Equal(xa, ya) {
		t.Fatalf("%s: MCP result differs from the REST body\nmcp:  %s\nrest: %s", what, xa, ya)
	}
}

func structured(t *testing.T, res *mcp.CallToolResult) []byte {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// conforms checks a result against the tool's declared output schema, as a client may.
func conforms(t *testing.T, tools map[string]*mcp.Tool, name string, body []byte) {
	t.Helper()
	raw, _ := json.Marshal(tools[name].OutputSchema)
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	_ = json.Unmarshal(body, &v)
	if err := rs.Validate(v); err != nil {
		t.Fatalf("%s: result does not match its output schema: %v", name, err)
	}
}

func TestMCP(t *testing.T) {
	dsn := testkit.Database(t)
	populate(t, dsn)
	e := start(t, dsn, func(c *api.Config) { c.MCP = true })
	cs, err := connect(t, e, key)
	if err != nil {
		t.Fatal(err)
	}

	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tl := range lt.Tools {
		tools[tl.Name] = tl
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Fatalf("%s is not marked read-only", tl.Name)
		}
	}
	if len(tools) != 3 || tools["search_subjects"] == nil || tools["lookup_subject"] == nil || tools["get_data_freshness"] == nil {
		t.Fatalf("tools: %v", tools)
	}
	if init := cs.InitializeResult(); !strings.Contains(init.Instructions, "never report it as") {
		t.Fatalf("instructions: %q", init.Instructions)
	}

	t.Run("search is the REST body", func(t *testing.T) {
		res := callTool(t, cs, "search_subjects", map[string]any{"q": "zlata ryb"})
		if res.IsError {
			t.Fatal(text(res))
		}
		got := structured(t, res)
		sameJSON(t, "search", got, restBody(t, e, "/v1/search?q="+q("zlata ryb")))
		conforms(t, tools, "search_subjects", got)
		if txt := text(res); !strings.Contains(txt, "Zlatá Rybka") || !strings.Contains(txt, "Attribution:") || !strings.Contains(txt, "lookup_subject") {
			t.Fatalf("text: %s", txt)
		}
	})

	t.Run("lookup is the REST body, with the VAT rules in words", func(t *testing.T) {
		res := callTool(t, cs, "lookup_subject", map[string]any{"id": "99900004"})
		if res.IsError {
			t.Fatal(text(res))
		}
		got := structured(t, res)
		sameJSON(t, "lookup", got, restBody(t, e, "/v1/lookup/99900004"))
		conforms(t, tools, "lookup_subject", got)
		if txt := text(res); !strings.Contains(txt, "§7a (not a VAT payer)") {
			t.Fatalf("§7a must read as not a payer: %s", txt)
		}
		res = callTool(t, cs, "lookup_subject", map[string]any{"id": "SK2099900100"})
		var b api.LookupBody
		_ = json.Unmarshal(structured(t, res), &b)
		if len(b.Subjects) != 2 {
			t.Fatalf("a VAT group has two subjects: %s", text(res))
		}
	})

	t.Run("freshness", func(t *testing.T) {
		res := callTool(t, cs, "get_data_freshness", nil)
		if res.IsError {
			t.Fatal(text(res))
		}
		got := structured(t, res)
		sameJSON(t, "sources", got, restBody(t, e, "/v1/sources"))
		conforms(t, tools, "get_data_freshness", got)
	})

	t.Run("outcomes are tool errors in words", func(t *testing.T) {
		res := callTool(t, cs, "lookup_subject", map[string]any{"id": "99900099"})
		if !res.IsError || !strings.HasPrefix(text(res), "Not found: ") || !strings.Contains(text(res), `"code":"not_found"`) {
			t.Fatalf("not found: %v %s", res.IsError, text(res))
		}
		res = callTool(t, cs, "lookup_subject", map[string]any{"id": "12"})
		if !res.IsError || !strings.Contains(text(res), "invalid_identifier") {
			t.Fatalf("invalid: %s", text(res))
		}
		res = callTool(t, cs, "search_subjects", map[string]any{"q": "rybka", "mode": "nonsense"})
		if !res.IsError {
			t.Fatalf("an unknown mode must be refused: %s", text(res))
		}
	})

	t.Run("logs hold no query or identifier", func(t *testing.T) {
		le := start(t, dsn, func(c *api.Config) { c.MCP = true })
		lcs, err := connect(t, le, key)
		if err != nil {
			t.Fatal(err)
		}
		callTool(t, lcs, "search_subjects", map[string]any{"q": "zlata rybka"})
		callTool(t, lcs, "lookup_subject", map[string]any{"id": "99900004"})
		callTool(t, lcs, "lookup_subject", map[string]any{"id": "SK2099900100"})
		logs := le.logs.String()
		for _, secret := range []string{"99900004", "SK2099900100", "zlata", "rybka"} {
			if strings.Contains(strings.ToLower(logs), strings.ToLower(secret)) {
				t.Fatalf("the logs contain %q:\n%s", secret, logs)
			}
		}
		if !strings.Contains(logs, `"tool":"lookup_subject"`) || !strings.Contains(logs, `"route":"/mcp"`) {
			t.Fatalf("expected the /mcp request and the tool line:\n%s", logs)
		}
		if strings.Contains(logs, `"route":"/v1/`) {
			t.Fatalf("a tool's in-process call must not be logged as a second request:\n%s", logs)
		}
	})

	t.Run("no key: HTTP 401, not a tool result", func(t *testing.T) {
		if _, err := connect(t, e, ""); err == nil || !strings.Contains(err.Error(), "401") && !strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
			t.Fatalf("connect without a key: %v", err)
		}
		if _, err := connect(t, e, "wrong-key"); err == nil {
			t.Fatal("connect with a wrong key must fail")
		}
	})

	t.Run("a cross-origin browser request is refused", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+api.MCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-origin: %d", resp.StatusCode)
		}
	})

	t.Run("anonymous instance: no natural-person lookup (D26)", func(t *testing.T) {
		anon := start(t, dsn, func(c *api.Config) { c.MCP, c.AllowAnonymous = true, true })
		acs, err := connect(t, anon, "")
		if err != nil {
			t.Fatal(err)
		}
		res := callTool(t, acs, "lookup_subject", map[string]any{"id": "2099900050"})
		if !res.IsError || !strings.HasPrefix(text(res), "Forbidden") {
			t.Fatalf("natural person, anonymous: %s", text(res))
		}
	})

	t.Run("fuzzy is offered only when enabled", func(t *testing.T) {
		mode := func(e env) []any {
			cs, err := connect(t, e, key)
			if err != nil {
				t.Fatal(err)
			}
			lt, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tl := range lt.Tools {
				if tl.Name == "search_subjects" {
					raw, _ := json.Marshal(tl.InputSchema)
					var s jsonschema.Schema
					_ = json.Unmarshal(raw, &s)
					return s.Properties["mode"].Enum
				}
			}
			return nil
		}
		if got := mode(e); len(got) != 2 {
			t.Fatalf("fuzzy on: %v", got)
		}
		off := start(t, dsn, func(c *api.Config) { c.MCP, c.Fuzzy = true, false })
		if got := mode(off); len(got) != 1 || got[0] != "default" {
			t.Fatalf("fuzzy off: %v", got)
		}
	})

	t.Run("REST and MCP share one rate limit", func(t *testing.T) {
		lim := start(t, dsn, func(c *api.Config) { c.MCP, c.RatePerSecond, c.RateBurst = true, 0.001, 3 })
		for range 3 {
			lim.get(t, "/v1/sources", true)
		}
		if _, err := connect(t, lim, key); err == nil {
			t.Fatal("MCP must be rate-limited after the REST calls used the bucket")
		}
	})

	t.Run("off by default", func(t *testing.T) {
		off := start(t, dsn, nil)
		if code, _, _ := off.get(t, api.MCPPath, true); code != http.StatusNotFound {
			t.Fatalf("/mcp with REGISTER_MCP off: %d", code)
		}
		if _, _, m := off.get(t, "/v1/capabilities", true); path(m, "mcp", "enabled") != false {
			t.Fatalf("capabilities: %v", m["mcp"])
		}
		if _, _, m := e.get(t, "/v1/capabilities", true); path(m, "mcp", "path") != api.MCPPath {
			t.Fatalf("capabilities: %v", m["mcp"])
		}
	})
}

func TestMCPBootstrapping(t *testing.T) {
	e := start(t, testkit.Database(t), func(c *api.Config) { c.MCP = true })
	cs, err := connect(t, e, key)
	if err != nil {
		t.Fatal(err)
	}
	res := callTool(t, cs, "lookup_subject", map[string]any{"id": "99900001"})
	if !res.IsError || !strings.HasPrefix(text(res), "Unknown: ") || !strings.Contains(text(res), "NOT a 'not found'") {
		t.Fatalf("a mirror still being built must not say not found: %s", text(res))
	}
}

// TestMCPTools keeps the committed tool list (api/mcp-tools.json) in step with the code, like
// the OpenAPI document: the tool names, descriptions and schemas are a public contract (D36).
func TestMCPTools(t *testing.T) {
	got, err := api.MCPToolsJSON()
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("../../api/mcp-tools.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, committed) {
		t.Fatal("api/mcp-tools.json is out of date: run `go run ./cmd/accquix-register mcp-tools > api/mcp-tools.json`")
	}
}
