# Agent-managed onboarding

When a user asks to set up, configure, add, remove, update, or troubleshoot
Bifrost Model Router, own the requirements gathering and implementation. Do not
require the user to understand Bifrost JSON, Docker networking, adapters, model
catalogs, or Codex provider configuration.

## Conversation contract

- Treat vague requests such as “set it up” as the start of requirements
  gathering, not as missing information or a reason to stop.
- Ask focused questions until the provider and plan scope is clear. Prefer one
  consolidated question at a time over a long questionnaire.
- First establish which provider plans or API accounts the user wants available
  in Codex. Support any Bifrost-native or OpenAI-compatible provider without
  advertising, recommending, or implying affiliation with specific downstream
  companies.
- After authenticated discovery, ask whether each provider should expose all
  available models or a model/family allowlist. Do not make users enumerate
  models before showing them what their account can access.
- Always retain OpenAI through the user's Codex login. This is an invariant,
  not a requirements question.
- Inspect `~/.config/bifrost-model-router`, the managed Codex block, and the
  named Docker containers to detect first-time versus additive setup. Preserve
  existing providers, credentials, and model entries by default. Ask only if
  the user explicitly requests removal or discovered state is contradictory.
- Treat the virtual key currently stored in the active Codex provider block as
  part of the existing state. A newly declared bootstrap key does not update an
  older persisted virtual key automatically.
- Default new Codex threads to `gpt-5.6-sol` with `medium` reasoning. Do not ask
  the user to choose a default. Change it only when the user explicitly asks or
  authenticated discovery proves it unavailable; in that case select the
  closest available OpenAI coding model and explain the fallback.
- Resolve informal names and likely typos through research, then confirm the
  interpretation instead of rejecting the request.
- Explain material constraints and tradeoffs in plain language. Ask for user
  input when a plan is ambiguous, API access is uncertain, provider
  documentation conflicts with account discovery, or a change would remove
  existing access.
- Never claim every provider is automatically compatible. Bifrost-native and
  OpenAI-compatible Chat Completions providers should normally work. A provider
  with another protocol or unusual authentication may require an adapter; if
  so, explain the gap and offer to implement or configure it.

## Requirements to discover

For each requested provider, determine:

1. Provider name and the exact subscription, coding plan, or API product.
2. Whether that plan includes API access and which API endpoint applies.
3. Credential type and a clear environment variable name.
4. Models actually available to that account and plan—not the provider's full
   marketing catalog.
5. Wire protocol: Bifrost native, OpenAI-compatible Chat Completions, or native
   Responses.
6. Verified model capabilities, context windows, modalities, reasoning levels,
   and tool support.
7. Existing local router state that must be merged and preserved.
8. The model permissions of the exact virtual key installed in Codex.

After authenticated, account-aware discovery, summarize the available models
and ask whether to expose all of them or only a subset. If the user wants a
subset, ask which exact models or model families should be visible. Do not ask
them to curate IDs before discovery, and do not silently expose everything
without confirming that choice.

Use current official provider documentation as the primary source. Use an
authenticated model-list endpoint when it is account-aware. If discovery is
global rather than plan-aware, intersect it with plan documentation. When
availability remains uncertain, perform a minimal model request after warning
that it may consume a small amount of plan quota.

Resolve context windows and other capability metadata independently from model
availability. Fields returned by the configured provider always take
precedence. When its model endpoint omits a context window, let the router use
the context length from an exact or unambiguous OpenRouter model-ID match.
Retain the conservative provider default only when neither dynamic source has
a trustworthy value. Do not hardcode per-model context windows in application
configuration merely to compensate for an incomplete model endpoint, and
never raise a provider-wide default merely because most models are larger.

Never invent model IDs, context windows, plan entitlements, or capabilities.
For an OpenAI-compatible provider with unverified richer capabilities, use the
conservative `chat_polyfill` and text-only catalog contract.

## Credential handoff

Never ask the user to paste a provider secret into chat, commit it, place it in
the Bifrost JSON, or place it in `~/.codex/config.toml`.

Create these local files outside the repository:

```text
~/.config/bifrost-model-router/config.json
~/.config/bifrost-model-router/providers.env
```

