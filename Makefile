# regied
#
# Tests are split by the privileges they need. `make test` passes with nothing but
# the Go toolchain. The netns integration tests, which need root / CAP_NET_ADMIN,
# sit behind the build tag `netns` so that `go test ./...` does not pick them up.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO ?= go
BIN ?= bin/regied

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: ## Build regied into bin/.
	$(GO) build -o $(BIN) ./cmd/regied

.PHONY: build-arm64
build-arm64: ## Cross-build for the deployment target (an arm64 SBC).
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -o bin/regied-linux-arm64 ./cmd/regied

.PHONY: fmt
fmt: ## Format the source tree.
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt'd.
	@unformatted="$$(gofmt -l . | grep -v '^bin/' || true)"; \
	if [ -n "$$unformatted" ]; then \
		echo "Not gofmt'd. Run \`make fmt\`:"; \
		echo "$$unformatted" | sed 's/^/  /'; \
		exit 1; \
	fi

.PHONY: vet
vet: fmt-check ## Run go vet over every package, including the tagged tests.
	$(GO) vet ./...
	$(GO) vet -tags netns ./...

##@ Test

.PHONY: test
test: ## Unit tests. Needs neither privileges nor external commands.
	$(GO) test ./...

##@ Netns

# The netns integration tests build a pseudo WAN (a PPPoE server, a DS-Lite AFTR
# and reachability servers) out of network namespaces and run against it. They need
# root plus nft / pppd / pppoe-server / socat. The local development environment has
# none of those, so the usual entry point is test-netns-docker, which calls them
# through a privileged container (ADR 0010).

NETNS_IMAGE ?= regied-netns:latest

# Asking for this target is a statement that the tools are expected to be there, so a
# missing prerequisite fails rather than skipping. go test buffers the skip reason away
# and prints ok, which reads as a pass on a run that never happened (ADR 0010). Pass an
# empty value to get the skip back: make test-netns REGIED_NETNS_REQUIRE=
REGIED_NETNS_REQUIRE ?= 1

.PHONY: test-netns
test-netns: ## The netns integration tests. Needs root / CAP_NET_ADMIN and external commands.
	REGIED_NETNS_REQUIRE=$(REGIED_NETNS_REQUIRE) $(GO) test -tags netns -count=1 -timeout 20m ./test/netns/...

.PHONY: netns-image
netns-image: ## Build the container image for the netns integration tests.
	docker build -t $(NETNS_IMAGE) hack/netns

.PHONY: test-netns-docker
test-netns-docker: netns-image ## Bring up a privileged container and run the netns integration tests.
	hack/netns/run-in-docker.sh make test-netns

.PHONY: netns-shell
netns-shell: netns-image ## Open a shell in the same container. Build the topology with hack/netns/topo.sh up.
	hack/netns/run-in-docker.sh bash
