# Bifrost Codex Model Router: Implementation Plan

Status: accepted; core implementation delivered, production-hardening backlog retained below

Last reviewed: 2026-09-19

Target: a reproducible, provider-agnostic Codex-compatible gateway built around Bifrost

## 1. Outcome

Build a local-first gateway that lets one Codex model provider expose:

- OpenAI's built-in Codex models using Codex's existing OpenAI/ChatGPT authentication, passed through per request and never stored by this project.
- Models from any Bifrost-supported provider using credentials held by Bifrost, never by Codex.
- A Codex-compatible model catalog, produced by hydrating ordinary `GET /v1/models` entries with the metadata Codex needs.
- `POST /v1/responses` for providers that do not implement it natively, using capability-selected plugins and Bifrost's Responses-to-Chat conversion rather than provider-specific forks.
- Native Responses behavior for providers that already support it.
- A pinned Nix development, build, test, packaging, release, and deployment workflow.

The result should behave as one endpoint from Codex's perspective while maintaining a strict credential boundary:

```text
Codex
  |
  | OpenAI auth + Responses API
  v
Bifrost HTTP gateway
  |-- OpenAI model --------> OpenAI, using request-scoped Codex auth
  |-- native Responses ----> provider, using Bifrost-held credentials
  `-- missing Responses ---> Responses polyfill ---> provider Chat Completions
