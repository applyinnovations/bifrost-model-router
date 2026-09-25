# Proposal: Context-window model variants

Status: Proposed  
Audience: Router maintainers and operators  
Scope: Codex model catalog hydration and provider routing

## Summary

The router should publish explicit context-window variants as ordinary model
entries. A user selects a variant such as `gpt-5.6-sol-872k` or
`managed/text-pro-1m` from the Codex model menu; the router advertises the
variant's context metadata but sends the provider's original model name
upstream.

An unsuffixed model always retains its upstream/default metadata. Merely
enabling variants must not change the behavior, context budget, or compaction
threshold of `gpt-5.6-sol`, `managed/text-fast`, or any other base entry.

Variants are explicitly configured. The router must not infer that every name
ending in `-256k`, `-872k`, or `-1m` is a variant, because those strings can be
part of a real provider model name. An unknown suffixed model is rejected rather
than silently routed to a base model.

## Goals

- Make context choices visible in the normal Codex model picker.
- Preserve upstream defaults for every unsuffixed model.
- Route all variants to the correct underlying provider model.
- Support the same mechanism for OpenAI and managed providers.
- Keep authentication and hosted-tool routing independent from presentation
  slugs.
- Prevent users from selecting context sizes beyond a verified provider limit.
- Keep catalog output deterministic and collision-free.

## Non-goals

- Dynamically changing the context window during a running Codex task.
- Claiming context capacity that the upstream provider has not documented or
  returned.
- Treating arbitrary suffixes as routing instructions.
- Changing the context of existing unsuffixed catalog entries.
- Encoding context-window policy in each user's Codex configuration.

## User experience

Given the following configured variants:

```text
GPT-5.6 Sol
GPT-5.6 Sol (872K)
Managed Text Pro
Managed Text Pro (256K)
Managed Text Pro (1M)
```

Codex receives these slugs:

```text
gpt-5.6-sol
gpt-5.6-sol-872k
managed/text-pro
managed/text-pro-256k
managed/text-pro-1m
```

Selecting `gpt-5.6-sol` uses the exact context metadata returned by the
authenticated OpenAI Codex catalog. Selecting `gpt-5.6-sol-872k` uses the
configured 872,000-token variant metadata, while the request sent upstream
still contains `"model": "gpt-5.6-sol"`.

The model picker should show both raw and effective capacity in descriptions
where useful. For example, an 872,000-token variant with a 95 percent effective
window has an effective budget of 828,400 tokens.

## Naming rules

Context suffixes use decimal token counts:

| Context window | Suffix |
|---:|---|
| 128,000 | `-128k` |
| 256,000 | `-256k` |
| 272,000 | `-272k` |
| 872,000 | `-872k` |
| 1,000,000 | `-1m` |
| 1,050,000 | `-1050k` |

The suffix is generated from `context_window`; operators do not specify an
independent suffix. This prevents misleading combinations such as a `-1m`
slug advertising 900,000 tokens.

Only exact multiples of 1,000 are accepted initially. Values below one million
use `<n>k`. Exact whole millions use `<n>m`. Non-whole millions continue to use
`k`, so 1,050,000 becomes `-1050k` rather than an ambiguous decimal suffix.

## Proposed configuration

Add an upstream identity and zero or more context variants to each model:

```yaml
models:
  openai/gpt-5.6-sol:
    aliases: [gpt-5.6-sol, gpt-5.6]
    upstream_model: gpt-5.6-sol
    codex:
      # Fallback metadata only. For the unsuffixed OpenAI entry, fields returned
      # by the authenticated upstream Codex catalog take precedence.
      context_window: 272000
      max_context_window: 872000
      effective_context_window_percent: 95
    context_variants:
      - context_window: 872000
        effective_context_window_percent: 95

  managed/text-pro:
    aliases: [text-pro]
    upstream_model: text-pro
    codex:
      context_window: 256000
      max_context_window: 1000000
      effective_context_window_percent: 95
    context_variants:
      - context_window: 256000
        effective_context_window_percent: 95
      - context_window: 1000000
        effective_context_window_percent: 95
```

