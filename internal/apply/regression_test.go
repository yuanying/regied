package apply

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// hostFixtureNoDNS is the same host without the address handout, so that adding one is a
// change that has nothing to do with the session.
const hostFixtureNoDNS = `  global:
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
`

// A-1. A change that has nothing to do with a session must not restart it. ADR 0005
// rests on this: a session restart is the one thing a rollback cannot undo, so it is
// reserved for a session whose own configuration moved.
func TestAnUnrelatedUnitDoesNotRestartTheLine(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixtureNoDNS))
	delete(runner.fail, "nft list table inet regied")

	// Declaring an address handout writes a dnsmasq unit. Nothing pppd reads changes.
	plan := mustPlan(t, engine, load(t, hostFixture))

	if plan.Empty() {
		t.Fatal("adding an address handout changes nothing")
	}
	for _, step := range plan.Steps {
		if strings.Contains(step.Command.String(), "regied-pppoe@") {
			t.Errorf("declaring an address handout touches the line: %v", step.Command)
		}
	}
}

// A-1, the other half: a session whose own configuration changed is restarted, and a
// change to the template unit reaches every session because every session runs from it.
func TestASessionIsRestartedWhenItsOwnConfigurationChanges(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixture))
	delete(runner.fail, "nft list table inet regied")

	changed := load(t, strings.Replace(hostFixture, "passwordFile: /etc/regied/secrets/pppoe-password",
		"passwordFile: /etc/regied/secrets/pppoe-password\n        mtu: 1454", 1))
	plan := mustPlan(t, engine, changed)

	if !slices.Contains(stepCommands(plan), "systemctl restart regied-pppoe@pppoe0.service") {
		t.Errorf("a session whose peer file changed is not restarted:\n%s", plan.Summary())
	}

	// And the template every session runs from.
	files.put("/etc/systemd/system/regied-pppoe@.service", ownershipMarker+"\nstale\n", 0o644)
	plan = mustPlan(t, engine, load(t, hostFixture))
	if !slices.Contains(stepCommands(plan), "systemctl restart regied-pppoe@pppoe0.service") {
		t.Errorf("a changed PPPoE template does not reach the sessions running from it:\n%s", plan.Summary())
	}
}

// A-2. A unit file may not be taken away while something still has to be stopped through
// it: systemctl cannot stop an instance whose template is gone, so the stop would fail
// and the whole apply would roll back.
func TestAUnitIsReclaimedOnlyAfterWhatRunsFromItIsStopped(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixture))
	delete(runner.fail, "nft list table inet regied")

	template := "/etc/systemd/system/regied-pppoe@.service"
	unit := "/etc/systemd/system/regied-dnsmasq.service"
	var stopped, unitsAtStop []string
	runner.onRun = func(cmd Command) {
		if !strings.Contains(cmd.String(), "disable --now") {
			return
		}
		stopped = append(stopped, cmd.String())
		for _, path := range []string{template, unit} {
			if _, ok := files.content(path); ok {
				unitsAtStop = append(unitsAtStop, path)
			}
		}
	}

	// Everything that runs from a unit goes away at once: the last session and the
	// address handout.
	bare := load(t, `  resources:
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses: [192.168.10.1/24]
`)
	if _, err := engine.Apply(context.Background(), bare); err != nil {
		t.Fatalf("taking the last session and the handout away failed: %v", err)
	}

	if len(stopped) != 2 {
		t.Errorf("expected the session and dnsmasq to be stopped, got %v", stopped)
	}
	if !slices.Contains(unitsAtStop, template) || !slices.Contains(unitsAtStop, unit) {
		t.Errorf("a unit was already gone when systemctl was asked to stop what runs from it: %v", unitsAtStop)
	}
	for _, path := range []string{template, unit} {
		if _, ok := files.content(path); ok {
			t.Errorf("%s was not reclaimed once nothing ran from it", path)
		}
	}
	// systemd has to be told the units went away.
	if last := runner.commands()[len(runner.commands())-1]; last != "systemctl daemon-reload" {
		t.Errorf("the last thing done is %q, want a daemon-reload after the units were reclaimed", last)
	}
}

