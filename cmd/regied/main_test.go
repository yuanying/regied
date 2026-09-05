package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/apply"
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

// The ruleset holds no uplink address, so a rendering needs none supplied: the hairpin
// translation names the uplink's set and is there whatever the line is doing (ADR 0015).
func TestRenderNeedsNoUplinkAddress(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"render",
		"-config", "../../config/example.yaml",
		"-aftr", "dslite=2001:db8:53::1",
		"-duid", "/etc/regied/secrets/dhcpv6-duid=00:03:00:01:00:00:5e:00:53:01",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("rendering the example exits %d:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ip daddr @uplink4_pppoe0") {
		t.Error("the rendering has no hairpin translation, so it is not complete without the host")
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

// Round 3. Asking for help is not an error, whichever command is asked.
func TestHelpIsNotAnError(t *testing.T) {
	for _, command := range []string{"apply", "render"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{command, "-h"}, &stdout, &stderr); code != 0 {
			t.Errorf("regied %s -h exits %d, want 0", command, code)
		}
		if !strings.Contains(stderr.String(), "-config") {
			t.Errorf("regied %s -h does not print the flags:\n%s", command, stderr.String())
		}
	}
}

// ADR 0017. Five verbs: every host-writing turn belongs to serve.
func TestUsageNamesTheFiveVerbs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"help"}, &stdout, &stderr)
	for _, verb := range []string{"regied render", "regied apply", "regied serve", "regied confirm", "regied cancel"} {
		if !strings.Contains(stdout.String(), verb) {
			t.Errorf("the usage does not name %q:\n%s", verb, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "regied reconcile") {
		t.Errorf("the removed reconcile verb remains in usage:\n%s", stdout.String())
	}
}

func TestServeTakesNoConfigurationFileAndHasAResyncFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"serve", "-config", "/etc/regied/config.yaml"}, &stdout, &stderr); code != 2 {
		t.Errorf("regied serve -config exits %d, want 2 (usage)", code)
	}
	stderr.Reset()
	if code := run([]string{"serve", "-h"}, &stdout, &stderr); code != 0 {
		t.Errorf("regied serve -h exits %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "-resync") || strings.Contains(stderr.String(), "-config") {
		t.Errorf("serve flags are wrong:\n%s", stderr.String())
	}
}

// reconcile takes no configuration file. That is the whole point of it: the one way to
// ask for a turn that reads nothing but the record (ADR 0016).
func TestReconcileIsNotACommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"reconcile"}, &stdout, &stderr); code != 2 {
		t.Errorf("regied reconcile exits %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `there is no "reconcile" command`) {
		t.Errorf("the removed verb was not rejected:\n%s", stderr.String())
	}
}

func TestApplyWithoutDaemonSaysToStartTheService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("not even parsed by the client"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"apply", "-config", path, "-control", filepath.Join(t.TempDir(), "missing.sock")}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "start regied.service") {
		t.Fatalf("apply without daemon exits %d and says:\n%s", code, stderr.String())
	}
}

func TestApplySendsBytesAndPrintsTheDaemonsReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("bytes validated only by the daemon")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "control.sock")
	requests, closeControl, err := (apply.OSControl{}).Listen(t.Context(), socket, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	go func() {
		request := <-requests
		if request.Verb != apply.ControlSubmit || !bytes.Equal(request.Declaration, data) || request.Source != path {
			request.Reply(apply.ControlResponse{Error: "wrong submission"})
			return
		}
		request.Reply(apply.ControlResponse{Revision: "sha256:test", State: apply.StateWaiting, Report: &apply.TurnReport{
			Revision: "sha256:test", State: apply.StateWaiting, Outcome: apply.OutcomeUnchanged,
			Waiting: []string{"an address"}, Notes: []string{"kept running"},
		}})
	}()
	var stdout, stderr bytes.Buffer
	code := run([]string{"apply", "-config", path, "-control", socket}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("apply exits %d: %s", code, stderr.String())
	}
	for _, want := range []string{"sha256:test", "waiting", "an address", "kept running"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, stdout.String())
		}
	}
}

// The state a turn ended in is what the console says and what the exit status follows:
// failing is non-zero, waiting and converged are zero (ADR 0016).
func TestReportResultSaysTheStateAndWhatIsWaitedFor(t *testing.T) {
	var stdout bytes.Buffer
	reportResult(&stdout, &apply.Result{
		Plan:     &apply.Plan{Waiting: []string{"DSLiteTunnel/dslite: waiting for the AFTR aftr.example.net to resolve to an IPv6 address"}},
		Changed:  false,
		State:    apply.StateWaiting,
		Revision: "sha256:0000",
	})
	out := stdout.String()
	for _, want := range []string{"waiting", "aftr.example.net", "sha256:0000"} {
		if !strings.Contains(out, want) {
			t.Errorf("the result does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "already holds this configuration") {
		t.Errorf("a host that waits is told it holds the whole configuration:\n%s", out)
	}

	stdout.Reset()
	reportResult(&stdout, &apply.Result{Plan: &apply.Plan{}, State: apply.StateConverged, Revision: "sha256:0000"})
	if !strings.Contains(stdout.String(), "converged") {
		t.Errorf("the result does not say the host converged:\n%s", stdout.String())
	}
}
