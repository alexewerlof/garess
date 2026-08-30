# AGENTS.md — internal/llm

Custom Google ADK `model.LLM` over OpenAI-compatible `/v1/chat/completions`
endpoints (llama.cpp, DeepSeek, OpenRouter). No OpenAI SDK is used — the wire
format is hand-rolled so `reasoning_content` stays under our control.

- `NewChatCompletionsModel(providerName, endpoint, apiKey, modelName)` builds
  the `model.LLM`; the endpoint must include `/v1`.
- `GenerateContent` implements the two-method ADK interface (`Name` +
  `GenerateContent(ctx, *model.LLMRequest, stream bool) iter.Seq2[*LLMResponse,
  error]`). Content is genai-typed end-to-end.
- **The ADK flow sets `req.Model` from `Name()`, so the wire request ALWAYS
  uses the configured `modelName`** — never `req.Model`.
- The system instruction arrives in `req.Config.SystemInstruction` (a genai
  Content, role "user") — converted to a `system` message. Conversation
  `req.Contents` map to chat messages: `model` → `assistant`, FunctionCall
  parts → `tool_calls[]` (full arguments JSON), FunctionResponse parts →
  `tool` role keyed by `tool_call_id`. **Thought parts are never sent back**.
- Streaming: partial `LLMResponse{Partial: true}` deltas for text and
  `reasoning_content` (→ `genai.Part{Thought: true}`), then one final
  `Partial: false, TurnComplete: true` response with assembled content +
  tool calls + usage. Tool-call arguments are buffered and parsed only at the
  end.
- `Ping`/`ListModels` (in doctor.go) hit `GET /v1/models` for `garess doctor`.
- Tests use an `httptest` fake OpenAI endpoint (`model_test.go`) — never hit
  real networks in tests.