```

## 2. Findings from the adjacent `~/bifrost` proof of concept

The adjacent implementation proves several useful pieces:

- A Bifrost HTTP transport plugin can identify Codex's catalog request by `GET /v1/models?client_version=...`.
- Codex accepts a `{"models":[...]}` catalog containing fields such as `slug`, `model_messages.instructions_template`, context-window metadata, reasoning levels, tool flags, and truncation policy.
- A transport pre-hook can normalize Responses request bodies before provider dispatch.
- Structural logging can be useful without logging prompts.
- Building the Bifrost host and native Go plugin with exactly the same Go toolchain and dependency graph is required for reliable `.so` loading.

It also exposes the limitations this project should remove:

- The plugin and configuration are tied to one managed provider and two hard-coded model names.
- The model endpoint is replaced with a static catalog instead of hydrating upstream/Bifrost model records.
- Compatibility behavior is selected by model-name matching rather than declared capabilities.
- Request normalization is coupled to model-catalog serving in one file.
- The hand-built Docker image and checked-in `.so` are not reproducible or portable.
- The current setup script replaces the user's Codex configuration and does not support one provider that safely splits OpenAI passthrough from Bifrost-owned provider credentials.
- The proof of concept tests request hoisting well, but not streaming Responses event fidelity, auth isolation, provider capability selection, catalog merge semantics, or reproducible deployment.

Reuse the behavioral tests and lessons, not the provider-specific structure or binary artifact.

## 3. Constraints and design principles

### 3.1 Codex contract

Codex custom providers currently use the Responses wire API; `responses` is the only supported `wire_api` value. A custom provider may set `requires_openai_auth = true`, which makes Codex use its existing OpenAI authentication with that proxy. These are documented in the [official Codex configuration reference](https://developers.openai.com/codex/config-reference) and [authentication guide](https://developers.openai.com/codex/auth).

Therefore:

- The public gateway must present a Responses-compatible surface.
- The generated Codex profile should use one custom provider pointing at the gateway with `requires_openai_auth = true`.
- The gateway must treat the inbound bearer as request-scoped OpenAI material, not as the gateway's universal provider credential.
- Custom-provider configuration belongs in the user's Codex configuration/profile, not project-local `.codex/config.toml`, because Codex reserves provider settings to user-level configuration.

### 3.2 Bifrost contract

Pin one audited Bifrost revision. Do not target floating `latest` behavior. At the inspected v1.9.0 core API:

- HTTP transport plugins can modify request/response bodies and stream chunks.
- LLM plugins can run once per request and per attempt.
- Bifrost core contains Responses-to-Chat request conversion, Chat-to-Responses non-streaming conversion, and a streaming conversion path.
- Setting `BifrostContextKeyChangeRequestType` to `ChatCompletionRequest` on a Responses request selects that conversion path.
- Direct keys can bypass the configured key pool when `client.allow_direct_keys` is enabled and the request supplies a raw provider key.

The implementation must verify these statements against the pinned source in CI. If an upgrade changes any hook or context-key contract, the upgrade PR should fail until its contract tests and compatibility table are updated.

### 3.3 General principles

- Capability-driven, not provider-name-driven.
- Preserve native behavior whenever it exists; polyfill only declared gaps.
- Keep provider profiles declarative. Code adapters are for wire-semantic differences, not model lists.
- Fail closed on ambiguous credential routing.
- Never send the inbound OpenAI bearer to a non-OpenAI upstream.
- Never log request/response content or credentials by default.
- Keep conversion functions pure and fixture-testable.
- Preserve unknown JSON fields where possible so newer Codex and provider fields survive the gateway.
- Treat streaming SSE as a protocol with ordering and identity invariants, not as arbitrary text chunks.
- Build Bifrost and native Go plugins together; do not distribute an independently compiled `.so`.

## 4. Proposed architecture

Use the stock pinned `bifrost-http` transport plus one project-owned native plugin bundle. Avoid a second reverse proxy in the initial design: current Bifrost has the conversion and direct-key primitives needed. Add a facade only if the phase-zero spike proves that transport hooks cannot safely implement conditional direct-key handling on the pinned revision.

### 4.1 Components

#### A. `codex-credential-router` transport plugin

Runs before Bifrost authentication and provider key selection.

Responsibilities:

1. Parse only the minimal request envelope needed to resolve `model`; cap body size and do not retain prompt content.
2. Resolve the model through the shared catalog to a provider and capability profile.
3. For an OpenAI passthrough model:
   - require an inbound `Authorization: Bearer ...` supplied by Codex;
   - preserve it only in request-scoped memory;
   - enable Bifrost's direct-key path for the OpenAI provider;
   - reject a provider/model mismatch instead of forwarding an uncertain credential.
4. For every non-OpenAI model:
   - remove the inbound OpenAI `Authorization` header before provider dispatch;
   - disable/direct-key bypass headers even if the caller supplied them;
   - allow Bifrost to choose only its configured provider key.
5. For catalog and health endpoints, follow explicit route rules rather than inferring a provider.
6. Redact credential-bearing fields from all plugin logs and errors.

This plugin is the security boundary. Its policy must be an allowlist: only models resolved to the exact configured OpenAI provider may use request-scoped direct keys.

#### B. `codex-model-catalog` HTTP transport plugin

Hydrates Bifrost's normal `GET /v1/models` result instead of always replacing it.

Pipeline:

1. Detect a Codex catalog request through a configurable matcher, initially `client_version` plus an optional Codex user-agent matcher.
2. Let Bifrost obtain its model list where possible.
3. In `HTTPTransportPostHook`, accept both the ordinary OpenAI list envelope (`data`) and any pinned Bifrost catalog envelope.
4. Normalize each model to an internal `DiscoveredModel`.
5. Resolve a capability profile using exact model override, provider default, and safe global defaults.
6. Hydrate the model with Codex metadata without discarding upstream fields.
7. Convert to the Codex catalog envelope and order deterministically.
8. Add configured OpenAI passthrough models that Bifrost cannot discover without a persistent OpenAI key.
9. Deduplicate aliases and reject conflicting ownership at startup.

Precedence, from highest to lowest:

1. exact model override;
2. provider profile;
3. Bifrost model-catalog capabilities;
4. upstream model fields;
5. conservative project defaults.

The hydrated record should cover at least:

- stable slug and aliases;
- display name and description;
- context and maximum context windows;
- effective context percentage;
- input modalities;
- default and supported reasoning levels;
- reasoning-summary and verbosity support;
- tool/search/image support;
- truncation policy;
- instructions template selection;
- visibility, priority, and API availability;
- explicit `responses_mode`: `native`, `chat_polyfill`, or `unsupported`.

Do not silently advertise a capability the adapter cannot preserve. For example, omit image input or built-in web search for a Chat Completions polyfill unless an end-to-end test demonstrates it.

Catalog data should come from declarative files, not Go constants. The loader should support a compiled default catalog plus operator overlays. Validate overlays against a JSON Schema at startup.

#### C. `responses-capability-router` LLM plugin

This is the provider-agnostic polyfill selector.

For every Responses request:

1. Resolve the requested model through the same immutable catalog snapshot used by the model endpoint.
2. Read its `responses_mode`.
3. `native`: leave the request type unchanged.
4. `chat_polyfill`: validate the request against the polyfill's supported subset, apply any declared normalization, then set Bifrost's request-type change context to Chat Completions.
5. `unsupported`: return a clear, stable 400 error before any provider call.

Bifrost core then performs Responses-to-Chat conversion, provider invocation, and Chat-to-Responses conversion. This plugin should not duplicate Bifrost's mux unless a conformance test identifies a gap.

Extension points should be small interfaces registered by name:

```go
type CapabilityAdapter interface {
    Name() string
    Validate(*schemas.BifrostResponsesRequest, ModelProfile) *CompatError
    Normalize(*schemas.BifrostResponsesRequest, ModelProfile) *CompatError
}
```

Initial adapters:

- `native`: no-op and validation of native-only advertised features.
- `openai-chat`: generic Responses-to-OpenAI-compatible-Chat polyfill selection.
- `single-system-message`: optional normalization for backends that permit only one leading system message and activated by a capability flag.
- `strict-text-only`: rejects image/file/audio content before it is lost.

Adapters are dynamically selected from configuration. A new provider should normally require a model/provider profile only. New Go code is justified only for a genuinely different wire constraint.

#### D. Shared catalog and configuration library

All plugins must use one parsed, immutable configuration snapshot so routing, authentication, advertised metadata, and conversion agree.

Suggested schema:

```yaml
version: 1

