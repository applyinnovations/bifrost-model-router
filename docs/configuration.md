# Configuration

The router configuration is versioned and validated strictly. Unknown fields,
unknown adapters, conflicting aliases, invalid reasoning defaults, and unsafe
credential modes stop plugin initialization. The schema is
`config/router.schema.json`; `config/router.example.yaml` shows every major
concept.

Provider fields:

- `display_name`: optional readable hosting-source label appended to managed
  models as `(Provider)`. When omitted, the provider key is humanized. Direct
  OpenAI models omit the redundant source suffix.
- `credential_mode`: `request_passthrough` only for the exact `openai`
  provider, otherwise `bifrost`.
- `responses_mode`: `native`, `chat_polyfill`, or `unsupported`.
- `adapter`: `native`, `openai-chat`, `single-system-message`, or
  `strict-text-only`.
- `native_hosted_tools`: optional mapping from a Codex hosted-tool type to the
  type accepted by this provider's native Responses endpoint. This lets any
  built-in or custom provider bypass `chat_polyfill` only for requests whose
  hosted tools all have a verified native execution path.
- `discover_models`: accept and publish models returned by that provider's
  authenticated model catalog without requiring per-model configuration.
- `model_name_overrides`: optional exact upstream-model-ID to display-name
  mappings. Explicit overrides take precedence over editorial and upstream
  names.
- `codex_defaults`: conservative Codex metadata applied to newly discovered
  models when the upstream catalog omits those fields.

Models use canonical `provider/model` slugs and may have unambiguous aliases.
Model settings override provider Responses mode and adapter. Codex metadata is
conservatively defaulted; a polyfilled model may advertise only text input and
cannot claim hosted search or native image-detail support.

With `discover_models: true`, upstream catalog entries flow into Codex as soon
as they appear. Explicit model entries become metadata, alias, and context
variant overrides; configured overrides absent from the authenticated upstream
catalog are not advertised. Enable discovery only when the provider's model
endpoint is account-aware. For providers without such an endpoint, leave it
disabled and configure a verified static model list.

Discovered catalog IDs are normalized to canonical `provider/model` IDs before
Codex sees them. This is required when the upstream model ID itself contains a
slash: Codex must send the configured Bifrost provider, not interpret the
upstream vendor prefix as a provider. The router uses OpenRouter's public model
catalog as the primary source of standardized editorial names and as a dynamic
fallback for a context length omitted by the configured provider. Matching is
exact or by an unambiguous model-ID suffix. Provider-returned context metadata
always takes precedence, and OpenRouter never controls model availability,
routing, permissions, modalities, tools, or reasoning support. If that catalog
is unavailable or has no safe match, names and context use provider/ID
fallbacks. Every managed-model name ends in the configured hosting source,
such as
`(OpenRouter)` or `(VokeAPI)`, so the same model remains distinguishable across
providers. OpenRouter publisher prefixes are removed from `Publisher: Model`;
direct OpenAI names therefore read like `GPT-5.6 Sol`. Machine-like provider
fallbacks are humanized and provider marketing-tier suffixes are removed.
Descriptions and reasoning metadata continue to come from the configured
provider.

`upstream_model` controls the provider model sent after resolution and defaults
to the portion of the canonical slug after `provider/`. Optional
`context_variants` publish explicit catalog entries such as `-256k`, `-872k`,
and `-1m` without changing the unsuffixed model. Each variant must be a positive
multiple of 1,000 and no larger than the model's verified
`codex.max_context_window`. The effective percentage inherits from the base
profile when omitted:

```yaml
models:
  openai/gpt-5.6-sol:
    aliases: [gpt-5.6-sol]
    upstream_model: gpt-5.6-sol
    codex:
      context_window: 272000
      max_context_window: 872000
      effective_context_window_percent: 95
    context_variants:
      - context_window: 872000
```

The generated `gpt-5.6-sol-872k` entry routes to upstream
`gpt-5.6-sol`. Unknown suffixes are rejected. Unsuffixed entries retain context
and capability fields returned by the upstream catalog; configured values only
fill fields the provider omitted.

Validate and print the effective defaulted configuration with:

```sh
nix run .#config-check -- config/router.example.yaml
```

The plugin configuration is nested inside Bifrost's `plugins[].config`, as in
`config/bifrost.example.json`. Bifrost provider keys should use `env.NAME`.
Never put key literals in a tracked config or Nix expression.

Codex provider configuration is user-level. Generate it with `router profile
render` or install it reversibly with `router profile install`. The generated
profile uses the Responses wire API and Codex's native OpenAI authentication.
It also maps the `x-bf-vk` header from `BIFROST_API_KEY`, so every gateway
request is authorized independently by a Bifrost-managed virtual key.

When omitted, `hosted_tool_fallback_model` defaults to `openai/gpt-6-luna` if
the OpenAI request-passthrough provider can resolve that native Responses model.
Set it explicitly to choose a different available native OpenAI model. When a
Chat Completions-polyfilled model is requested with an OpenAI-hosted/server-side
tool, the transport rewrites the whole request to the fallback before credential
selection. The fallback uses the caller's forwarded OpenAI token; the router
never stores an OpenAI key.

A `chat_polyfill` provider can opt into native Responses for specific hosted
tools without changing its normal text path. A standard OpenAI-compatible
provider that accepts `web_search` unchanged uses:

```yaml
native_hosted_tools:
  web_search: web_search
```

A provider with its own server-tool spelling maps to that spelling instead:

```yaml
native_hosted_tools:
  web_search: openrouter:web_search
```

Renamed tools are accepted only when the offered tool has no extra parameters;
unchanged standard tool types retain their parameters. If any offered hosted
tool lacks a mapping, the complete request uses the OpenAI fallback.

`namespace` is not considered hosted. Bifrost flattens namespace members into
ordinary function tools for providers without native namespace support and
restores namespaced calls in the returned Responses payload.
