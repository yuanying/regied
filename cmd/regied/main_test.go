package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestNoCommandPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Errorf("regied with no command exits %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "regied render") {
		t.Errorf("the usage does not name the commands:\n%s", stderr.String())
	}
}

func TestAnUnknownCommandSaysSo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"reboot"}, &stdout, &stderr); code != 2 {
		t.Errorf("an unknown command exits %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `there is no "reboot" command`) {
		t.Errorf("the error does not name what was asked for:\n%s", stderr.String())
	}
}

// TestRenderShowsTheExample is the task's third completion condition from the command
// line: the nftables ruleset, the dnsmasq configuration and the routing a configuration
// produces are all visible without applying anything.
func TestRenderShowsTheExample(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"render",
		"-config", "../../config/example.yaml",
		"-aftr", "dslite=2001:db8:53::1",
		"-duid", "/etc/regied/secrets/dhcpv6-duid=00:03:00:01:00:00:5e:00:53:01",
		"-uplink-address", "pppoe0=192.0.2.10",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("rendering the example exits %d:\n%s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"table inet regied",                // the firewall, NAT and the policy-routing match
		"/etc/regied/dnsmasq/dnsmasq.conf", // the address handout and DNS
		"[RoutingPolicyRule]",              // which uplink a class of traffic leaves by
		"[Route]",                          // the routes each table holds
		"/etc/regied/ppp/peers/pppoe0.conf",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendering does not show %q", want)
		}
	}
	if !strings.Contains(out, "content not shown") {
		t.Error("the credentials file is shown without saying its content is withheld")
	}
}

func TestRenderRefusesAValueItCannotRead(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"render", "-config", "../../config/example.yaml", "-aftr", "dslite=not-an-address"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("a bad -aftr exits %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "-aftr dslite=not-an-address") {
		t.Errorf("the error does not name the flag that was wrong:\n%s", stderr.String())
	}
}

func TestPairsWantsNameEqualsValue(t *testing.T) {
	values := pairs{}
	if err := values.Set("no-equals-sign"); err == nil {
		t.Error("a flag with no = was accepted")
	}
	if err := values.Set("name=value"); err != nil {
		t.Errorf("name=value was refused: %v", err)
	}
	if values["name"] != "value" {
		t.Errorf("the flag parsed to %v", values)
	}
}
