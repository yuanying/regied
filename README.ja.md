# regied

1 台の Linux ホストのネットワーク政策を、1 つの YAML の宣言から組み立てるデーモン。

名前は仏語の régie（調整室）から。**自分では演じず、演者にキューを出す。**
リンクと経路の層は systemd-networkd が、DHCP と DNS は dnsmasq が、PPPoE は pppd が持つ。
regied はそれらに指示を出し、誰も持っていない層（nftables のファイアウォール、NAT、
ポリシールーティングの判定）を自分で持ち、全体を 1 つの宣言に束ねる。

ルーターは regied の適用先の一種であって、唯一の適用先ではない。

> **状態: 開発中。** まだ動かせるものは何も無い。設定スキーマは形が決まった段階で、
> 細部は確定していない。

## 言語

英語を正とする。英語版は [README.md](README.md)。

## 何を自分で持ち、何を委譲するか

ここがこのプロジェクトの要点なので最初に書く。理由は
[ADR 0008](docs/ja/adr/0008-delegate-to-existing-implementations.md) にある。

| 領域 | 持ち主 |
|---|---|
| インターフェース、アドレス、MTU、ブリッジ | systemd-networkd |
| 静的ルート（IPv4 / IPv6、テーブル指定込み） | systemd-networkd |
| ルーティングポリシールール（マーク → テーブル） | systemd-networkd |
| ip6tnl トンネル（DS-Lite） | systemd-networkd |
| DHCPv6-PD、RA / SLAAC の配布 | systemd-networkd |
| DHCP サーバー、RA のオプション、条件付き DNS フォワード | dnsmasq |
| PPPoE セッション | pppd |
| **nftables のファイアウォール（IPv4 / IPv6）** | **regied** |
| **NAT（masquerade / ポートフォワード / hairpin）** | **regied** |
| **ポリシールーティングの判定（送信元レンジ、宛先の除外、集合）** | **regied** |
| **pppd と dnsmasq の設定生成と監督** | **regied** |
| **上記すべてを束ねる 1 つの宣言、dry-run、ロールバック、状態の API** | **regied** |

この分担から出てくる帰結を 2 つ、先に書いておく。

**regied は自分が宣言したものだけを所有する。** nftables はルールセット全体を flush せず
自分のテーブルだけを作り替え、経路は自分が入れたものだけを消す。ルーティングデーモンが
学習した経路や、コンテナランタイム・CNI が持つ状態には触らない
（[ADR 0009](docs/ja/adr/0009-ownership-boundary.md)）。

**配布は regied の仕事ではない。** regied は 1 ノードを見るデーモンであり、
設定ファイルをどう配るかは別の仕組みが持つ。

## 対象とする構成

regied は、次の 7 領域を持つ実際に動いている構成を相手に作っている。

- **PPPoE** — メインの上位回線
- **DS-Lite** — ipip6 トンネル、IPv4 over IPv6
- **ポリシールーティング** — 送信元レンジで出口を選ぶ。宛先の除外つき
- **NAT** — masquerade、ポートフォワード、hairpin
- **nftables ファイアウォール** — IPv4 / IPv6、名前付きアドレス集合
- **DHCP と DNS** — 静的割り当て、RA / DHCPv6、条件付きフォワード
- **静的ルート** — IPv4 / IPv6

加えて、適用状態と回線状態を返す読み取り中心の HTTP API を持つ。

想定する構成は**上位回線が 1 本、機材が 1 台**である。冗長構成は無い。
この前提が安全性の要件を決めている。適用は冪等であること、失敗したらロールバックすること、
`--dry-run` で触る前に何が変わるかを見せられること。

意図的にやらないことは [docs/ja/scope.md](docs/ja/scope.md) にまとめてある。

## プラットフォーム

regied は **Debian 13（trixie）** の上で作り、動かす。何も導入しない。
systemd-networkd、dnsmasq、pppd、nftables はディストリビューションのものを使い、
networkd は有効にしておく必要があり、ルーターのリンクを他の何かに持たせてはならない。
前提にするバージョンと、trixie の networkd にまだ無い指定 1 つは
[ADR 0011](docs/ja/adr/0011-target-platform.md) にある。

## 設定

設定はリソースを並べた 1 つの YAML ファイルで、Kubernetes のカスタムリソース風の書式を取る。
`kind: NetworkConfig`、ホスト全体のスイッチを `spec.global`、11 個のリソース kind を
`spec.resources[]` に並べる。

- [`docs/ja/spec/configuration.md`](docs/ja/spec/configuration.md) — 文書の形、
  リソース間の参照、どのバックエンドに落ちるか
