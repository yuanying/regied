# ADR 0008: 既にある実装に委譲する

- 状態: 決定（2026-08-23）

## 背景

「1 台のホストのネットワーク状態を宣言的に管理する」という説明は、既存の道具の説明でもある。
netplan、NetworkManager、nmstate、そして systemd-networkd。実際に Ubuntu 26.04 で確認したところ、
対象とする 7 領域のうち複数を networkd が既に宣言的に持っていた。

| 領域 | systemd-networkd |
|---|---|
| DS-Lite（ipip6） | あり。`Mode=ipip6`、`Local=` は `dhcp_pd` を取れる |
| ポリシールーティング | あり。`RoutingPolicyRule` は `FirewallMark=` を持つ |
| 静的ルート（v4 / v6、テーブル指定） | あり |
| DHCPv6-PD | あり。`SubnetId=`（prefix-id 相当）、`Token=`（host-address 相当） |
| RA / SLAAC 配布 | あり。`IPv6SendRA` |
| NAT | `IPMasquerade` のみ。ポートフォワードも hairpin も無い |
| nftables ファイアウォール | なし |
| dnsmasq（DHCP サーバー・条件付き DNS） | なし |
| PPPoE | **なし** |

**2026-08-31 追記。** 上の表は Ubuntu 26.04 で実測したものである。プラットフォームは
Debian 13 になり、その systemd は 257 である。1 行を除いてそのまま成り立つ。
`Local=dhcp_pd` が入ったのは systemd 258 で、DS-Lite のトンネルは代わりに下位リンク側から
端点アドレスを取る（[ADR 0011](0011-target-platform.md)）。

同じ判断は他の層にも当てはまる。BGP を自前実装する必要はない（将来 MetalLB を BGP 化する際は
FRR / BIRD / GoBGP に委譲する。imksoo/routerd も GoBGP を包んでいる）。

## 決定

**誰も持っていない層だけを自分で持ち、既にある実装には設定を生成して渡す。**

自分で持つもの:

- nftables のファイアウォールモデル（ゾーン、アドレスグループ、穴あけ、IPv4 / IPv6）
- NAT（masquerade、ポートフォワード、hairpin）
- ポリシールーティングの **条件判定**（送信元レンジ、宛先の除外、集合。nftables でマークを打つ）
- PPPoE（pppd の設定生成と監督。networkd に無い）
- dnsmasq の設定生成と監督
- 7 領域を 1 つの宣言に束ねるモデル、dry-run と差分、ロールバック、状態の API

systemd-networkd に渡すもの:

- インターフェース、アドレス、MTU、ブリッジ
- 静的ルート（v4 / v6、テーブル指定込み）
- `RoutingPolicyRule`（`FirewallMark=` → テーブル）
- ip6tnl（DS-Lite）
- DHCPv6-PD と RA / SLAAC 配布

ポリシールーティングが境界をまたぐのは意図的である。**難しい判定（レンジ、否定、集合）は
nftables が得意で、マークからテーブルへの配線は networkd が持っている。**

## netplan を経由しない

netplan は networkd への renderer だが、**語彙が狭い**。トンネルの `Local=dhcp_pd` も、
`DHCPPrefixDelegation` の `SubnetId=` / `Token=` も出せない。移植で最も落としたくない
DS-Lite と PD が、まさにそこで落ちる。

加えて、1 本しかない回線の下に翻訳層を 2 枚重ねることになり、障害時に読む場所が増える。
netplan の出力先は cloud-init も触る場所であり、所有が曖昧になる（→ ADR 0009）。

したがって **`/etc/systemd/network/` に直接置く。** networkd の探索順は `/etc` が
netplan の吐き先 `/run` に優先するため、ルーターでは regied の記述が勝つ。

**ルーター以外のノードでは base のネットワークに触らない。** netplan が持ったままにし、
regied は自分の nftables テーブルだけを持つ。

## 帰結

- 最も壊すと痛い層（トンネル・経路・PD のライフサイクル）を自分で書かずに済む
- 規模が落ち、ADR 0001 の「自分で読み切れる規模に保つ」に効く
- networkd の再読み込みと nftables の適用の順序を、適用モデルで決める必要がある（→ ADR 0004）
- 生成物の所有を明示する必要がある（→ ADR 0009）
