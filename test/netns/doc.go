//go:build netns

// Package netns は network namespace で組んだ擬似 WAN の上で、ルーターの
// 振る舞いを外形的に検証する。
//
// このテストは被試験体（router netns の中身）に依存しない。検証はすべて
// client netns と internet netns から行い、ルーターの内部状態は覗かない。
// ルーターを組み立てる処理は 1 つのスクリプトに閉じ込めてあり、環境変数
// REGIED_NETNS_ROUTER_SETUP で差し替えられる（ADR 0010）。これにより
// 手書きの ip / nft で組んだ参照実装、既存の実装、自作の実装のどれに対しても
// 同じ 7 項目を掛けられる。
//
// 検証するのは次の 7 点である。
//
//   - PPPoE 経由で外向き疎通ができる
//   - DS-Lite 経由で外向き疎通ができる
//   - PBR が送信元レンジごとに経路を振り分ける
//   - ポートフォワードが外から内に通る
//   - hairpin NAT が内から自分のグローバル宛に通る
//   - ファイアウォールが許可していない通信を落とす
//   - NAT のマッピングが endpoint-independent である
//
// root / CAP_NET_ADMIN と nft / pppd / pppoe-server / socat が要る。
// 手元の開発環境には入っていないので、通常は `make test-netns-docker` から
// 特権コンテナ越しに走らせる。
package netns