providers:
  openai:
    credential_mode: request_passthrough
    responses_mode: native
    adapter: native
  managed:
    credential_mode: bifrost
    responses_mode: chat_polyfill
    adapter: single-system-message

models:
  openai/gpt-example:
    aliases: [gpt-example]
    codex:
      display_name: GPT Example
      context_window: 128000
      input_modalities: [text, image]
      supported_reasoning_levels: [low, medium, high]
  managed/example-model:
    codex:
      display_name: Example Model
      context_window: 131072
      input_modalities: [text]
```

Startup validation must catch:

- duplicate slugs or aliases;
- alias ownership conflicts;
- unknown adapter names;
- OpenAI passthrough on a non-OpenAI provider;
- `native` Responses claims for a provider marked Chat-only;
- impossible defaults, such as a default reasoning level not in the supported set;
- invalid context windows and percentages;
- secret-looking literal values in committed configuration.

### 4.2 OpenAI authentication and passthrough flow

The generated Codex profile should look conceptually like:

```toml
model_provider = "bifrost-router"

[model_providers.bifrost-router]
name = "Bifrost Router"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "responses"
requires_openai_auth = true
```

Do not set `env_key`, a static bearer, or a provider API key in this profile. Official Codex documentation says `requires_openai_auth` uses the user's OpenAI login/API key and ignores `env_key`.

At the gateway:

- OpenAI model: inbound bearer becomes the direct request key for the OpenAI provider only.
- Non-OpenAI model: inbound bearer is stripped; Bifrost resolves its configured provider key.
- Unknown/bare ambiguous model: reject it before key selection.

The Bifrost OpenAI provider should be configured with no stored OpenAI key and direct keys enabled. Non-OpenAI providers use only `env.*`, file-descriptor/credential-file indirection where supported, or a deployment secret manager. Never render secrets into the Nix store.

If the pinned Bifrost transport cannot conditionally activate direct keys without exposing the bearer to its own inference-auth middleware, implement a minimal front controller as a fallback:

- stream-preserving reverse proxy for OpenAI model requests;
- Bifrost proxy for all other models;
- catalog aggregation at the front controller;
- the same catalog and plugin interfaces as above.

That fallback is an explicit ADR decision after the spike, not the default architecture.

### 4.3 Responses compatibility contract

Define and publish three support tiers:

1. `native`: upstream/provider implementation receives Responses semantics directly.
2. `polyfilled`: text, developer/system/user/assistant history, function tools, tool results, structured output where Bifrost preserves it, reasoning text where available, usage, errors, and SSE lifecycle are translated.
3. `unsupported`: stateful Responses lifecycle operations, hosted tools, or modalities that cannot be represented are rejected clearly.

For the first release, do not claim parity for:

- `previous_response_id` or server-side conversation storage;
- background responses and retrieval/cancel lifecycle;
- hosted OpenAI tools such as web/file search or code interpreter;
- computer-use events;
- encrypted reasoning round trips;
- image/file/audio inputs on text-only Chat providers;
- WebSocket Responses transport.

Each may be promoted only after an adapter implementation and contract tests exist.

### 4.4 Streaming invariants

The stream bridge must maintain:

- a `response.created` event before output events;
- monotonically increasing sequence numbers where the schema uses them;
- stable response, item, content-part, and tool-call IDs;
- item-added before item delta, and content-part-added before text delta;
- ordered tool argument deltas and a completed tool item;
- exactly one terminal completed/failed/incomplete event;
- valid SSE framing, including heartbeats/comments;
- cancellation propagation when Codex disconnects;
- no goroutine or request-state leaks.

Treat known upstream bridge regressions as mandatory regression fixtures, especially missing item/content-part announcements and loss of reasoning or tool-call structure.

## 5. Repository layout

```text
.
├── cmd/
│   └── config-check/               # validates and renders effective config
├── internal/
│   ├── catalog/                    # discovery, merge, hydration, snapshots
│   ├── config/                     # schema, loading, validation
│   ├── credentials/                # provider resolution and auth policy
│   ├── responses/                  # adapter registry and compatibility errors
│   └── testkit/                    # SSE parser, mock providers, golden helpers
├── plugins/
│   └── codex-router/
│       ├── plugin.go               # exports Bifrost plugin entry points
│       ├── transport_auth.go
│       ├── model_catalog.go
│       └── responses_router.go
├── config/
│   ├── router.example.yaml
│   ├── router.schema.json
│   └── bifrost.example.json
├── fixtures/
│   ├── codex-models/
│   ├── responses/
│   └── streams/
├── nix/
│   ├── bifrost.nix
│   ├── plugin.nix
│   ├── module.nix                 # NixOS module
│   └── checks.nix
├── scripts/                        # thin wrappers only; logic stays in Go/Nix
├── docs/
│   ├── architecture.md
│   ├── compatibility.md
│   ├── configuration.md
│   ├── security.md
│   ├── operations.md
│   └── adr/
├── flake.nix
├── flake.lock
├── go.mod
├── go.sum
├── justfile
└── .tekton/
```

## 6. Delivery phases

### Phase 0: feasibility spikes and ADRs

Deliverables:

- Pin the candidate Bifrost revision and Codex client versions.
- Prove OpenAI direct-key passthrough with a fake OpenAI upstream while Bifrost stores no OpenAI key.
- Prove the same Codex provider can select a non-OpenAI model whose credential comes only from Bifrost.
- Capture outbound headers at both fake upstreams and prove cross-provider credential isolation.
- Prove `BifrostContextKeyChangeRequestType` polyfills both non-streaming and streaming Responses requests.
- Prove `HTTPTransportPostHook` can hydrate Bifrost's model response; document the actual response envelopes.
- Compare native `.so` and any supported WASM loader on the pinned revision. Choose native unless WASM supports all required LLM, HTTP response, and stream hooks.
- Record ADRs for plugin format, auth flow, catalog envelope, and whether a front controller is necessary.

Exit criterion: one end-to-end test exercises an OpenAI passthrough model and one Chat-only mock provider through the same Codex provider URL without credential crossover.

### Phase 1: reproducible skeleton

- Create the Go module, package boundaries, configuration schema, and example config.
- Add the flake, lockfile, dev shell, formatter, checks, package outputs, and `just` commands.
- Package the pinned Bifrost source and plugin in one derivation/toolchain closure.
- Add config validation and deterministic effective-config output with secrets redacted.
- Add a local `nix run .#router` process composition with ephemeral state.

