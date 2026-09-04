// Package chat implements a Google ADK session.Service backed by JSONL
// transcript files (.garess/sessions/<id>.jsonl, one JSON-encoded
// session.Event per line) plus a per-session metadata sidecar.
//
// State scoping follows ADK conventions: keys prefixed `app:` are shared
// across all sessions of an app, `user:` keys across a user's sessions, and
// un-prefixed keys are session-local.
package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
)

// Service implements session.Service with one JSONL file per session plus a
// metadata sidecar.
type Service struct {
	dir          string
	historyLimit int

	mu        sync.Mutex
	appState  map[string]map[string]any            // appName -> state
	userState map[string]map[string]map[string]any // appName -> userID -> state
}

// NewService builds a JSONL-backed session service. historyLimit caps the
// number of events Get returns (the model context window); the full transcript
// stays on disk.
func NewService(dir string, historyLimit int) *Service {
	if historyLimit <= 0 {
		historyLimit = 40
	}
	return &Service{
		dir:          dir,
		historyLimit: historyLimit,
		appState:     make(map[string]map[string]any),
		userState:    make(map[string]map[string]map[string]any),
	}
}

// Dir returns the transcript directory.
func (s *Service) Dir() string { return s.dir }

// meta is the per-session metadata sidecar.
type meta struct {
	AppName    string         `json:"appName"`
	UserID     string         `json:"userID"`
	State      map[string]any `json:"state,omitempty"`
	Compaction *compaction    `json:"compaction,omitempty"`
}

// compaction records one context-compression: in the model's view, every
// event up to and including CoveredThroughEventID was replaced by the summary
// event SummaryEventID. The raw transcript on disk is untouched. Only the
// latest compaction is stored — each new compression covers a prefix of the
// event log that includes any earlier summary, so the latest record subsumes
// all previous ones.
type compaction struct {
	SummaryEventID        string    `json:"summaryEventId"`
	CoveredThroughEventID string    `json:"coveredThroughEventId"`
	CoveredCount          int       `json:"coveredCount"`
	Timestamp             time.Time `json:"timestamp"`
}

// Create implements session.Service.
func (s *Service) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if req.SessionID == "" {
		req.SessionID = NewSessionID()
	}
	if s.exists(req.SessionID) {
		return nil, fmt.Errorf("chat: session %q already exists", req.SessionID)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	m := &meta{AppName: req.AppName, UserID: req.UserID}
	m.State = make(map[string]any, len(req.State))
	for k, v := range req.State {
		s.routeState(req.AppName, req.UserID, k, v)
		if !isScopedKey(k) {
			m.State[k] = v
		}
	}
	if err := writeMeta(s.metaPath(req.SessionID), m); err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.jsonlPath(req.SessionID), nil, 0o644); err != nil {
		return nil, err
	}
	sess := s.newSession(req.AppName, req.UserID, req.SessionID, m.State)
	return &session.CreateResponse{Session: sess}, nil
}

// Get implements session.Service.
func (s *Service) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	jsonl := s.jsonlPath(req.SessionID)
	if !s.exists(req.SessionID) {
		return nil, fmt.Errorf("chat: session %q not found", req.SessionID)
	}
	m, err := readMeta(s.metaPath(req.SessionID))
	if err != nil {
		return nil, err
	}
	if m.AppName != req.AppName || m.UserID != req.UserID {
		return nil, fmt.Errorf("chat: session %q not found", req.SessionID)
	}
	events, err := s.readEvents(jsonl)
	if err != nil {
		return nil, err
	}
	events, sticky := applyCompaction(events, m.Compaction)
	events = s.trim(events, req, sticky)
	sess := s.newSession(req.AppName, req.UserID, req.SessionID, m.State)
	sess.events = events
	return &session.GetResponse{Session: sess}, nil
}

// List implements session.Service, newest first. An empty UserID lists all
// users for the app.
func (s *Service) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &session.ListResponse{}, nil
		}
		return nil, err
	}
	var out []session.Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		m, err := readMeta(s.metaPath(id))
		if err != nil {
			continue
		}
		if m.AppName != req.AppName {
			continue
		}
		if req.UserID != "" && m.UserID != req.UserID {
			continue
		}
		events, err := s.readEvents(s.jsonlPath(id))
		if err != nil {
			continue
		}
		events, sticky := applyCompaction(events, m.Compaction)
		sess := s.newSession(req.AppName, m.UserID, id, m.State)
		sess.events = s.trim(events, nil, sticky)
		out = append(out, sess)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastUpdateTime().After(out[j].LastUpdateTime())
	})
	return &session.ListResponse{Sessions: out}, nil
}

// Delete implements session.Service. Deleting a session owned by a different
// user, or a non-existent session, is a no-op (returns nil).
func (s *Service) Delete(ctx context.Context, req *session.DeleteRequest) error {
	m, err := readMeta(s.metaPath(req.SessionID))
	if err != nil {
		return nil
	}
	if m.AppName != req.AppName || m.UserID != req.UserID {
		return nil
	}
	_ = os.Remove(s.jsonlPath(req.SessionID))
	_ = os.Remove(s.metaPath(req.SessionID))
	return nil
}

