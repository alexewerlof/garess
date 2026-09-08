package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	"garess/internal/chat"
	"garess/internal/config"
	"garess/internal/personas"
	"garess/internal/tools"
)

// requestBodyText returns the raw chat-completions request body so tests can
// assert on the persona instruction it carries.
func requestBodyText(r *http.Request) string {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	return string(b)
}

// personaDef builds a small researcher persona for tests.
func personaDef(name, desc, body string, toolNames []string) personas.Persona {
	return personas.Persona{
		Name:        name,
		Description: desc,
		Tools:       toolNames,
		Body:        body,
		Path:        "/test/" + name + ".md",
		Scope:       "project",
	}
}

// TestRunSubagentDelegatesAgenticLoop is the core sub-agent test: the outer
// model calls run_subagent, the persona agent runs its own full agentic loop
// (read_file on a real temp file) against an isolated session, returns its
// final answer, and the outer model finishes. This also validates that a
// nested ADK runner.Run inside a tool handler works (the Phase-0 spike).
func TestRunSubagentDelegatesAgenticLoop(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("the password is hunter2"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		n := reqCount
		mu.Unlock()
		switch n {
		case 1: // outer model decides to delegate
			sse(w, toolCallChunk(RunSubagentTool, "call_o1",
				`{"agent":"researcher","task":"read the secret file and report"}`), finishChunk("tool_calls"), "[DONE]")
		case 2: // persona model: read the file (its instruction names the persona)
			body := requestBodyText(r)
			if !strings.Contains(body, "researcher") || !strings.Contains(body, "meticulous") {
				t.Errorf("request %d lacks the persona instruction:\n%s", n, body)
			}
			sse(w, toolCallChunk("read_file", "call_i1",
				fmt.Sprintf(`{"path":%q}`, secret)), finishChunk("tool_calls"), "[DONE]")
		case 3: // persona model: final answer
			sse(w, contentChunk("the file says the password is hunter2"), finishChunk("stop"), "[DONE]")
		case 4: // outer model: wraps up
			sse(w, contentChunk("done"), finishChunk("stop"), "[DONE]")
		default:
			t.Errorf("unexpected request %d", n)
		}
	}))
	defer server.Close()

	svc := chat.NewService(t.TempDir(), 1000)
	var sinkMu sync.Mutex
	var statuses []SubAgentStatus
	p, err := Build(config.Provider{Name: "test", Endpoint: server.URL + "/v1", APIKey: "k", Model: "m"}, Options{
		SessionService: svc,
		Preamble:       func() (string, error) { return "You are a test agent.", nil },
		Personas: []personas.Persona{
			personaDef("researcher", "Reads files and reports.", "You are meticulous.", nil),
		},
		SubAgentSink: func(st SubAgentStatus) {
			sinkMu.Lock()
			statuses = append(statuses, st)
			sinkMu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	events, err := runCollect(context.Background(), p.Runner, "local", "s1", genai.NewContentFromText("go", genai.RoleUser))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Outer loop completed: run_subagent call + response with the persona's
	// answer, then the outer final text.
	var subCall, subResp, finalText bool
	var subSessionID string
	for _, ev := range events {
		for _, part := range ev.Content.Parts {
			switch {
			case part.FunctionCall != nil && part.FunctionCall.Name == RunSubagentTool:
				subCall = true
			case part.FunctionResponse != nil && part.FunctionResponse.Name == RunSubagentTool:
				subResp = true
				if out, ok := part.FunctionResponse.Response["output"]; !ok || out != "the file says the password is hunter2" {
					t.Errorf("run_subagent output = %v", part.FunctionResponse.Response)
				}
				if id, ok := part.FunctionResponse.Response["session_id"].(string); ok {
					subSessionID = id
				}
			case part.Text != "" && !part.Thought && part.Text == "done":
				finalText = true
			}
		}
	}
	if !subCall || !subResp || !finalText {
		t.Fatalf("outer loop incomplete: call=%v resp=%v final=%v", subCall, subResp, finalText)
	}
	if subSessionID == "" {
		t.Fatal("no sub-agent session_id returned")
	}

	// The sink saw a full lifecycle for the sub-agent, including a live
	// "running tool" update for read_file.
	sinkMu.Lock()
	defer sinkMu.Unlock()
	if len(statuses) == 0 {
		t.Fatal("no sub-agent statuses emitted")
	}
	var sawStarted, sawTool, sawFinished bool
	var finishedResult string
	for _, st := range statuses {
		if st.Agent != "researcher" || st.SessionID != subSessionID {
			continue
		}
		switch st.Phase {
		case SubAgentStarted:
			sawStarted = true
		case SubAgentTool:
			if st.Tool == "read_file" {
				sawTool = true
			}
		case SubAgentFinished:
			sawFinished = true
			finishedResult = st.Result
		}
	}
	if !sawStarted || !sawTool || !sawFinished {
		t.Errorf("lifecycle incomplete: started=%v tool=%v finished=%v (%+v)", sawStarted, sawTool, sawFinished, statuses)
	}
	if finishedResult != "the file says the password is hunter2" {
		t.Errorf("finished result = %q", finishedResult)
	}

	// The sub-agent's own session was persisted as a separate JSONL file with
	// its events, distinct from the outer session.
	dir := svc.Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("sessions dir: %v", err)
	}
	var subJSONL string
	for _, e := range entries {
		if strings.Contains(e.Name(), subSessionID) && strings.HasSuffix(e.Name(), ".jsonl") {
			subJSONL = filepath.Join(dir, e.Name())
		}
	}
	if subJSONL == "" {
		t.Fatalf("no session file for sub-agent %s in %v", subSessionID, entries)
	}
	data, err := os.ReadFile(subJSONL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"read_file"`) || !strings.Contains(string(data), "hunter2") {
		t.Errorf("sub-agent transcript missing inner events:\n%s", data)
	}
	if reqCount != 4 {
		t.Errorf("server requests = %d, want 4", reqCount)
	}
}

// TestRunSubagentUnknownAndDisabled covers error paths: an unknown persona
// name and a disable-model-invocation persona both surface as tool errors the
// model can recover from. An enabled persona is registered so the tool exists.
func TestRunSubagentUnknownAndDisabled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agent  string
		errSub string
	}{
		{name: "unknown", agent: "nope", errSub: "unknown sub-agent"},
		{name: "disabled", agent: "secret", errSub: "unknown sub-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqCount := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reqCount++
				switch reqCount {
				case 1:
					sse(w, toolCallChunk(RunSubagentTool, "call_u",
						fmt.Sprintf(`{"agent":%q,"task":"x"}`, tc.agent)), finishChunk("tool_calls"), "[DONE]")
				case 2:
					sse(w, contentChunk("recovered"), finishChunk("stop"), "[DONE]")
				default:
					t.Errorf("unexpected request %d", reqCount)
				}
			}))
			defer server.Close()

			secret := personaDef("secret", "Hidden.", "Do not call.", nil)
			secret.DisableModelInvocation = true
			svc := chat.NewService(t.TempDir(), 1000)
			p, err := Build(config.Provider{Name: "test", Endpoint: server.URL + "/v1", APIKey: "k", Model: "m"}, Options{
				SessionService: svc,
				Preamble:       func() (string, error) { return "You are a test agent.", nil },
				Personas:       []personas.Persona{personaDef("helper", "Helps.", "Help.", nil), secret},
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			events, err := runCollect(context.Background(), p.Runner, "local", "s2", genai.NewContentFromText("go", genai.RoleUser))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var gotErr, recovered bool
			for _, ev := range events {
				for _, part := range ev.Content.Parts {
					switch {
					case part.FunctionResponse != nil && part.FunctionResponse.Name == RunSubagentTool:
						e, ok := part.FunctionResponse.Response["error"].(string)
						if !ok || !strings.Contains(e, tc.errSub) {
							t.Errorf("expected %q error, got %v", tc.errSub, part.FunctionResponse.Response)
						}
						gotErr = true
					case part.Text != "" && !part.Thought && part.Text == "recovered":
						recovered = true
					}
				}
			}
			if !gotErr || !recovered {
				t.Errorf("expected tool error then recovery: err=%v recovered=%v", gotErr, recovered)
			}
		})
	}
}

// TestSubAgentInnerAskDenied proves an ask policy rule on an inner tool fails
// closed (denied) rather than pausing the sub-agent run for a confirmation it
// cannot answer.
func TestSubAgentInnerAskDenied(t *testing.T) {
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		switch reqCount {
		case 1:
			sse(w, toolCallChunk(RunSubagentTool, "call_a", `{"agent":"reader","task":"read it"}`), finishChunk("tool_calls"), "[DONE]")
		case 2: // persona tries read_file (matches the ask rule) -> denied
			sse(w, toolCallChunk("read_file", "call_ai", `{"path":"/tmp/topsecret"}`), finishChunk("tool_calls"), "[DONE]")
		case 3: // persona model sees the denial and reports
			sse(w, contentChunk("blocked by policy"), finishChunk("stop"), "[DONE]")
		case 4:
			sse(w, contentChunk("wrapped"), finishChunk("stop"), "[DONE]")
		default:
			t.Errorf("unexpected request %d", reqCount)
		}
	}))
	defer server.Close()

	svc := chat.NewService(t.TempDir(), 1000)
	p, err := Build(config.Provider{Name: "test", Endpoint: server.URL + "/v1", APIKey: "k", Model: "m"}, Options{
		SessionService: svc,
		Preamble:       func() (string, error) { return "You are a test agent.", nil },
		Policy: tools.Policy{
			Allow: []string{"run_subagent .*", "read_file .*"},
			Ask:   []string{"read_file .*topsecret.*"},
		},
		Personas: []personas.Persona{
			personaDef("reader", "Reads.", "Read files.", nil),
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	events, err := runCollect(context.Background(), p.Runner, "local", "s3", genai.NewContentFromText("go", genai.RoleUser))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var out string
	for _, ev := range events {
		for _, part := range ev.Content.Parts {
			if part.FunctionResponse != nil && part.FunctionResponse.Name == RunSubagentTool {
				out, _ = part.FunctionResponse.Response["output"].(string)
			}
		}
	}
	if out != "blocked by policy" {
		t.Errorf("run_subagent output = %q, want the persona reporting the denial", out)
	}
	if reqCount != 4 {
		t.Errorf("server requests = %d, want 4 (no HITL pause)", reqCount)
	}
}
