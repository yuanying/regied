# リソース kind

11 個の kind。これを収める文書の形は [`configuration.md`](configuration.md)、
なぜこの 11 個なのかは [ADR 0002](../adr/0002-configuration-schema.md) にある。

英語が正である。[docs/spec/kinds.md](../../spec/kinds.md) を参照。

以下、**リンクリソース**とはホストにリンクを作る 3 つの kind
（`Interface` / `PPPoESession` / `DSLiteTunnel`）を、**回線**とは
`PPPoESession` または `DSLiteTunnel` を指す。

| kind | バックエンド | 参照される名前 |
|---|---|---|
| [`Interface`](#interface) | systemd-networkd | `interfaceRef`、`underlayRef`、`linkRefs` |
| [`PPPoESession`](#pppoesession) | pppd | `egressRef`、`linkRefs` |
| [`DSLiteTunnel`](#dslitetunnel) | systemd-networkd | `egressRef`、`linkRefs` |
| [`EgressRoutePolicy`](#egressroutepolicy) | nftables + systemd-networkd | — |
| [`IPAddressSet`](#ipaddressset) | nftables | `addressSetRefs` |
| [`FirewallZone`](#firewallzone) | nftables | `from`、`to` |
| [`FirewallPolicy`](#firewallpolicy) | nftables | — |
| [`SourceNAT`](#sourcenat) | nftables | — |
| [`PortForward`](#portforward) | nftables | — |
| [`DHCPServer`](#dhcpserver) | dnsmasq | — |
| [`DNSForwarder`](#dnsforwarder) | dnsmasq | — |

---

## Interface

両端を regied が持つリンク。物理 NIC か、複数をまとめたブリッジ。そのリンクの属性で
あるものをすべて持つ —— アドレス、MTU、そこから出ていく静的ルート、プレフィックス委譲の
クライアントを走らせるか、下のセグメントに広告するか。

バックエンド: `/etc/systemd/network/` の下、regied の接頭辞を持つ `.network`（ブリッジなら
`.netdev` も）。

| フィールド | 必須 | 値 |
|---|---|---|
| `ifname` | はい | カーネルのインターフェース名。ブリッジなら作る名前 |
| `bridge.members` | いいえ | 束ねるインターフェース名。あればこれはブリッジ |
| `mtu` | いいえ | バイト。既定はカーネルのもの |
| `addresses` | いいえ | アドレスの一覧。後述 |
| `routes` | いいえ | 静的ルートの一覧。後述 |
| `dhcpv6` | いいえ | DHCPv6 クライアントの設定。後述 |
| `ipv6.advertise` | いいえ | ルーター広告の設定。後述 |

他のインターフェースの `bridge.members` に現れるインターフェースは、
自分のアドレスを持ってはならない。

### `addresses[]`

各要素は、プレフィックス長付きのアドレスのリテラルか、委譲プレフィックスから導く指定。

| フィールド | 必須 | 値 |
|---|---|---|
| *(文字列)* | — | リテラル。例 `192.0.2.1/24`、`2001:db8::1/64` |
| `fromDelegatedPrefix.interfaceRef` | はい | PD クライアントを走らせているインターフェース |
| `fromDelegatedPrefix.subnetID` | はい | 委譲されたプレフィックスのどのサブネットを取るか |
| `fromDelegatedPrefix.token` | いいえ | ホスト部。例 `::1`。既定はインターフェース識別子 |

導いたアドレスは委譲が変われば変わり、その上に建っているもの —— トンネルのローカル
アドレス、広告するプレフィックス、DNS が待ち受けるアドレス —— も一緒に変わる。
**結果を書き留めるのではなく導出を宣言するのは、この追随のためである。**

### `routes[]`

| フィールド | 必須 | 値 |
|---|---|---|
| `destination` | はい | CIDR。ファミリは推論する |
| `via` | いいえ | next hop。省略するとこのインターフェースの on-link |
| `metric` | いいえ | 小さいほうが勝つ |

**テーブルを指定するフィールドは無い。** regied が作る追加のルーティングテーブルは
`EgressRoutePolicy` が必要とするものだけで、その中身は regied が自分で入れる。

### `dhcpv6`

上流（事業者に面する）インターフェースに置く。

| フィールド | 必須 | 値 |
|---|---|---|
| `prefixDelegation.duidFile` | いいえ | 送る DUID を収めたファイルのパス。委譲を DUID に紐づける事業者がある |
| `prefixDelegation.prefixLength` | はい | 要求するプレフィックス長。例 `56` |
| `prefixDelegation.rapidCommit` | いいえ | 既定 `true` |
| `useDNS` | いいえ | 既定 `false`。事業者からリゾルバを受け取るか |

### `ipv6.advertise`

下流インターフェースに置く。**ルーター広告は dnsmasq ではなく systemd-networkd のもの
である。** プレフィックスは委譲から来るもので、networkd が既にそれを追っている。
インターフェースに置いてあるので、**同じリンクに 2 人目の広告者を頼む場所が
スキーマのどこにも無い。**

| フィールド | 必須 | 値 |
|---|---|---|
| `mode` | はい | `slaac` —— ステートレス自動設定のためにプレフィックスを広告する |
| `otherInformation` | いいえ | 既定 `false`。O フラグを立て、残りは DHCPv6 に聞けと伝える |
| `dnsServers` | いいえ | RDNSS として広告する |
| `validLifetime` | いいえ | 既定 `24h` |
| `preferredLifetime` | いいえ | 既定は `validLifetime` の半分 |

広告するプレフィックスはこのインターフェースが持っているものである。ここに書かないので、
実際に設定されているアドレスとずれようが無い。

---

## PPPoESession

PPPoE の回線。systemd-networkd に PPPoE は無いので、regied が pppd の設定を生成し、
プロセスを監督する。

バックエンド: pppd の peer ファイル、権限を絞った secrets ファイル、監督下の `pppd`。

| フィールド | 必須 | 値 |
|---|---|---|
| `interfaceRef` | はい | この回線を載せる `Interface` |
| `userIDFile` | はい | アカウント名を収めたファイルのパス。認証情報として扱う |
| `passwordFile` | はい | パスワードを収めたファイルのパス |
| `mtu` | いいえ | 既定 `1492`。Ethernet 上の PPPoE で取れる最大 |
| `persist` | いいえ | 既定 `true`。切れたら掛け直す |
| `holdoff` | いいえ | 既定 `5s`。掛け直す前に待つ時間 |
| `useDNS` | いいえ | 既定 `false`。相手からリゾルバを受け取るか |
| `defaultRoute.install` | いいえ | 既定 `true`。main テーブルにデフォルトルートを入れる |
| `defaultRoute.metric` | いいえ | 既定 `0`。上げるとこの回線は待機側になる |
| `routes` | いいえ | `Interface` と同じ |

リンク名は `metadata.name` から付ける。掛け直しをまたいで、他のリソースと
ファイアウォールから見える名前が変わらない。

`defaultRoute.metric` は、回線が 2 本あるホストが「**自分自身の通信**はどちらを使うか」
を言う場所である。ルーター起点の通信はポリシールーティングに掛からないので、
これだけが決め手になる。

認証情報のファイルは `--dry-run` の出力にも差分にも状態 API にも現れない
（[ADR 0003](../adr/0003-secrets-out-of-configuration.md)）。

---

## DSLiteTunnel

IPv4 over IPv6 の回線。RFC 6333 の B4 側である。IPv4 パケットを IPv6 でカプセル化して
事業者の AFTR に渡し、アドレス変換は AFTR が行う。

バックエンド: kind `ip6tnl` / mode `ipip6` の `.netdev` と、その `.network`。

| フィールド | 必須 | 値 |
|---|---|---|
| `underlayRef` | はい | トンネルを載せる IPv6 を持つ `Interface` |
| `localAddressFrom.interfaceRef` | どちらか | このインターフェースの IPv6 アドレスをトンネルのローカルアドレスにする |
| `localAddress` | どちらか | IPv6 アドレスのリテラル。静的にアドレスを持つ構成向け |
| `aftrAddress` | はい | 事業者の AFTR。IPv6 アドレス |
| `mtu` | いいえ | 既定 `1454` |
| `ttl` | いいえ | 既定 `64` |
| `defaultRoute.install` | いいえ | 既定 `true` |
| `defaultRoute.metric` | いいえ | 既定 `0` |
| `routes` | いいえ | `Interface` と同じ |

`localAddressFrom` と `localAddress` はちょうど一方が必須。プレフィックスが委譲で
来る構成では `localAddressFrom` を使う。**プレフィックスが変わったときに、誰かが
ファイルを直すまで沈黙するのではなく、トンネルが追随する。**

**この回線に `SourceNAT` を置いてはならない。** AFTR が変換するので、内側の送信元
アドレスはそのままにする。ここで masquerade を掛けると二重変換になる。同じ理由から
この回線で内向きに公開できるものは無く、これを名指す `PortForward` は検証エラーになる。

---

## EgressRoutePolicy

ある種別のトラフィックがどの回線から出るか。トラフィックを特定する**判定**と、
そこから決まる**経路**の両方を、1 つのリソースが持つ。

バックエンド: マークを打つ nftables ルールと、専用テーブルの `[RoutingPolicyRule]` および
デフォルトルート（どちらも systemd-networkd）。

| フィールド | 必須 | 値 |
|---|---|---|
| `family` | いいえ | 既定 `ipv4` |
| `priority` | はい | 小さいほうを先に評価する。ファミリの中で一意 |
| `egressRef` | はい | このトラフィックが出ていく回線 |
| `sourceRanges` | いいえ | CIDR、または `192.0.2.130-192.0.2.255` のような閉区間 |
| `sourceAddressSetRefs` | いいえ | `IPAddressSet` の名前。レンジを直接書く代わりに使う |
| `excludeDestinations` | いいえ | この回線に送ってはならない宛先の CIDR |
| `table` | いいえ | ルーティングテーブル番号を regied に選ばせず固定する |
| `mark` | いいえ | ファイアウォールマークを固定する |

`sourceRanges` と `sourceAddressSetRefs` は少なくとも一方が必要。

**`excludeDestinations` がローカルの通信をローカルに留める。** 「このホスト群は PPPoE から
出る」と書いた政策は LAN 自身を除外しなければならず、さもないと LAN 内どうしの通信まで
回線に送られる。これは hairpin を成立させるものでもある。ただし間接的に、である。
マークは宛先変換の後で打たれるので、LAN 側の端末が回線のグローバルアドレス経由で
公開サービスに繋ぎに行くとき、その時点で宛先は既に内側のホストになっている。
だからこの除外に当たってローカルに留まる。**グローバルアドレスを知る必要がどこにも無い。**

`table` と `mark` は、ルーティングテーブルを他の何かと共有するホストのためにある。
省略すると regied が割り当て、その結果を `regied render` が報告する。

`EgressRoutePolicy` を 1 つでも書くと、`spec.global.sourceValidation` は無効でなければ
ならない。ポリシールーティングは戻りの経路を非対称にし、リバースパスフィルタは
まさにそれを落とす。

---

## IPAddressSet

名前の付いたアドレス／プレフィックスの集合。ホストのまとまりを 1 度書いて、
複数のルールから参照するためのもの。

バックエンド: regied のテーブルの中の nftables set。

| フィールド | 必須 | 値 |
|---|---|---|
| `family` | はい | `ipv4` または `ipv6` |
| `addresses` | いいえ | 個々のアドレス |
| `networks` | いいえ | プレフィックス |

2 つのリストの少なくとも一方は空でないこと。プレフィックスを含む集合は interval set になる。

---

## FirewallZone

名前の付いたリンクの集合。ファイアウォールの政策は、ゾーンとゾーンの間に書く。

バックエンド: インターフェース名の nftables set。

| フィールド | 必須 | 値 |
|---|---|---|
| `linkRefs` | はい | リンクリソースの名前 —— `Interface` / `PPPoESession` / `DSLiteTunnel` |

**`wan` や `lan` はただのゾーン名であって、スキーマが知っている概念ではない**
（[ADR 0009](../adr/0009-ownership-boundary.md)）。ルーターでないホストは、
実際にあるものに沿ってゾーンに名前を付ける。

`self` は予約名で、ゾーン名に使えない。ホスト自身を指す。

---

## FirewallPolicy

あるゾーンから別のゾーンへ、またはあるゾーンからホスト自身へ向かうトラフィックに
適用するルール。

バックエンド: regied の nftables テーブルの中のチェーン。hook のチェーンから
インターフェース集合で振り分ける。

| フィールド | 必須 | 値 |
|---|---|---|
| `from` | はい | `FirewallZone` の名前 |
| `to` | はい | `FirewallZone` の名前、または `self` |
| `defaultAction` | はい | `accept` / `drop` / `reject` —— どのルールにも当たらなかったとき |
| `logDefault` | いいえ | `defaultAction` が `accept` でなければ既定 `true` |
| `stateful` | いいえ | 既定 `true`。後述 |
| `rules` | いいえ | 順に評価する。最初に当たったものが勝つ |

netfilter の hook は組から決まる。`to: self` は input、ゾーンからゾーンは forward。
**output に対する政策は無い。** ホスト自身が送り出すものをここで絞ることはしない。

**政策が書かれていないゾーンの組は drop される。** 誰も書かなかったからといって
その組が暗黙に開くことは無い。

**`stateful: true` は、どのチェーンにも要る 2 つのルールを先頭に置く。**
established / related を accept し、invalid を drop する。政策ごとに手で書くのは、
1 つ書き忘れる書き方である。

同じ `from` と `to` を持つ政策は 1 つだけ。

### `rules[]`

| フィールド | 必須 | 値 |
|---|---|---|
| `name` | はい | 診断のため。カウンタとログの接頭辞に出る |
| `action` | はい | `accept` / `drop` / `reject` |
| `family` | いいえ | `ipv4` / `ipv6`。省略すると両方 |
| `protocol` | いいえ | `tcp` / `udp` / `icmp` / `icmpv6` / `ipip` / `esp`、またはプロトコル番号 |
| `sourceCIDRs` | いいえ | |
| `sourceAddressSetRefs` | いいえ | `IPAddressSet` の名前 |
| `sourcePorts` | いいえ | 数値、または `60000-60010` のような範囲 |
| `destinationCIDRs` | いいえ | |
| `destinationAddressSetRefs` | いいえ | |
| `destinationPorts` | いいえ | |
| `log` | いいえ | 既定 `false` |

ルールとファミリの合わないアドレス集合を参照するのは検証エラー。

**落とすと静かに壊れる**ので知っておく価値のあるルールが 2 つある。

- **上流ゾーンから `self` への `protocol: ipip`** が無いと、DS-Lite のトンネルが
  そもそも上がらない。カプセル化されたパケットはこのホスト宛に届く
- **上流ゾーンから `self` への udp 宛先ポート 546** が無いと、DHCPv6 クライアントが
  応答を受け取れず、したがってプレフィックスの委譲も受けられない

---

## SourceNAT

出ていくトラフィックの送信元アドレスを書き換える。

バックエンド: regied の nftables postrouting チェーンのルール。

| フィールド | 必須 | 値 |
|---|---|---|
| `type` | いいえ | `masquerade` が唯一の値であり既定 |
| `egressRef` | はい | 適用する回線。アドレスはその回線に追随する |
| `sourceRanges` | いいえ | CIDR。省略するとすべての送信元 |
| `excludeDestinations` | いいえ | 変換せずに通す宛先の CIDR |

`masquerade` はパケットが出る時点で出口リンクからアドレスを取る。だから動的アドレスの
回線でも、書き留めるものは何も無く、アドレスが変わっても再適用は要らない。

`DSLiteTunnel` に対する `SourceNAT` は検証エラー。AFTR が既に変換している。

---

## PortForward

回線に着いたトラフィックの宛先を書き換え、内側のホストに外から到達できるようにする。

バックエンド: 宛先 NAT のルール、hairpin のための送信元 NAT のルール、
そして（切らない限り）変換後のトラフィックを通すファイアウォールの穴。

| フィールド | 必須 | 値 |
|---|---|---|
| `egressRef` | はい | トラフィックが着く回線。待ち受けアドレスはその回線に追随する |
| `protocol` | はい | `tcp` または `udp` |
| `port` | どちらか | ポート 1 つ |
| `portRange` | どちらか | 閉区間。例 `60000-60010`。**1 本のルールで表す** |
| `target.address` | はい | 内側のホスト |
| `target.port` / `target.portRange` | いいえ | 既定は受けと同じ |
| `hairpin` | いいえ | 既定 `true` |
| `openFirewall` | いいえ | 既定 `true` |

**待ち受けアドレスを書くフィールドは無い。** それは回線のものであり、動的アドレスの
回線でそれを書き留めると、アドレスが変わるまでは動き、変わった瞬間に「別の何かが
壊れたように見える形」で失敗する設定ができる。`egressRef` がすべてである。

**`hairpin`** は、内側のホストが回線のグローバルアドレス経由で同じサービスに繋ぐ場合を
扱う。内側の端末が外側と同じ公開名を解決すれば、これが起きる。無いと応答が転送先から
端末へ直接返り、変換を通らないので端末はそれを捨てる。

**`openFirewall`** は、変換後のトラフィックが通る経路に、この転送に対応する accept を
足す。既定で有効なのは、既定 drop のファイアウォールで穴の無いポートフォワードは
**どの場合でも間違い**だからである。切るのは、より狭いルールを手で書くときだけ。

`egressRef` が `DSLiteTunnel` を指す `PortForward` は検証エラー。
そこから公開できるものは無い。

---

## DHCPServer

配下のリンクへのアドレス配布。

バックエンド: dnsmasq。すべての `DHCPServer` と `DNSForwarder` が 1 つの dnsmasq 設定と
1 つの監督下プロセスに落ちる。

| フィールド | 必須 | 値 |
|---|---|---|
| `interfaceRef` | はい | 対象のリンク |
| `subnet` | はい | プールと静的マッピングが収まる CIDR |
| `pool.start`、`pool.end` | はい | 両端を含む。サブネットの一部でよい |
| `leaseTime` | いいえ | 既定 `24h` |
| `gateway` | いいえ | 既定はインターフェース自身のアドレス |
| `dnsServers` | いいえ | 既定はインターフェース自身のアドレス |
| `domain` | いいえ | 端末に渡す検索ドメイン |
| `staticMappings` | いいえ | 後述 |
| `ipv6` | いいえ | ステートレス DHCPv6。後述 |

サブネットより狭いプールは普通の使い方である。残りは静的マッピングと、
手で振るアドレスのために空けておく。

### `staticMappings[]`

| フィールド | 必須 | 値 |
|---|---|---|
| `name` | はい | 配り、登録するホスト名 |
| `macAddress` | はい | |
| `address` | はい | `subnet` の中、かつ `pool` の外 |

ポリシールーティングが送信元レンジで回線を選ぶ構成では、**静的マッピングが境界の
どちら側に落ちるかが、そのホストの使う回線を決める。** マッピングを境界の向こうに
動かすのは見た目の変更ではなく経路の変更である。

### `ipv6`

IPv6 設定のステートレスな半分のためにある。プレフィックスとルーター広告は
インターフェース側（[`Interface.ipv6.advertise`](#ipv6advertise)）から出る。
ここにあるのは、**広告に言われた端末が DHCPv6 に聞きに来たときに答えるもの**である。

| フィールド | 必須 | 値 |
|---|---|---|
| `mode` | はい | `stateless` —— 情報要求に答え、アドレスは配らない |
| `dnsServers` | いいえ | |
| `informationRefreshTime` | いいえ | 既定 `6h` |

インターフェースの広告に `otherInformation` が無いままこれを書くと検証の警告になる。
誰も聞きに来ないからである。

---

## DNSForwarder

配下のセグメントのための名前解決。条件付きフォワードと名前の上書きを含む。

バックエンド: dnsmasq。すべての `DHCPServer` と併合して 1 つの設定になる。

| フィールド | 必須 | 値 |
|---|---|---|
| `listenOn` | はい | リンクリソースの名前と、予約名 `loopback` |
| `cacheSize` | いいえ | 既定 `150` 件 |
| `upstreams` | はい | 上位リゾルバのアドレス。IPv4 / IPv6 |
| `conditional` | いいえ | 後述 |
| `staticHosts` | いいえ | 後述 |

`listenOn` はアドレスではなくリンクを名指す。だから委譲プレフィックスからアドレスを
導いているインターフェースでも、プレフィックスが変わった後も待ち受け続ける。

### `conditional[]`

| フィールド | 必須 | 値 |
|---|---|---|
| `domain` | はい | 振り分けるゾーン |
| `servers` | はい | 上位の代わりに聞きに行く先 |

内部のゾーン —— たとえばクラスタのサービスドメイン —— を、それを知っているリゾルバに
解かせ、それ以外は上位に流すための仕組みである。

### `staticHosts[]`

| フィールド | 必須 | 値 |
|---|---|---|
| `name` | はい | FQDN 1 つ |
| `address` | はい | 返す答え |

名前 1 つの上書き。よくある用途は、公開名を内側の端末には内側のアドレスで返し、
回線を出て戻ってくるのではなく直接そのサービスに繋がせることである。
