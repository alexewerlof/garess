package harness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"garess/internal/chat"
	"garess/internal/config"
	"garess/internal/tools"
)

func runCollect(ctx context.Context, r *runner.Runner, userID, sessionID string, content *genai.Content) ([]*session.Event, error) {
	var out []*session.Event
	for ev, err := range r.Run(ctx, userID, sessionID, content, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if err != nil {
			return out, err
		}
		if ev != nil {
			out = append(out, ev)
		}
	}
	return out, nil
}

func sse(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

func toolCallChunk(name, id, args string) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`, id, name, args)
}

func contentChunk(text string) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q}}]}`, text)
}

func finishChunk(reason string) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, reason)
}

func TestAgenticToolLoop(t *testing.T) {
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		switch reqCount {
		case 1:
			sse(w, toolCallChunk("bash", "call_1", `{"command":"printf hi"}`), finishChunk("tool_calls"), "[DONE]")
		case 2:
			sse(w, contentChunk("done"), finishChunk("stop"), "[DONE]")
		default:
			t.Errorf("unexpected request %d", reqCount)
		}
	}))
	defer server.Close()

	svc := chat.NewService(t.TempDir(), 1000)
	p, err := Build(config.Provider{Name: "test", Endpoint: server.URL + "/v1", APIKey: "k", Model: "m"}, Options{
		SessionService: svc,
		Preamble:       func() (string, error) { return "You are a test agent.", nil },
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	events, err := runCollect(context.Background(), p.Runner, "local", "s1", genai.NewContentFromText("run it", genai.RoleUser))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var bashCall, bashResp, finalText bool
	for _, ev := range events {
		for _, part := range ev.Content.Parts {
			switch {
			case part.FunctionCall != nil && part.FunctionCall.Name == "bash":
				bashCall = true
			case part.FunctionResponse != nil && part.FunctionResponse.Name == "bash":
				bashResp = true
				if out, ok := part.FunctionResponse.Response["output"]; !ok || out != "hi" {
					t.Errorf("bash output = %v, want hi", part.FunctionResponse.Response)
				}
			case part.Text != "" && !part.Thought && part.Text == "done":
				finalText = true
			}
		}
	}
	if !bashCall || !bashResp || !finalText {
		t.Errorf("loop incomplete: call=%v resp=%v final=%v", bashCall, bashResp, finalText)
	}
	if reqCount != 2 {
		t.Errorf("server requests = %d, want 2", reqCount)
	}
}

func TestToolConfirmationRoundTrip(t *testing.T) {
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		switch reqCount {
		case 1:
			// The model wants to run a command that matches the ask policy.
			sse(w, toolCallChunk("bash", "call_1", `{"command":"printf secret"}`), finishChunk("tool_calls"), "[DONE]")
		case 2:
			sse(w, contentChunk("approved"), finishChunk("stop"), "[DONE]")
		default:
			t.Errorf("unexpected request %d", reqCount)
		}
	}))
	defer server.Close()

	svc := chat.NewService(t.TempDir(), 1000)
	p, err := Build(config.Provider{Name: "test", Endpoint: server.URL + "/v1", APIKey: "k", Model: "m"}, Options{
		SessionService: svc,
		Policy:         tools.Policy{Ask: []string{".*secret.*"}},
		Preamble:       func() (string, error) { return "You are a test agent.", nil },
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Run 1: the tool call requires confirmation, so the run ends with an
	// adk_request_confirmation wrapper call.
	events, err := runCollect(context.Background(), p.Runner, "local", "c1", genai.NewContentFromText("do it", genai.RoleUser))
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	var wrapperID string
	for _, ev := range events {
		for _, part := range ev.Content.Parts {
			if part.FunctionCall != nil && part.FunctionCall.Name == toolconfirmation.FunctionCallName {
				wrapperID = part.FunctionCall.ID
			}
		}
	}
	if wrapperID == "" {
		t.Fatal("no adk_request_confirmation wrapper in run 1 events")
	}

	// Run 2: the user approves; the tool executes and the model answers.
	confirmResp := genai.NewContentFromParts([]*genai.Part{
		{FunctionResponse: &genai.FunctionResponse{
			Name:     toolconfirmation.FunctionCallName,
			ID:       wrapperID,
			Response: map[string]any{"confirmed": true},
		}},
	}, genai.RoleUser)
	events2, err := runCollect(context.Background(), p.Runner, "local", "c1", confirmResp)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	var gotResp, approved bool
	for _, ev := range events2 {
		for _, part := range ev.Content.Parts {
			switch {
			case part.FunctionResponse != nil && part.FunctionResponse.Name == "bash":
				gotResp = true
				if out, ok := part.FunctionResponse.Response["output"]; !ok || out != "secret" {
					t.Errorf("bash output = %v, want secret", part.FunctionResponse.Response)
				}
			case part.Text != "" && !part.Thought && part.Text == "approved":
				approved = true
			}
		}
	}
	if !gotResp || !approved {
		t.Errorf("resume incomplete: resp=%v approved=%v", gotResp, approved)
	}
	if reqCount != 2 {
		t.Errorf("server requests = %d, want 2", reqCount)
	}
}

// TestHookBeforeToolBlocksToolCall proves a failing before_tool hook aborts
// the tool call: the model still gets a function response, but it carries the
// hook's error (git pre-hook semantics), and the loop can continue.
func TestHookBeforeToolBlocksToolCall(t *testing.T) {
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		switch reqCount {
		case 1:
			sse(w, toolCallChunk("bash", "call_1", `{"command":"printf hi"}`), finishChunk("tool_calls"), "[DONE]")
		case 2:
			sse(w, contentChunk("done"), finishChunk("stop"), "[DONE]")
		default:
			t.Errorf("unexpected request %d", reqCount)
		}
	}))
	defer server.Close()

	svc := chat.NewService(t.TempDir(), 1000)
	p, err := Build(config.Provider{Name: "test", Endpoint: server.URL + "/v1", APIKey: "k", Model: "m"}, Options{
		SessionService: svc,
		Preamble:       func() (string, error) { return "You are a test agent.", nil },
		Hooks:          []config.Hook{{Event: "before_tool", Command: "echo nah >&2; exit 7"}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	events, err := runCollect(context.Background(), p.Runner, "local", "h1", genai.NewContentFromText("run it", genai.RoleUser))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var blocked, finalText bool
	for _, ev := range events {
		for _, part := range ev.Content.Parts {
			switch {
			case part.FunctionResponse != nil && part.FunctionResponse.Name == "bash":
				e, ok := part.FunctionResponse.Response["error"].(string)
				if !ok {
					t.Fatalf("bash response has no error: %v", part.FunctionResponse.Response)
				}
				for _, want := range []string{"before_tool", "exit status 7", "nah"} {
					if !strings.Contains(e, want) {
						t.Errorf("block error %q missing %q", e, want)
					}
				}
				blocked = true
			case part.Text != "" && !part.Thought && part.Text == "done":
				finalText = true
			}
		}
	}
	if !blocked || !finalText {
		t.Errorf("expected blocked tool + final text: blocked=%v final=%v", blocked, finalText)
	}
	if reqCount != 2 {
		t.Errorf("server requests = %d, want 2", reqCount)
	}
}

// TestHookBeforeRunAbortsRun proves a failing before_run hook aborts the whole
// run before the model is ever contacted.
func TestHookBeforeRunAbortsRun(t *testing.T) {
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
	}))
	defer server.Close()

	svc := chat.NewService(t.TempDir(), 1000)
	p, err := Build(config.Provider{Name: "test", Endpoint: server.URL + "/v1", APIKey: "k", Model: "m"}, Options{
		SessionService: svc,
		Preamble:       func() (string, error) { return "You are a test agent.", nil },
		Hooks:          []config.Hook{{Event: "before_run", Command: "exit 2"}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	_, err = runCollect(context.Background(), p.Runner, "local", "h2", genai.NewContentFromText("hi", genai.RoleUser))
	if err == nil {
		t.Fatal("expected run to abort")
	}
	if !strings.Contains(err.Error(), "before_run") {
		t.Errorf("err = %q, want before_run abort", err)
	}
	if reqCount != 0 {
		t.Errorf("server requests = %d, want 0 (run aborted before the model)", reqCount)
	}
}

// TestHookPassingStillRunsTool proves a passing blocking hook does not get in
// the way of the normal tool loop.
func TestHookPassingStillRunsTool(t *testing.T) {
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		switch reqCount {
		case 1:
			sse(w, toolCallChunk("bash", "call_1", `{"command":"printf hi"}`), finishChunk("tool_calls"), "[DONE]")
		case 2:
			sse(w, contentChunk("done"), finishChunk("stop"), "[DONE]")
		default:
			t.Errorf("unexpected request %d", reqCount)
		}
	}))
	defer server.Close()

	svc := chat.NewService(t.TempDir(), 1000)
	p, err := Build(config.Provider{Name: "test", Endpoint: server.URL + "/v1", APIKey: "k", Model: "m"}, Options{
		SessionService: svc,
		Preamble:       func() (string, error) { return "You are a test agent.", nil },
		Hooks: []config.Hook{
			{Event: "before_tool", Command: "exit 0"},
			{Event: "after_model", Command: "exit 0"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	events, err := runCollect(context.Background(), p.Runner, "local", "h3", genai.NewContentFromText("run it", genai.RoleUser))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var gotOut bool
	for _, ev := range events {
		for _, part := range ev.Content.Parts {
			if part.FunctionResponse != nil && part.FunctionResponse.Name == "bash" {
				if out, ok := part.FunctionResponse.Response["output"]; ok && out == "hi" {
					gotOut = true
				}
			}
		}
	}
	if !gotOut {
		t.Error("bash did not run (passing hook interfered)")
	}
	if reqCount != 2 {
		t.Errorf("server requests = %d, want 2", reqCount)
	}
}

// TestHookAfterModelFiresOncePerModelCall proves after_model is a model-call
// notification, not a per-token one: even when a generation streams many
// partial chunks, the hook fires exactly once — on the final non-partial
// response — mirroring before_model's once-per-call cadence. Regression for
// the per-streamed-chunk firing found during on-device validation (134 shell
// spawns for one agentic turn on the RPi).
func TestHookAfterModelFiresOncePerModelCall(t *testing.T) {
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		switch reqCount {
		case 1:
			sse(w, toolCallChunk("bash", "call_1", `{"command":"printf hi"}`), finishChunk("tool_calls"), "[DONE]")
		case 2:
			// Stream several partial content chunks before the final one; an
			// unfiltered after_model would fire once per chunk here.
			sse(w, contentChunk("hel"), contentChunk("lo "), contentChunk("wor"), contentChunk("ld"), finishChunk("stop"), "[DONE]")
		default:
			t.Errorf("unexpected request %d", reqCount)
		}
	}))
	defer server.Close()

	countFile := filepath.Join(t.TempDir(), "after_model.count")
	svc := chat.NewService(t.TempDir(), 1000)
	p, err := Build(config.Provider{Name: "test", Endpoint: server.URL + "/v1", APIKey: "k", Model: "m"}, Options{
		SessionService: svc,
		Preamble:       func() (string, error) { return "You are a test agent.", nil },
		Hooks:          []config.Hook{{Event: "after_model", Command: "echo x >> " + countFile}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, err := runCollect(context.Background(), p.Runner, "local", "h4", genai.NewContentFromText("run it", genai.RoleUser)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	b, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("read hook count: %v", err)
	}
	// Two model generations (the tool call and the final text) => two fires,
	// no matter how many chunks the second generation streamed.
	if got := strings.Count(string(b), "x\n"); got != 2 {
		t.Fatalf("after_model fired %d times, want 2 (once per model call)", got)
	}
	if reqCount != 2 {
		t.Errorf("server requests = %d, want 2", reqCount)
	}
}
