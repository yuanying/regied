// peerAddr は build tag の外に置いてある。応答の読み取りは netns も特権も
// 要らない純粋な処理であり、環境差で壊れたのもここだったので、ユニット
// テストで押さえられる場所に置く。

package netns

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// peerAddr は whoami サービスが見た接続元。NAT を抜けた後の姿である。
type peerAddr struct {
	IP   string
	Port int
}

func (p peerAddr) String() string { return net.JoinHostPort(p.IP, strconv.Itoa(p.Port)) }

// parsePeer は whoami サービスの応答を読む。
//
// 応答が何回に分かれて届くかは環境によって変わる。socat 1.8 は送信側が
// EOF を伝えるために空のデータグラムを足すので、受け側がそれにも応答し、
// 同じ観測が 2 つ並んで届く。1 度に 1 行しか来ない前提で書くと、ルーターの
// 振る舞いとは無関係な理由でテストが落ちる。
//
// 同じ観測が並んでいるだけなら 1 つとして扱う。食い違っていればどれを
// 信じてよいか分からないので失敗させる。黙って片方を採ると、NAT の
// マッピングを読み違えたまま合格しうる。
func parsePeer(out string) (peerAddr, error) {
	var (
		first peerAddr
		found bool
	)
	for _, field := range strings.Fields(out) {
		p, err := parseOnePeer(field)
		if err != nil {
			return peerAddr{}, err
		}
		if !found {
			first, found = p, true
			continue
		}
		if p != first {
			return peerAddr{}, fmt.Errorf("whoami の応答が食い違っている (%q)", strings.TrimSpace(out))
		}
	}
	if !found {
		return peerAddr{}, fmt.Errorf("whoami サービスから応答がない")
	}
	return first, nil
}

func parseOnePeer(text string) (peerAddr, error) {
	host, port, err := net.SplitHostPort(text)
	if err != nil {
		return peerAddr{}, fmt.Errorf("whoami の応答を読めない (%q): %w", text, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return peerAddr{}, fmt.Errorf("whoami の応答のポートを読めない (%q): %w", text, err)
	}
	return peerAddr{IP: host, Port: n}, nil
}
