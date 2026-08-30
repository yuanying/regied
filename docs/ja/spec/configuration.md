# 設定の形式

regied の設定ファイルのスキーマ。この形にした理由は
[ADR 0002](../adr/0002-configuration-schema.md)、kind ごとのフィールドは
[`kinds.md`](kinds.md)、実例は [`config/example.yaml`](../../../config/example.yaml) にある。

英語が正である。[docs/spec/configuration.md](../../spec/configuration.md) を参照。

> **状態: スキーマは形が決まった段階で、細部は確定していない。** 以下の kind と
> 持ち分は決定である。個々のフィールド名は最初のリリースまでに動くことがあり、
> `v1alpha1` はその意味である。

## 文書

ホスト 1 台につき YAML 1 文書。

| フィールド | 必須 | 値 |
|---|---|---|
| `apiVersion` | はい | `net.unstable.cloud/v1alpha1` |
| `kind` | はい | `Router` |
| `metadata.name` | はい | このホストの設定に付ける名前。ホスト名ではない |
| `spec.global` | いいえ | ホスト全体のスイッチ。後述 |
| `spec.resources` | はい | リソースの一覧 |

apiGroup にプロジェクト名を入れないのは意図的である。バイナリを改名しても
設定ファイルが無効になってはならない。

`spec.resources` の各要素は次を持つ。

| フィールド | 必須 | 値 |
|---|---|---|
| `kind` | はい | [`kinds.md`](kinds.md) の 11 kind のいずれか |
| `metadata.name` | はい | kind の中で一意。他のリソースから参照される |
| `spec` | はい | kind ごとに異なる |

**一覧の順序に意味は無い。** 評価順が意味を持つところ（ポリシールーティングと、
1 つの政策の中のルール）は、明示の `priority` フィールドか内側のリストの順序で表す。
`spec.resources` の順序では表さない。

## リソース間の参照

リソースは `<kind の lower camel case>Ref` というフィールドで相手を参照し、
値は相手の `metadata.name` である。kind はフィールド側で決まるので値には書かない。

| フィールド | 指す先 |
|---|---|
| `interfaceRef`、`underlayRef` | `Interface` |
| `egressRef` | 回線 —— `PPPoESession` または `DSLiteTunnel` |
| `linkRefs` | リンクリソースの一覧 —— `Interface` / `PPPoESession` / `DSLiteTunnel` |
| `from`、`to` | `FirewallZone`、または予約名 `self` |
| `addressSetRefs` | `IPAddressSet` の一覧 |

以下 2 つの総称を使う。**リンクリソース**とは、ホストにリンクを作る kind
（`Interface` / `PPPoESession` / `DSLiteTunnel`）のことである。**回線**とは
`PPPoESession` または `DSLiteTunnel`、すなわち外に向かうリンクリソースであり、
NAT とポリシールーティングが指せるのはこれだけである。

存在しないリソースへの参照は検証エラー。参照の循環も同じ。

## ホスト全体のスイッチ: `spec.global`

カーネルの設定と、ファイアウォール全体に効く挙動が 1 つ。これらはリソースではない。
ホストにつき 1 つずつしかなく、誰も参照しない。

| フィールド | 既定 | 効果 | バックエンド |
|---|---|---|---|
| `ipForwarding` | `false` | インターフェース間で IPv4 / IPv6 を転送する | カーネル |
| `synCookies` | `true` | SYN flood 対策 | カーネル |
| `logMartians` | `false` | ありえない送信元アドレスのパケットをログする | カーネル |
| `sendRedirects` | `false` | ICMP redirect を送る | カーネル |
| `receiveRedirects` | `false` | ICMP redirect を受け入れる | カーネル |
| `sourceValidation` | `false` | リバースパスフィルタ | カーネル |
| `mssClamp` | `auto` | 手前のセグメントより MTU の小さい経路で TCP MSS を抑える | nftables |

2 つ注記する。

**`sourceValidation` の既定は無効。** ポリシールーティングは設計として戻りの経路を
非対称にする。経路表が選ばないインターフェースに応答が返ってくるのであり、
strict なリバースパスフィルタはまさにそれを落とす。有効にしてよいのは
`EgressRoutePolicy` が 1 つも無いホストだけで、regied はこの組み合わせを拒否する。

**`mssClamp: auto`** は、インターフェースの種別を 1 つ名指すのではなく、
手前のセグメントより MTU が低いすべての経路で抑える。トンネルは PPPoE と同じくらい
これを必要とする。`off` で無効、数値を書くとその値に固定する。

