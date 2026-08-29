//go:build netns

package netns

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 検証項目 6。許可していない外からの通信を落とすこと。
//
// 「繋がらない」だけでは、ファイアウォールが落としたのか、単に待ち受けが
// 無くて RST が返ったのかを区別できない。落としているなら接続は時間切れに
// なり、落としていないなら即座に拒否される。ここではその差を見る。
func TestFirewallDropsUnsolicitedInbound(t *testing.T) {
	// 先に、許可されている入口（ポートフォワード）が通ることを確かめる。
	// これが通らないと、以下の失敗が「そもそも外から届いていない」ことの
	// 裏返しになってしまう。
	eventuallyPeer(t, "許可されている入口の疎通", func() (peerAddr, error) {
		if _, err := dialStub(t, nsInternet, internetA, pppoeGlobalIP, forwardWANPort); err != nil {
			return peerAddr{}, err
		}
		return peerAddr{IP: pppoeGlobalIP, Port: forwardWANPort}, nil
	})

	const connectTimeout = 4 * time.Second

	out, elapsed, err := nsExec(t, nsInternet, cmdTimeout, "",
		"socat", "-t", "3", "-T", "10", "STDIO",
		fmt.Sprintf("TCP4:%s:%d,bind=%s,connect-timeout=%d",
			pppoeGlobalIP, blockedWANPort, internetA, int(connectTimeout.Seconds())))

	if err == nil {
		t.Fatalf("許可していない %s:%d への接続が通ってしまった: 応答は %q",
			pppoeGlobalIP, blockedWANPort, strings.TrimSpace(out))
	}
	if strings.Contains(err.Error(), "refused") {
		t.Fatalf("許可していない %s:%d への接続が拒否で返ってきた（%v）。"+
			"RST を返すということはパケットが router まで届いて処理されており、"+
			"フィルタで落ちていない", pppoeGlobalIP, blockedWANPort, err)
	}
	if elapsed < connectTimeout/2 {
		t.Fatalf("許可していない %s:%d への接続が %s で失敗した。落としているなら"+
			"接続要求は時間切れ（%s 前後）になるはずで、これは早すぎる: %v",
			pppoeGlobalIP, blockedWANPort, elapsed, connectTimeout, err)
	}
}