// B-2. Reading a link a few milliseconds after its session was restarted answers that it
// is not there. Rendering that and installing it would take the hairpin rules away and
// record the result as what is in effect.
func TestSettlingNeverTakesTheHairpinRulesAway(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	links := host.Links.(fakeLinks)
	links["pppoe0"] = addrs(t, "192.0.2.10")
	cfg := load(t, hostFixture+forwardResource)

	mustApply(t, engine, cfg)
	delete(runner.fail, "nft list table inet regied")
	recorded, _ := files.content("/var/lib/regied/applied/ruleset.nft")
	if !strings.Contains(recorded, "192.0.2.10") {
		t.Fatalf("the first apply did not install the hairpin rules:\n%s", recorded)
	}

	// A change to the session restarts it, and the link is gone while it redials.
	changed := load(t, strings.Replace(hostFixture, "passwordFile: /etc/regied/secrets/pppoe-password",
		"passwordFile: /etc/regied/secrets/pppoe-password\n        mtu: 1454", 1)+forwardResource)
	runner.onRun = func(cmd Command) {
		if strings.Contains(cmd.String(), "restart regied-pppoe@pppoe0") {
			delete(links, "pppoe0")
		}
	}

	result := mustApply(t, engine, changed)

	if result.FirewallReapplied {
		t.Error("a ruleset rendered while the line was down was installed over the one that had the address")
	}
	after, _ := files.content("/var/lib/regied/applied/ruleset.nft")
	if !strings.Contains(after, "192.0.2.10") {
		t.Errorf("the recorded ruleset lost the uplink address:\n%s", after)
	}
	if len(result.Notes) == 0 {
		t.Error("nothing says the ruleset could not be settled")
	}
}

// C-1. The plan is what dry-run and diagnostics walk. A credential must not be in it, so
// that printing one is impossible rather than merely avoided.
func TestNoCredentialIsAnywhereInThePlan(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)
	delete(runner.fail, "nft list table inet regied")

	// Rotate the credential: the file changes, so its content and its previous content
	// are both in play.
	files.put("/etc/regied/secrets/pppoe-password", "correct-horse\n", 0o600)
	plan := mustPlan(t, engine, cfg)

	for _, change := range plan.Files {
		for _, secret := range []string{"hunter2", "correct-horse", "account@example.net"} {
			if strings.Contains(change.Content, secret) || strings.Contains(change.Before, secret) {
				t.Errorf("%s carries the credential %q", change.Path, secret)
			}
		}
	}
	if strings.Contains(plan.Summary(), "hunter2") {
		t.Error("the summary carries a credential")
	}

	// It still has to be written, and correctly.
	if _, err := engine.ApplyPlan(context.Background(), cfg, plan); err != nil {
		t.Fatalf("applying failed: %v", err)
	}
	written, _ := files.content("/etc/regied/ppp/credentials/pppoe0.conf")
	if !strings.Contains(written, "correct-horse") {
		t.Errorf("the rotated credential was not written:\n%s", written)
	}
	// And rotating one restarts the session, because that is what makes pppd read it.
	if !slices.Contains(runner.commands(), "systemctl restart regied-pppoe@pppoe0.service") {
		t.Error("rotating a credential does not restart the session that uses it")
	}
}

// C-3. A staging failure leaves files half written, and what could not be put back is
// the first thing an operator needs.
func TestAStagingFailureNamesWhatCouldNotBePutBack(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixture))
	delete(runner.fail, "nft list table inet regied")

	// The disk stops taking this file. The apply cannot replace it, and cannot put back
	// what it half wrote either.
	conf := "/etc/regied/dnsmasq/dnsmasq.conf"
	files.writeErr[conf] = errFake

	changed := load(t, strings.Replace(hostFixture, "192.168.10.127", "192.168.10.200", 1))
	_, err := engine.Apply(context.Background(), changed)

	requireErrorContaining(t, err, conf)
	requireErrorContaining(t, err, "could not be put back")
	requireErrorContaining(t, err, "the rollback also failed")
}

// C-4. Everything is on the host and working; only the note of what was installed could
// not be written. Reporting that as a failed apply tells the operator the opposite of
// what happened.
func TestAFailureAfterTheCommitIsReportedRatherThanCalledAFailedApply(t *testing.T) {
	engine, files, _, _ := planFixture(t)
	files.writeErr["/var/lib/regied/applied/ruleset.nft"] = errFake

	result, err := engine.Apply(context.Background(), load(t, hostFixture))
	if err != nil {
		t.Fatalf("a configuration that was applied is reported as a failure: %v", err)
	}
	if !result.Changed {
		t.Error("the result does not say the host changed")
	}
	if len(result.Notes) == 0 || !strings.Contains(strings.Join(result.Notes, "\n"), "ruleset.nft") {
		t.Errorf("nothing says the ruleset could not be recorded: %v", result.Notes)
	}
}