カーネルの設定は適用時に反映し、適用が失敗したときに戻せるよう記録する。
**`/etc/sysctl.d/` には書かない。** そこに置くと起動時に反映され、
ファイアウォールがまだ無いうちに転送が有効になるからである。

## どこに落ちるか

| 領域 | kind | バックエンド |
|---|---|---|
| リンク、アドレス、MTU、ブリッジ | `Interface` | systemd-networkd |
| 静的ルート | `Interface` | systemd-networkd |
| プレフィックス委譲、ルーター広告 | `Interface` | systemd-networkd |
| IPv4 over IPv6 のトンネル | `DSLiteTunnel` | systemd-networkd |
| ルーティングポリシールールとそのテーブル | `EgressRoutePolicy` | systemd-networkd |
| PPPoE の回線 | `PPPoESession` | pppd |
| アドレス配布、DNS | `DHCPServer`、`DNSForwarder` | dnsmasq |
| ファイアウォール、NAT、ポリシールーティングの判定 | `FirewallZone`、`FirewallPolicy`、`IPAddressSet`、`SourceNAT`、`PortForward`、`EgressRoutePolicy` | nftables |
| カーネルのスイッチ | `spec.global` | カーネル |

regied は networkd の設定を自分の接頭辞のファイルとして `/etc/systemd/network/` に置き、
すべての `DHCPServer` と `DNSForwarder` から 1 つの dnsmasq 設定を組み立て、
nftables は自分のテーブルだけを作り替える。ルールセット全体を flush することは無く、
自分が作っていないファイルを書き換えることも無い
（[ADR 0009](../adr/0009-ownership-boundary.md)）。

## 秘密情報

**このスキーマに秘密情報を入れられるフィールドは無い。** 認証情報は、それを収めた
ファイルのパスで名指し、そのファイルは設定の外に置く
（[ADR 0003](../adr/0003-secrets-out-of-configuration.md)）。PPPoE のユーザー ID も
認証情報として扱う。参照先が無い・読めない場合は検証エラーになる。

## 導出される値

利用者が手で整合を取らねばならないものを、いくつか導出に変えてある。

| 導出されるもの | 元 | 固定できるか |
|---|---|---|
| ルーティングテーブル番号 | `EgressRoutePolicy` | できる（`spec.table`） |
| ファイアウォールマーク | `EgressRoutePolicy` | できる（`spec.mark`） |
| 政策のテーブルに入るデフォルトルート | 政策の `egressRef` | できない |
| マークからテーブルへのルーティングポリシールール | 政策 | できない |
| ポートフォワードのためのファイアウォールの穴 | `PortForward` | できる（`spec.openFirewall: false`） |
| 政策の先頭に置く状態追跡のルール | `FirewallPolicy` | できる（`spec.stateful: false`） |

導出された番号は `regied render` で見える。**regied の外からこれに依存してはならない。**
判定が経路になるまでの実装の都合でしかない。

## 回線 2 本のホストで、部品がどう噛み合うか

設定はトラフィックについての判断を表し、その判断は 2 つの半分に分かれてカーネルに届く。

1. `EgressRoutePolicy` は**どのトラフィックか**を書く（送信元のレンジと、
   除外すべき宛先）。この半分は照合であり、マークを打つ nftables ルールになる
2. 同じリソースが**どの回線か**を名指す。この半分は、その回線へのデフォルトルートを
   持つルーティングテーブルと、マークでそのテーブルを選ぶルーティングポリシールールになる

**マークを打つチェーンは nat prerouting より後ろで走る。** この順序が hairpin を
成立させる。LAN 側の端末が回線のグローバルアドレス経由で公開サービスに繋ぐとき、
マークを考える時点で宛先は既に内側のホストに書き換わっている。だから政策の
ローカル宛除外に当たり、ローカルに配送される。**回線のグローバルアドレスを
どこにも書き留める必要が無い。** このスキーマにそれを受け取るフィールドが無いのは
そのためで、`PortForward` と `SourceNAT` は `egressRef` を取る。

## 検証

参照が解けること・必須フィールドがあることに加えて、regied は次を拒否する。

- `sourceValidation: true` と `EgressRoutePolicy` の同居
- 同じファミリで `priority` が重複する `EgressRoutePolicy`
- `from` と `to` が同じ `FirewallPolicy` が 2 つ
- ブリッジのメンバーでありながらアドレスも持つ `Interface`
- 上流インターフェースに PD クライアントが無いのに、委譲プレフィックスから導く指定
- `egressRef` が `DSLiteTunnel` を指す `PortForward` / `SourceNAT`
  （AFTR 側で変換されるので、内向きに公開できない）
- `self` という名前の `FirewallZone`
- 参照先の秘密情報ファイルが無い・読めない・空