`upstream_model` defaults to the portion of the canonical slug after the first
`/`, preserving compatibility with existing configuration.

`max_context_window` records the verified provider ceiling. A variant is
invalid when its `context_window` exceeds that ceiling. If no verified maximum
exists, explicit context variants are rejected by default.

The base `codex.context_window` remains useful as fallback metadata for
providers whose `/models` response contains only an ID. It does not overwrite a
Codex-compatible upstream value on an unsuffixed entry.

## Catalog construction

The catalog builder should use the following precedence.

### Unsuffixed base entries

1. Start with the upstream model record.
2. Fill missing Codex-required fields from the configured `codex` profile.
3. Fill any remaining required fields from safe router defaults.
4. Preserve upstream `context_window`, `max_context_window`, effective-window
   percentage, reasoning settings, capabilities, and instructions when present.

This changes hydration from unconditional replacement to fill-missing behavior
for base entries. It formalizes the behavior already desired for the native
OpenAI catalog.

### Suffixed variant entries

1. Copy the fully hydrated base entry.
2. Replace `slug` with the generated suffixed slug.
3. Add a display-name suffix such as `(872K)` or `(1M)`.
4. Set `context_window` and `max_context_window` to the variant window.
5. Set the configured effective-window percentage.
6. Recompute any derived context or compaction metadata.
7. Retain provider capabilities, reasoning levels, modalities, and Responses
   compatibility from the base entry unless explicitly overridden later.

The native OpenAI entries remain byte-for-byte equivalent in meaning to the
authenticated upstream catalog. Configured OpenAI variants are appended after
their base records rather than replacing them.

## Resolution and request routing

Build a deterministic lookup index at configuration load time:

```text
external slug or alias
  -> provider
  -> configured base model
  -> upstream model
  -> optional context variant
```

Examples:

```text
gpt-5.6-sol-872k
  -> openai/gpt-5.6-sol
  -> upstream gpt-5.6-sol

managed/text-pro-1m
  -> managed/text-pro
  -> upstream text-pro
```

The dispatch gateway must rewrite the request model using `upstream_model`, not
by trimming a suffix with a regular expression. That ensures real upstream
models containing similar suffixes remain routable.

Credential selection happens from the resolved provider:

- OpenAI request-passthrough variants use the caller's OpenAI authentication and
  Bifrost's ChatGPT passthrough route.
- Managed-provider variants use Bifrost-held credentials and the
  configured Responses implementation or polyfill.

Hosted-tool fallback is evaluated before final dispatch. Its configured target
may be either a base model or a context variant; final routing still rewrites to
the target's `upstream_model`.

## Missing and invalid suffix behavior

- **No suffix:** resolve the base model and use upstream/default metadata.
- **Configured suffix:** resolve the variant and use its advertised context.
- **Recognizable but unconfigured suffix:** return `unresolved_model`; do not
  silently downgrade to the base model.
- **Suffix on a real upstream model name:** treat it as part of the name unless
  an explicit configured variant owns the complete slug.
- **Alias collision:** reject configuration at startup.

Rejecting unknown variants is important. Silently interpreting a typo such as
`gpt-5.6-sol-827k` as `gpt-5.6-sol` would make Codex compact at an unexpected
point and conceal an operator mistake.

## Validation

Configuration loading should fail when:

- `upstream_model` is empty.
- A variant context is not a positive multiple of 1,000.
- A variant exceeds `max_context_window`.
- Two variants generate the same external slug.
- A generated variant collides with a base slug or alias.
- The effective percentage is outside 1–100.
- A request-passthrough variant belongs to a provider other than OpenAI under
  the current credential policy.

The configuration checker should print the generated slug, raw window,
effective window, provider, and upstream model for operator review.

## API example

The router's `GET /v1/models?client_version=...` response would include:

```json
{
  "models": [
    {
      "slug": "gpt-5.6-sol",
      "display_name": "GPT-5.6 Sol",
      "context_window": 272000,
      "max_context_window": 872000,
      "effective_context_window_percent": 95
    },
    {
      "slug": "gpt-5.6-sol-872k",
      "display_name": "GPT-5.6 Sol (872K)",
      "context_window": 872000,
      "max_context_window": 872000,
      "effective_context_window_percent": 95
    }
  ]
}
```

