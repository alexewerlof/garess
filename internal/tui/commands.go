package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// cmdSpec describes one slash command. name includes the leading slash;
// usage is an optional argument placeholder; desc is the one-liner shown in
// the slash palette and in /help.
type cmdSpec struct {
	name  string
	usage string
	desc  string
	run   func(m Model, args []string) (tea.Model, tea.Cmd)
}

// commands is the single source of truth for slash commands: /help, the
// slash palette and dispatchCommand all derive from it (no duplicated
// literals). The run handlers are attached in init() — storing closures that
// reference helpText/filterCommands inside the package-level literal would
// create an initialization cycle (those functions read `commands`).
var commands = []cmdSpec{
	{name: "/help", desc: "show this help and all commands"},
	{name: "/new", desc: "start a fresh session"},
	{name: "/quit", desc: "exit garess"},
	{name: "/model", usage: "[provider/model]", desc: "switch provider (optionally a model)"},
	{name: "/notes", usage: "list | read <name> | write [-g] <name> <text> | rm <name>", desc: "manage memory notes"},
	{name: "/agents", usage: "[reload]", desc: "show the AGENTS.md / SYSTEM.md files in effect"},
	{name: "/skills", usage: "[reload]", desc: "show installed skills in effect"},
	{name: "/tools", desc: "show the tool policy and available tools"},
	{name: "/sessions", desc: "list past sessions and resume one"},
	{name: "/compress", usage: "[instructions]", desc: "compress the conversation into a summary"},
}

func init() {
	byName := make(map[string]int, len(commands))
	for i, c := range commands {
		byName[c.name] = i
	}
	commands[byName["/help"]].run = func(m Model, _ []string) (tea.Model, tea.Cmd) {
		m.addInfo(helpText())
		return m, nil
	}
	commands[byName["/new"]].run = func(m Model, _ []string) (tea.Model, tea.Cmd) {
		return m.newSession()
	}
	commands[byName["/quit"]].run = func(m Model, _ []string) (tea.Model, tea.Cmd) {
		return m, tea.Quit
	}
	commands[byName["/model"]].run = func(m Model, args []string) (tea.Model, tea.Cmd) {
		if len(args) == 0 {
			m.addInfo(fmt.Sprintf("Current: **%s** (%s).\n\nProviders: %s.",
				m.current, m.providers[m.current].Model, providerNames(m.providers)))
			return m, nil
		}
		return m.switchProvider(args[0])
	}
	commands[byName["/notes"]].run = func(m Model, args []string) (tea.Model, tea.Cmd) {
		return m.handleNotes(args)
	}
	commands[byName["/agents"]].run = func(m Model, args []string) (tea.Model, tea.Cmd) {
		return m.handleAgents(args)
	}
	commands[byName["/skills"]].run = func(m Model, args []string) (tea.Model, tea.Cmd) {
		return m.handleSkills(args)
	}
	commands[byName["/tools"]].run = func(m Model, _ []string) (tea.Model, tea.Cmd) {
		return m.handleTools(nil)
	}
	commands[byName["/sessions"]].run = func(m Model, _ []string) (tea.Model, tea.Cmd) {
		if m.sessionSvc == nil {
			m.err = "no session service — /sessions is unavailable"
			return m, nil
		}
		m.sessionsShow = true
		m.clampSessionsSel()
		m.status = "sessions: ↑↓ pick · ↵ resume · esc close"
		m.updateViewport()
		if m.sessions == nil {
			// First open: kick the async list load (the picker shows
			// "loading…" and refreshes when the result lands).
			return m, m.loadSessions()
		}
		return m, nil
	}
	commands[byName["/compress"]].run = func(m Model, args []string) (tea.Model, tea.Cmd) {
		if m.streaming || m.confirming {
			m.err = "wait for the current response before compressing context"
			return m, nil
		}
		return m.startCompression(false, strings.Join(args, " "), nil, "")
	}
}

// commandAliases maps alias names to their canonical command.
var commandAliases = map[string]string{"/compact": "/compress"}

// findCommand resolves a typed slash name (with the leading "/") to its spec,
// following aliases. ok is false for unknown commands.
func findCommand(name string) (cmdSpec, bool) {
	if canon, ok := commandAliases[name]; ok {
		name = canon
	}
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return cmdSpec{}, false
}

// filterCommands returns the commands whose name starts with the typed prefix
// (the text after "/", e.g. "no" → /notes, /new). Used by the slash palette.
func filterCommands(prefix string) []cmdSpec {
	var out []cmdSpec
	for _, c := range commands {
		if strings.HasPrefix(strings.TrimPrefix(c.name, "/"), prefix) {
			out = append(out, c)
		}
	}
	return out
}

// dispatchCommand runs a typed slash line ("/name arg1 arg2 …") through the
// registry.
func dispatchCommand(m Model, line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return m, nil
	}
	cmd, ok := findCommand(fields[0])
	if !ok {
		m.err = fmt.Sprintf("unknown command %q — type /help", fields[0])
		return m, nil
	}
	return cmd.run(m, fields[1:])
}

// helpText renders the /help markdown from the command registry.
func helpText() string {
	var b strings.Builder
	b.WriteString("**garess commands**\n\n")
	for _, c := range commands {
		usage := c.usage
		if usage != "" {
			usage = " " + usage
		}
		fmt.Fprintf(&b, "- `%s%s` — %s\n", c.name, usage, c.desc)
	}
	b.WriteString("  (`/compact` is an alias of `/compress`)\n\n")
	b.WriteString("**Keys**\n\n" +
		"- `enter` — send\n" +
		"- `ctrl+j` — insert newline\n" +
		"- type `/` — slash-command palette\n" +
		"- `ctrl+t` — show/hide the model's thinking\n" +
		"- `y` / `n` — approve / deny a tool that asks for confirmation\n" +
		"- `esc` — stop the current response (or deny a confirmation)\n" +
		"- `ctrl+c` — quit")
	return b.String()
}
