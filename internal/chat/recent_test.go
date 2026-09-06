package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
)

// mkSess creates a session and appends a script of events with the given
// per-event delays so timestamps are distinct and ordered.
func mkSess(t *testing.T, svc *Service, id string, texts []string) {
	t.Helper()
	created, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "garess", UserID: "local", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range texts {
		ev := &session.Event{
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleUser)},
			Author:      "user",
		}
		if err := svc.AppendEvent(context.Background(), created.Session, ev); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecentNewestFirstWithPreviews(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 100)

	mkSess(t, svc, "old", []string{"first old question", "second old question"})
	time.Sleep(5 * time.Millisecond)
	mkSess(t, svc, "new", []string{"  newest question with space  "})

	got, err := svc.Recent(context.Background(), "garess", "local", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Recent returned %d sessions, want 2", len(got))
	}
	if got[0].ID != "new" || got[1].ID != "old" {
		t.Errorf("order = [%s, %s], want newest first [new, old]", got[0].ID, got[1].ID)
	}
	if got[0].Preview != "newest question with space" {
		t.Errorf("preview = %q", got[0].Preview)
	}
	if got[0].EventCount != 1 || got[1].EventCount != 2 {
		t.Errorf("event counts = %d, %d", got[0].EventCount, got[1].EventCount)
	}
	if got[0].UpdatedAt.Before(got[1].UpdatedAt) {
		t.Error("new session should update after old")
	}
}

func TestRecentSkipsOtherUserAndLimit(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 100)
	mkSess(t, svc, "a", []string{"hello"})
	mkSess(t, svc, "b", []string{"world"})

	other, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "garess", UserID: "elsewhere", SessionID: "other"})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.AppendEvent(context.Background(), other.Session, &session.Event{
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("secret", genai.RoleUser)},
		Author:      "user",
	})

	if got, _ := svc.Recent(context.Background(), "garess", "local", 1); len(got) != 1 {
		t.Errorf("limit=1 returned %d sessions", len(got))
	}
	if got, _ := svc.Recent(context.Background(), "garess", "local", 10); len(got) != 2 {
		t.Errorf("local user sees %d sessions, want 2 (other user excluded)", len(got))
	}
	if got, _ := svc.Recent(context.Background(), "garess", "nobody", 10); len(got) != 0 {
		t.Errorf("unknown user sees %d sessions, want 0", len(got))
	}
}

func TestRecentPreviewSkipsToolsAndThinking(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 100)
	created, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "garess", UserID: "local", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	// First event is a model function call (not user text) — must not become
	// the preview.
	_ = svc.AppendEvent(context.Background(), created.Session, &session.Event{
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{
			{FunctionCall: &genai.FunctionCall{ID: "c", Name: "bash", Args: map[string]any{"command": "ls"}}},
		}, genai.RoleModel)},
		Author: "garess",
	})
	_ = svc.AppendEvent(context.Background(), created.Session, &session.Event{
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("real question", genai.RoleUser)},
		Author:      "user",
	})
	got, err := svc.Recent(context.Background(), "garess", "local", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Preview != "real question" {
		t.Fatalf("preview = %+v, want the user question", got)
	}
}

func TestRecentLongPreviewClipped(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 100)
	long := strings.Repeat("a", 200)
	mkSess(t, svc, "s", []string{long})
	got, err := svc.Recent(context.Background(), "garess", "local", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len([]rune(got[0].Preview)) > 81 || !strings.HasSuffix(got[0].Preview, "…") {
		t.Errorf("preview not clipped: %q (%d runes)", got[0].Preview, len([]rune(got[0].Preview)))
	}
}

func TestSessionViewTrimmedAndSummaryMarker(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 3) // tiny history limit exercises the cap
	mkSess(t, svc, "s", []string{"one", "two", "three", "four", "five"})

	events, summaryID, err := svc.SessionView(context.Background(), "garess", "local", "s")
	if err != nil {
		t.Fatal(err)
	}
	if summaryID != "" {
		t.Errorf("summaryID = %q for an un-compacted session", summaryID)
	}
	if len(events) != 3 {
		t.Fatalf("SessionView events = %d, want the history cap (3)", len(events))
	}
	if text := events[0].Content.Parts[0].Text; text != "three" {
		t.Errorf("first view event = %q, want the 3rd message (tail kept)", text)
	}
	// Ownership: another user cannot read it.
	if _, _, err := svc.SessionView(context.Background(), "garess", "elsewhere", "s"); err == nil {
		t.Error("SessionView for another user should fail")
	}
	if _, _, err := svc.SessionView(context.Background(), "garess", "local", "missing"); err == nil {
		t.Error("SessionView for a missing session should fail")
	}
}

func TestSessionViewReportsStickySummary(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir, 100)
	mkSess(t, svc, "s", []string{"alpha", "beta"})
	evs, _, err := svc.SessionView(context.Background(), "garess", "local", "s")
	if err != nil {
		t.Fatal(err)
	}
	covered := evs[len(evs)-1]
	sumEv, err := svc.Compact(context.Background(), &CompactRequest{
		AppName:               "garess",
		UserID:                "local",
		SessionID:             "s",
		SummaryText:           "Earlier conversation (compressed):\nalpha, beta",
		CoveredThroughEventID: covered.ID,
		CoveredCount:          2,
	})
	if err != nil {
		t.Fatal(err)
	}
	view, summaryID, err := svc.SessionView(context.Background(), "garess", "local", "s")
	if err != nil {
		t.Fatal(err)
	}
	if summaryID != sumEv.ID {
		t.Errorf("summaryID = %q, want %q", summaryID, sumEv.ID)
	}
	if len(view) != 1 || view[0].ID != sumEv.ID {
		t.Fatalf("view = %d events, want just the sticky summary", len(view))
	}
}