Exit criterion: a clean clone builds and runs with `nix develop`, `nix flake check`, and `nix run` without Docker or globally installed Go tools.

### Phase 2: generic catalog hydration

- Implement model discovery and normalization.
- Implement overlay precedence, alias validation, and deterministic sorting.
- Implement versioned Codex metadata templates and the Codex request matcher.
- Preserve unknown upstream fields and define behavior for malformed/partial records.
- Add cache headers and a bounded catalog cache with explicit invalidation/reload behavior.
- Add a `router catalog explain <model>` diagnostic showing field provenance without secrets.

Exit criterion: golden tests cover empty, ordinary, mixed-provider, duplicate, malformed, and future-field catalogs; a pinned Codex client displays and selects hydrated models.

### Phase 3: Responses capability routing

- Implement the adapter registry and capability resolver.
- Implement `native`, `openai-chat`, `single-system-message`, and `strict-text-only` adapters.
- Use Bifrost's mux path through the context request-type flag.
- Produce stable, machine-readable compatibility errors.
- Add explicit rejection for unsupported Responses lifecycle operations and hosted tools.

Exit criterion: the same contract suite passes for a native mock provider and the polyfilled Chat-only provider, except for documented/advertised differences.

### Phase 4: authentication boundary

- Implement conditional OpenAI direct-key activation.
- Strip inbound auth and direct-key controls on all Bifrost-credential routes.
- Add provider/model resolution before credential routing.
- Disable content/raw request storage by default and harden error/log redaction.
- Generate a reversible Codex profile installer that preserves the user's existing configuration and supports uninstall/rollback.

