//go:build netns

package netns

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// トポロジの固定値。hack/netns/lib.sh と対になっている。片方だけ変えないこと。
const (
	nsClient   = "rg-client"
	nsInternet = "rg-internet"

	// client netns が持つ 3 つの送信元。PBR の振り分けを外から見分けるために
	// レンジをまたいで置いてある。
	clientPPPoESrc  = "192.168.1.20"  // 192.168.1.10-99 → PPPoE
	clientDSLiteSrc = "192.168.1.200" // それ以外 → DS-Lite（既定）
	lanServerIP     = "192.168.1.30"  // ポートフォワードの宛先。PPPoE レンジ内

	routerLANIP = "192.168.0.1"

	// 外から見えるグローバル。どちらの経路を通ったかはこの 2 つで見分ける。
	pppoeGlobalIP  = "198.51.100.2" // PPPoE で払い出される router のアドレス
	dsliteGlobalIP = "192.0.2.1"    // AFTR が NAT44 に使う外側アドレス

	// internet netns の 2 アドレス。NAT のマッピングが宛先に依らないことを
	// 確かめるために宛先を 2 つ用意している。
	internetA = "203.0.113.10"
	internetB = "203.0.113.20"

	whoamiTCPPort = 8080
	whoamiUDPPort = 9999

	forwardWANPort = 8022 // 外 → 192.168.1.30:22 に転送される
	forwardLANPort = 22
	blockedWANPort = 9999 // 転送も許可もされていない

	sshStubBanner = "sshd-stub"
)

// 待ち時間。PPPoE の再接続やトンネルの立ち上がりを見込んで、疎通は
// 一定時間リトライする。
const (
	readyTimeout = 45 * time.Second
	cmdTimeout   = 20 * time.Second
)

func TestMain(m *testing.M) {
	if reason := unmetPrerequisite(); reason != "" {
		// 道具が揃った環境で走らせたつもりのときに黙って飛ばすと、
		// 通っていないものが通ったように見える。コンテナ経由の実行では
		// REGIED_NETNS_REQUIRE が立っているので、そこでは失敗させる。
		if os.Getenv("REGIED_NETNS_REQUIRE") != "" {
			fmt.Fprintf(os.Stderr, "netns テストの前提が満たされていない: %s\n", reason)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "netns テストを飛ばす: %s\n", reason)
		fmt.Fprintf(os.Stderr, "道具の揃った環境で走らせるには `make test-netns-docker` を使う。\n")
		os.Exit(0)
	}

	if out, err := topo("up"); err != nil {
		fmt.Fprintf(os.Stderr, "トポロジを組めなかった: %v\n%s\n", err, out)
		if _, derr := topo("down"); derr != nil {
			fmt.Fprintf(os.Stderr, "後始末にも失敗した: %v\n", derr)
		}
		os.Exit(1)
	}

	code := m.Run()

	if os.Getenv("REGIED_NETNS_KEEP") != "" {
		fmt.Fprintf(os.Stderr, "REGIED_NETNS_KEEP が立っているのでトポロジを残す。"+
			"消すには hack/netns/topo.sh down を実行すること。\n")
	} else if out, err := topo("down"); err != nil {
		fmt.Fprintf(os.Stderr, "後始末に失敗した: %v\n%s\n", err, out)
	}

	os.Exit(code)
}

// unmetPrerequisite は前提が欠けていればその理由を返す。揃っていれば空文字。
func unmetPrerequisite() string {
	if os.Geteuid() != 0 {
		return "root で走っていない（netns の作成に CAP_NET_ADMIN が要る）"
	}
	for _, bin := range []string{"ip", "nft", "pppd", "pppoe-server", "socat"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Sprintf("%s が見つからない", bin)
		}
	}
	return ""
}

// topo は擬似 WAN の構築・破棄スクリプトを呼ぶ。被試験体の差し替えは
// このスクリプトの中で環境変数越しに行われる。
func topo(action string) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, filepath.Join(root, "hack", "netns", "topo.sh"), action)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func repoRoot() (string, error) {
	// go test の作業ディレクトリはパッケージのディレクトリ（test/netns）。
	return filepath.Abs(filepath.Join("..", ".."))
}

// nsExec は指定した netns の中でコマンドを実行し、標準出力・所要時間・
// 実行結果を返す。所要時間は「落とされた（タイムアウト）」と
// 「拒否された（RST が返った）」を見分けるのに使う。
func nsExec(tb testing.TB, ns string, timeout time.Duration, stdin string, args ...string) (string, time.Duration, error) {
	tb.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	full := append([]string{"netns", "exec", ns}, args...)
	cmd := exec.CommandContext(ctx, "ip", full...)
	cmd.Stdin = strings.NewReader(stdin)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if err != nil {
		err = fmt.Errorf("%s: %w (%s)", strings.Join(full, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), elapsed, err
}

// tcpWhoami は client netns から送信元アドレスを選んで internet netns の
// whoami サービスに TCP で接続し、外から見えた送信元を返す。
func tcpWhoami(tb testing.TB, srcIP, dstIP string) (peerAddr, error) {
	tb.Helper()
	out, _, err := nsExec(tb, nsClient, cmdTimeout, "",
		"socat", "-t", "3", "-T", "10", "STDIO",
		fmt.Sprintf("TCP4:%s:%d,bind=%s,connect-timeout=5", dstIP, whoamiTCPPort, srcIP))
	if err != nil {
		return peerAddr{}, err
	}
	return parsePeer(out)
}

// udpWhoami は送信元ポートまで固定して UDP の whoami サービスを叩く。
// 宛先を変えても外部ポートが変わらないこと（endpoint-independent mapping）を
// 確かめるのに使う。
func udpWhoami(tb testing.TB, srcIP string, srcPort int, dstIP string) (peerAddr, error) {
	tb.Helper()
	out, _, err := nsExec(tb, nsClient, cmdTimeout, "probe\n",
		"socat", "-t", "3", "-T", "10", "STDIO",
		// shut-none を付けないと、socat 1.8 は EOF を伝えるために空の
		// データグラムを足す。受け側はそれも 1 つの問い合わせとして扱うので
		// 応答が 2 つ返る。読み取り側でも吸収しているが、線の上の振る舞いを
		// 環境によらず同じにしておく。
		fmt.Sprintf("UDP4:%s:%d,bind=%s:%d,shut-none", dstIP, whoamiUDPPort, srcIP, srcPort))
	if err != nil {
		return peerAddr{}, err
	}
	return parsePeer(out)
}

// eventuallyPeer は疎通が立ち上がるまでリトライする。PPPoE の接続や
// トンネルの経路が入るまでに数秒かかることがあるため。
func eventuallyPeer(tb testing.TB, what string, probe func() (peerAddr, error)) peerAddr {
	tb.Helper()

	deadline := time.Now().Add(readyTimeout)
	var last error
	for {
		p, err := probe()
		if err == nil {
			return p
		}
		last = err
		if time.Now().After(deadline) {
			tb.Fatalf("%s が %s 以内に成立しなかった: %v", what, readyTimeout, last)
		}
		time.Sleep(time.Second)
	}
}
