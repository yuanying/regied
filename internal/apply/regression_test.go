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
	tablePresent(runner)

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
	tablePresent(runner)

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
	tablePresent(runner)

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
// is not there. Rendering that used to be able to take the hairpin rules away; nothing in
// the ruleset depends on the answer any more, so a redial in the middle of an apply
// cannot (ADR 0015).
func TestARedialDuringAnApplyKeepsTheHairpinRules(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	links := host.Links.(fakeLinks)
	links["pppoe0"] = addrs(t, "192.0.2.10")
	cfg := load(t, hostFixture+forwardResource)

	mustApply(t, engine, cfg)
	tablePresent(runner)
	recorded, _ := files.content("/var/lib/regied/applied/ruleset.nft")
	if !strings.Contains(recorded, "@uplink4_pppoe0") {
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
	before := len(runner.ran)

	mustApply(t, engine, changed)

	after, _ := files.content("/var/lib/regied/applied/ruleset.nft")
	if !strings.Contains(after, "@uplink4_pppoe0") {
		t.Errorf("the recorded ruleset lost the hairpin rules:\n%s", after)
	}
	// The ruleset did not change, so the table was left alone and its sets kept what the
	// first apply seeded. Replacing it here would have emptied them behind a link that
	// cannot be read.
	for _, cmd := range runner.ran[before:] {
		if cmd.Name == "nft" && strings.Contains(cmd.Stdin, "table inet regied {") {
			t.Error("the table was replaced by an apply that did not change the ruleset")
		}
	}
}

// C-1. The plan is what dry-run and diagnostics walk. A credential must not be in it, so
// that printing one is impossible rather than merely avoided.
func TestNoCredentialIsAnywhereInThePlan(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)
	tablePresent(runner)

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
	if _, err := engine.ApplyPlan(context.Background(), plan); err != nil {
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
	tablePresent(runner)

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

// Round 2. The credential a reclaim reads is a credential like any other. Marking a
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
	if _, err := engine.ApplyPlan(context.Background(), plan); err == nil {
		t.Fatal("the failing reload was not reported")
	}
	restored, ok := files.content(stale)
	if !ok || !strings.Contains(restored, "swordfish") {
		t.Errorf("the reclaimed credentials file was not put back: %q", restored)
	}
}

// Round 2. Narrowing what counts as "a unit that affects this process" must not
// narrow it past a unit that was written back after somebody deleted it: the apply then
// reports success while nothing is running.
func TestAUnitWrittenBackStartsWhatRunsFromIt(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)
	tablePresent(runner)

	delete(files.files, "/etc/systemd/system/regied-pppoe@.service")
	delete(files.files, "/etc/systemd/system/regied-dnsmasq.service")

	plan := mustPlan(t, engine, cfg)

	// Enabled, and restarted rather than started: what runs from the unit may still be
	// running from systemd's copy of the old one (round 3).
	commands := stepCommands(plan)
	for _, want := range []string{
		"systemctl enable regied-pppoe@pppoe0.service",
		"systemctl restart regied-pppoe@pppoe0.service",
		"systemctl enable regied-dnsmasq.service",
		"systemctl restart regied-dnsmasq.service",
	} {
		if !contains(commands, want) {
			t.Errorf("a unit written back does not start what runs from it; want %q, got:\n%s",
				want, strings.Join(commands, "\n"))
		}
	}
}

// Round 2. nft not being installed is not the same answer as the table not being
// there, and reading one as the other makes a dry-run away from the host claim it would
// install a ruleset the host already has (ADR 0006).
func TestNFTBeingAbsentIsNotTheSameAsTheTableBeingAbsent(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)

	// A machine with no nft at all: neither the probe nor the check can be run.
	runner.fail["nft list tables"] = ErrCommandNotFound
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

// Round 2. Render says the apply-time values may all be absent. Absent has to include
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