// AppendEvent implements session.Service. Partial events are not persisted.
// Temporary (temp:) state keys are stripped; app:/user:/session state deltas
// are routed to their stores.
func (s *Service) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if !s.exists(sess.ID()) {
		return fmt.Errorf("chat: session %q not found", sess.ID())
	}
	if ev.Partial {
		return nil // streaming deltas are display-only
	}
	m, err := readMeta(s.metaPath(sess.ID()))
	if err != nil {
		return fmt.Errorf("chat: session %q not found", sess.ID())
	}
	if ev.Actions.StateDelta != nil {
		for k := range ev.Actions.StateDelta {
			if strings.HasPrefix(k, "temp:") {
				delete(ev.Actions.StateDelta, k)
			}
		}
		for k, v := range ev.Actions.StateDelta {
			s.routeState(m.AppName, m.UserID, k, v)
			if !isScopedKey(k) {
				if m.State == nil {
					m.State = make(map[string]any)
				}
				m.State[k] = v
			}
		}
		if err := writeMeta(s.metaPath(sess.ID()), m); err != nil {
			return err
		}
	}
	if ev.ID == "" {
		ev.ID = NewEventID()
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	if err := s.appendLine(sess.ID(), ev); err != nil {
		return err
	}
	if impl, ok := sess.(*sessionImpl); ok {
		impl.events = append(impl.events, ev)
		applyStateDelta(impl.state, ev.Actions.StateDelta)
		if ev.Timestamp.After(impl.lastUpdate) {
			impl.lastUpdate = ev.Timestamp
		}
	}
	return nil
}

// appendLine marshals ev and appends it to the session's JSONL. ID and
// Timestamp must already be set.
func (s *Service) appendLine(id string, ev *session.Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("chat: encode event: %w", err)
	}
	f, err := os.OpenFile(s.jsonlPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// routeState merges a state key into the app/user/session stores.
func (s *Service) routeState(appName, userID, key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.HasPrefix(key, "app:"):
		if s.appState[appName] == nil {
			s.appState[appName] = make(map[string]any)
		}
		s.appState[appName][key] = value
	case strings.HasPrefix(key, "user:"):
		if s.userState[appName] == nil {
			s.userState[appName] = make(map[string]map[string]any)
		}
		if s.userState[appName][userID] == nil {
			s.userState[appName][userID] = make(map[string]any)
		}
		s.userState[appName][userID][key] = value
	}
}

func isScopedKey(k string) bool {
	return strings.HasPrefix(k, "app:") || strings.HasPrefix(k, "user:")
}

// mergedState returns the app + user + session state for a session.
func (s *Service) mergedState(appName, userID string, sessionState map[string]any) map[string]any {
	out := make(map[string]any)
	s.mu.Lock()
	for k, v := range s.appState[appName] {
		out[k] = v
	}
	for k, v := range s.userState[appName][userID] {
		out[k] = v
	}
	s.mu.Unlock()
	for k, v := range sessionState {
		out[k] = v
	}
	return out
}

func (s *Service) jsonlPath(id string) string { return filepath.Join(s.dir, id+".jsonl") }
func (s *Service) metaPath(id string) string  { return filepath.Join(s.dir, id+".meta.json") }

func (s *Service) exists(id string) bool {
	if _, err := os.Stat(s.jsonlPath(id)); err == nil {
		return true
	}
	_, err := os.Stat(s.metaPath(id))
	return err == nil
}

func (s *Service) newSession(appName, userID, id string, sessionState map[string]any) *sessionImpl {
	merged := s.mergedState(appName, userID, sessionState)
	st := newState()
	for k, v := range merged {
		_ = st.Set(k, v)
	}
	return &sessionImpl{
		id:         id,
		appName:    appName,
		userID:     userID,
		state:      st,
		lastUpdate: time.Time{},
	}
}

func (s *Service) readEvents(path string) ([]*session.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []*session.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev session.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("chat: decode event: %w", err)
		}
		out = append(out, &ev)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return out, nil
}

// trim applies the After/NumRecentEvents filters and the historyLimit cap.
// protectFirst (set when the leading event is a compaction summary) keeps
// that event even when the cap would otherwise drop it — the summary stands
// for the whole earlier conversation, so it must not age out of the window.
// A nil request applies only the historyLimit cap.
func (s *Service) trim(events []*session.Event, req *session.GetRequest, protectFirst bool) []*session.Event {
	start := 0
	if req != nil && !req.After.IsZero() {
		for i, ev := range events {
			if !ev.Timestamp.Before(req.After) {
				start = i
				break
			}
		}
	}
	if start > 0 {
		if protectFirst {
			// Keep the summary even if the After filter precedes it.
			events = append([]*session.Event{events[0]}, events[start:]...)
		} else {
			events = events[start:]
		}
	}
	limit := s.historyLimit
	if req != nil && req.NumRecentEvents > 0 {
		limit = req.NumRecentEvents
	}
	if limit > 0 && len(events) > limit {
		if protectFirst {
			tail := events[1:]
			if limit-1 < len(tail) {
				tail = tail[len(tail)-(limit-1):]
			}
			events = append([]*session.Event{events[0]}, tail...)
		} else {
			events = events[len(events)-limit:]
		}
	}
	return events
}

