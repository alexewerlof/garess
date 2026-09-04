package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"garess/internal/config"
	"garess/internal/tools"
)

// ---- Fake MCP server helpers (go-sdk in-process, never real networks) ----

type scrapeArgs struct {
	URL string `json:"url" jsonschema:"url to scrape"`
}

func scrapeTool(ctx context.Context, req *mcp.CallToolRequest, args scrapeArgs) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "markdown for " + args.URL}},
	}, nil, nil
}

type crawlArgs struct {
	Targets []string `json:"targets" jsonschema:"urls to crawl"`
}

func crawlTool(ctx context.Context, req *mcp.CallToolRequest, args crawlArgs) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "crawled " + strings.Join(args.Targets, ",")}},
	}, nil, nil
}

// newFakeSSEServer starts an in-process SSE MCP server exposing the scrape and
// crawl tools. requireAuth, when non-empty, rejects requests whose
// Authorization header differs; seenAuth records the last Authorization seen.
func newFakeSSEServer(t *testing.T, requireAuth string, seenAuth *string) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "scrape", Description: "scrape one URL to markdown"}, scrapeTool)
	mcp.AddTool(server, &mcp.Tool{Name: "crawl", Description: "crawl many URLs"}, crawlTool)
	handler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireAuth != "" && r.Header.Get("Authorization") != requireAuth {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if seenAuth != nil {
			*seenAuth = r.Header.Get("Authorization")
		}
		handler.ServeHTTP(w, r)
	}))
	// The toolset keeps its SSE stream open for the process lifetime (no
	// session close in the ADK client), so force-close lingering connections
	// before shutting the server down or httptest.Close blocks forever.
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts
}

func fakeCtx(t *testing.T) *agent.StrictContextMock {
	t.Helper()
	ctx := agent.NewStrictContextMock(context.Background())
	return &ctx
}

func sseConfig(name, url string) config.MCPServer {
	return config.MCPServer{Name: name, Transport: config.MCPTransportSSE, URL: url}
}

// ---- Tests ----

