# Responses compatibility

| Mode | Request path | Credential owner | Supported contract |
| --- | --- | --- | --- |
| `native` | Provider Responses API | Codex for OpenAI; otherwise Bifrost | Provider's native contract |
| `chat_polyfill` | Bifrost Responses-to-Chat mux | Bifrost | Text history, function tools/results, ordinary usage and SSE text/tool events supported by pinned Bifrost |
| `unsupported` | No upstream request | None | Stable `responses_unsupported` error |

The initial polyfill rejects features that cannot be represented safely:

- `previous_response_id`, Conversations, background mode, and response
  retrieval/cancellation;
- hosted tools such as web search, file search, computer use, and code
  interpreter;
- non-function tool types and image/file/audio content on text-only adapters.

Any provider can declare `native_hosted_tools` mappings. A request whose hosted
tools all have mappings uses that provider's native Responses API, while normal
requests continue through its configured polyfill. Mappings can preserve a
standard type (`web_search: web_search`) or translate it to a provider server
tool (`web_search: openrouter:web_search`). Renamed tools must be
parameter-free; nested and unmapped hosted tools keep the whole-request OpenAI
fallback so their semantics are not silently discarded.

Adapters are selected by configuration, not provider name. `openai-chat`
validates the common Chat-compatible subset. `strict-text-only` makes modality
loss explicit. `single-system-message` hoists every textual system/developer
message into top-level instructions while preserving all other input order;
Bifrost remains responsible for wire conversion.

CI exercises native and polyfilled non-streaming requests, streamed Responses
event ordering, one terminal completion, and distinct canary credentials at
two fake upstreams. This is a compatibility floor, not a claim of parity with
all stateful OpenAI Responses features.