No additional Codex client configuration is required. The selected catalog
entry supplies the context metadata Codex uses for its local budget and
compaction behavior.

## Observability

For each inference request, structured logs and metrics should distinguish:

- requested external slug;
- resolved base slug;
- upstream model;
- advertised context window;
- provider and Responses mode;
- whether optional unsupported hosted tools were removed.

Model names are safe operational metadata; credentials and prompt contents
remain excluded.

Suggested counters:

```text
router_requests_total{provider,base_model,variant}
router_unresolved_models_total{requested_model}
router_hosted_tool_filter_total{provider,model,tool_type}
```

## Test plan

### Configuration tests

- Generate `-256k`, `-872k`, and `-1m` suffixes correctly.
- Reject non-round values and variants above the verified maximum.
- Reject base, alias, and generated-slug collisions.
- Preserve existing configurations that omit `upstream_model` and variants.

### Catalog tests

- Preserve every upstream field on an unsuffixed OpenAI record.
- Fill only missing fields on a non-Codex-compatible provider record.
- Generate variants with the expected slug, display name, raw window, maximum,
  and effective percentage.
- Keep deterministic ordering with each variant adjacent to its base.
- Do not generate variants that are absent from configuration.

### Routing tests

- Route `gpt-5.6-sol` and `gpt-5.6-sol-872k` to upstream
  `gpt-5.6-sol`.
- Route all managed variants to upstream `text-pro` with Bifrost-managed
  credentials.
- Send OpenAI variants through the raw ChatGPT passthrough.
- Send managed-provider variants through the Responses polyfill.
- Reject an unknown `-512k` variant.
- Prove that an actual provider model whose real name ends in `-1m` is not
  modified without explicit variant configuration.

### End-to-end tests

- Confirm Codex lists base and variant entries in `codex debug models`.
- Launch Codex with each variant and assert the selected provider remains
  `bifrost-router`.
- Capture the mock upstream request and assert the suffix is absent from the
  transmitted model.
- Verify the unsuffixed entry reports the same metadata before and after the
  feature is enabled.
- Verify optional hosted tools are removed without changing the selected model.

### Deployment tests

- Run the full Nix flake checks and image build.
- Build and push through Tekton.
- Confirm registry-webhook-driven image reconciliation.
- Verify the Flux rollout, readiness probes, and zero unexpected restarts.
- Compare direct OpenAI records with the router's unsuffixed OpenAI records.

## Rollout plan

1. Add the schema, resolver, and unit tests without enabling variants.
2. Add catalog generation and upstream-model rewriting behind configuration.
3. Deploy with no configured variants and verify exact catalog parity.
4. Add one OpenAI canary variant, `gpt-5.6-sol-872k`.
5. Exercise it with a fresh Codex task and inspect upstream routing.
6. Add only provider-verified managed-provider variants.
7. Document the entries and remove the canary designation after operational
   validation.

Rollback is configuration-only once the implementation is deployed: removing
`context_variants` removes the synthetic catalog entries while leaving every
base model unchanged.

## Review questions

1. Should variant `max_context_window` equal the selected window, or retain the
   upstream maximum while `context_window` expresses the chosen budget?
2. Should generated variants inherit the base model's display-name style or use
   a configurable display-name template?
3. Should an effective-window percentage be mandatory per variant, or inherit
   the base value when omitted?
4. Do we want variants grouped immediately after their base entry, even when
   upstream priorities would otherwise sort them differently?
5. Which managed-provider context ceilings have been verified strongly enough to expose in
   the initial rollout?

## Recommendation

Adopt explicit, generated catalog variants with `upstream_model` mapping. Keep
unsuffixed models tied to upstream defaults, reject unknown variants, and begin
with a single `gpt-5.6-sol-872k` canary. This keeps the feature discoverable in
Codex, centrally managed, provider-agnostic, and safe to roll back.
