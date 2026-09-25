# Agent-managed Codex setup

The recommended setup interface is a Codex agent. The agent gathers provider
and plan requirements, enables account-aware model discovery where available,
obtains credentials through a local env file, pulls the published image, and
configures new Codex threads.

## Quick start

You need Linux or macOS, Docker with a running daemon, `jq`, and either Codex
Desktop or the Codex CLI. Nix is not required, and the setup script does not
require the CLI.
Sign in through the Codex client you use before starting setup.

Open this repository in Codex and describe the outcome at any level of detail:

> Set this up for me.

That starts requirements gathering. Codex should ask which provider plans or
API accounts you want to add and any provider-specific scope it cannot safely
infer. It always retains OpenAI through your Codex login, detects and preserves
an existing router setup, and defaults new threads to `gpt-5.6-sol` with
`medium` reasoning. It does not ask you to make those decisions. It then
researches current provider documentation and account-visible models rather
than requiring you to supply Bifrost configuration.

The agent follows [the repository onboarding instructions](../AGENTS.md).

## Credential step

After resolving the providers and required environment variable names, Codex
creates:

```text
~/.config/bifrost-model-router/providers.env
```

with mode `0600` and empty placeholders such as:

```dotenv
MANAGED_PROVIDER_API_KEY=
```

Open that file locally, fill the values, save it, and tell Codex it is ready.
Do not paste provider credentials into chat. Codex preserves existing entries
when adding another provider and never copies their values into generated JSON
or `~/.codex/config.toml`.

## Low-level executor

After requirements and credentials are complete, Codex invokes:

```sh
./scripts/setup-local.sh \
  --config ~/.config/bifrost-model-router/config.json \
  --env-file ~/.config/bifrost-model-router/providers.env \
  --accept-plaintext-key \
  --accept-new-threads-only \
  --replace
```

Humans normally do not need to construct this command. The executor defaults to
`gpt-5.6-sol` with `medium` reasoning; model flags are used only for an explicit
override or verified availability fallback. Running the script without agent
flags remains supported and presents interactive notices.

The default image is the public
`ghcr.io/applyinnovations/bifrost-model-router:main`. To use another published
tag or registry, set `BIFROST_ROUTER_IMAGE`:

```sh
BIFROST_ROUTER_IMAGE=ghcr.io/applyinnovations/bifrost-model-router:TAG \
  ./scripts/setup-local.sh
```

Before invoking the executor, the agent explains two effects:

1. The Bifrost virtual key will be written in plaintext to
   `~/.codex/config.toml`. This is convenient for a loopback-only development
   setup, but anyone who can read that file can use the key.
2. Provider and model defaults apply only when a **new Codex thread** starts.
   Existing and currently running threads keep the provider and model with
   which they were created.

The executor makes a timestamped backup before changing an existing
`~/.codex/config.toml`. It prints the exact backup path when it finishes.

## What the script starts

The script pulls the configured GHCR image and starts two containers from it:

- `bifrost-model-router-core` runs the ABI-matched Bifrost host and plugin;
- `bifrost-model-router-gateway` dispatches native Codex traffic and publishes
  `127.0.0.1:80`.

Both containers share a private Docker network. Bifrost state is kept in the
`bifrost-model-router-data` Docker volume. The router is deliberately bound to
loopback and should not be exposed directly to another machine.

## Codex configuration

The selected model and generated virtual key replace the placeholders in the
installed configuration:

```toml
model = "gpt-5.6-sol"
model_provider = "bifrost-router"
model_reasoning_effort = "medium"

[model_providers.bifrost-router]
name = "Local Bifrost Router"
base_url = "http://127.0.0.1/v1"
wire_api = "responses"
requires_openai_auth = true
http_headers = { "x-bf-vk" = "sk-bf-xxx" }
```

The provider uses Codex's existing OpenAI authentication for OpenAI models.
The static `x-bf-vk` value authorizes requests at the local Bifrost gateway.
The script creates a random key rather than using the example above.