func TestBuildToolsetsEmpty(t *testing.T) {
	sets, err := BuildToolsets(nil, tools.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 0 {
		t.Fatalf("got %d toolsets, want 0", len(sets))
	}
}

func TestTransportSelection(t *testing.T) {
	// stdio -> CommandTransport with command/args/env.
	tr, err := transportFor(config.MCPServer{
		Name: "fs", Transport: config.MCPTransportStdio,
		Command: "/usr/bin/npx", Args: []string{"-y", "tool"},
		Env: map[string]string{"A": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ct, ok := tr.(*mcp.CommandTransport)
	if !ok {
		t.Fatalf("stdio transport = %T, want *mcp.CommandTransport", tr)
	}
	if ct.Command.Path != "/usr/bin/npx" || len(ct.Command.Args) != 3 {
		t.Fatalf("command = %+v", ct.Command)
	}
	foundA := false
	for _, e := range ct.Command.Env {
		if e == "A=1" {
			foundA = true
		}
	}
	if !foundA {
		t.Fatalf("env override missing from %v", ct.Command.Env)
	}

	// sse -> SSEClientTransport with endpoint + authed client.
	cfg := config.MCPServer{Name: "s", Transport: config.MCPTransportSSE, URL: "http://h:1/mcp/sse", Headers: map[string]string{"Authorization": "Bearer x"}}
	tr, err = transportFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := tr.(*mcp.SSEClientTransport)
	if !ok {
		t.Fatalf("sse transport = %T, want *mcp.SSEClientTransport", tr)
	}
	if st.Endpoint != cfg.URL || st.HTTPClient == nil {
		t.Fatalf("sse transport = %+v", st)
	}

	// http -> StreamableClientTransport.
	cfg.Transport = config.MCPTransportHTTP
	tr, err = transportFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ht, ok := tr.(*mcp.StreamableClientTransport)
	if !ok {
		t.Fatalf("http transport = %T, want *mcp.StreamableClientTransport", tr)
	}
	if ht.Endpoint != cfg.URL || ht.HTTPClient == nil {
		t.Fatalf("http transport = %+v", ht)
	}

	// No headers -> nil client (transport default), bad transport -> error.
	tr, err = transportFor(sseConfig("s", "http://h:1/mcp/sse"))
	if err != nil {
		t.Fatal(err)
	}
	if st2 := tr.(*mcp.SSEClientTransport); st2.HTTPClient != nil {
		t.Fatalf("expected nil HTTPClient without headers, got %+v", st2.HTTPClient)
	}
	if _, err := transportFor(config.MCPServer{Name: "s", Transport: "carrier-pigeon"}); err == nil {
		t.Fatal("expected error for unsupported transport")
	}
}

func TestEnvKeyAndBearerToken(t *testing.T) {
	if got := envKey("Crawl4AI-Server"); got != "CRAWL4AI_SERVER" {
		t.Fatalf("envKey = %q", got)
	}
	if got := envKey("simple"); got != "SIMPLE" {
		t.Fatalf("envKey = %q", got)
	}
	t.Setenv("GARESS_MCP_MY_SERVER_TOKEN", "secret")
	if got := bearerToken("my-server"); got != "secret" {
		t.Fatalf("bearerToken = %q", got)
	}
	if got := bearerToken("other"); got != "" {
		t.Fatalf("bearerToken = %q, want empty", got)
	}
}

func TestListToolsOverSSE(t *testing.T) {
	ts := newFakeSSEServer(t, "", nil)
	names, err := ListTools(context.Background(), sseConfig("fake", ts.URL))
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("got tools %v, want scrape+crawl", names)
	}
	want := map[string]bool{"scrape": true, "crawl": true}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected tool %q in %v", n, names)
		}
	}
}

func TestToolsetListsToolsOverSSE(t *testing.T) {
	ts := newFakeSSEServer(t, "", nil)
	set, err := New(sseConfig("fake", ts.URL), tools.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	toolsFound, err := set.Tools(fakeCtx(t))
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(toolsFound) != 2 {
		t.Fatalf("got %d tools, want 2: %v", len(toolsFound), namesOf(toolsFound))
	}
	if s := set.(*Server); s.Status() != nil {
		t.Fatalf("Status = %v, want nil after a successful discovery", s.Status())
	}
}

func namesOf(ts []tool.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func TestAuthHeaderSentAndEnvOverridesInline(t *testing.T) {
	var seen string
	ts := newFakeSSEServer(t, "Bearer env-token", &seen)

	// Inline header says "inline"; the env override must win.
	cfg := sseConfig("crawl", ts.URL)
	cfg.Headers = map[string]string{"Authorization": "Bearer inline"}
	t.Setenv("GARESS_MCP_CRAWL_TOKEN", "env-token")

	set, err := New(cfg, tools.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Tools(fakeCtx(t)); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if seen != "Bearer env-token" {
		t.Fatalf("Authorization = %q, want env token override", seen)
	}
}

func TestAuthRequiredWithoutToken(t *testing.T) {
	// A server that requires auth and no token is provided -> discovery
	// degrades gracefully (empty tools, non-nil Status).
	ts := newFakeSSEServer(t, "Bearer needed", nil)
	set, err := New(sseConfig("crawl", ts.URL), tools.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	found, err := set.Tools(fakeCtx(t))
	if err != nil {
		t.Fatalf("Tools must degrade, not error: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("got %d tools, want 0 when unauthorized", len(found))
	}
	s := set.(*Server)
	if s.Status() == nil {
		t.Fatal("Status = nil, want the discovery error")
	}
}

func TestDegradesWhenServerDown(t *testing.T) {
	ts := newFakeSSEServer(t, "", nil)
	url := ts.URL
	ts.Close() // now unreachable

	set, err := New(sseConfig("down", url), tools.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	found, err := set.Tools(fakeCtx(t))
	if err != nil {
		t.Fatalf("Tools must degrade, not error: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("got %d tools, want 0", len(found))
	}
	s := set.(*Server)
	if s.Status() == nil {
		t.Fatal("Status = nil, want connection error")
	}
}

func TestAskProvider(t *testing.T) {
	// Empty policy allows everything -> never ask.
	prov := askProvider(tools.Policy{})
	if prov("scrape", map[string]any{"url": "http://x"}) {
		t.Fatal("empty policy should not ask")
	}

	// Ask rule matches by tool name prefix.
	prov = askProvider(tools.Policy{Ask: []string{"^scrape "}})
	if !prov("scrape", map[string]any{"url": "http://x"}) {
		t.Fatal("ask rule should trigger for scrape")
	}
	if prov("crawl", map[string]any{"urls": []string{"http://x"}}) {
		t.Fatal("ask rule should not trigger for crawl")
	}

	// Non-map args (e.g. struct) still resolve.
	prov = askProvider(tools.Policy{Ask: []string{".*secret.*"}})
	if !prov("bash", map[string]any{"command": "cat secret"}) {
		t.Fatal("ask rule should match on rendered args")
	}
}