Exit criterion: adversarial auth tests prove that OpenAI tokens cannot reach non-OpenAI mocks, provider keys cannot reach Codex/OpenAI, and unknown models fail closed.

### Phase 5: production hardening and operations

- Add metrics, traces, structured audit events, readiness, and degraded-state reporting.
- Add bounded timeouts, connection limits, retry budgets, backpressure, and clean shutdown.
- Add hot-reload or atomic restart for catalog/config changes; never expose a partially parsed config.
- Add the NixOS module, systemd hardening, secret credentials, state directories, and rollback documentation.
- Add release artifacts, SBOM, provenance, vulnerability scanning, and upgrade automation.

Exit criterion: soak, fault-injection, upgrade/rollback, and restart tests pass; the operator runbook covers all alerts and failure modes.

## 7. Test strategy

### 7.1 Unit tests

Catalog:

- all merge-precedence cases;
- alias and slug normalization;
- collision detection;
- defaulting and validation;
- deterministic ordering/serialization;
- unknown-field preservation;
- Codex client-version template selection;
- model capability provenance.

Credentials:

- exact OpenAI provider allowlist;
- provider-prefixed and bare aliases;
- unknown/ambiguous model rejection;
- inbound bearer removal on every non-OpenAI path;
- spoofed `x-bf-direct-key` removal;
- case-insensitive header handling;
- errors and logs never contain token substrings.

Responses adapters:

- string and array input;
- system/developer instruction hoisting across the entire input;
- preservation of non-instruction order;
- function definitions, tool choice, parallel calls, and tool outputs;
- structured output conversion;
- reasoning effort/summary handling;
- unsupported image/file/audio/hosted-tool rejection;
- parameter dropping only when explicitly configured;
- idempotence where normalization is applied twice;
- malformed JSON passes to normal validation or returns the declared stable error.

### 7.2 Property and fuzz tests

- Fuzz catalog JSON and request inputs for panics, excessive allocation, and accidental content logging.
- Property: hydration is deterministic.
- Property: hydration never removes an unrelated upstream field.
- Property: non-instruction input order is preserved.
- Property: no secret-classified header appears in a non-OpenAI captured request.
- Property: each emitted stream has one legal terminal event and no deltas reference unknown IDs.
- Run `go test -race` over plugin state, reload, streaming cancellation, and catalog cache tests.

