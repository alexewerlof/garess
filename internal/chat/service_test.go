package chat

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/sessiontestsuite"
)

func collectEvents(sess session.Session) []*session.Event {
	var out []*session.Event
	for ev := range sess.Events().All() {
		out = append(out, ev)
	}
	return out
}

func TestServiceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 100)

	created, err := svc.Create(context.Background(), &session.CreateRequest{
		AppName: "garess", UserID: "local", SessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	evs := []*session.Event{
		{
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("hi", genai.RoleUser)},
			Author:      "user",
		},
		{
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{
				{Text: "think", Thought: true},
				{Text: "hello"},
			}, genai.RoleModel)},
			Author: "garess",
		},
		{
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "bash", Args: map[string]any{"command": "ls"}}},
			}, genai.RoleModel)},
			Author: "garess",
		},
		{
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "bash", Response: map[string]any{"output": "f.txt"}}},
			}, genai.RoleUser)},
			Author: "garess",
		},
	}
	for _, ev := range evs {
		if err := svc.AppendEvent(context.Background(), created.Session, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	if got := len(evs[0].ID); got == 0 {
		t.Error("AppendEvent should assign an event ID")
	}

	got, err := svc.Get(context.Background(), &session.GetRequest{
		AppName: "garess", UserID: "local", SessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	out := collectEvents(got.Session)
	if len(out) != 4 {
		t.Fatalf("events = %d, want 4", len(out))
	}
	if out[0].Author != "user" || out[0].Content.Parts[0].Text != "hi" {
		t.Errorf("event[0] = %+v", out[0])
	}
	if out[1].Content.Parts[0].Thought != true || out[1].Content.Parts[1].Text != "hello" {
		t.Errorf("event[1] parts not round-tripped: %+v", out[1].Content.Parts)
	}
	if out[2].Content.Parts[0].FunctionCall == nil || out[2].Content.Parts[0].FunctionCall.Name != "bash" {
		t.Errorf("event[2] function call lost: %+v", out[2].Content.Parts[0])
	}
	if out[3].Content.Parts[0].FunctionResponse == nil || out[3].Content.Parts[0].FunctionResponse.ID != "c1" {
		t.Errorf("event[3] function response lost: %+v", out[3].Content.Parts[0])
	}

	// State deltas round-trip into session state.
	_ = svc.AppendEvent(context.Background(), got.Session, &session.Event{
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("x", genai.RoleUser)},
		Author:      "user",
		Actions:     session.EventActions{StateDelta: map[string]any{"user:foo": "bar"}},
	})
	again, err := svc.Get(context.Background(), &session.GetRequest{
		AppName: "garess", UserID: "local", SessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("Get after state: %v", err)
	}
	if v, err := again.Session.State().Get("user:foo"); err != nil || v != "bar" {
		t.Errorf("state not persisted: %v, %v", v, err)
	}
}

func TestServiceHistoryTrim(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 3)
	created, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "a", UserID: "u", SessionID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		ev := &session.Event{
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(strings.Repeat("x", i+1), genai.RoleUser)},
			Author:      "user",
		}
		if err := svc.AppendEvent(context.Background(), created.Session, ev); err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.Get(context.Background(), &session.GetRequest{AppName: "a", UserID: "u", SessionID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	out := collectEvents(got.Session)
	if len(out) != 3 {
		t.Fatalf("trimmed events = %d, want 3", len(out))
	}
	if out[0].Content.Parts[0].Text != "xxx" {
		t.Errorf("first trimmed event = %q, want the 3rd message", out[0].Content.Parts[0].Text)
	}
	// NumRecentEvents overrides.
	got2, _ := svc.Get(context.Background(), &session.GetRequest{AppName: "a", UserID: "u", SessionID: "t", NumRecentEvents: 2})
	if out := collectEvents(got2.Session); len(out) != 2 {
		t.Errorf("NumRecentEvents=2 returned %d", len(out))
	}
}

func TestServiceConformance(t *testing.T) {
	sessiontestsuite.RunServiceTests(t, sessiontestsuite.SuiteOptions{
		SupportsUserProvidedSessionID: true,
		ProvidesServerAssignedEventID: true,
		AppName:                       "garess",
	}, func(t *testing.T) session.Service {
		return NewService(t.TempDir(), 1000)
	})
}
