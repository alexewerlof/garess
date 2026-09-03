package hooks

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"garess/internal/config"
)

func parseOne(t *testing.T, event, command, timeout string) *Set {
	t.Helper()
	s, err := Parse([]config.Hook{{Event: event, Command: command, Timeout: timeout}})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

func quietLogs() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestParseValidation(t *testing.T) {
	if _, err := Parse(nil); err != nil {
		t.Fatalf("empty parse should succeed: %v", err)
	}
	cases := []struct {
		name string
		h    config.Hook
		want string
	}{
		{"unknown event", config.Hook{Event: "pre_commit", Command: "x"}, "unknown hook event"},
		{"missing event", config.Hook{Command: "x"}, "missing an event"},
		{"empty command", config.Hook{Event: EventBeforeTool}, "empty command"},
		{"bad timeout", config.Hook{Event: EventBeforeTool, Command: "x", Timeout: "soon"}, "invalid timeout"},
		{"zero timeout", config.Hook{Event: EventBeforeTool, Command: "x", Timeout: "0s"}, "must be positive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]config.Hook{c.h})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}

	// A valid timeout parses and is applied.
	s := parseOne(t, EventBeforeTool, "true", "3s")
	if got := s.byEvent[EventBeforeTool][0].Timeout; got != 3*time.Second {
		t.Fatalf("timeout = %v, want 3s", got)
	}
	// Empty timeout gets the default.
	s = parseOne(t, EventBeforeTool, "true", "")
	if got := s.byEvent[EventBeforeTool][0].Timeout; got != DefaultTimeout {
		t.Fatalf("timeout = %v, want %v", got, DefaultTimeout)
	}
}

func TestRunHooksBlockingAbortsAndSkips(t *testing.T) {
	quietLogs()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	cmds := []config.Hook{
		{Event: EventBeforeTool, Command: "echo first >> " + log},
		{Event: EventBeforeTool, Command: "echo denied >&2; exit 3"},
		{Event: EventBeforeTool, Command: "echo third >> " + log},
	}
	s, err := Parse(cmds)
	if err != nil {
		t.Fatal(err)
	}
	err = s.RunHooks(context.Background(), &Payload{Event: EventBeforeTool, Tool: "bash"})
	if err == nil {
		t.Fatal("expected abort error")
	}
	for _, want := range []string{"before_tool", "exit status 3", "denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err, want)
		}
	}
	data, _ := os.ReadFile(log)
	if string(data) != "first\n" {
		t.Fatalf("log = %q, want only the first hook to have run (abort must skip later hooks)", data)
	}
}

func TestRunHooksNotificationDoesNotAbort(t *testing.T) {
	quietLogs()
	s := parseOne(t, EventAfterTool, "echo warn >&2; exit 1", "")
	if err := s.RunHooks(context.Background(), &Payload{Event: EventAfterTool, Tool: "bash"}); err != nil {
		t.Fatalf("notification hook failure should not abort: %v", err)
	}
}

func TestRunHooksPassingBlockingHook(t *testing.T) {
	quietLogs()
	s := parseOne(t, EventBeforeTool, "true", "")
	if err := s.RunHooks(context.Background(), &Payload{Event: EventBeforeTool}); err != nil {
		t.Fatalf("exit 0 should pass: %v", err)
	}
}

func TestHookReceivesEventArgAndStdinPayload(t *testing.T) {
	dir := t.TempDir()
	eventFile := filepath.Join(dir, "event")
	payloadFile := filepath.Join(dir, "payload")
	s := parseOne(t, EventBeforeTool, "printf '%s' \"$1\" > "+eventFile+" && cat > "+payloadFile, "")
	longCmd := strings.Repeat("x", maxArgRunes+50)
	payload := &Payload{
		Event: EventBeforeTool,
		Tool:  "bash",
		Args:  map[string]any{"command": longCmd, "timeout_ms": 5},
		Text:  strings.Repeat("y", maxRunes+50),
	}
	if err := s.RunHooks(context.Background(), payload); err != nil {
		t.Fatal(err)
	}

	// $1 is the event name.
	ev, _ := os.ReadFile(eventFile)
	if string(ev) != EventBeforeTool {
		t.Fatalf("$1 = %q, want %q", ev, EventBeforeTool)
	}
	// stdin is the JSON payload; long fields are clipped.
	raw, _ := os.ReadFile(payloadFile)
	var got Payload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, raw)
	}
	if got.Event != EventBeforeTool || got.Tool != "bash" {
		t.Fatalf("payload = %+v", got)
	}
	if got.Args["timeout_ms"] != float64(5) {
		t.Fatalf("args lost non-string value: %+v", got.Args)
	}
	if !strings.Contains(got.Args["command"].(string), "…[+50 runes]") {
		t.Fatalf("command arg not clipped: %q", got.Args["command"])
	}
	if !strings.Contains(got.Text, "…[+50 runes]") {
		t.Fatalf("text not clipped: %.30s…", got.Text)
	}
}

func TestHookTimeout(t *testing.T) {
	quietLogs()
	s := parseOne(t, EventBeforeRun, "sleep 5", "150ms")
	start := time.Now()
	err := s.RunHooks(context.Background(), &Payload{Event: EventBeforeRun})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out after 150ms") {
		t.Fatalf("err = %v, want timeout message", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took %v, hook did not honor the deadline", d)
	}
}

func TestHookOutputSurfacesInError(t *testing.T) {
	quietLogs()
	s := parseOne(t, EventBeforeTool, "echo reason-on-stderr >&2; echo reason-on-stdout; exit 1", "")
	err := s.RunHooks(context.Background(), &Payload{Event: EventBeforeTool})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"reason-on-stderr", "reason-on-stdout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing hook output %q", err, want)
		}
	}
}

func TestPluginRegistersOnlyConfiguredEvents(t *testing.T) {
	s := parseOne(t, EventBeforeTool, "true", "")
	p, err := NewPlugin(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "garess_hooks" {
		t.Fatalf("plugin name = %q", p.Name())
	}
	if p.BeforeToolCallback() == nil {
		t.Error("before_tool callback not registered")
	}
	if p.OnEventCallback() != nil {
		t.Error("on_event callback should not be registered (no hooks for it)")
	}
	if p.BeforeRunCallback() != nil {
		t.Error("before_run callback should not be registered (no hooks for it)")
	}
	_ = p.Close() // must not panic (CloseFunc defaulted)
}

func TestPayloadAndBlockingClassification(t *testing.T) {
	// Every ValidEvents entry parses, and the blocking set is exactly the
	// documented pre-hooks.
	wantBlocking := map[string]bool{
		EventBeforeRun: true, EventOnUserMessage: true, EventBeforeAgent: true,
		EventBeforeModel: true, EventBeforeTool: true,
	}
	if len(ValidEvents) != len(blockingEvents)+7 {
		t.Fatalf("unexpected event count: %d events, %d blocking", len(ValidEvents), len(blockingEvents))
	}
	for _, ev := range ValidEvents {
		if !IsValidEvent(ev) {
			t.Errorf("IsValidEvent(%q) = false", ev)
		}
		if got := Blocking(ev); got != wantBlocking[ev] {
			t.Errorf("Blocking(%q) = %v, want %v", ev, got, wantBlocking[ev])
		}
		if !parseOne(t, ev, "true", "").Has(ev) {
			t.Errorf("Has(%q) = false after parse", ev)
		}
	}
}
