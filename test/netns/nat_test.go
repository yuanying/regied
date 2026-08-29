//go:build netns

package netns

import (
	"fmt"
	"strings"
	"testing"
)

// dialStub は指定した netns から TCP で接続し、接続先が返す一行を読む。
func dialStub(tb testing.TB, ns, srcIP, dstIP string, dstPort int) (string, error) {
	tb.Helper()

	target := fmt.Sprintf("TCP4:%s:%d,connect-timeout=5", dstIP, dstPort)
	if srcIP != "" {
		target = fmt.Sprintf("TCP4:%s:%d,bind=%s,connect-timeout=5", dstIP, dstPort, srcIP)
	}
	out, _, err := nsExec(tb, ns, cmdTimeout, "", "socat", "-t", "3", "-T", "10", "STDIO", target)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// 検証項目 4。外から router のグローバル宛に来た通信が、LAN の中の
// 端末まで届くこと。
func TestPortForwardFromInternet(t *testing.T) {
	var banner string
	eventuallyPeer(t, "外からのポートフォワード", func() (peerAddr, error) {
		got, err := dialStub(t, nsInternet, internetA, pppoeGlobalIP, forwardWANPort)
		if err != nil {
			return peerAddr{}, err
		}
		banner = got
		return peerAddr{IP: internetA, Port: forwardWANPort}, nil
	})

	if !strings.HasPrefix(banner, sshStubBanner) {
		t.Fatalf("%s:%d に繋いだ先が LAN の %s:%d ではない: 応答は %q",
			pppoeGlobalIP, forwardWANPort, lanServerIP, forwardLANPort, banner)
	}
	// 転送では送信元を書き換えない。LAN 側の端末には外の相手がそのまま見える。
	if !strings.Contains(banner, internetA) {
		t.Errorf("LAN 側から見た接続元が外のアドレスになっていない: 応答は %q、期待は %s を含むこと",
			banner, internetA)
	}
}

// 検証項目 5。LAN の中から自分のグローバル宛に繋いだときも、同じ
// ポートフォワードが効くこと（hairpin）。戻りが router を経由するように
// 送信元が router の LAN アドレスに書き換わっている必要がある。
func TestHairpinNAT(t *testing.T) {
	var banner string
	eventuallyPeer(t, "hairpin NAT", func() (peerAddr, error) {
		got, err := dialStub(t, nsClient, clientPPPoESrc, pppoeGlobalIP, forwardWANPort)
		if err != nil {
			return peerAddr{}, err
		}
		banner = got
		return peerAddr{IP: clientPPPoESrc, Port: forwardWANPort}, nil
	})

	if !strings.HasPrefix(banner, sshStubBanner) {
		t.Fatalf("LAN から %s:%d に繋いだ先が LAN の %s:%d ではない: 応答は %q",
			pppoeGlobalIP, forwardWANPort, lanServerIP, forwardLANPort, banner)
	}
	if !strings.Contains(banner, routerLANIP) {
		t.Errorf("hairpin の送信元が router の LAN アドレスに書き換わっていない: "+
			"応答は %q、期待は %s を含むこと。書き換わっていないと戻りが router を通らず、"+
			"接続が成立しないか非対称になる", banner, routerLANIP)
	}
}

// 検証項目 7。宛先を変えても外部ポートが変わらないこと
// （endpoint-independent mapping）。Switch のオンライン対戦などが
// これに依存する。
func TestNATMappingIsEndpointIndependent(t *testing.T) {
	const srcPort = 40001

	first := eventuallyPeer(t, "1 つ目の宛先への UDP", func() (peerAddr, error) {
		return udpWhoami(t, clientPPPoESrc, srcPort, internetA)
	})
	second := eventuallyPeer(t, "2 つ目の宛先への UDP", func() (peerAddr, error) {
		return udpWhoami(t, clientPPPoESrc, srcPort, internetB)
	})

	// 測っている NAT が router のものであることを先に固定する。経路が
	// DS-Lite 側に落ちていると AFTR の NAT を測ってしまい、router の
	// マッピングを見ないまま通ってしまう。
	if first.IP != pppoeGlobalIP {
		t.Fatalf("%s からの UDP が router の NAT を通っていない: 外から見えた送信元は %s、期待は %s",
			clientPPPoESrc, first.IP, pppoeGlobalIP)
	}
	if first.IP != second.IP {
		t.Fatalf("宛先ごとに外部アドレスが変わっている: %s へは %s、%s へは %s",
			internetA, first.IP, internetB, second.IP)
	}
	if first.Port != second.Port {
		t.Fatalf("宛先ごとに外部ポートが変わっている（endpoint-dependent mapping）: "+
			"%s へは %d、%s へは %d。どちらも送信元ポートは %d である",
			internetA, first.Port, internetB, second.Port, srcPort)
	}
}
