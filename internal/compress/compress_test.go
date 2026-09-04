package compress

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
)

func userEvent(text string) *session.Event {
	return &session.Event{
		Author:      "user",
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleUser)},
	}
}

func agentEvent(text string) *session.Event {
	return &session.Event{
		Author:      "garess",
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel)},
	}
}

func toolCallEvent() *session.Event {
	return &session.Event{
		Author: "garess",
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{
			{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "bash", Args: map[string]any{"command": "ls"}}},
		}, genai.RoleModel)},
	}
}

func toolResultEvent() *session.Event {
	return &session.Event{
		Author: "garess",
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "bash", Response: map[string]any{"output": "f.txt"}}},
		}, genai.RoleUser)},
	}
}

func thoughtEvent(text string) *session.Event {
	return &session.Event{
		Author: "garess",
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromParts([]*genai.Part{
			{Text: text, Thought: true},
		}, genai.RoleModel)},
	}
}

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Error("empty string should estimate 0")
	}
	if got := EstimateTokens("abcd"); got != 1 {
		t.Errorf("4 chars -> %d tokens, want 1", got)
	}
	if got := EstimateTokens("abcdefgh"); got != 2 {
		t.Errorf("8 chars -> %d tokens, want 2", got)
	}
	if got := EstimateTokens("héllo wörld"); got <= 0 {
		t.Errorf("non-ASCII text should estimate > 0, got %d", got)
	}
}

func TestPickCutoff(t *testing.T) {
	// user -> assistant -> tool -> tool result -> user2 -> assistant2
	events := []*session.Event{
		userEvent("first question"),
		agentEvent("first answer"),
		userEvent("second question"),
		agentEvent("second answer"),
	}
	cut, ok := PickCutoff(events)
	if !ok {
		t.Fatal("expected a cutoff for a multi-exchange history")
	}
	if cut != 1 {
		t.Fatalf("cutoff = %d, want 1 (before the last user message at index 2)", cut)
	}

	// A third exchange with tool events: the cutoff is still right before the
	// last user message, so its tool chain stays verbatim in the tail.
	events = append(events, userEvent("third question"), toolCallEvent(), toolResultEvent(), agentEvent("done"))
	cut, ok = PickCutoff(events)
	if !ok || cut != 3 {
		t.Fatalf("cutoff = %d (%v), want 3", cut, ok)
	}
}

func TestPickCutoffNothingToCompress(t *testing.T) {
	if _, ok := PickCutoff([]*session.Event{userEvent("only one")}); ok {
		t.Error("a single user message should not be compressible")
	}
	if _, ok := PickCutoff([]*session.Event{agentEvent("no user at all")}); ok {
		t.Error("no user messages should not be compressible")
	}
	if _, ok := PickCutoff(nil); ok {
		t.Error("empty history should not be compressible")
	}
}

func TestTranscriptRolesAndParts(t *testing.T) {
	events := []*session.Event{
		userEvent("hello"),
		agentEvent("hi there"),
		toolCallEvent(),
		toolResultEvent(),
		thoughtEvent("hidden reasoning"),
	}
	out := Transcript(events, 0)
	for _, want := range []string{"USER: hello", "ASSISTANT: hi there", "TOOL CALL: bash {\"command\":\"ls\"}", "TOOL RESULT: bash =>"} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hidden reasoning") {
		t.Error("thinking parts must be omitted from the transcript")
	}
}

func TestTranscriptCapsKeepingNewest(t *testing.T) {
	events := []*session.Event{
		userEvent(strings.Repeat("old", 500)),
		userEvent("FRESH"),
	}
	out := Transcript(events, 100)
	if !strings.Contains(out, "FRESH") {
		t.Error("transcript cap should keep the newest content")
	}
	if strings.Contains(out, strings.Repeat("old", 500)) {
		t.Error("oldest content should be trimmed past the cap")
	}
	if !strings.Contains(out, "omitted") {
		t.Error("truncation should be marked")
	}
	if len(out) > 200 {
		t.Errorf("capped transcript too long: %d", len(out))
	}
}

func TestSummaryPrompt(t *testing.T) {
	sys, user := SummaryPrompt("", "TRANSCRIPT")
	if !strings.Contains(sys, "coding assistant") {
		t.Error("system prompt should target a coding assistant digest")
	}
	if !strings.Contains(user, "TRANSCRIPT") || strings.Contains(user, "Compression focus") {
		t.Errorf("unexpected user prompt without instruction:\n%s", user)
	}
	_, user2 := SummaryPrompt("keep the API design details", "T")
	if !strings.Contains(user2, "keep the API design details") {
		t.Error("user instruction should be threaded into the summary prompt")
	}
}

func TestFormatSummary(t *testing.T) {
	got := FormatSummary("  digest text  ")
	if !strings.HasPrefix(got, SummaryHeader) || !strings.Contains(got, "digest text") {
		t.Errorf("FormatSummary = %q", got)
	}
}

// fakeModel implements model.LLM for Summarize tests.
type fakeModel struct {
	name string
	out  string
	err  error
	got  *model.LLMRequest
}

func (f *fakeModel) Name() string { return f.name }

func (f *fakeModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.got = req
		if f.err != nil {
			yield(nil, f.err)
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText(f.out, genai.RoleModel), TurnComplete: true}, nil)
	}
}

func TestSummarize(t *testing.T) {
	fm := &fakeModel{name: "fake", out: "  the digest  "}
	got, err := Summarize(context.Background(), fm, "sys", "usr", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != "the digest" {
		t.Errorf("summary = %q, want trimmed digest", got)
	}
	if fm.got == nil {
		t.Fatal("model was not called")
	}
	if fm.got.Config == nil || fm.got.Config.SystemInstruction == nil {
		t.Fatal("system instruction missing from the summary request")
	}
	if fm.got.Config.MaxOutputTokens != DefaultMaxSummaryTokens {
		t.Errorf("max output = %d, want default %d", fm.got.Config.MaxOutputTokens, DefaultMaxSummaryTokens)
	}
	if len(fm.got.Contents) != 1 || fm.got.Contents[0].Parts[0].Text != "usr" {
		t.Errorf("contents = %+v", fm.got.Contents)
	}
}

func TestSummarizeCustomBudgetAndError(t *testing.T) {
	fm := &fakeModel{name: "fake", out: "x"}
	if _, err := Summarize(context.Background(), fm, "s", "u", 500); err != nil {
		t.Fatal(err)
	}
	if fm.got.Config.MaxOutputTokens != 500 {
		t.Errorf("max output = %d, want 500", fm.got.Config.MaxOutputTokens)
	}

	fm2 := &fakeModel{name: "fake", err: errors.New("boom")}
	if _, err := Summarize(context.Background(), fm2, "s", "u", 0); err == nil {
		t.Error("expected the model error to surface")
	}

	if _, err := Summarize(context.Background(), nil, "s", "u", 0); err == nil {
		t.Error("nil model should error")
	}
}
