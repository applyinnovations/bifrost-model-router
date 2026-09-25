# Responses compatibility

| Mode | Request path | Credential owner | Supported contract |
| --- | --- | --- | --- |
| `native` | Provider Responses API | Codex for OpenAI; otherwise Bifrost | Provider's native contract |
| `chat_polyfill` | Bifrost Responses-to-Chat mux | Bifrost | Text history, function tools/results, ordinary usage and SSE text/tool events supported by pinned Bifrost |
| `unsupported` | No upstream request | None | Stable `responses_unsupported` error |

The polyfill rejects features that cannot be represented safely:

- `previous_response_id`, Conversations, background mode, and response
  retrieval/cancellation;
- non-function tool types and image/file/audio content on text-only adapters.

Optional hosted tools such as web search, file search, computer use, and code
interpreter are removed before polyfill validation. This lets Codex offer a
tool globally without making an otherwise compatible model unusable. The
selected model does not change. Explicitly selecting an unsupported hosted
tool fails with `hosted_tool_unsupported`.

Adapters are selected by configuration, not provider name. `openai-chat`
validates the common Chat-compatible subset. `strict-text-only` makes modality
loss explicit. `single-system-message` hoists every textual system/developer
message into top-level instructions while preserving all other input order;
Bifrost remains responsible for wire conversion.

CI exercises native and polyfilled non-streaming requests, streamed Responses
event ordering, one terminal completion, and distinct canary credentials at
two fake upstreams. This is a compatibility floor, not a claim of parity with
all stateful OpenAI Responses features.
