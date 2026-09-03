package apply

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// hostFixture declares one of each thing that reaches a different backend: a link and a
// bridge for networkd, a PPPoE session for pppd, an address handout for dnsmasq, and a
// kernel switch.
const hostFixture = `  global:
    ipForwarding: true
    logMartians: true
  resources:
    - kind: Interface
      metadata: {name: wan}
      spec: {ifname: eth0}
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        bridge:
          members: [eth1, eth2]
        addresses: [192.168.10.1/24]
    - kind: PPPoESession
      metadata: {name: pppoe0}
      spec:
        interfaceRef: wan
        userIDFile: /etc/regied/secrets/pppoe-user-id
        passwordFile: /etc/regied/secrets/pppoe-password
    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
        subnet: 192.168.10.0/24
        pool:
          start: 192.168.10.64
          end: 192.168.10.127
`

// forwardResource publishes a service through the session. It is the one thing whose
// rendering depends on the address the uplink is holding: the hairpin translation has to
// match on it (ADR 0013).
const forwardResource = `    - kind: PortForward
      metadata: {name: https}
      spec:
        egressRef: pppoe0
        protocol: tcp
        port: 443
        hairpin: true
        target:
          address: 192.168.10.20
          port: 443
`

// planFixture is an engine over a host holding nothing, with the fixture's secrets in
// place. The runner answers `nft list table` with a failure, which is how a host that
// has never been applied to says the table is not there.
func planFixture(t *testing.T) (*Engine, *fakeFiles, *fakeRunner, Host) {
	t.Helper()
	host, files, runner := testHost()
	files.put("/etc/regied/secrets/pppoe-user-id", "account@example.net\n", 0o600)
	files.put("/etc/regied/secrets/pppoe-password", "hunter2\n", 0o600)
	runner.fail["nft list table inet regied"] = errFake
	// A kernel that answers every switch, all of them off. A key this kernel does not
	// have is skipped rather than written, so a fake with nothing in it would leave the
	// kernel phase out of every plan.
	sysctl := host.Sysctl.(*fakeSysctl)
	for _, key := range kernelSwitches(v1alpha1.Global{}) {
		sysctl.values[key.key] = "0"
	}
	return New(host, Options{}), files, runner, host
}

func mustPlan(t *testing.T, engine *Engine, cfg *config.Config) *Plan {
	t.Helper()
	plan, err := engine.Plan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("planning failed: %v", err)
	}
	return plan
}

// mustApply puts a configuration on the fake host, so that the next plan is the second
// one and can be asked whether it would change anything.
func mustApply(t *testing.T, engine *Engine, cfg *config.Config) *Result {
	t.Helper()
	result, err := engine.Apply(context.Background(), cfg)
	if err != nil {
		t.Fatalf("applying failed: %v", err)
	}
	return result
}

func TestPlanOnAnUntouchedHostCreatesEveryFile(t *testing.T) {
	engine, _, _, _ := planFixture(t)
	plan := mustPlan(t, engine, load(t, hostFixture))

	for _, path := range []string{
		"/etc/systemd/network/50-regied-lan.netdev",
		"/etc/systemd/network/50-regied-wan.network",
		"/etc/regied/ppp/peers/pppoe0.conf",
		"/etc/regied/ppp/credentials/pppoe0.conf",
		"/etc/regied/dnsmasq/dnsmasq.conf",
		"/etc/systemd/system/regied-pppoe@.service",
		"/etc/systemd/system/regied-dnsmasq.service",
	} {
		change, ok := fileChangeFor(plan, path)
		if !ok {
			t.Errorf("nothing in the plan writes %s", path)
			continue
		}
		if change.Kind != ChangeCreate {
			t.Errorf("%s is %s, want %s", path, change.Kind, ChangeCreate)
		}
	}
}

