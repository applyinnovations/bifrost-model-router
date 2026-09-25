#!/usr/bin/env bash
set -euo pipefail

: "${BIFROST_BIN:?set BIFROST_BIN}"
: "${ROUTER_PLUGIN:?set ROUTER_PLUGIN}"
: "${MOCK_PROVIDER_BIN:?set MOCK_PROVIDER_BIN}"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/bifrost-router-e2e.XXXXXX")"
app_dir="${test_root}/app"
mkdir -p "${app_dir}"

processes=()
cleanup() {
	for pid in "${processes[@]:-}"; do
		kill "${pid}" 2>/dev/null || true
	done
	rm -rf "${test_root}"
}
trap cleanup EXIT
dump_logs() {
	status=$?
	printf 'e2e failed with status %s\n' "${status}" >&2
	for log_file in "${test_root}"/*.log; do
		if [[ -f "${log_file}" ]]; then
			printf '\n==> %s <==\n' "${log_file}" >&2
			cat "${log_file}" >&2
		fi
	done
	exit "${status}"
}
trap dump_logs ERR

"${MOCK_PROVIDER_BIN}" -listen 127.0.0.1:18101 -mode native -expected-token openai-canary >"${test_root}/native.log" 2>&1 &
processes+=("$!")
"${MOCK_PROVIDER_BIN}" -listen 127.0.0.1:18102 -mode chat -expected-token bifrost-canary >"${test_root}/chat.log" 2>&1 &
processes+=("$!")

cat >"${app_dir}/pricing.json" <<'JSON'
{}
JSON
cat >"${app_dir}/model-parameters.json" <<'JSON'
{}
JSON
cat >"${app_dir}/config.json" <<JSON
{
  "\$schema": "https://www.getbifrost.ai/schema",
  "version": 2,
  "config_store": {
    "enabled": true,
    "type": "sqlite",
    "config": { "path": "${app_dir}/config.db" }
  },
  "logs_store": { "enabled": false },
  "framework": {
    "pricing": {
      "pricing_url": "file://${app_dir}/pricing.json",
      "model_parameters_url": "file://${app_dir}/model-parameters.json"
    }
  },
  "client": {
    "allow_direct_keys": true,
    "disable_content_logging": true,
    "enable_logging": false,
    "enforce_auth_on_inference": true
  },
  "governance": {
    "auth_config": { "is_enabled": false },
    "virtual_keys": [{
      "id": "e2e-virtual-key",
      "name": "e2e virtual key",
      "value": "env.BIFROST_TEST_VK",
      "is_active": true,
      "provider_configs": [{
        "provider": "openai",
        "allowed_models": ["regex:.*"],
        "blacklisted_models": [],
        "key_ids": ["*"]
      }, {
        "provider": "mock-chat",
        "allowed_models": ["*"],
        "blacklisted_models": [],
        "key_ids": ["*"]
      }]
    }]
  },
  "providers": {
    "openai": {
      "keys": [],
      "network_config": { "base_url": "http://127.0.0.1:18101", "allow_private_network": true }
    },
    "mock-chat": {
      "keys": [{ "name": "mock-chat", "value": "env.MOCK_CHAT_KEY", "models": ["*"], "weight": 1 }],
      "network_config": { "base_url": "http://127.0.0.1:18102", "allow_private_network": true },
      "custom_provider_config": {
        "base_provider_type": "openai",
        "allowed_requests": {
          "chat_completion": true,
          "chat_completion_stream": true,
          "responses": false,
          "responses_stream": false,
            "list_models": true
        },
        "request_path_overrides": {
          "chat_completion": "/v1/chat/completions",
          "chat_completion_stream": "/v1/chat/completions"
        }
      }
    }
  },
  "plugins": [{
    "enabled": true,
    "name": "governance",
    "config": { "is_vk_mandatory": true }
  }, {
    "enabled": true,
    "name": "codex-model-router",
    "path": "${ROUTER_PLUGIN}",
    "config": {
      "version": 1,
      "instructions_template": "You are a coding agent.",
      "image_generation_model": "openai/native-model",
      "providers": {
        "openai": { "credential_mode": "request_passthrough", "responses_mode": "native" },
        "mock-chat": {
          "credential_mode": "bifrost",
          "responses_mode": "chat_polyfill",
          "discover_models": true,
          "codex_defaults": { "context_window": 64000, "input_modalities": ["text"] }
        }
      },
      "models": {
        "openai/native-model": { "aliases": ["native-model"], "codex": { "context_window": 128000 } },
        "mock-chat/chat-model": { "aliases": ["chat-model"], "codex": { "context_window": 64000 } }
      }
    }
  }]
}
JSON

MOCK_CHAT_KEY=bifrost-canary BIFROST_TEST_VK=sk-bf-e2e "${BIFROST_BIN}" -app-dir "${app_dir}" -host 127.0.0.1 -port 18080 >"${test_root}/bifrost.log" 2>&1 &
processes+=("$!")

ready=0
# A clean Bifrost database currently applies a long migration chain. Shared CI
# runners can need several minutes even though steady-state startup is fast,
# especially when the full flake check builds in parallel. Allow ten minutes
# while still polling at a short interval.
for _ in $(seq 1 2400); do
	if curl --fail --silent http://127.0.0.1:18080/health >/dev/null; then
		ready=1
		break
	fi
	sleep 0.25
done
if [[ "${ready}" != 1 ]]; then
	cat "${test_root}/bifrost.log" >&2
	exit 1
fi

catalog_json="$(curl --fail-with-body --silent 'http://127.0.0.1:18080/v1/models?client_version=e2e' \
	-H 'x-bf-vk: sk-bf-e2e')"
jq -e '
  ([.models[] | select(.slug == "openai/native-model" and .responses_mode == "native")] | length == 1)
  and ([.models[] | select(.slug == "mock-chat/chat-model" and .responses_mode == "chat_polyfill")] | length == 1)
  and ([.models[] | select(.slug == "mock-chat/chat-new-model" and .responses_mode == "chat_polyfill")] | length == 1)
' <<<"${catalog_json}" >/dev/null

native_json="$(curl --fail-with-body --silent http://127.0.0.1:18080/v1/responses \
	-H 'Content-Type: application/json' \
	-H 'Authorization: Bearer openai-canary' \
	-H 'x-bf-vk: sk-bf-e2e' \
	--data '{"model":"openai/native-model","input":"hello"}')"
jq -e '.object == "response" and .output[0].content[0].text == "native ok"' <<<"${native_json}" >/dev/null

polyfill_json="$(curl --fail-with-body --silent http://127.0.0.1:18080/v1/responses \
	-H 'Content-Type: application/json' \
	-H 'Authorization: Bearer openai-canary' \
	-H 'x-bf-vk: sk-bf-e2e' \
	--data '{"model":"mock-chat/chat-model","input":"hello"}')"
jq -e '.object == "response" and .output[0].content[0].text == "polyfill ok"' <<<"${polyfill_json}" >/dev/null

discovered_polyfill_json="$(curl --fail-with-body --silent http://127.0.0.1:18080/v1/responses \
	-H 'Content-Type: application/json' \
	-H 'x-bf-vk: sk-bf-e2e' \
	--data '{"model":"mock-chat/chat-new-model","input":"hello"}')"
jq -e '.object == "response" and .model == "chat-new-model" and .output[0].content[0].text == "polyfill ok"' <<<"${discovered_polyfill_json}" >/dev/null

filtered_hosted_tool_json="$(curl --fail-with-body --silent http://127.0.0.1:18080/v1/responses \
	-H 'Content-Type: application/json' \
	-H 'Authorization: Bearer openai-canary' \
	-H 'x-bf-vk: sk-bf-e2e' \
	--data '{"model":"mock-chat/chat-model","input":"hello","tools":[{"type":"web_search"}]}')"
jq -e '.object == "response" and .model == "chat-model" and .output[0].content[0].text == "polyfill ok"' <<<"${filtered_hosted_tool_json}" >/dev/null

namespace_polyfill_json="$(curl --fail-with-body --silent http://127.0.0.1:18080/v1/responses \
	-H 'Content-Type: application/json' \
	-H 'Authorization: Bearer openai-canary' \
	-H 'x-bf-vk: sk-bf-e2e' \
	--data '{"model":"mock-chat/chat-model","input":"hello","tools":[{"type":"namespace","name":"codex","tools":[{"type":"function","name":"shell","description":"Run a shell command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]}]}')"
jq -e '.object == "response" and .model == "chat-model" and .output[0].content[0].text == "polyfill ok"' <<<"${namespace_polyfill_json}" >/dev/null

native_stream="$(curl --fail-with-body --silent --no-buffer http://127.0.0.1:18080/v1/responses \
	-H 'Content-Type: application/json' \
	-H 'Authorization: Bearer openai-canary' \
	-H 'x-bf-vk: sk-bf-e2e' \
	--data '{"model":"openai/native-model","input":"hello","stream":true}')"
[[ "$(grep -c '"type":"response.created"' <<<"${native_stream}")" == 1 ]]
[[ "$(grep -c '"type":"response.completed"' <<<"${native_stream}")" == 1 ]]
[[ "${native_stream}" == *'native stream ok'* ]]

polyfill_stream="$(curl --fail-with-body --silent --no-buffer http://127.0.0.1:18080/v1/responses \
	-H 'Content-Type: application/json' \
	-H 'Authorization: Bearer openai-canary' \
	-H 'x-bf-vk: sk-bf-e2e' \
	--data '{"model":"mock-chat/chat-model","input":"hello","stream":true}')"
created_line="$(grep -n -m1 '"type":"response.created"' <<<"${polyfill_stream}" | cut -d: -f1)"
delta_line="$(grep -n -m1 '"type":"response.output_text.delta"' <<<"${polyfill_stream}" | cut -d: -f1)"
completed_line="$(grep -n -m1 '"type":"response.completed"' <<<"${polyfill_stream}" | cut -d: -f1)"
[[ -n "${created_line}" && -n "${delta_line}" && -n "${completed_line}" ]]
((created_line < delta_line && delta_line < completed_line))
[[ "$(grep -c '"type":"response.completed"' <<<"${polyfill_stream}")" == 1 ]]
[[ "${polyfill_stream}" == *'polyfill '* && "${polyfill_stream}" == *'stream ok'* ]]

missing_gateway_auth_status="$(curl --silent --output /dev/null --write-out '%{http_code}' \
	http://127.0.0.1:18080/v1/responses \
	-H 'Content-Type: application/json' \
	-H 'Authorization: Bearer openai-canary' \
	--data '{"model":"openai/native-model","input":"hello"}')"
[[ "${missing_gateway_auth_status}" == 401 ]]

invalid_gateway_auth_status="$(curl --silent --output /dev/null --write-out '%{http_code}' \
	http://127.0.0.1:18080/v1/responses \
	-H 'Content-Type: application/json' \
	-H 'Authorization: Bearer openai-canary' \
	-H 'x-bf-vk: sk-bf-invalid' \
	--data '{"model":"openai/native-model","input":"hello"}')"
[[ "${invalid_gateway_auth_status}" == 401 ]]

unauthorized_status="$(curl --silent --output /dev/null --write-out '%{http_code}' \
	http://127.0.0.1:18080/v1/responses \
	-H 'Content-Type: application/json' \
	--data '{"model":"openai/native-model","input":"hello"}')"
[[ "${unauthorized_status}" == 401 ]]