Create `providers.env` with mode `0600` and one empty assignment for each
required credential, for example:

```dotenv
MANAGED_PROVIDER_API_KEY=
```

Tell the user exactly which file to open, ask them to fill the values locally,
and wait for them to say it is ready. This is the normal and minimal required
human secret-handling step. Afterward, verify only that required variables are
non-empty; never print their values. Preserve existing credential entries when
adding another provider.

Provider keys in `config.json` must use `env.NAME` references matching this env
file. The setup script rejects literal provider credentials and env files that
are accessible by group or other users.

## Configuration invariants

Generate one complete Bifrost JSON configuration. Use
`config/quickstart.json`, `config/bifrost.example.json`, and
`docs/configuration.md` as structural references.

Treat the Codex router URL and each Bifrost upstream URL as different layers:

- The Codex model provider `base_url` must point at this router's
  OpenAI-compatible API root and must end in `/v1` without a trailing slash.
  Codex appends operation paths such as `/responses` and `/models`; the setup
  script must generate this value for the user.
- For a standard OpenAI-compatible provider handled by Bifrost, set
  `providers.<name>.network_config.base_url` to the upstream origin or path
  prefix immediately before `/v1`, without a terminal `/v1`. Bifrost appends
  paths such as `/v1/models` and `/v1/chat/completions`; including `/v1` in
  this base URL would produce a duplicated `/v1/v1/...` path.
- Do not add or strip `/v1` globally. For a Bifrost-native provider, follow its
  adapter contract. When an OpenAI-compatible provider uses nonstandard paths,
  keep the appropriate upstream base and configure operation-specific Bifrost
  `request_path_overrides` from official provider documentation.

Before applying a provider configuration, construct and verify the final model
listing and inference URLs. Reject duplicated version segments and do not copy
a provider-documented API base into Bifrost without accounting for the path
that the selected Bifrost adapter appends.

Prefer authenticated, account-aware model discovery. When the provider's model
endpoint reflects the models available to the supplied credential:

1. set the router provider's `discover_models` to `true`;
2. leave the provider credential unrestricted with `models: ["*"]` so it can
   discover present and future account-visible models;
3. allow the local virtual key to use discovered model IDs rather than
   maintaining a static per-model list, unless the user requests an allowlist;
4. use explicit router model entries only for aliases, context variants, or
   capability overrides.

The router publishes newly discovered upstream models without requiring a
configuration regeneration. Provider-level `codex_defaults` supply
conservative metadata for new models, while fields returned by the upstream
catalog take precedence. Codex may still require a full application restart
and a new task before it reloads the catalog.

Use OpenRouter's catalog as the primary display-name source and as a fallback
for missing context length when an exact or unambiguous model-ID match exists.
Never use it to infer account availability, routing, permissions, modalities,
tool support, or reasoning support. Provider-returned context metadata takes
precedence. Name precedence is:

1. an exact `model_name_overrides` entry;
2. an exact or unambiguous OpenRouter editorial match;
3. the display name returned by the configured provider;
4. a readable label derived from the upstream model ID.

Use `model_name_overrides` only for a genuine exception after confirming that
the editorial catalog and provider metadata cannot supply a correct name.
Remove overrides once a reliable dynamic source has the exact model. Remove
editorial publisher prefixes from `Publisher: Model`. Append the configured
provider `display_name` as the hosting source in parentheses for managed
models, for example `Model (VokeAPI)`. Omit the redundant suffix for direct
OpenAI models. OpenRouter metadata is cached and temporary lookup failures must
degrade to the dynamic provider/ID fallback. Do not replace discovery with a
static catalog merely to improve labels. Preserve upstream descriptions,
capabilities, context windows, and reasoning-level metadata; only fill a
context window when the provider omitted it.

To offer an optional managed-provider model allowlist without restoring a
static router catalog:

1. leave the provider credential's model permission unrestricted so Bifrost can
   perform complete authenticated discovery;
2. keep the router provider's `discover_models` set to `true`;
3. set that provider's `allowed_models` on the local Bifrost virtual key to
   exact upstream model IDs or validated `regex:` patterns.