func TestPlanKeepsTheCredentialsFileOutOfWhatCanBePrinted(t *testing.T) {
	engine, _, _, _ := planFixture(t)
	plan := mustPlan(t, engine, load(t, hostFixture))

	change, ok := fileChangeFor(plan, "/etc/regied/ppp/credentials/pppoe0.conf")
	if !ok {
		t.Fatal("nothing in the plan writes the credentials file")
	}
	if !change.Secret {
		t.Error("the credentials file is not marked secret, so nothing stops it being printed")
	}
	if change.Mode != 0o600 {
		t.Errorf("the credentials file would be written %o, want 600", change.Mode)
	}
	// The peer file is the half that is safe to print, and it must stay that way.
	peer, _ := fileChangeFor(plan, "/etc/regied/ppp/peers/pppoe0.conf")
	if peer.Secret {
		t.Error("the peer file is marked secret, which would blank the useful half of a dry-run")
	}
	if strings.Contains(peer.Content, "hunter2") || strings.Contains(peer.Content, "account@example.net") {
		t.Error("the peer file carries a credential")
	}
}

func TestPlanRunsThePhasesInOrder(t *testing.T) {
	engine, _, _, _ := planFixture(t)
	plan := mustPlan(t, engine, load(t, hostFixture))

	var phases []Phase
	for _, step := range plan.Steps {
		if len(phases) == 0 || phases[len(phases)-1] != step.Phase {
			phases = append(phases, step.Phase)
		}
	}
	want := []Phase{PhaseFirewall, PhaseKernel, PhaseNetworkd, PhaseProcessConfig, PhaseProcesses}
	if !slices.Equal(phases, want) {
		t.Errorf("the phases run as %v, want %v", phases, want)
	}
	if len(plan.Steps) == 0 || plan.Steps[0].Command.Name != "nft" {
		t.Error("the first thing an apply does is not the firewall")
	}
}

func TestPlanSetsTheKernelSwitchesTheDocumentAsksFor(t *testing.T) {
	engine, _, _, host := planFixture(t)
	sysctl := host.Sysctl.(*fakeSysctl)
	sysctl.values["net.ipv4.ip_forward"] = "0"
	sysctl.values["net.ipv4.conf.all.log_martians"] = "1"

	plan := mustPlan(t, engine, load(t, hostFixture))

	forwarding, ok := switchFor(plan, "net.ipv4.ip_forward")
	if !ok || !forwarding.Changed || forwarding.Value != "1" {
		t.Errorf("ipForwarding: true does not turn net.ipv4.ip_forward on: %+v", forwarding)
	}
	if _, ok := switchFor(plan, "net.ipv6.conf.all.forwarding"); !ok {
		t.Error("ipForwarding covers IPv4 only")
	}
	// A switch already holding the value the document asks for is not written, so an
	// apply that changes nothing runs nothing.
	martians, ok := switchFor(plan, "net.ipv4.conf.all.log_martians")
	if !ok {
		t.Fatal("log_martians is not in the plan at all")
	}
	if martians.Changed {
		t.Error("a switch already holding the value asked for would be written again")
	}
	for _, step := range plan.Steps {
		if step.Kind == StepSysctl && step.Switch.Key == "net.ipv4.conf.all.log_martians" {
			t.Error("a switch that does not change has a step")
		}
	}
}

func TestPlanIsEmptyTheSecondTime(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)

	mustApply(t, engine, cfg)
	// The table is in the kernel now, so the probe that says it is missing must stop
	// saying so.
	delete(runner.fail, "nft list table inet regied")

	second := mustPlan(t, engine, cfg)
	if !second.Empty() {
		t.Errorf("applying the same configuration twice is not a no-op:\n%s", second.Summary())
	}
	if len(second.Steps) != 0 {
		t.Errorf("a plan that changes nothing still runs %d commands", len(second.Steps))
	}
}

func TestPlanReappliesARulesetTheKernelHasLost(t *testing.T) {
	engine, _, _, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)

	// Everything is still on disk, but the table is gone: a reboot, or somebody's
	// flush. The rendering has not changed, so only the missing table can call for it.
	plan := mustPlan(t, engine, cfg)
	if plan.Empty() {
		t.Fatal("a ruleset the kernel has lost is not put back")
	}
	if plan.Steps[0].Command.Name != "nft" {
		t.Errorf("the plan does something other than reinstall the table: %v", plan.Steps[0].Command)
	}
}