// Round 3. A probe that could not be asked is not an answer, and "the table is not
// there" is the one answer that lets a rollback delete the table. Reading a failure as
// that answer takes the firewall off a host over a question nobody could answer
// (ADR 0005).
func TestAProbeThatCouldNotBeAskedNeverLetsARollbackDeleteTheTable(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixture))

	// The record is gone, and the kernel cannot be asked — not because nft is missing,
	// but because asking failed: a netlink error, a capability this process lacks.
	delete(files.files, "/var/lib/regied/applied/ruleset.nft")
	runner.fail["nft list tables"] = errFake

	cfg := load(t, hostFixture+forwardResource)
	plan := mustPlan(t, engine, cfg)
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "could not be asked") {
		t.Errorf("nothing says the kernel could not be asked about the table: %v", plan.Notes)
	}

	// The install fails, so the firewall step is the one undone.
	runner.fail["nft -f -"] = errFake
	if _, err := engine.Apply(context.Background(), cfg); err == nil {
		t.Fatal("the failing install was not reported")
	}
	// A ruleset opens by deleting the table it then declares, which is the atomic
	// replacement (ADR 0013); what the undo must not hand nft is a deletion alone.
	for _, cmd := range runner.ran {
		if cmd.Stdin == "table inet regied\ndelete table inet regied\n" {
			t.Fatal("the rollback deleted the table over a probe that had no answer")
		}
	}
}

// hostFixtureLinksOnly is the same host with neither the session nor the address
// handout, so that both of what ran are to be stopped.
const hostFixtureLinksOnly = `  global:
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
`

// Round 3. A unit written back is written back to a process that may still be running
// from systemd's copy of the old one, and with a configuration that may have changed in
// the same apply. Starting what is already running does nothing; the process has to be
// restarted, the way it is for any other file it reads.
func TestAUnitWrittenBackRestartsWhatReadsAFileThatChanged(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixture))
	tablePresent(runner)

	delete(files.files, "/etc/systemd/system/regied-pppoe@.service")
	delete(files.files, "/etc/systemd/system/regied-dnsmasq.service")
	changed := strings.Replace(hostFixture, "end: 192.168.10.127", "end: 192.168.10.200", 1)
	changed = strings.Replace(changed, "interfaceRef: wan\n", "interfaceRef: wan\n        mtu: 1454\n", 1)

	plan := mustPlan(t, engine, load(t, changed))

	commands := stepCommands(plan)
	for _, unit := range []string{"regied-pppoe@pppoe0.service", "regied-dnsmasq.service"} {
		enable := slices.Index(commands, "systemctl enable "+unit)
		restart := slices.Index(commands, "systemctl restart "+unit)
		if enable < 0 || restart < 0 || restart < enable {
			t.Errorf("%s is not enabled and then restarted; got:\n%s", unit, strings.Join(commands, "\n"))
		}
		if contains(commands, "systemctl enable --now "+unit) {
			t.Errorf("%s is started as if nothing ran from it, so a changed file is never read", unit)
		}
	}
}

// Round 3. What says a process is to be stopped is that the configuration no longer
// declares it, not that one particular file of its is being reclaimed. A file somebody
// deleted by hand must not leave the process running with no unit behind it.
func TestAProcessIsStoppedWhenOnlyItsUnitIsLeft(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixture))
	tablePresent(runner)

	delete(files.files, "/etc/regied/dnsmasq/dnsmasq.conf")
	delete(files.files, "/etc/regied/ppp/peers/pppoe0.conf")

	plan := mustPlan(t, engine, load(t, hostFixtureLinksOnly))

	var described []string
	for _, step := range plan.Steps {
		described = append(described, step.describe())
	}
	for unit, path := range map[string]string{
		"regied-pppoe@pppoe0.service": "/etc/systemd/system/regied-pppoe@.service",
		"regied-dnsmasq.service":      "/etc/systemd/system/regied-dnsmasq.service",
	} {
		stop := slices.Index(described, "systemctl disable --now "+unit)
		reclaim := slices.Index(described, "reclaim "+path)
		if stop < 0 {
			t.Errorf("%s is not stopped; got:\n%s", unit, strings.Join(described, "\n"))
			continue
		}
		if reclaim >= 0 && reclaim < stop {
			t.Errorf("%s is reclaimed before what runs from it is stopped", path)
		}
	}
}