// applyCompaction replaces, in the model's view, every event up to and
// including the compaction's CoveredThroughEventID with the summary event.
// The raw transcript is untouched. The bool reports whether the compaction
// applied (the returned list starts with the sticky summary event).
func applyCompaction(events []*session.Event, comp *compaction) ([]*session.Event, bool) {
	if comp == nil || comp.SummaryEventID == "" {
		return events, false
	}
	var summary *session.Event
	covered := -1
	for i, ev := range events {
		if ev.ID == comp.SummaryEventID {
			summary = ev
		}
		if ev.ID == comp.CoveredThroughEventID {
			covered = i
		}
	}
	if summary == nil || covered < 0 {
		// Marker references events no longer in the log (should not happen:
		// the log is append-only) — fail open to the raw history.
		return events, false
	}
	out := make([]*session.Event, 0, len(events)-covered)
	out = append(out, summary)
	for i := covered + 1; i < len(events); i++ {
		if events[i].ID == comp.SummaryEventID {
			continue // the summary event itself is only ever shown at the head
		}
		out = append(out, events[i])
	}
	return out, true
}

// CompactRequest describes a context-compression: events up to and including
// CoveredThroughEventID are replaced, in the model's view, by one summary
// event carrying SummaryText. The raw transcript is preserved.
type CompactRequest struct {
	AppName               string
	UserID                string
	SessionID             string
	SummaryText           string
	CoveredThroughEventID string
	CoveredCount          int
}

// Compact appends a summary event authored by the user and records the
// compaction marker in the session metadata. Subsequent Get calls return the
// summary followed by the events after the covered prefix. It is meant to run
// between turns, when no run holds the session open.
func (s *Service) Compact(ctx context.Context, req *CompactRequest) (*session.Event, error) {
	if !s.exists(req.SessionID) {
		return nil, fmt.Errorf("chat: session %q not found", req.SessionID)
	}
	m, err := readMeta(s.metaPath(req.SessionID))
	if err != nil {
		return nil, fmt.Errorf("chat: session %q not found", req.SessionID)
	}
	if m.AppName != req.AppName || m.UserID != req.UserID {
		return nil, fmt.Errorf("chat: session %q not found", req.SessionID)
	}
	ev := &session.Event{
		Author: "user",
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{
				Role: genai.RoleUser,
				Parts: []*genai.Part{
					{Text: req.SummaryText},
				},
			},
		},
	}
	ev.ID = NewEventID()
	ev.Timestamp = time.Now()
	if err := s.appendLine(req.SessionID, ev); err != nil {
		return nil, err
	}
	m.Compaction = &compaction{
		SummaryEventID:        ev.ID,
		CoveredThroughEventID: req.CoveredThroughEventID,
		CoveredCount:          req.CoveredCount,
		Timestamp:             ev.Timestamp,
	}
	if err := writeMeta(s.metaPath(req.SessionID), m); err != nil {
		return nil, err
	}
	return ev, nil
}

func writeMeta(path string, m *meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func readMeta(path string) (*meta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func applyStateDelta(st *stateImpl, delta map[string]any) {
	for k, v := range delta {
		_ = st.Set(k, v)
	}
}

// sessionImpl implements session.Session.
type sessionImpl struct {
	id         string
	appName    string
	userID     string
	state      *stateImpl
	events     []*session.Event
	lastUpdate time.Time
}

func (s *sessionImpl) ID() string             { return s.id }
func (s *sessionImpl) AppName() string        { return s.appName }
func (s *sessionImpl) UserID() string         { return s.userID }
func (s *sessionImpl) State() session.State   { return s.state }
func (s *sessionImpl) Events() session.Events { return &eventsImpl{evs: s.events} }
func (s *sessionImpl) LastUpdateTime() time.Time {
	return s.lastUpdate
}

type eventsImpl struct {
	evs []*session.Event
}

func (e *eventsImpl) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, ev := range e.evs {
			if !yield(ev) {
				return
			}
		}
	}
}
func (e *eventsImpl) Len() int { return len(e.evs) }
func (e *eventsImpl) At(i int) *session.Event {
	if i < 0 || i >= len(e.evs) {
		return nil
	}
	return e.evs[i]
}

// stateImpl implements session.State.
type stateImpl struct {
	mu sync.RWMutex
	m  map[string]any
}

func newState() *stateImpl { return &stateImpl{m: make(map[string]any)} }

func (s *stateImpl) Get(key string) (any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}

func (s *stateImpl) Set(key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

func (s *stateImpl) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		for k, v := range s.m {
			if !yield(k, v) {
				return
			}
		}
	}
}