The relevant settings are described in the official OpenAI
[Codex configuration reference](https://developers.openai.com/codex/config-reference/).

## Verify

Check the containers and local health endpoint:

```sh
docker ps --filter name=bifrost-model-router
curl --fail http://127.0.0.1/health
```

Then start a **new** Codex thread. It should use `gpt-5.6-sol` with `medium`
reasoning unless you explicitly requested an override or it required a verified
availability fallback. Threads that were open before setup are expected to
remain on their previous provider and model.

## Troubleshooting

### Port 80 is already in use

The quickstart intentionally uses `http://127.0.0.1/v1`. Stop the service using
loopback port 80, then rerun the script.

### Docker startup fails

Inspect both logs:

```sh
docker logs bifrost-model-router-core
docker logs bifrost-model-router-gateway
```

On a clean database, Bifrost may spend several minutes applying migrations.

### Codex still uses the previous provider

Start a new thread. Configuration defaults do not replace the provider or model
attached to an existing thread.

### “Model is not supported when using Codex with a ChatGPT account”

For example:

```text
The 'managed/text-model' model is not supported when using Codex with a
ChatGPT account.
```

This usually means the thread was created before the router was configured.
The thread still belongs to Codex's built-in `openai` provider even though
`config.toml` now selects `bifrost-router` and a router model. Codex therefore
validates the router model as if it were a built-in ChatGPT model and rejects
it before the request reaches Bifrost.

To recover:

1. Leave the existing thread. Do not resume or reopen it for router models.
2. Create a new Codex thread after setup. In the CLI, exit the resumed session
   and run `codex` without `resume`; in the app, use **New task**.
3. Select the router model in that new thread. Move any needed context by
   summarizing or copying it from the old thread.

If you must continue the existing thread, switch it back to a model supported
by its original ChatGPT/OpenAI provider. Editing `config.toml` does not migrate
an existing thread to a different provider.

Do not rename the custom provider to `openai` as a workaround. The official
OpenAI configuration reference reserves the built-in provider IDs `openai`,
`ollama`, and `lmstudio`; custom providers cannot override them. Keeping the
stable custom ID `bifrost-router` avoids collisions. Reusing another custom
provider ID helps only for threads that were originally created with that same
custom ID.

### `missing_openai_auth`

Sign in through Codex Desktop, or run `codex login` if you use the CLI. Then
fully restart Codex and create a new thread. The router does not store an
OpenAI credential.

### Hosted tools on a Chat Completions polyfill

Any built-in or custom provider can declare a `native_hosted_tools` mapping for
tools verified on its native Responses endpoint. When Codex offers only mapped
hosted tools, the router keeps the selected provider and lets that endpoint
decide whether to execute them. For OpenRouter, `web_search` maps to
`openrouter:web_search`; a text-only turn stays on the selected model, while a
search turn is executed and completed by OpenRouter.

Providers without a declared path, and tools that cannot be represented safely,
still use a whole-request OpenAI fallback. The router defaults missing
`plugins[].config.hosted_tool_fallback_model` to `openai/gpt-6-luna` when the
OpenAI request-passthrough provider can resolve it. An explicit value overrides
the default. Pull the current image and rerun setup to apply this to an existing
local configuration. Restart Codex and create a new task after the router is
updated.

### OpenAI usage limit while a managed model is selected

When a hosted tool has no compatible managed-provider execution path, the
router sends the whole request to the configured OpenAI fallback model. That
request uses the Codex login and its OpenAI usage allowance, even though a
managed model was selected. The routing log records the requested model,
effective provider, and fallback reason without recording prompt text or
credentials.

### HTTP 401

Rerun the setup script so the virtual key in `~/.codex/config.toml` and the key
given to the Bifrost container are replaced together.

## Stop or remove the local router

Stop the containers while preserving their configuration and state:

```sh
docker stop bifrost-model-router-gateway bifrost-model-router-core
```

Remove the containers and private network:

```sh
docker rm -f bifrost-model-router-gateway bifrost-model-router-core
docker network rm bifrost-model-router
```

The `bifrost-model-router-data` volume is retained. Delete it separately only
when you also want to discard the local Bifrost database.

To undo the Codex change, copy the backup printed by the setup script over
`~/.codex/config.toml`. If no original file existed, remove the two marked
quickstart blocks from that file.

For production deployment and secret handling, use the
[operations](operations.md) and [security](security.md) guides instead.
