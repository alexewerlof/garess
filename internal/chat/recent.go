package chat

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/session"
)

// RecentSession is a lightweight summary of one session for the TUI session
// rail and the /sessions picker: enough to label and resume it without the
// cost of a full model-view load.
type RecentSession struct {
	ID         string
	UserID     string
	UpdatedAt  time.Time
	EventCount int
	Preview    string // first user-authored line ("" when the session has none)
}

// Recent lists an app/user's sessions, newest first, capped at limit. Each
// entry carries a lightweight preview (the first user line, so a resume list
// reads like a recent-chats picker) and the timestamp of the last persisted
// event. Sessions whose meta or transcript are missing/corrupt are skipped.
// It is display-only: it never applies compaction filtering or the history
// cap (see SessionView for the model view).
func (s *Service) Recent(ctx context.Context, appName, userID string, limit int) ([]RecentSession, error) {
	if limit <= 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]RecentSession, 0, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		m, err := readMeta(s.metaPath(id))
		if err != nil || m.AppName != appName || (userID != "" && m.UserID != userID) {
			continue
		}
		events, err := s.readEvents(s.jsonlPath(id))
		if err != nil {
			continue
		}
		r := RecentSession{ID: id, UserID: m.UserID, EventCount: len(events)}
		for _, ev := range events {
			if ev.Timestamp.After(r.UpdatedAt) {
				r.UpdatedAt = ev.Timestamp
			}
			if r.Preview == "" {
				r.Preview = firstUserLine(ev)
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// firstUserLine returns the first non-empty, single-line excerpt of a
// user-authored text event (the resume-picker preview). Tool calls/results,
// thinking and empty events return "".
func firstUserLine(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	if ev.Content.Role != string(genai.RoleUser) && ev.Author != "user" {
		return ""
	}
	for _, p := range ev.Content.Parts {
		if p == nil || p.Text == "" || p.Thought {
			continue
		}
		if p.FunctionCall != nil || p.FunctionResponse != nil {
			continue
		}
		for _, line := range strings.Split(p.Text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return clipLine(line)
			}
		}
	}
	return ""
}

// clipLine shortens a preview line to a displayable length.
func clipLine(s string) string {
	r := []rune(s)
	if len(r) <= 80 {
		return s
	}
	return string(r[:77]) + "…"
}

// SessionView returns the model-visible events for one session — compaction-
// filtered and history-capped, exactly what the runner feeds the model on its
// next run — plus the sticky compaction summary event id ("" when the
// session was never compacted). The TUI renders this view when a session is
// resumed, so the display matches the transcript the model continues from.
func (s *Service) SessionView(ctx context.Context, appName, userID, id string) ([]*session.Event, string, error) {
	notFound := func() error { return fmt.Errorf("chat: session %q not found", id) }
	if !s.exists(id) {
		return nil, "", notFound()
	}
	m, err := readMeta(s.metaPath(id))
	if err != nil {
		return nil, "", err
	}
	if m.AppName != appName || (userID != "" && m.UserID != userID) {
		return nil, "", notFound()
	}
	events, err := s.readEvents(s.jsonlPath(id))
	if err != nil {
		return nil, "", err
	}
	events, sticky := applyCompaction(events, m.Compaction)
	events = s.trim(events, nil, sticky)
	summaryID := ""
	if sticky && m.Compaction != nil {
		summaryID = m.Compaction.SummaryEventID
	}
	return events, summaryID, nil
}
