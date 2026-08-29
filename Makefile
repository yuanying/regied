# regied
#
# テストは必要な権限で分かれている。`make test` は Go ツールチェインだけで通る。
# root / CAP_NET_ADMIN が要る netns 統合テストは build tag `netns` の後ろに置き、
# `go test ./...` が拾わないようにする。

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO ?= go
BIN ?= bin/regied

##@ General

.PHONY: help
help: ## このヘルプを表示する。
	@awk 'BEGIN {FS = ":.*##"} \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: ## regied を bin/ にビルドする。
	$(GO) build -o $(BIN) ./cmd/regied

.PHONY: build-arm64
build-arm64: ## 投入先（arm64 の SBC）向けにクロスビルドする。
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -o bin/regied-linux-arm64 ./cmd/regied

.PHONY: fmt
fmt: ## ソースツリーを整形する。
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## gofmt されていないファイルがあれば失敗する。
	@unformatted="$$(gofmt -l . | grep -v '^bin/' || true)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt されていない。\`make fmt\` を実行すること:"; \
		echo "$$unformatted" | sed 's/^/  /'; \
		exit 1; \
	fi

.PHONY: vet
vet: fmt-check ## go vet を全パッケージに掛ける（tag 付きテストを含む）。
	$(GO) vet ./...
	$(GO) vet -tags netns ./...

##@ Test

.PHONY: test
test: ## ユニットテスト。特権も外部コマンドも要らない。
	$(GO) test ./...

##@ Netns

# netns 統合テストは擬似 WAN（PPPoE サーバー / DS-Lite の AFTR / 到達確認用の
# サーバー）を network namespace で組んで走らせる。root と nft / pppd /
# pppoe-server / socat が要る。手元の開発環境にはこれらが無いので、通常は
# test-netns-docker から特権コンテナ越しに呼ぶ（ADR 0010）。

NETNS_IMAGE ?= regied-netns:latest

.PHONY: test-netns
test-netns: ## netns 統合テスト。root / CAP_NET_ADMIN と外部コマンドが要る。
	$(GO) test -tags netns -count=1 -timeout 20m ./test/netns/...

.PHONY: netns-image
netns-image: ## netns 統合テスト用のコンテナイメージを作る。
	docker build -t $(NETNS_IMAGE) hack/netns

.PHONY: test-netns-docker
test-netns-docker: netns-image ## 特権コンテナを用意して netns 統合テストを走らせる。
	hack/netns/run-in-docker.sh make test-netns

.PHONY: netns-shell
netns-shell: netns-image ## 同じコンテナのシェルに入る。トポロジは hack/netns/topo.sh up で組む。
	hack/netns/run-in-docker.sh bash
