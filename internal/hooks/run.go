package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// maxOutput caps the hook output kept for logs/errors so a chatty hook cannot
// balloon memory (relevant on the RPi's 426MB).
const maxOutput = 64 * 1024

// maxSnippet caps the hook output quoted inside an error message.
const maxSnippet = 800

// RunHooks runs every hook configured for p.Event in order. The payload is
// marshalled to JSON and piped to each hook's stdin; the event name is passed
// as $1 and GARESS_HOOK_EVENT.
//
// For blocking events (see blockingEvents) the first non-zero exit aborts:
// an error is returned and the remaining hooks for the event are skipped. For
// notification events every hook runs and failures are logged as warnings.
func (s *Set) RunHooks(ctx context.Context, p *Payload) error {
	cmds := s.byEvent[p.Event]
	if len(cmds) == 0 {
		return nil
	}
	finalSanitize(p)
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("hooks: marshal %s payload: %w", p.Event, err)
	}
	blocking := blockingEvents[p.Event]
	for _, c := range cmds {
		if err := runOne(ctx, c, data); err != nil {
			if blocking {
				return fmt.Errorf("hook %s (%s) blocked: %w", p.Event, c.Command, err)
			}
			slog.Warn("garess hook failed", "event", p.Event, "command", c.Command, "err", err)
		}
	}
	return nil
}

// finalSanitize is the single choke point that bounds every payload just
// before it is marshalled, no matter which caller built it. Plugin payload
// builders already clip, but RunHooks is exported and must stay safe for
// arbitrary Payload values too.
func finalSanitize(p *Payload) {
	p.Text = clip(p.Text, maxRunes)
	p.Error = clip(p.Error, maxRunes)
	p.Tool = clip(p.Tool, 128)
	p.Model = clip(p.Model, 128)
	p.FinishReason = clip(p.FinishReason, 128)
	p.Author = clip(p.Author, 128)
	p.Agent = clip(p.Agent, 128)
	p.EventID = clip(p.EventID, 128)
	p.InvokeID = clip(p.InvokeID, 128)
	p.Session = clip(p.Session, 128)
	p.UserID = clip(p.UserID, 128)
	p.AppName = clip(p.AppName, 128)
	p.Branch = clip(p.Branch, 128)
	p.Args = sanitizeArgs(p.Args)
}

// runOne executes one hook: `sh -c <command> garess-hook <event>` so $0 is the
// command name and $1 is the event, with the JSON payload on stdin. Hook
// output is captured (never inherits the TUI's stdout). Any exit failure,
// including a timeout, is returned as an error.
//
// The hook runs in its own process group (Setpgid) and a timeout kills the
// whole group, not just `sh`: many shells fork the command (dash runs
// `sh -c "sleep 5"` as sh + a child sleep), and killing only the direct child
// leaves orphaned grandchildren holding the captured-output pipe open, which
// makes cmd.Wait() block until they exit on their own — defeating the
// timeout. WaitDelay bounds that pathological case too (a daemonized child
// that left the process group).
func runOne(parent context.Context, c Command, stdin []byte) error {
	ctx, cancel := context.WithTimeout(parent, c.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", c.Command, "garess-hook", c.Event)
	setHookProcGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Kill the hook's whole process group, not just the shell (see
		// setHookProcGroup); on platforms without process groups this falls
		// back to killing the direct child.
		return killHookProcGroup(cmd.Process.Pid)
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "GARESS_HOOK_EVENT="+c.Event)
	cmd.Stdin = bytes.NewReader(stdin)
	var out capBuffer
	out.max = maxOutput
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		snippet := snippetOf(out.String())
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			return fmt.Errorf("timed out after %s%s", c.Timeout, snippet)
		default:
			return fmt.Errorf("%v%s", err, snippet)
		}
	}
	return nil
}

// snippetOf formats captured hook output for embedding in an error: an empty
// " — <trimmed first 800 chars>", otherwise "".
func snippetOf(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	if len(out) > maxSnippet {
		out = out[:maxSnippet] + "…"
	}
	return " — " + out
}

// capBuffer is a bytes.Buffer that keeps only the most recent max bytes,
// bounding memory for runaway hook output while preserving the tail (where an
// error message usually is).
type capBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	c.buf.Write(p)
	if c.buf.Len() > c.max && c.max > 0 {
		b := c.buf.Bytes()
		keep := len(b) - c.max
		// Drop the oldest bytes, keeping the tail.
		c.buf.Reset()
		c.buf.Write(b[keep:])
	}
	return len(p), nil
}

func (c *capBuffer) String() string { return c.buf.String() }