### 7.3 Contract tests with fake providers

Implement `httptest` upstreams for:

- native OpenAI Responses;
- OpenAI-compatible Chat-only;
- non-OpenAI native message APIs if exercised through Bifrost;
- models endpoint with partial/nonstandard metadata;
- rate limit, timeout, disconnect, malformed JSON, malformed SSE, and mid-stream failure.

Record every received path, header, body shape, and cancellation. Assert credential identities with canary tokens unique to each provider.

Run a shared Responses contract suite against native and polyfilled modes:

- basic text, multi-turn text, instruction ordering;
- one and multiple function calls;
- tool-call continuation;
- usage accounting;
- incomplete/max-token completion;
- provider 4xx/429/5xx mapping;
- streaming text and tool calls;
- client cancellation and timeout.

### 7.4 Golden and compatibility tests

- Store canonical model-catalog responses per supported Codex client contract.
- Store non-streaming Responses bodies and normalized SSE event sequences.
- Normalize volatile IDs/timestamps before golden comparison, but separately test ID relationships.
- Require an explicit golden-update command and human-reviewed diff.
- Test at least the oldest and newest supported Codex versions; add a scheduled canary against the current release.

### 7.5 End-to-end tests

In an isolated network namespace/process composition:

1. Start mock OpenAI and non-OpenAI providers.
2. Start the Nix-built Bifrost host and plugin with temporary configuration/state.
3. Query the Codex catalog and select each model.
4. Run non-streaming and streaming prompts with function tools.
5. Assert the chosen upstream and captured credential.
6. Kill/restart Bifrost during idle and active requests.
7. Reload catalog overlays and verify atomic visibility.

Where automation is stable, run the pinned Codex CLI in non-interactive mode against the gateway. Keep a lower-level HTTP conformance suite so a Codex binary change cannot obscure a gateway regression.

### 7.6 Performance tests

Track, but do not prematurely optimize:

- catalog hydration latency and allocations at 100, 1,000, and 10,000 models;
- added non-streaming latency;
- time to first SSE event;
- per-event conversion overhead;
- concurrent streams and memory per stream;
- cancellation cleanup and goroutine counts.

Set release budgets after the baseline spike. A reasonable initial goal is sub-millisecond p95 plugin overhead excluding upstream/network time, with no unbounded buffering of streamed response bodies.

### 7.7 Security tests

- Canary-secret matrix across every route/provider/error/retry/fallback.
- Header smuggling, mixed-case and duplicate authorization headers.
- Oversized model catalog and request bodies.
- Alias confusion and provider-prefix traversal.
- SSRF through provider base URLs and catalog sources.
- Symlink and permission attacks on config/secret files.
- Logs, traces, metrics, crash output, and Nix derivations scanned for secrets.
- Dependency and container/Nix closure vulnerability scans.
- Negative test that raw request/response storage remains disabled.

## 8. Nix flake, DevEx, and packaging

### 8.1 Flake inputs and outputs

Pin:

- `nixpkgs` to a reviewed revision;
- Bifrost source to an immutable revision/hash;
- `flake-parts` or an equally small flake composition library if useful;
- supported systems explicitly, initially `x86_64-linux` and `aarch64-linux`.

Outputs:

- `packages.<system>.bifrost-router`: host binary, native plugin, schemas, default catalog, and launcher in one closure;
- `packages.<system>.plugin`: diagnostic/debug output only, built with the identical Go dependency graph;
- `apps.<system>.default`: local router;
- `apps.<system>.config-check`;
- `devShells.<system>.default`;
- `checks.<system>.*` for formatting, lint, unit, race where supported, contract, integration, schema, vulnerability policy, and package smoke tests;
- `nixosModules.default`;
- optional OCI image generated from the Nix closure, not a separate Dockerfile build.

### 8.2 Native plugin reproducibility

Use one derivation or tightly coupled derivations to build the pinned Bifrost host and `.so` with:

- the same Go compiler;
- identical versions of every shared package;
- `CGO_ENABLED=1` and a declared libc/linker choice;
- no `vendorHash = null` or network access during build;
- `-trimpath` and deterministic build metadata;
- a post-build smoke test that starts the host, loads the plugin, and checks `/health`.

Record the host revision, plugin revision, Go version, target, and configuration schema version in `router version` output. Refuse to start on a detectable ABI/version mismatch.

Do not commit `.so` files.

### 8.3 Development shell

Include pinned versions of:

- Go and CGO toolchain;
- `gopls`, `go-tools`, `golangci-lint` or a smaller pinned lint set;
- `gotestsum`, `govulncheck`, `staticcheck`;
- `jq`, `yq`, `curl`, `just`, `shellcheck`, `shfmt`;
- JSON Schema/YAML validation tools;
- an SSE inspection helper;
- optional `delve` and load-test tooling.

Set no secrets in the shell derivation. Use `.envrc`/direnv only to enter the flake; keep secrets in the process environment or runtime credentials ignored by Git.

### 8.4 Developer commands

Provide a small, memorable surface:

```text
just fmt
just lint
just test
just test-race
just test-contract
just test-e2e
just run
just check-config
just update-goldens
just package
```

Every command should call a pinned Nix/Go tool. CI should invoke the same commands or `nix flake check`, not maintain separate behavior.

### 8.5 NixOS module

Options should cover:

- enable/package/listen address;
- Bifrost and router declarative configuration;
- provider credential files or systemd credentials;
- state and cache directories;
- log level without content logging;
- metrics listener and firewall choice;
- systemd restart policy and resource limits.

Hardening defaults:

- dedicated dynamic user;
- read-only system and package closure;
- private temporary directory;
- no new privileges;
- restricted address families and capabilities;
- explicit writable state paths;
- systemd `LoadCredential` for provider secrets;
- loopback binding by default.

## 9. CI/CD and release engineering

### 9.1 Pull-request CI

Run:

1. flake evaluation and lockfile consistency;
2. formatting for Nix, Go, shell, JSON/YAML;
3. schema and example-config validation;
4. lint/static analysis;
5. unit and fuzz smoke tests;
6. race tests on Linux;
7. fake-provider contract tests;
8. Nix build and plugin-load smoke test on both supported architectures where runners exist;
9. end-to-end Responses and auth-isolation suite;
10. secret scan, `govulncheck`, Nix dependency audit, and license policy;
11. golden diff upload on failure.

No live provider secrets are required for PR CI.

### 9.2 Scheduled CI

Nightly or weekly:

- longer fuzzing and race suites;
- concurrency/soak and cancellation leak tests;
- latest supported Codex canary against mocks;
- Bifrost upstream-change canary on a separate non-blocking branch;
- vulnerability and dependency freshness report;
- optional live-provider smoke tests in a protected environment, using low quotas and provider-specific test accounts.

Live tests must never run on untrusted fork code and must assert cleanup/cost ceilings.

### 9.3 Release pipeline

For signed version tags:

- require all checks and a clean lockfile;
- build packages for supported systems from the tag;
- run the full integration suite against the exact artifacts;
- generate SBOM and SLSA-style provenance/attestations;
- sign checksums and OCI manifests if images are published;
- publish a compatibility manifest containing Bifrost revision, Go version, Codex versions, providers/adapters, and known limitations;
- generate release notes from user-visible changes and migration notes;
- retain previous packages for rollback.

Version the router independently, but make the Bifrost revision visible and immutable inside every release.

### 9.4 Dependency updates

Use automated PRs for Nix inputs and Go modules, but never auto-merge Bifrost or Codex-contract updates. Such PRs must run the complete golden, stream, auth, ABI-load, and end-to-end matrix and include a compatibility diff.

## 10. Observability and operations

Metrics, with bounded labels:

- requests by public operation, resolved provider, mode (`native`/`polyfill`), and status class;
- catalog discovery/hydration count, duration, cache age, and errors;
- polyfill validation rejections by stable reason code;
- upstream latency, gateway overhead, time to first event, stream duration;
- retries/fallbacks and provider rate limits;
- active streams and cancellation count;
- config reload success/failure and effective revision.