// Round 3. Only an uplink's address is anything the apply has to put anywhere. Reading
// every link makes every pre-dial apply say a LAN link is missing an address it never
// needed.
func TestOnlyTheUplinksAreAskedForTheirAddresses(t *testing.T) {
	engine, _, _, _ := planFixture(t)

	plan := mustPlan(t, engine, load(t, hostFixture+forwardResource))

	notes := strings.Join(plan.Notes, "\n")
	for _, ifname := range []string{"eth0", "br-lan"} {
		if strings.Contains(notes, ifname) {
			t.Errorf("a link that is not an uplink is reported as missing an address: %v", plan.Notes)
		}
	}
	if !strings.Contains(notes, "pppoe0") {
		t.Errorf("the uplink that has not dialled is not named: %v", plan.Notes)
	}
}

// Round 3, second half. A LAN link's address is nothing the ruleset depends on, so it is
// neither read nor seeded into any set.
func TestOnlyTheUplinksAreSeeded(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	links := host.Links.(fakeLinks)
	links["br-lan"] = addrs(t, "192.168.10.1")
	links["pppoe0"] = addrs(t, "192.0.2.10")

	mustApply(t, engine, load(t, hostFixture+forwardResource))

	if !ranNFTWith(runner.ran, "add element inet regied uplink4_pppoe0 { 192.0.2.10 }") {
		t.Errorf("the uplink's address was not seeded:\n%s", strings.Join(runner.commands(), "\n"))
	}
	for _, cmd := range runner.ran {
		if cmd.Name == "nft" && strings.Contains(cmd.Stdin, "add element") && strings.Contains(cmd.Stdin, "192.168.10.1 ") {
			t.Errorf("a LAN address was put into a set:\n%s", cmd.Stdin)
		}
	}
}

// Round 3. Reverse-path filtering is the larger of `all` and the interface's own value,
// so turning it off on `all` and `default` leaves it on for every link that already
// existed with it on. The links the configuration names are set too; one that is not
// there yet inherits `default` when it appears.
func TestSourceValidationReachesTheLinksAlreadyThere(t *testing.T) {
	engine, _, _, host := planFixture(t)
	sysctl := host.Sysctl.(*fakeSysctl)
	sysctl.values["net.ipv4.conf.eth0.rp_filter"] = "1"
	sysctl.values["net.ipv4.conf.br-lan.rp_filter"] = "2"

	plan := mustPlan(t, engine, load(t, hostFixture))

	for _, key := range []string{"net.ipv4.conf.eth0.rp_filter", "net.ipv4.conf.br-lan.rp_filter"} {
		change, ok := switchFor(plan, key)
		if !ok {
			t.Errorf("%s is not set although the link is there", key)
			continue
		}
		if change.Value != "0" || !change.Changed {
			t.Errorf("%s = %q (changed %v), want 0 and changed", key, change.Value, change.Changed)
		}
	}
	if _, ok := switchFor(plan, "net.ipv4.conf.pppoe0.rp_filter"); ok {
		t.Error("a link that is not on the host yet is given a switch that cannot be written")
	}
}

// Round 3. The rules a directory carries are decided by comparing paths, and a path
// with a slash too many is not the same string. Options are cleaned so that where a
// caller puts a slash cannot decide whether a credential is hidden.
func TestOptionsWithATrailingSlashKeepTheirRules(t *testing.T) {
	_, _, _, host := planFixture(t)
	engine := New(host, Options{Root: "/etc/regied/", UnitDir: "/etc/systemd/system/"})

	plan := mustPlan(t, engine, load(t, hostFixture))

	credentials, ok := fileChangeFor(plan, "/etc/regied/ppp/credentials/pppoe0.conf")
	if !ok {
		t.Fatalf("the credentials file is not at its path; files:\n%s", plan.Summary())
	}
	if !credentials.Secret || credentials.Content != "" {
		t.Error("a trailing slash on Root put the credential into the plan")
	}
	template, ok := fileChangeFor(plan, "/etc/systemd/system/regied-pppoe@.service")
	if !ok {
		t.Fatalf("the template unit is not at its path; files:\n%s", plan.Summary())
	}
	if !template.Deferred {
		t.Error("a trailing slash on UnitDir made the unit an ordinary file")
	}
}