// 差し戻し 2. The credential a reclaim reads is a credential like any other. Marking a
// path secret is a property of the directory it is in, so that neither the writing path
// nor the reclaiming path can forget it (ADR 0003).
func TestAReclaimedCredentialsFileIsNotInThePlan(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	stale := "/etc/regied/ppp/credentials/gone.conf"
	files.put(stale, ownershipMarker+"\nuser \"gone@example.net\"\npassword \"swordfish\"\n", 0o600)
	files.put("/etc/regied/ppp/peers/gone.conf", ownershipMarker+"\nifname gone\n", 0o644)

	plan := mustPlan(t, engine, load(t, hostFixture))

	change, ok := fileChangeFor(plan, stale)
	if !ok {
		t.Fatal("the stale credentials file is not reclaimed")
	}
	if !change.Secret {
		t.Error("a reclaimed credentials file is not marked secret")
	}
	for _, field := range []string{change.Before, change.Content} {
		if strings.Contains(field, "swordfish") || strings.Contains(field, "gone@example.net") {
			t.Errorf("%s carries the credential it is taking away", stale)
		}
	}
	if strings.Contains(renderReport(plan), "swordfish") {
		t.Error("the report prints the credential of a file it is reclaiming")
	}

	// It still has to come back if the apply is rolled back.
	runner.fail["networkctl reload"] = errFake
	if _, err := engine.ApplyPlan(context.Background(), load(t, hostFixture), plan); err == nil {
		t.Fatal("the failing reload was not reported")
	}
	restored, ok := files.content(stale)
	if !ok || !strings.Contains(restored, "swordfish") {
		t.Errorf("the reclaimed credentials file was not put back: %q", restored)
	}
}

// 差し戻し 2. Narrowing what counts as "a unit that affects this process" must not
// narrow it past a unit that was written back after somebody deleted it: the apply then
// reports success while nothing is running.
func TestAUnitWrittenBackStartsWhatRunsFromIt(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)
	delete(runner.fail, "nft list table inet regied")

	delete(files.files, "/etc/systemd/system/regied-pppoe@.service")
	delete(files.files, "/etc/systemd/system/regied-dnsmasq.service")

	plan := mustPlan(t, engine, cfg)

	commands := stepCommands(plan)
	for _, want := range []string{
		"systemctl enable --now regied-pppoe@pppoe0.service",
		"systemctl enable --now regied-dnsmasq.service",
	} {
		if !contains(commands, want) {
			t.Errorf("a unit written back does not start what runs from it; want %q, got:\n%s",
				want, strings.Join(commands, "\n"))
		}
	}
}

// 差し戻し 2. nft not being installed is not the same answer as the table not being
// there, and reading one as the other makes a dry-run away from the host claim it would
// install a ruleset the host already has (ADR 0006).
func TestNFTBeingAbsentIsNotTheSameAsTheTableBeingAbsent(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)

	// A machine with no nft at all: neither the probe nor the check can be run.
	runner.fail["nft list table inet regied"] = ErrCommandNotFound
	runner.fail["nft --check -f -"] = ErrCommandNotFound
	_ = files

	plan := mustPlan(t, engine, cfg)

	if plan.Firewall.Apply {
		t.Error("a host that already holds this ruleset is told the whole table goes in")
	}
	if !plan.Empty() {
		t.Errorf("nothing changed, but the plan is not empty:\n%s", plan.Summary())
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "nft") {
		t.Errorf("nothing says the table could not be checked for: %v", plan.Notes)
	}
}

// 差し戻し 2. Render says the apply-time values may all be absent. Absent has to include
// the whole of them.
func TestRenderAcceptsNoRuntimeAtAll(t *testing.T) {
	plan, err := New(Host{}, Options{}).Render(load(t, hostFixture), nil)
	if err != nil {
		t.Fatalf("rendering with no runtime values failed: %v", err)
	}
	if len(plan.Files) == 0 {
		t.Error("nothing was rendered")
	}
}
