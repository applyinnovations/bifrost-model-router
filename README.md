# Bifrost Model Router

Use OpenAI and Bifrost-managed models through one Codex model provider. Native
Responses requests pass through unchanged; Chat Completions-only models are
translated by Bifrost. Codex's OpenAI credential is forwarded only to declared
OpenAI routes and is stripped from every managed-provider route.

## Set up with Codex

Open this repository in Codex and ask it to set up the router. You do not need
to know the configuration format or provide a complete specification up front:

> Set this up for me.

Codex reads [AGENTS.md](AGENTS.md) and gathers the requirements with you. It
asks which provider plans or API accounts you want in Codex, researches their
current endpoints and account-visible models, explains compatibility limits,
and asks whether each provider should show every discovered model or only a
model/family allowlist. It uses authenticated model discovery when it reflects
that account. You can name one or many Bifrost-native or OpenAI-compatible
providers; other protocols may need an adapter.

OpenAI through the existing Codex login is always retained. Codex detects and
merges an existing router setup automatically, and defaults new threads to
`gpt-5.6-sol` with `medium` reasoning unless you explicitly request otherwise.

Codex handles the public GHCR image, Docker, router configuration, catalog
policy, validation, backups, and Codex settings. Nix is not required for setup.
The only required secret-handling step is filling the credential placeholders
it creates in the mode-`0600` file
`~/.config/bifrost-model-router/providers.env`; credentials are never pasted
into chat or stored in the repository.

After setup, fully quit and reopen Codex, then create a new task. Existing
tasks retain their original provider and session state.

See the **[Codex setup guide](docs/codex-setup.md)** for prerequisites,
the agent-managed flow, verification, troubleshooting, and cleanup.

## How it works

- `native` models use their provider's Responses API.
- `chat_polyfill` models use Bifrost's Responses-to-Chat translation.
- OpenAI requests use the caller's Codex authentication; other providers use
  credentials managed by Bifrost.
- Hosted tools on polyfilled models reroute the whole request to an OpenAI
  Responses model. The fallback defaults to `openai/gpt-6-luna` when available
  through the configured OpenAI passthrough; it can be overridden.
- The Codex Images API generation path uses that native OpenAI Responses model
  and its hosted `image_generation` tool. The gateway adapts the result to the
  Images API response shape, so Codex login auth works without an API key.
- Account-aware provider catalogs are discovered dynamically. New upstream
  models flow through without editing a static router model list. An optional
  virtual-key allowlist can deliberately limit what a user sees: `"*"` admits
  all current and future discovered models, exact IDs pin a fixed selection,
  and validated `regex:` entries admit matching model families over time.
- Except for explicit exception overrides, model labels prefer OpenRouter
  editorial names, then the upstream display name, then a readable form of the
  model ID. Publisher prefixes are removed; managed routes append their
  configured hosting source, such as `Model (Provider)`, while direct OpenAI
  models have no redundant suffix. Overrides are not used as a model list.
- Context windows come from the configured provider when available. If its
  catalog omits them, the router dynamically fills an exact or unambiguous
  OpenRouter catalog match; unmatched models retain a conservative fallback.
  Per-model context limits do not need to be hardcoded in application config.
- Unknown models and unsupported features fail closed.

See [architecture](docs/architecture.md),
[configuration](docs/configuration.md),
[compatibility](docs/compatibility.md),
[security](docs/security.md), and [operations](docs/operations.md).

## Development

The host and plugin are built together with Go 1.27 and Bifrost core v1.9.0 to
preserve Go plugin ABI compatibility.

```sh
nix develop
just test
just check-config
nix build .#default
```

Run `nix flake check -L` for the complete validation suite. Provider secrets
are runtime inputs; never add them to Nix expressions, tracked configuration,
or the Codex provider profile.

## License and attribution

Licensed under the [Apache License 2.0](LICENSE). This project is built heavily
on [Bifrost](https://github.com/maximhq/bifrost), copyright H3 Labs Inc. and
licensed under Apache 2.0. See [NOTICE](NOTICE) for attribution details.