Do not put model prompts, tokens, raw model IDs from untrusted requests, full URLs, or credentials in labels.

Structured logs should contain request ID, resolved provider/model from the validated catalog, chosen mode/adapter, counts of input/tool items, status, and timing. Content logging and raw request/response storage remain off by default.

Health model:

- liveness: process and plugin pipeline alive;
- readiness: config valid, plugin loaded, catalog snapshot available, required Bifrost providers initialized;
- degraded: optional provider discovery failed but a safe cached/static catalog remains available.

## 11. Documentation deliverables

- Architecture and credential-flow diagrams.
- Compatibility table per adapter and capability.
- Configuration schema reference with complete examples.
- Codex setup, profile switching, and safe uninstall instructions.
- Provider onboarding guide focused on capability profiles.
- Security threat model and secret-handling guide.
- Operations/runbook for startup, reload, rate limits, provider failures, catalog drift, rollback, and debugging streams.
- Upgrade guide for Bifrost and Codex contract changes.

## 12. Definition of done for v1

v1 is complete when:

- One Codex provider URL exposes at least one native OpenAI model and at least two non-OpenAI providers.
- OpenAI uses Codex's native auth through request-scoped direct-key routing; Bifrost stores no OpenAI credential.
- Every non-OpenAI request uses only a Bifrost-held credential, with automated proof that the inbound OpenAI bearer was stripped.
- `GET /v1/models` hydrates discovered models, preserves safe upstream fields, merges configured OpenAI entries, and is deterministic.
- Native Responses providers stay native.
- A Chat-only provider passes the documented non-streaming and streaming polyfill contract suite, including function tools.
- Unsupported capabilities fail clearly and are not advertised.
- The full test suite runs without paid APIs.
- `nix flake check` covers build, plugin ABI/load, unit, contract, integration, auth isolation, and package smoke tests.
- NixOS deployment and rollback are documented and tested.
- Released artifacts include compatibility metadata, SBOM, provenance, and no embedded secrets.

## 13. Principal risks and mitigations

| Risk | Mitigation |
|---|---|
| Go plugin ABI mismatch | Build host and plugin in one pinned Nix closure; startup ABI smoke test; never ship a standalone prebuilt `.so`. |
| OpenAI bearer leaks to another provider | Resolve model first, allowlist OpenAI direct-key mode, strip auth/direct-key controls otherwise, canary-token tests on every route/fallback. |
| Codex model schema changes | Versioned hydration templates, pinned-client goldens, scheduled current-client canary, preserve unknown fields. |
| Responses streaming looks valid but violates event ordering | Protocol state-machine tests, ID relationship assertions, real Codex end-to-end tests, regression fixtures. |
| Polyfill silently loses unsupported features | Capability matrix, conservative advertised metadata, validation before dispatch, stable errors instead of lossy conversion. |
| Bifrost upgrade changes private context-key behavior | Pin revision; compile-time and end-to-end contract tests; upstream contribution for a stable public capability hook if needed. |
| Model aliases route to the wrong provider | Startup collision rejection, explicit provider ownership, explain command, fail closed on ambiguity. |
| Secrets enter Nix store or logs | Runtime credentials only, systemd credentials, redaction tests, secret scanning, content logging disabled. |
| Upstream model discovery is unavailable | Static/overlay catalog snapshot, bounded stale-cache policy, degraded readiness, no fabricated capabilities. |

## 14. First implementation slice

The first mergeable slice should be deliberately narrow:

1. flake and coupled Bifrost/plugin build;
2. configuration schema with one fake OpenAI model and one fake Chat-only model;
3. conditional direct-key auth plugin;
4. Responses-mode selector using Bifrost's existing conversion context flag;
5. post-hook model hydration;
6. two fake upstreams and an end-to-end test proving routing, streaming, and credential isolation;
7. minimal Codex profile generator with backup/rollback.

That slice retires the highest-risk assumptions before adding real provider profiles or operational features.
