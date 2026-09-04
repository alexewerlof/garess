// Package mcp exposes MCP (Model Context Protocol) servers configured in
// config.toml (`[[mcp_servers]]`) as ADK toolsets, so the agent can call
// remote or subprocess tools alongside the built-ins.
//
// Three transports are supported, selected per server by
// config.MCPServer.Transport:
//
//   - stdio: spawn a local command (config.Command/Args/Env) and speak MCP
//     over its stdin/stdout (go-sdk mcp.CommandTransport).
//   - sse: connect to a Server-Sent Events endpoint (config.URL), the
//     transport many Docker-hosted servers expose (e.g. Crawl4AI's /mcp/sse).
//   - http: connect to a streamable HTTP endpoint (config.URL).
//
// Auth: HTTP requests carry config.Headers verbatim; when the
// GARESS_MCP_<NAME>_TOKEN environment variable is set (NAME = config name
// uppercased, non-alphanumerics -> '_') it wins over an inline Authorization
// header, mirroring how GA_RESS_API_KEY overrides api_key.
//
// Failure semantics: ADK resolves toolset tools at the start of every run and
// an error there aborts the whole turn, so Server.Tools degrades gracefully —
// an unreachable server logs a warning and yields zero tools for that turn
// instead of failing the run. Server.Status reports the last discovery error
// (used by `garess doctor` and logs). MCP tool calls ride the same
// allow/ask/deny policy as the built-ins (deny via the harness DenyCallback,
// ask via the per-toolset RequireConfirmationProvider).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"garess/internal/config"
	"garess/internal/tools"
)

// tokenEnvPrefix is the prefix of the per-server bearer-token env override:
// GARESS_MCP_<NAME>_TOKEN.
const tokenEnvPrefix = "GARESS_MCP_"

// BuildToolsets converts configured MCP servers into one ADK toolset each.
// An empty list yields an empty result, so the harness pays no overhead when
// no MCP servers are configured.
func BuildToolsets(servers []config.MCPServer, policy tools.Policy) ([]tool.Toolset, error) {
	out := make([]tool.Toolset, 0, len(servers))
	for _, s := range servers {
		ts, err := New(s, policy)
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", s.Name, err)
		}
		out = append(out, ts)
	}
	return out, nil
}

// New builds a single ADK toolset for one configured MCP server. The server
// is not contacted here — the connection happens lazily on the first tool
// discovery during a run.
func New(cfg config.MCPServer, policy tools.Policy) (tool.Toolset, error) {
	transport, err := transportFor(cfg)
	if err != nil {
		return nil, err
	}
	inner, err := mcptoolset.New(mcptoolset.Config{
		Transport:                   transport,
		RequireConfirmationProvider: askProvider(policy),
	})
	if err != nil {
		return nil, fmt.Errorf("build toolset: %w", err)
	}
	return &Server{name: cfg.Name, cfg: cfg, inner: inner}, nil
}

// Server wraps one MCP toolset so a discovery failure degrades to zero tools
// instead of aborting the run (ADK treats a toolset Tools() error as fatal).
// It implements tool.Toolset.
type Server struct {
	name  string
	cfg   config.MCPServer
	inner tool.Toolset

	mu      sync.Mutex
	lastErr error
}

// Name implements tool.Toolset (used in ADK error messages and logs).
func (s *Server) Name() string { return s.name }

// Tools implements tool.Toolset: it discovers the server's tools, degrading
// gracefully when the server cannot be reached.
func (s *Server) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	ts, err := s.inner.Tools(ctx)
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
	if err != nil {
		slog.Warn("mcp: skipping unreachable server for this turn",
			"server", s.name, "transport", s.cfg.Transport, "err", err)
		return nil, nil
	}
	return ts, nil
}

// Status returns the error from the most recent tool discovery, or nil when
// the last discovery succeeded (or nothing has run yet).
func (s *Server) Status() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// transportFor builds the go-sdk transport for a configured server, applying
// headers and the bearer-token env override to HTTP transports.
func transportFor(cfg config.MCPServer) (mcp.Transport, error) {
	switch cfg.Transport {
	case config.MCPTransportStdio:
		cmd := exec.Command(cfg.Command, cfg.Args...)
		if len(cfg.Env) > 0 {
			cmd.Env = append(os.Environ(), envSlice(cfg.Env)...)
		}
		return &mcp.CommandTransport{Command: cmd}, nil
	case config.MCPTransportSSE:
		return &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: httpClientFor(cfg)}, nil
	case config.MCPTransportHTTP:
		return &mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: httpClientFor(cfg)}, nil
	default:
		return nil, fmt.Errorf("unsupported transport %q", cfg.Transport)
	}
}

// envSlice renders config env overrides as KEY=VALUE pairs (later entries win
// when appended to the parent environment).
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// httpClientFor returns an *http.Client that stamps the server's headers onto
// every request. The GARESS_MCP_<NAME>_TOKEN env var, when set, overrides an
// inline Authorization header. With no headers it returns nil so the
// transport falls back to its default client.
func httpClientFor(cfg config.MCPServer) *http.Client {
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	if tok := bearerToken(cfg.Name); tok != "" {
		headers["Authorization"] = "Bearer " + tok
	}
	if len(headers) == 0 {
		return nil
	}
	return &http.Client{Transport: &headerTransport{headers: headers}}
}

// bearerToken resolves the GARESS_MCP_<NAME>_TOKEN override for a server.
func bearerToken(name string) string {
	key := tokenEnvPrefix + envKey(name) + "_TOKEN"
	return os.Getenv(key)
}

// envKey normalizes a config name into an environment-variable fragment:
// uppercased; non-alphanumeric runes become '_'.
func envKey(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - ('a' - 'A')
		case (r >= 'A' && r <= 'Z') || unicode.IsDigit(r):
			return r
		default:
			return '_'
		}
	}, name)
}

// headerTransport adds a fixed set of headers to every request.
type headerTransport struct {
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// askProvider builds the mcptoolset RequireConfirmationProvider for one
// server: it asks for confirmation exactly when the policy decision for the
// tool call is ApprovalAsk. (Deny is enforced separately by the harness
// DenyCallback, which runs before every tool.)
func askProvider(policy tools.Policy) tool.ConfirmationProvider {
	return func(name string, in any) bool {
		args, err := asMap(in)
		if err != nil {
			return false
		}
		return policy.DecisionFor(name, args) == tools.ApprovalAsk
	}
}

// asMap converts a decoded tool input (usually already a map) to the
// map[string]any the policy matcher expects.
func asMap(in any) (map[string]any, error) {
	if m, ok := in.(map[string]any); ok {
		return m, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// ListTools connects to a configured MCP server and returns the names of the
// tools it exposes. It is used by `garess doctor` to verify connectivity and
// show what the agent would get; it uses a plain go-sdk client, so no ADK
// invocation context is needed.
func ListTools(ctx context.Context, cfg config.MCPServer) ([]string, error) {
	transport, err := transportFor(cfg)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "garess-doctor", Version: "1"}, nil)
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	res, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(res.Tools))
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	return names, nil
}