- [`docs/ja/spec/kinds.md`](docs/ja/spec/kinds.md) — kind ごとのフィールド
- [`config/example.yaml`](config/example.yaml) — 回線 2 本のホストの実例

kind の一覧と持ち分は確定している（[ADR 0002](docs/ja/adr/0002-configuration-schema.md)）。
個々のフィールド名は最初のリリースまでに動くことがあり、`v1alpha1` はその意味である。

読む前に知っておく価値のある性質が 2 つある。

**秘密情報を入れられるフィールドが 1 つも無い。** 認証情報は、それを収めたファイルの
パスとして名指す。だから設定ファイルは、検閲なしに公開したりバグ報告に貼ったりできる
（[ADR 0003](docs/ja/adr/0003-secrets-out-of-configuration.md)）。

**回線側のグローバルアドレスを書けるフィールドも無い。** NAT とポートフォワードは
アドレスではなく回線のリソースを参照し、ポリシールーティングのマークを打つチェーンは
宛先変換より後ろに置く。この 2 つによって、変わりうるアドレスをどこにも書かないまま
hairpin が成立する。

## ビルド

```sh
make build              # ホスト向けにビルドする
make build-arm64        # arm64 の SBC 向けにクロスビルドする
```

投入先は arm64 の vendor kernel 6.1 である。開発と試験は amd64 Linux で行う。

## テスト

テストは必要な権限で分かれている。

| 対象 | コマンド | 要るもの |
|---|---|---|
| ユニットテスト | `make test` | Go ツールチェインだけ |
| netns 統合テスト | `make test-netns-docker` | Docker（特権コンテナを起動する） |
| netns 統合テスト（直接） | `make test-netns` | root / CAP_NET_ADMIN と nft / pppd / pppoe-server / socat |

netns 統合テストは network namespace で擬似 WAN（PPPoE サーバー、DS-Lite の
AFTR、到達確認用のサーバー）を組み、その中でルーターを動かして次の 7 点を
外側から確かめる。

1. PPPoE 経由で外向き疎通ができる
2. DS-Lite 経由で外向き疎通ができる
3. PBR が送信元レンジごとに経路を振り分ける
4. ポートフォワードが外から内に通る
5. hairpin NAT が内から自分のグローバル宛に通る
6. ファイアウォールが許可していない通信を落とす
7. NAT のマッピングが endpoint-independent である

root と、開発環境には入っていない外部コマンドが要るので、build tag `netns` の
後ろに分離してある。`go test ./...` はこれを拾わない。通常は
`make test-netns-docker` を使う。必要な道具を入れた特権コンテナを用意し、
その中で `make test-netns` を呼ぶ。

道具が揃った環境なら `make test-netns` で直接走る。前提が欠けているときは、理由を述べて
**失敗する**。飛ばした結果が `ok` と表示されるなら、通ったものと見分けが付かないからである。
`go test -tags netns` を素で叩いた場合は飛ばす。ターゲットに同じことをさせたいときは
`make test-netns REGIED_NETNS_REQUIRE=` と書く。

### 被試験体を差し替える

ルーターを組み立てる処理は 1 つのスクリプトに閉じ込めてあり、環境変数
`REGIED_NETNS_ROUTER_SETUP` で差し替えられる。既定は手書きの `ip` と `nft` で
組んだ参照実装である。受け渡しの約束は `docs/adr/0010-netns-testbed.md` にある。

別の実装を掛けるときは、同じ約束を満たすスクリプトを 1 本書いて
`REGIED_NETNS_ROUTER_SETUP` に指す。テスト側は変えなくてよい。

### 調べる

`REGIED_NETNS_KEEP=1` を付けて走らせるとトポロジを残す。`make netns-shell` で
同じコンテナのシェルに入り、`hack/netns/topo.sh up` で組み立て、
`hack/netns/topo.sh status` で各 netns のアドレスと経路を見られる。片付けは
`hack/netns/topo.sh down`。

## ドキュメント

- [`docs/ja/spec/`](docs/ja/spec/) — 設定の形式とリソース kind
- [`docs/ja/scope.md`](docs/ja/scope.md) — やらないことと、その理由
- [`docs/ja/adr/`](docs/ja/adr/) — 設計判断の記録。
  実装に入る前に読むこと。ここに書かれた決定を黙って覆さないこと

## 先行実装

設定モデルの kind 名とスキーマの作法は、EdgeOS と
[imksoo/routerd](https://github.com/imksoo/routerd) から借りている。
何を実測し、なぜどちらも採らずに自作したかは ADR 0001 にある。
