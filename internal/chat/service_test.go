package chat

import (
	"context"
	"fmt"
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

// appendN appends n plain user-text events and returns their assigned IDs.
func appendN(t *testing.T, svc *Service, sess session.Session, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ev := &session.Event{
			Author:      "user",
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(fmt.Sprintf("m%d", i), genai.RoleUser)},
		}
		if err := svc.AppendEvent(context.Background(), sess, ev); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ev.ID)
	}
	return ids
}

func textOf(t *testing.T, ev *session.Event) string {
	t.Helper()
	if ev == nil || ev.Content == nil || len(ev.Content.Parts) == 0 {
		return ""
	}
	return ev.Content.Parts[0].Text
}

func TestServiceCompactView(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 100)
	created, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "a", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ids := appendN(t, svc, created.Session, 5)

	sumEv, err := svc.Compact(context.Background(), &CompactRequest{
		AppName: "a", UserID: "u", SessionID: "s",
		SummaryText: "SUMMARY", CoveredThroughEventID: ids[2], CoveredCount: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(context.Background(), &session.GetRequest{AppName: "a", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	out := collectEvents(got.Session)
	if len(out) != 3 {
		t.Fatalf("view events = %d, want 3", len(out))
	}
	if out[0].ID != sumEv.ID || textOf(t, out[0]) != "SUMMARY" {
		t.Errorf("view[0] = %+v, want the summary event", out[0])
	}
	if textOf(t, out[1]) != "m3" || textOf(t, out[2]) != "m4" {
		t.Errorf("view tail wrong: %q, %q", textOf(t, out[1]), textOf(t, out[2]))
	}

	// The raw transcript keeps everything: 5 original events + the summary.
	all, err := svc.readEvents(svc.jsonlPath("s"))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("transcript events = %d, want 6 (full history preserved)", len(all))
	}
}

func TestServiceCompactStickyUnderHistoryLimit(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 3)
	created, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "a", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	first := appendN(t, svc, created.Session, 2)
	if _, err := svc.Compact(context.Background(), &CompactRequest{
		AppName: "a", UserID: "u", SessionID: "s",
		SummaryText: "SUMMARY", CoveredThroughEventID: first[1], CoveredCount: 2,
	}); err != nil {
		t.Fatal(err)
	}
	// Three more messages after compaction push the visible tail past the cap.
	tail := appendN(t, svc, created.Session, 3)

	got, err := svc.Get(context.Background(), &session.GetRequest{AppName: "a", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	out := collectEvents(got.Session)
	if len(out) != 3 {
		t.Fatalf("view events = %d, want 3 (summary + cap-1 tail)", len(out))
	}
	if textOf(t, out[0]) != "SUMMARY" {
		t.Errorf("summary should be sticky at the head, got %q", textOf(t, out[0]))
	}
	// The cap keeps the summary plus the last (limit-1) tail events.
	if out[1].ID != tail[1] || out[2].ID != tail[2] {
		t.Errorf("tail wrong after sticky cap: %s, %s (want %s, %s)",
			out[1].ID, out[2].ID, tail[1], tail[2])
	}
}

func TestServiceCompactSecondCompactionSubsumesFirst(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 100)
	created, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "a", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	first := appendN(t, svc, created.Session, 5)
	if _, err := svc.Compact(context.Background(), &CompactRequest{
		AppName: "a", UserID: "u", SessionID: "s",
		SummaryText: "SUM1", CoveredThroughEventID: first[1], CoveredCount: 2,
	}); err != nil {
		t.Fatal(err)
	}
	// New turns land after the summary; the second compression summarizes a
	// prefix that includes SUM1 and the first of the new turns (a realistic
	// cutoff lies AFTER the previous summary in the raw log).
	tail := appendN(t, svc, created.Session, 3)
	if _, err := svc.Compact(context.Background(), &CompactRequest{
		AppName: "a", UserID: "u", SessionID: "s",
		SummaryText: "SUM2", CoveredThroughEventID: tail[0], CoveredCount: 8,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(context.Background(), &session.GetRequest{AppName: "a", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	out := collectEvents(got.Session)
	if len(out) != 3 {
		t.Fatalf("view events = %d, want 3 ([SUM2, t1, t2])", len(out))
	}
	if textOf(t, out[0]) != "SUM2" || out[1].ID != tail[1] || out[2].ID != tail[2] {
		t.Errorf("second compaction should subsume the first: got %q + %s, %s",
			textOf(t, out[0]), out[1].ID, out[2].ID)
	}
	for _, ev := range out {
		if textOf(t, ev) == "SUM1" {
			t.Error("SUM1 leaked into the view after the second compaction")
		}
	}
	// Transcript: 5 + SUM1 + 3 + SUM2 = 10 events, all preserved.
	all, err := svc.readEvents(svc.jsonlPath("s"))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 10 {
		t.Fatalf("transcript events = %d, want 10", len(all))
	}
}

func TestServiceCompactOwnership(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 100)
	created, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "a", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ids := appendN(t, svc, created.Session, 2)
	if _, err := svc.Compact(context.Background(), &CompactRequest{
		AppName: "a", UserID: "someone-else", SessionID: "s",
		SummaryText: "SUM", CoveredThroughEventID: ids[1],
	}); err == nil {
		t.Fatal("expected ownership error for another user's session")
	}
	if _, err := svc.Compact(context.Background(), &CompactRequest{
		AppName: "a", UserID: "u", SessionID: "missing",
		SummaryText: "SUM", CoveredThroughEventID: ids[1],
	}); err == nil {
		t.Fatal("expected error for a missing session")
	}
}
