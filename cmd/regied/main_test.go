package main

import (
	"bytes"
	"os"
	"path/filepath"
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

// 差し戻し 2. An uplink may hold an address in each family, and the engine takes them
// all. A flag that keeps only the last one cannot say so.
func TestUplinkAddressesAccumulate(t *testing.T) {
	values := multiPairs{}
	for _, argument := range []string{"pppoe0=192.0.2.10", "pppoe0=2001:db8::1", "dslite=192.0.2.20"} {
		if err := values.Set(argument); err != nil {
			t.Fatalf("%s was refused: %v", argument, err)
		}
	}
	if got := values["pppoe0"]; len(got) != 2 {
		t.Errorf("the uplink holds %v, want both addresses", got)
	}
	if got := values["dslite"]; len(got) != 1 {
		t.Errorf("the other uplink holds %v", got)
	}
}

// 差し戻し 2. A configuration warning is about the configuration, not about the host, so
// it belongs in the one command that is only about the configuration.
func TestRenderReportsTheConfigurationsOwnWarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	// Prefix delegation with no DUID file: networkd sends one of its own, and a host
	// replacing one that already holds a delegation silently changes its prefix.
	if err := os.WriteFile(path, []byte(`apiVersion: net.unstable.cloud/v1alpha1
kind: NetworkConfig
metadata:
  name: warns
spec:
  resources:
    - kind: Interface
      metadata: {name: wan}
      spec:
        ifname: eth0
        dhcpv6:
          prefixDelegation:
            prefixLength: 56
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", "-config", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("rendering exits %d:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "duidFile") {
		t.Errorf("render says nothing about what validation warned of:\n%s", stderr.String())
	}
}
