# Operations

## Deployment

Build the non-root OCI image stream with:

```sh
nix build .#bifrost-model-router-image
```

The image expects declarative configuration at `/etc/bifrost/config.json`,
copies it into its writable runtime directory, and listens on loopback for a
same-pod gateway by default.

Build `.#default` and configure the NixOS module:

```nix
services.bifrost-model-router = {
  enable = true;
  configFile = ./bifrost.json;
  credentials.MANAGED_PROVIDER_API_KEY = "/run/secrets/managed-provider-api-key";
};
```

The service listens on `127.0.0.1:8080` by default. Expose it remotely only
behind an authenticated, TLS-terminating boundary. The module copies the
declarative Bifrost config into its systemd-managed state directory on restart.

## Health and verification

- `GET /health` verifies that the HTTP host is accepting requests.
- `GET /v1/models?client_version=operator-check` exercises Codex hydration.
- A native OpenAI request without Codex auth must return 401.
- A configured non-OpenAI request must work without passing the inbound OpenAI
  bearer to its upstream.

Run `nix flake check -L` before deployment. It includes formatting, schema,
unit, race, ABI-load, auth-isolation, non-streaming, and streaming integration
checks.

## Upgrade and rollback

Upgrade only by changing pinned flake inputs in a reviewed commit. Bifrost
updates must pass the ABI and full e2e checks because even a patch-level core
change can invalidate a Go plugin. Retain the previous flake lock and system
generation; use normal NixOS generation rollback if health or conformance
checks fail.

Codex profile changes are independently reversible with `router profile
uninstall` or `router profile rollback BACKUP`.

## Failure triage

- Plugin load error mentioning a different package version: host/plugin ABI
  mismatch; rebuild both from this flake.
- `unresolved_model`: add the canonical slug or a unique alias to router config.
- `polyfill_*` error: the request uses a feature excluded by the model adapter.
- Provider 401/403 on a non-OpenAI model: verify the Bifrost `env.NAME` reference
  and systemd credential mapping; do not add Codex auth as a workaround.
- Catalog lacks a model: verify that account-aware discovery is enabled and the
  authenticated upstream lists it. For providers without model discovery,
  declare the model explicitly in router config.

### Image requests

Every Images generation or edit request receives a generated `X-Request-ID`.
The gateway forwards that ID to Bifrost and emits an `images_route` JSON log
record. On failure, the same record appears under `error.diagnostics` in the
JSON response, alongside a stable error code. It contains the UTC incident
timestamp, operation, requested image-tool model, selected Responses model,
provider, capability classification, reference count, failing stage, upstream
method and URL, and observed HTTP status. The upstream URL omits credentials
and query parameters. Upstream request IDs and Server metadata are included
when present and safe to record.

- `image_operation_unsupported` at `backend_selection`: configure a compatible
  `image_generation_model`; no upstream call occurred.
- `invalid_image_request` at `request_parse` or `reference_upload`: inspect the
  request format and upload limits. Client-side files must arrive as uploaded
  bytes or supported image URLs.
- `image_upstream_unavailable` at `upstream_transport`: the gateway did not
  receive an upstream HTTP response.
- `image_upstream_error` at `upstream_http`: Bifrost returned a non-200 response.
  Its status is retained, but HTML and other raw error bodies are replaced with
  JSON diagnostics.
- `image_generation_failed` at `response_decode`: the upstream returned HTTP
  200, but its Responses stream yielded no image.

`response_hop: "bifrost"` identifies the gateway's immediate peer. A reported
`upstream_server: "nginx/1.27.5"` alone cannot identify a deeper service that
produced or forwarded the response. Correlate the request ID, upstream request
ID and timestamp with Bifrost/service traces to attribute that hop. Router
diagnostics never include credentials, prompts, reference filenames, image
contents, raw error bodies or arbitrary request headers.

Run `go test ./internal/gateway -run 'TestImages|TestImage'` for the controlled
regression: the same prompt with and without a generated, valid 1206 × 2622 PNG,
endpoint and payload checks, unsupported backends, and simulated upstream HTTP
and transport failures. These tests use a mock backend and consume no quota.
