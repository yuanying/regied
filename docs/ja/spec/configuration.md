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
| `kind` | はい | `NetworkConfig` |
| `metadata.name` | はい | このホストの設定に付ける名前。ホスト名ではない |
| `spec.global` | いいえ | ホスト全体のスイッチ。後述 |
| `spec.resources` | はい | リソースの一覧 |

apiGroup にプロジェクト名を入れないのは意図的である。バイナリを改名しても
設定ファイルが無効になってはならない。

**文書の kind にも役割名は入れない。** ファイアウォールだけのホストも、回線を 2 本
終端するホストと同じ `NetworkConfig` を書く。そのホストが何をするかは、どのリソースを
並べたかが表すのであって、文書の名前が表すのではない。必須のリソース kind は 1 つも無く、
`router` や `firewall` は型の側に現れない（[ADR 0009](../adr/0009-ownership-boundary.md)）。

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
値は `all`、`default`、そして設定が名指すリンクのうちその時点でホストに在るものの
それぞれに書く。カーネルは `all` とリンク自身の設定の大きい方を適用するからである。

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

テーブル番号は 100 から、マークは 0x100 から順に割り当てる。割り当ては政策が評価される
順序、つまりファミリ・`priority` の順で走り、**文書がリソースを並べた順では走らない。**
だからファイル内でリソースを動かしても番号は変わらず、何も変えていない適用は何も変えない。
固定された値はそのまま残し、割り当てはそれを避けて進む。カーネルが自分用に持っている
`main` / `local` / `default` のテーブルは、固定値としても拒否する。

この 2 つはあくまで割り当ての開始点であり、個々の政策が何番を取るかの約束ではない。
上の一文はそのまま生きている。

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
- `ifname` が他のインターフェースの `bridge.members` に現れ、かつアドレスも持つ `Interface`
- 上流インターフェースに PD クライアントが無いのに、委譲プレフィックスから導く指定
- `egressRef` が `DSLiteTunnel` を指す `PortForward` / `SourceNAT`
  （AFTR 側で変換されるので、内向きに公開できない）
- 待ち受けるレンジと変換先が同じ数のポートを覆っていない `PortForward`
- `protocol` が `tcp` / `udp` の名前以外である `PortForward`
- `sourceRanges` に CIDR でも閉区間でもない裸のアドレスを書いた `EgressRoutePolicy`
- `aftrHost` と `aftrAddress` の両方を持つ `DSLiteTunnel`
- そのどちらも持たない `DSLiteTunnel`
- ホストのリンク名になる名前が 15 文字を超えるもの。カーネルが保持できるのはそこまでである。
  対象は `Interface` の `ifname` と `bridge.members`、`PPPoESession` と `DSLiteTunnel` の
  `metadata.name`
- 2 つ目の `DNSForwarder`。ホストを 1 つの dnsmasq が受け持ち、
  キャッシュも上位リゾルバも 1 組しか無い
- `self` という名前の `FirewallZone`
- `from` が `self` である `FirewallPolicy`
- 参照先の秘密情報ファイルが無い・読めない・空

次は警告にとどめ、適用は続ける。

- `dhcpv6.prefixDelegation` を持ちながら `duidFile` が無い `Interface`。
  新規に引く回線には持ち越す DUID が無く、それは正当な構成である。
  既に委譲を受けている機器を入れ替える構成でこれを省くと、
  **黙って別のプレフィックスを引く。**
- `ipv6` を持つが、そのインターフェースが `otherInformation` を広告していない
  `DHCPServer`。そこに書いた内容を誰も聞きに来ない。
- `target.address` が、そのフォワードが乗る回線へ通信を送る `EgressRoutePolicy` の
  どのレンジにも入っていない `PortForward`。
  **回線に入ってきた接続の戻りは、届いた回線ではなく、答えるホストの
  送信元アドレスで経路が決まる。** だから変換は起き、パケットはホストに届き、
  戻りは別の回線から出ていく。接続は成立せず、設定のどこも間違って見えない。
  戻りを届いた回線に戻すには、ポリシーごとではなく回線ごとのマークが要る。
  それができるまで、regied はこれを言うだけで経路は書き換えない。
