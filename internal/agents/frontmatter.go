package agents

import "strings"

// SplitFrontmatter splits a leading YAML frontmatter block (a document
// starting with a `---` line and ending with a closing `---` line) from the
// markdown body that follows it. ok is false when the document has no
// frontmatter block.
//
// Instruction files, skill files and custom-agent (persona) files all share
// this delimiter; each package decides whether frontmatter is required and
// which fields it keeps.
func SplitFrontmatter(content string) (frontmatter, body string, ok bool) {
	content = strings.TrimPrefix(content, "\ufeff")
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", "", false
}