func TestPlanReclaimsWhatAnEarlierApplyLeftAndNothingElse(t *testing.T) {
	engine, files, _, _ := planFixture(t)

	// Ours, from a configuration that no longer declares it.
	files.put("/etc/systemd/network/50-regied-gone.network", "[Match]\nName=eth9\n", 0o644)
	files.put("/etc/regied/ppp/peers/gone.conf", ownershipMarker+"\nifname gone\n", 0o644)
	files.put("/etc/systemd/system/regied-pppoe@.service", ownershipMarker+"\nstale\n", 0o644)
	// Somebody else's, in the same directories.
	files.put("/etc/systemd/network/80-distribution.network", "[Match]\nName=eth9\n", 0o644)
	files.put("/etc/regied/ppp/peers/handwritten.conf", "# mine\n", 0o644)
	files.put("/etc/systemd/system/other.service", "[Unit]\n", 0o644)

	plan := mustPlan(t, engine, load(t, hostFixture))

	for _, path := range []string{
		"/etc/systemd/network/50-regied-gone.network",
		"/etc/regied/ppp/peers/gone.conf",
	} {
		change, ok := fileChangeFor(plan, path)
		if !ok || change.Kind != ChangeRemove {
			t.Errorf("%s is not reclaimed", path)
		}
	}
	for _, path := range []string{
		"/etc/systemd/network/80-distribution.network",
		"/etc/regied/ppp/peers/handwritten.conf",
		"/etc/systemd/system/other.service",
	} {
		if _, ok := fileChangeFor(plan, path); ok {
			t.Errorf("%s is somebody else's and the plan touches it", path)
		}
	}
	// A file of ours that the configuration still declares is rewritten, not reclaimed.
	if change, ok := fileChangeFor(plan, "/etc/systemd/system/regied-pppoe@.service"); !ok || change.Kind != ChangeUpdate {
		t.Errorf("the unit an earlier apply wrote is not updated: %+v", change)
	}
}

func TestPlanStartsANewSessionAndStopsOneThatWentAway(t *testing.T) {
	engine, files, _, _ := planFixture(t)
	files.put("/etc/regied/ppp/peers/gone.conf", ownershipMarker+"\nifname gone\n", 0o644)
	files.put("/etc/regied/ppp/credentials/gone.conf", ownershipMarker+"\nuser \"x\"\n", 0o600)

	plan := mustPlan(t, engine, load(t, hostFixture))

	commands := stepCommands(plan)
	if !slices.Contains(commands, "systemctl enable --now regied-pppoe@pppoe0.service") {
		t.Errorf("a session that is new is not started:\n%s", strings.Join(commands, "\n"))
	}
	if !slices.Contains(commands, "systemctl disable --now regied-pppoe@gone.service") {
		t.Errorf("a session that went away is not stopped:\n%s", strings.Join(commands, "\n"))
	}
}

func TestPlanLeavesTheLineAloneWhenOnlyTheFirewallChanged(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixture))
	delete(runner.fail, "nft list table inet regied")

	// The same host, with one address set added. Nothing pppd or dnsmasq reads changes.
	changed := load(t, hostFixture+forwardResource)
	plan := mustPlan(t, engine, changed)

	for _, step := range plan.Steps {
		if strings.Contains(step.Command.String(), "regied-pppoe@") {
			t.Errorf("a firewall-only change restarts the line: %v", step.Command)
		}
	}
	if plan.Empty() {
		t.Fatal("the changed ruleset is not applied at all")
	}
}

func TestEveryStepKnowsHowToPutTheHostBack(t *testing.T) {
	engine, _, _, _ := planFixture(t)
	plan := mustPlan(t, engine, load(t, hostFixture))

	for _, step := range plan.Steps {
		if step.Undo == nil {
			t.Errorf("the step %q has no way back, so a later failure could not roll it back", step.describe())
		}
	}
}

// --- helpers -----------------------------------------------------------------------

func fileChangeFor(plan *Plan, path string) (FileChange, bool) {
	for _, change := range plan.Files {
		if change.Path == path {
			return change, true
		}
	}
	return FileChange{}, false
}

func switchFor(plan *Plan, key string) (SwitchChange, bool) {
	for _, change := range plan.Switches {
		if change.Key == key {
			return change, true
		}
	}
	return SwitchChange{}, false
}

func stepCommands(plan *Plan) []string {
	var out []string
	for _, step := range plan.Steps {
		if step.Kind == StepCommand {
			out = append(out, step.Command.String())
		}
	}
	return out
}
