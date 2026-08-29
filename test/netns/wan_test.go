//go:build netns

package netns

import "testing"

// 検証項目 1。PPPoE レンジの端末から外に出られること。外から見えた
// 送信元が PPPoE で払い出されたグローバルであることまで確かめる。
// 単に「繋がる」ではなく「PPPoE を通った」ことを見ている。
func TestOutboundViaPPPoE(t *testing.T) {
	got := eventuallyPeer(t, "PPPoE 経由の外向き疎通", func() (peerAddr, error) {
		return tcpWhoami(t, clientPPPoESrc, internetA)
	})

	if got.IP != pppoeGlobalIP {
		t.Fatalf("%s からの通信が PPPoE を通っていない: 外から見えた送信元は %s、期待は %s",
			clientPPPoESrc, got.IP, pppoeGlobalIP)
	}
}

// 検証項目 2。DS-Lite（ipip6 トンネル）側の端末から外に出られること。
// AFTR が NAT44 に使うアドレスで出ていれば、トンネルを通っている。
func TestOutboundViaDSLite(t *testing.T) {
	got := eventuallyPeer(t, "DS-Lite 経由の外向き疎通", func() (peerAddr, error) {
		return tcpWhoami(t, clientDSLiteSrc, internetA)
	})

	if got.IP != dsliteGlobalIP {
		t.Fatalf("%s からの通信が DS-Lite を通っていない: 外から見えた送信元は %s、期待は %s",
			clientDSLiteSrc, got.IP, dsliteGlobalIP)
	}
}

// 検証項目 3。同じ LAN の端末でも、送信元レンジによって出口が変わること。
// 192.168.1.10-99 は PPPoE、それ以外は DS-Lite（既定）。
func TestPolicyRoutingSplitsBySourceRange(t *testing.T) {
	cases := []struct {
		name   string
		srcIP  string
		wantIP string
	}{
		{"PPPoE レンジの下端寄り", clientPPPoESrc, pppoeGlobalIP},
		{"PPPoE レンジのポートフォワード先", lanServerIP, pppoeGlobalIP},
		{"レンジ外は既定の DS-Lite", clientDSLiteSrc, dsliteGlobalIP},
	}

	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		got := eventuallyPeer(t, tc.name+"の外向き疎通", func() (peerAddr, error) {
			return tcpWhoami(t, tc.srcIP, internetA)
		})
		seen[tc.srcIP] = got.IP

		if got.IP != tc.wantIP {
			t.Errorf("%s（%s）の出口が違う: 外から見えた送信元は %s、期待は %s",
				tc.name, tc.srcIP, got.IP, tc.wantIP)
		}
	}

	// 全部同じ出口に出ているなら、そもそも振り分けが効いていない。
	if seen[clientPPPoESrc] == seen[clientDSLiteSrc] {
		t.Fatalf("送信元レンジで経路が分かれていない: %s も %s も %s から出ている",
			clientPPPoESrc, clientDSLiteSrc, seen[clientPPPoESrc])
	}
}
