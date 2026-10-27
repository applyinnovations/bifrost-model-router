{
  description = "Provider-agnostic Bifrost model router for Codex";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    systems.url = "github:nix-systems/default-linux";
    bifrost = {
      # Forked upstream to preserve OpenAI-compatible model display names and
      # reasoning-level metadata through Bifrost's model conversion pipeline.
      url = "github:applyinnovations/bifrost/bc0ff9e2bc56381c668301d35fa0cc378edff4d3";
      flake = false;
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      systems,
      bifrost,
    }:
    let
      eachSystem = nixpkgs.lib.genAttrs (import systems);
    in
    {
      packages = eachSystem (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          version = "0.1.0-dev";
          bifrostUI = pkgs.buildNpmPackage {
            pname = "bifrost-ui";
            inherit version;
            src = "${bifrost}/ui";
            npmDepsHash = "sha256-cOswnT4ZahWX66h9oiw4t3r5GZeOH/yjbnTCAsjVgnw=";
            npmBuildScript = "build-enterprise";
            installPhase = ''
              runHook preInstall
              cp -R out $out
              runHook postInstall
            '';
          };
          common = {
            pname = "bifrost-model-router";
            inherit version;
            src = self;
            vendorHash = "sha256-4louYB9FqGkC/FyN6LY/1TtcvGxaZMucvm19lNDRAZI=";
            nativeBuildInputs = [ pkgs.pkg-config ];
            buildInputs = [ pkgs.sqlite ];
            go = pkgs.go_1_27;
            doCheck = true;
          };
          configCheck = pkgs.buildGo127Module (
            common
            // {
              pname = "bifrost-router-config-check";
              subPackages = [ "cmd/config-check" ];
            }
          );
          router = pkgs.buildGo127Module (
            common
            // {
              pname = "bifrost-router-cli";
              subPackages = [ "cmd/router" ];
              ldflags = [ "-X main.version=${version}" ];
            }
          );
          gateway = pkgs.buildGo127Module (
            common
            // {
              pname = "bifrost-router-gateway";
              subPackages = [ "cmd/gateway" ];
            }
          );
          mockProvider = pkgs.buildGo127Module (
            common
            // {
              pname = "bifrost-router-mock-provider";
              subPackages = [ "cmd/mock-provider" ];
              doCheck = false;
            }
          );
          plugin = pkgs.buildGo127Module (
            common
            // {
              pname = "codex-model-router-plugin";
              vendorHash = "sha256-t4CxfKmrKw7pUXX6wEjT3cDTyjMze6UsLr0jrQKaSfY=";
              doCheck = false;
              postPatch = ''
                cp -R ${bifrost}/core bifrost-core
                chmod -R u+w bifrost-core
                go mod edit -replace github.com/maximhq/bifrost/core=./bifrost-core
              '';
              buildPhase = ''
                runHook preBuild
                CGO_ENABLED=1 go build -buildmode=plugin -trimpath \
                  -o codex-model-router.so ./plugins/codex-router
                runHook postBuild
              '';
              installPhase = ''
                runHook preInstall
                install -Dm755 codex-model-router.so \
                  $out/lib/bifrost/plugins/codex-model-router.so
                runHook postInstall
              '';
            }
          );
          bifrostHost = pkgs.buildGo127Module {
            pname = "bifrost-http";
            inherit version;
            src = bifrost;
            modRoot = "transports";
            vendorHash = "sha256-rcqc3JCHdXkLdev1qEYPJAPs+I2iBImlT7TY3x4f9EM=";
            subPackages = [ "bifrost-http" ];
            tags = [ "sqlite_static" ];
            postPatch = ''
              cp -R ${bifrostUI} transports/bifrost-http/ui
            '';
            nativeBuildInputs = [ pkgs.pkg-config ];
            buildInputs = [ pkgs.sqlite ];
            go = pkgs.go_1_27;
            ldflags = [
              "-s"
              "-w"
              "-X main.Version=v2.2.1-router"
            ];
            doCheck = false;
          };
          launcher = pkgs.writeShellApplication {
            name = "bifrost-model-router-server";
            runtimeInputs = [ pkgs.coreutils ];
            text = ''
              app_dir="''${BIFROST_APP_DIR:-/var/lib/bifrost}"
              config_file="''${BIFROST_CONFIG_FILE:-/etc/bifrost/config.json}"
              mkdir -p "$app_dir"
              install -m 0600 "$config_file" "$app_dir/config.json"
              exec ${bifrostHost}/bin/bifrost-http \
                -app-dir "$app_dir" \
                -host "''${BIFROST_HOST:-127.0.0.1}" \
                -port "''${BIFROST_PORT:-8080}"
            '';
          };
          licenseBundle = pkgs.runCommand "bifrost-model-router-licenses" { } ''
            install -Dm644 ${./LICENSE} $out/share/licenses/bifrost-model-router/LICENSE
            install -Dm644 ${./NOTICE} $out/share/licenses/bifrost-model-router/NOTICE
          '';
          imageRoot = pkgs.buildEnv {
            name = "bifrost-model-router-image-root";
            paths = [
              bifrostHost
              plugin
              gateway
              launcher
              licenseBundle
              pkgs.cacert
              pkgs.coreutils
              pkgs.curl
              pkgs.tzdata
            ];
            pathsToLink = [
              "/bin"
              "/etc"
              "/lib"
              "/share"
            ];
          };
          image = pkgs.dockerTools.streamLayeredImage {
            name = "bifrost-model-router";
            tag = "nix";
            contents = [ imageRoot ];
            config = {
              Entrypoint = [ "/bin/bifrost-model-router-server" ];
              Env = [
                "BIFROST_APP_DIR=/var/lib/bifrost"
                "BIFROST_CONFIG_FILE=/etc/bifrost/config.json"
                "BIFROST_HOST=127.0.0.1"
                "BIFROST_PORT=8080"
                "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
                "TZDIR=/share/zoneinfo"
              ];
              User = "65532:65532";
              WorkingDir = "/var/lib/bifrost";
              ExposedPorts = {
                "8080/tcp" = { };
              };
              Labels = {
                "org.opencontainers.image.source" = "https://github.com/applyinnovations/bifrost-model-router";
                "org.opencontainers.image.version" = version;
                "org.opencontainers.image.title" = "Bifrost Model Router";
                "org.opencontainers.image.licenses" = "Apache-2.0";
              };
            };
          };
          publishContainer = pkgs.writeShellApplication {
            name = "publish-container";
            runtimeInputs = [
              pkgs.coreutils
              pkgs.skopeo
            ];
            text = ''
              if (( $# < 1 )); then
                echo "usage: publish-container ghcr.io/applyinnovations/bifrost-model-router:TAG [...]" >&2
                exit 2
              fi
              if [[ -z "''${GHCR_USER:-}" || -z "''${GHCR_TOKEN:-}" ]]; then
                echo "GHCR_USER and GHCR_TOKEN are required" >&2
                exit 1
              fi

              work_dir="$(mktemp -d)"
              trap 'rm -rf -- "$work_dir"' EXIT
              export REGISTRY_AUTH_FILE="$work_dir/auth.json"
              policy_file="$work_dir/policy.json"
              image_tar="$work_dir/image.tar"
              printf '%s\n' '{"default":[{"type":"insecureAcceptAnything"}]}' > "$policy_file"

              printf '%s' "$GHCR_TOKEN" | \
                skopeo login --authfile "$REGISTRY_AUTH_FILE" \
                  --username "$GHCR_USER" --password-stdin ghcr.io >/dev/null
              ${image} > "$image_tar"

              for destination in "$@"; do
                case "$destination" in
                  ghcr.io/applyinnovations/bifrost-model-router:*) ;;
                  *)
                    echo "refusing unexpected destination: $destination" >&2
                    exit 2
                    ;;
                esac
                skopeo --policy "$policy_file" copy --all \
                  "docker-archive:$image_tar" "docker://$destination"
              done
            '';
          };
        in
        {
          inherit plugin image gateway;
          bifrost = bifrostHost;
          bifrost-ui = bifrostUI;
          bifrost-model-router-image = image;
          config-check = configCheck;
          inherit router;
          publish-container = publishContainer;
          mock-provider = mockProvider;
          default = pkgs.symlinkJoin {
            name = "bifrost-model-router-${version}";
            paths = [
              bifrostHost
              plugin
              configCheck
              router
              gateway
              licenseBundle
            ];
          };
        }
      );

      checks = eachSystem (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          inherit (self.packages.${system}) plugin config-check;
          bifrost = self.packages.${system}.bifrost;
          bifrost-ui = self.packages.${system}.bifrost-ui;
          image = self.packages.${system}.bifrost-model-router-image;
          image-metadata =
            pkgs.runCommand "bifrost-router-image-metadata"
              {
                nativeBuildInputs = [
                  pkgs.gnutar
                  pkgs.jq
                ];
              }
              ''
                work=$TMPDIR/image
                mkdir -p "$work"
                ${self.packages.${system}.bifrost-model-router-image} > "$work/image.tar"
                tar -xf "$work/image.tar" -C "$work" manifest.json
                config_name=$(jq -r '.[0].Config' "$work/manifest.json")
                tar -xf "$work/image.tar" -C "$work" "$config_name"
                jq -e '.config.User == "65532:65532"' "$work/$config_name" >/dev/null
                jq -e '.config.Entrypoint == ["/bin/bifrost-model-router-server"]' "$work/$config_name" >/dev/null
                jq -e '.config.WorkingDir == "/var/lib/bifrost"' "$work/$config_name" >/dev/null
                jq -e '.config.Labels."org.opencontainers.image.licenses" == "Apache-2.0"' "$work/$config_name" >/dev/null
                touch $out
              '';
          ui-content =
            pkgs.runCommand "bifrost-router-ui-content"
              {
                nativeBuildInputs = [ pkgs.ripgrep ];
              }
              ''
                test -s ${self.packages.${system}.bifrost-ui}/index.html
                test -d ${self.packages.${system}.bifrost-ui}/assets
                ! rg -q "API-only build" ${self.packages.${system}.bifrost-ui}/index.html
                touch $out
              '';
          licenses =
            pkgs.runCommand "bifrost-router-licenses"
              {
                nativeBuildInputs = [ pkgs.diffutils ];
              }
              ''
                packaged=${self.packages.${system}.default}/share/licenses/bifrost-model-router
                diff -u ${./LICENSE} "$packaged/LICENSE"
                diff -u ${./NOTICE} "$packaged/NOTICE"
                touch $out
              '';
          format =
            pkgs.runCommand "bifrost-router-format"
              {
                nativeBuildInputs = [
                  pkgs.go_1_27
                  pkgs.jq
                  pkgs.nixfmt
                  pkgs.shellcheck
                  pkgs.shfmt
                ];
              }
              ''
                cp -R ${self} source
                chmod -R u+w source
                cd source
                test -z "$(gofmt -l cmd internal plugins)"
                nixfmt --check flake.nix nix/*.nix
                shfmt -d scripts
                shellcheck scripts/*.sh
                jq empty config/*.json
                touch $out
              '';
          lint =
            pkgs.runCommand "bifrost-router-lint"
              {
                nativeBuildInputs = [
                  pkgs.go_1_27
                  pkgs.pkg-config
                  pkgs.sqlite
                  pkgs.stdenv.cc
                ];
              }
              ''
                export GOCACHE=$TMPDIR/go-cache
                export GOPATH=$TMPDIR/go
                cp -R ${self} source
                chmod -R u+w source
                cp -R ${self.packages.${system}.router.goModules} source/vendor
                cd source
                go vet ./...
                touch $out
              '';
          schema =
            pkgs.runCommand "bifrost-router-schema"
              {
                nativeBuildInputs = [
                  pkgs.check-jsonschema
                  self.packages.${system}.config-check
                ];
              }
              ''
                check-jsonschema --schemafile ${./config/router.schema.json} ${./config/router.example.yaml}
                config-check ${./config/router.example.yaml} >/dev/null
                touch $out
              '';
          tekton =
            pkgs.runCommand "bifrost-router-tekton"
              {
                nativeBuildInputs = [
                  pkgs.ripgrep
                  pkgs.yq-go
                ];
              }
              ''
                for pipeline in ${./.tekton}/*.yaml; do
                  yq -e '.metadata.annotations."pipelinesascode.tekton.dev/target-namespace" == "tekton-buildkit"' "$pipeline" >/dev/null
                  # Only the standard v1 Tasks, and no shared Nix store.
                  yq -e '[(.spec.pipelineSpec.tasks[], (.spec.pipelineSpec.finally // [])[]) | .taskRef.params[] | select(.name == "name") | .value | select(test("-v1$") | not)] | length == 0' "$pipeline" >/dev/null
                  yq -e '[.spec.workspaces[] | select(.name == "nix-store")] | length == 0' "$pipeline" >/dev/null
                  # Sandboxed Nix runs only inside a pod user namespace.
                  for task in $(yq -r '.spec.pipelineSpec.tasks[] | select(.taskRef.params[] | select(.name == "name" and .value == "nix-v1")) | .name' "$pipeline"); do
                    task="$task" yq -e '.spec.taskRunSpecs[] | select(.pipelineTaskName == strenv(task)) | .podTemplate.hostUsers == false' "$pipeline" >/dev/null
                  done
                done
                pipeline=${./.tekton/bifrost-model-router-images.yaml}
                ghcr_tags="$(yq -r '.spec.pipelineSpec.tasks[] | select(.name == "publish-ghcr") | .params[] | select(.name == "EXTRA_TAGS") | .value' "$pipeline")"
                printf '%s\n' "$ghcr_tags" | rg -x -F 'sha-$(tasks.clone.results.COMMIT_SHA)' >/dev/null
                printf '%s\n' "$ghcr_tags" | rg -x -F 'main' >/dev/null
                touch $out
              '';
          setup-executor =
            pkgs.runCommand "bifrost-router-setup-executor"
              {
                nativeBuildInputs = [
                  pkgs.bash
                  pkgs.coreutils
                  pkgs.gawk
                  pkgs.gnugrep
                  pkgs.gnused
                  pkgs.jq
                ];
              }
              ''
                cp -R ${self} source
                chmod -R u+w source
                ${pkgs.bash}/bin/bash source/scripts/setup-local-test.sh
                touch $out
              '';
          race =
            pkgs.runCommand "bifrost-router-race"
              {
                nativeBuildInputs = [
                  pkgs.go_1_27
                  pkgs.pkg-config
                  pkgs.sqlite
                  pkgs.stdenv.cc
                ];
              }
              ''
                export GOCACHE=$TMPDIR/go-cache
                export GOPATH=$TMPDIR/go
                cp -R ${self} source
                chmod -R u+w source
                cp -R ${self.packages.${system}.router.goModules} source/vendor
                cd source
                go test -race ./...
                touch $out
              '';
          fuzz-smoke =
            pkgs.runCommand "bifrost-router-fuzz-smoke"
              {
                nativeBuildInputs = [
                  pkgs.go_1_27
                  pkgs.pkg-config
                  pkgs.sqlite
                  pkgs.stdenv.cc
                ];
              }
              ''
                export GOCACHE=$TMPDIR/go-cache
                export GOPATH=$TMPDIR/go
                cp -R ${self} source
                chmod -R u+w source
                cp -R ${self.packages.${system}.router.goModules} source/vendor
                cd source
                go test ./internal/catalog -run '^$' -fuzz FuzzHydrateDeterministic -fuzztime 2s
                touch $out
              '';
          go-test =
            pkgs.runCommand "bifrost-router-go-test"
              {
                nativeBuildInputs = [
                  pkgs.go_1_27
                  pkgs.pkg-config
                  pkgs.sqlite
                  pkgs.stdenv.cc
                ];
              }
              ''
                export GOCACHE=$TMPDIR/go-cache
                export GOPATH=$TMPDIR/go
                cp -R ${self} source
                chmod -R u+w source
                cp -R ${self.packages.${system}.router.goModules} source/vendor
                cd source
                go test ./...
                touch $out
              '';
          plugin-load =
            pkgs.runCommand "bifrost-router-plugin-load"
              {
                nativeBuildInputs = [ pkgs.curl ];
              }
              ''
                mkdir -p $TMPDIR/app
                cat >$TMPDIR/app/pricing.json <<JSON
                {}
                JSON
                cat >$TMPDIR/app/model-parameters.json <<JSON
                {}
                JSON
                cat >$TMPDIR/app/config.json <<JSON
                {
                  "\$schema": "https://www.getbifrost.ai/schema",
                  "version": 2,
                  "config_store": { "enabled": false },
                  "logs_store": { "enabled": false },
                  "framework": {
                    "pricing": {
                      "pricing_url": "file://$TMPDIR/app/pricing.json",
                      "model_parameters_url": "file://$TMPDIR/app/model-parameters.json"
                    }
                  },
                  "client": { "allow_direct_keys": true, "disable_content_logging": true },
                  "providers": { "openai": { "keys": [] } },
                  "plugins": [{
                    "enabled": true,
                    "name": "codex-model-router",
                    "path": "${self.packages.${system}.plugin}/lib/bifrost/plugins/codex-model-router.so",
                    "config": {
                      "version": 1,
                      "providers": {
                        "openai": { "credential_mode": "request_passthrough", "responses_mode": "native" }
                      },
                      "models": { "openai/test": { "codex": {} } }
                    }
                  }]
                }
                JSON
                ${self.packages.${system}.bifrost}/bin/bifrost-http \
                  -app-dir $TMPDIR/app -host 127.0.0.1 -port 18079 \
                  >$TMPDIR/bifrost.log 2>&1 &
                server_pid=$!
                trap 'kill $server_pid 2>/dev/null || true' EXIT
                ready=0
                for attempt in $(seq 1 60); do
                  if curl --fail --silent http://127.0.0.1:18079/health >/dev/null; then
                    ready=1
                    break
                  fi
                  if ! kill -0 $server_pid 2>/dev/null; then
                    cat $TMPDIR/bifrost.log >&2
                    exit 1
                  fi
                  sleep 0.25
                done
                if [ "$ready" -ne 1 ]; then
                  cat $TMPDIR/bifrost.log >&2
                  exit 1
                fi
                touch $out
              '';
          e2e =
            pkgs.runCommand "bifrost-router-e2e"
              {
                nativeBuildInputs = [
                  pkgs.bash
                  pkgs.curl
                  pkgs.jq
                ];
              }
              ''
                export BIFROST_BIN=${self.packages.${system}.bifrost}/bin/bifrost-http
                export ROUTER_PLUGIN=${self.packages.${system}.plugin}/lib/bifrost/plugins/codex-model-router.so
                export MOCK_PROVIDER_BIN=${self.packages.${system}.mock-provider}/bin/mock-provider
                ${pkgs.bash}/bin/bash ${./scripts/e2e.sh}
                touch $out
              '';
        }
      );

      apps = eachSystem (system: {
        config-check = {
          type = "app";
          program = "${self.packages.${system}.config-check}/bin/config-check";
        };
        default = {
          type = "app";
          program = "${self.packages.${system}.router}/bin/router";
        };
        router = {
          type = "app";
          program = "${self.packages.${system}.router}/bin/router";
        };
        gateway = {
          type = "app";
          program = "${self.packages.${system}.gateway}/bin/gateway";
        };
        bifrost = {
          type = "app";
          program = "${self.packages.${system}.bifrost}/bin/bifrost-http";
        };
        publish-container = {
          type = "app";
          program = "${self.packages.${system}.publish-container}/bin/publish-container";
        };
      });

      devShells = eachSystem (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go_1_27
              gopls
              gotools
              gotestsum
              golangci-lint
              govulncheck
              just
              jq
              yq-go
              shellcheck
              shfmt
              nixfmt
              pkg-config
              sqlite
            ];
          };
        }
      );

      formatter = eachSystem (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        pkgs.writeShellApplication {
          name = "bifrost-router-format-nix";
          runtimeInputs = [ pkgs.nixfmt ];
          text = ''
            if (( $# > 0 )); then
              exec nixfmt "$@"
            fi
            mapfile -d "" files < <(find . -type f -name '*.nix' -not -path './.git/*' -print0)
            nixfmt "''${files[@]}"
          '';
        }
      );

      nixosModules.default = import ./nix/module.nix self;

      _bifrostSource = bifrost;
    };
}