The virtual-key policy must govern both authorization and the model listing
returned for that user. `allowed_models: ["*"]` opts into every currently and
subsequently discovered model. An exact list intentionally requires a policy
change for new models; a `regex:` family can admit matching future models. Do
not copy an allowlist into explicit router model entries, and do not disable
discovery merely to shorten the Codex model picker.

When changing a virtual-key policy, update or replace the exact key installed
in `~/.codex/config.toml`; do not assume a bootstrap key or another
administrator key proves the user's catalog. Verify `/v1/models` with the
installed key without printing it. A catalog that succeeds with a bootstrap
key but omits models with the Codex key is a virtual-key policy mismatch, not a
provider discovery failure.

If the provider has no model endpoint, or its endpoint is a global catalog that
does not reflect account/plan availability, disable `discover_models` and use
an explicit verified model list. This is the fallback, not the preferred path.

Use canonical router slugs in the form `provider/model`. For explicit
overrides, set `upstream_model` to the provider's exact model ID and keep
aliases unambiguous. For custom
OpenAI-compatible Chat providers, configure Bifrost with
`base_provider_type: openai`, allow Chat Completions and streaming, disable
native Responses and model listing when unsupported, and select a router
`chat_polyfill` adapter. Only advertise capabilities verified for that model.

Keep the exact `openai` router provider reserved for Codex request passthrough.
Never forward the Codex OpenAI bearer to a managed provider.

For `chat_polyfill` models, optional hosted tools offered by Codex are removed
before conversion while ordinary function and namespace tools are retained.
The selected model and provider must not change. Explicit hosted-tool choices
fail with `hosted_tool_unsupported`; `tool_choice: required` also fails when no
supported tools remain. Verify that filtered requests identify unavailable
tools to the model, advertise `supports_search_tool: false`, and emit only
privacy-safe capability logs. Native Responses models retain hosted tools only
when the resolved model capability supports them; native wire support alone
does not establish hosted-tool support for every model.

## Apply the setup

Before applying, summarize the resolved scope: providers, plans, enabled
models, credential variable names, and any unverified capabilities. State that
OpenAI passthrough remains enabled and that the default is `gpt-5.6-sol` with
`medium` reasoning unless an automatic availability fallback was necessary.
Tell the user that the local Bifrost virtual key is stored in their mode-`0600`
Codex config and that provider/model defaults affect only new threads.

Then run the low-level executor non-interactively:

```sh
./scripts/setup-local.sh \
  --config "$HOME/.config/bifrost-model-router/config.json" \
  --env-file "$HOME/.config/bifrost-model-router/providers.env" \
  --accept-plaintext-key \
  --accept-new-threads-only \
  --replace
```

The executor pulls the public
`ghcr.io/applyinnovations/bifrost-model-router:main` image by default; local Nix
and the Codex CLI are not required for Desktop users. Use an image override
only when the user explicitly requests another published build.

Omit `--env-file` only when the generated config has no managed-provider
credential references. Do not pass `--replace` unless the existing named
containers belong to this project; inspect them first. The setup script backs
up and preserves unrelated Codex configuration. Its defaults are
`gpt-5.6-sol` and `medium`; pass `--model` or `--reasoning-effort` only for an
explicit user override or verified availability fallback.

## Verification and handoff

1. Verify both containers are running and `http://127.0.0.1/health` succeeds.
2. Read the virtual key from the installed Codex provider block without
   printing it, then use that exact key to verify the hydrated model catalog
   contains exactly the enabled model set. Do not substitute a bootstrap or
   administrator key for this check.
3. For managed providers, make a minimal request to the selected default model
   when the user has accepted the possible quota use.
4. Confirm provider credentials do not appear in generated config, command
   output, logs, or Codex configuration.
5. Report the configured providers, plans, models, default, config location,
   env-file location, backup path, and verification performed.
6. Tell the user to fully quit and reopen Codex after the proxy and Codex
   configuration are installed. Closing only the current task is not a
   substitute for restarting the application.
7. After the restart, tell the user to create a new Codex task. Existing tasks
   retain their original provider/session state and must not be used to verify
   the new provider or model catalog.

If verification fails, diagnose and continue working. Ask the user only for
information or actions that cannot be safely discovered or performed locally.
