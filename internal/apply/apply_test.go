package apply

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestApplyPutsTheConfigurationOnTheHost(t *testing.T) {
	engine, files, runner, _ := planFixture(t)

	result := mustApply(t, engine, load(t, hostFixture))

	if _, ok := files.content("/etc/regied/dnsmasq/dnsmasq.conf"); !ok {
		t.Error("the dnsmasq configuration was not written")
	}
	peer, ok := files.content("/etc/regied/ppp/peers/pppoe0.conf")
	if !ok || !strings.HasPrefix(peer, ownershipMarker) {
		t.Error("the peer file was not written, or does not carry the ownership marker")
	}
	// The ruleset the apply installed is remembered, because it is the one thing regied
	// owns that is kernel state and has no file (ADR 0004).
	recorded, ok := files.content("/var/lib/regied/applied/ruleset.nft")
	if !ok || !strings.Contains(recorded, "table inet regied") {
		t.Error("the ruleset that was applied is not recorded")
	}
	if !slices.Contains(runner.commands(), "nft -f -") {
		t.Errorf("the ruleset never reached nft:\n%s", strings.Join(runner.commands(), "\n"))
	}
	if !result.Changed {
		t.Error("an apply on an untouched host reports that it changed nothing")
	}
}

func TestApplyProtectsTheCredentialsDirectoryAndNothingAboveIt(t *testing.T) {
	engine, files, _, _ := planFixture(t)

	mustApply(t, engine, load(t, hostFixture))

	if !slices.Contains(files.dirs, "/etc/regied/ppp/credentials 0700") {
		t.Errorf("the credentials directory is not created 0700: %v", files.dirs)
	}
	// The directory above it holds the peer files, which are meant to be readable.
	for _, dir := range files.dirs {
		if strings.HasPrefix(dir, "/etc/regied/ppp ") && !strings.HasSuffix(dir, "0755") {
			t.Errorf("the directory above the credentials is created %q", dir)
		}
	}
}

func TestApplyRollsBackAFailedCommand(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	sysctl := host.Sysctl.(*fakeSysctl)
	sysctl.values["net.ipv4.ip_forward"] = "0"
	cfg := load(t, hostFixture)

	// Starting the session is where it fails: the firewall, the switches and the reload
	// have all been done, and every file is written.
	runner.fail["systemctl enable --now regied-pppoe@pppoe0.service"] = errFake

	_, err := engine.Apply(context.Background(), cfg)
	requireErrorContaining(t, err, "regied-pppoe@pppoe0.service")
	requireErrorContaining(t, err, "rolled back")

	if _, ok := files.content("/etc/systemd/network/50-regied-wan.network"); ok {
		t.Error("a file this apply created is still there after the rollback")
	}
	if got := sysctl.values["net.ipv4.ip_forward"]; got != "0" {
		t.Errorf("net.ipv4.ip_forward was left at %q, want the 0 it held before", got)
	}
	// The table was installed and has to come back off, because before this apply there
	// was none.
	if !ranNFTWith(runner.ran, "delete table inet regied") {
		t.Errorf("the rollback did not take the table back off:\n%s", strings.Join(runner.commands(), "\n"))
	}
	if _, ok := files.content("/var/lib/regied/applied/ruleset.nft"); ok {
		t.Error("a rolled-back apply recorded its ruleset as applied")
	}
}

func TestApplyRollsBackToThePreviousGenerationRatherThanToNothing(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixture))
	tablePresent(runner)

	before, _ := files.content("/etc/regied/dnsmasq/dnsmasq.conf")
	beforeRuleset, _ := files.content("/var/lib/regied/applied/ruleset.nft")

	// A second apply that changes the handout, and fails while reloading networkd.
	changed := load(t, strings.Replace(hostFixture, "192.168.10.127", "192.168.10.200", 1)+forwardResource)
	runner.fail["systemctl restart regied-dnsmasq.service"] = errFake

	sinceSecondApply := len(runner.ran)
	_, err := engine.Apply(context.Background(), changed)
	requireErrorContaining(t, err, "regied-dnsmasq.service")

	if got, _ := files.content("/etc/regied/dnsmasq/dnsmasq.conf"); got != before {
		t.Error("the dnsmasq configuration was not put back as it was")
	}
	if got, _ := files.content("/var/lib/regied/applied/ruleset.nft"); got != beforeRuleset {
		t.Error("the recorded ruleset moved on even though the apply failed")
	}
	if !ranNFTWith(runner.ran[sinceSecondApply:], beforeRuleset) {
		t.Error("the rollback did not reinstall the ruleset the previous apply had left")
	}
}

func TestApplyLeavesTheHostAloneWhenStagingFails(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	// nft refuses the ruleset. Everything else about the configuration is fine, and the
	// point is that nothing has been run when this is discovered.
	runner.fail["nft --check -f -"] = errFake

	_, err := engine.Apply(context.Background(), load(t, hostFixture))
	requireErrorContaining(t, err, "nft --check")

	if _, ok := files.content("/etc/systemd/network/50-regied-wan.network"); ok {
		t.Error("a staging failure still left a file on the host")
	}
	for _, command := range runner.commands() {
		if strings.HasPrefix(command, "systemctl") || strings.HasPrefix(command, "networkctl") {
			t.Errorf("a staging failure still ran %q", command)
		}
	}
}

func TestApplyRefusesAConfigurationThatCannotBeRendered(t *testing.T) {
	engine, files, _, _ := planFixture(t)
	// No credentials on the host at all.
	delete(files.files, "/etc/regied/secrets/pppoe-password")

	_, err := engine.Apply(context.Background(), load(t, hostFixture))
	requireErrorContaining(t, err, "/etc/regied/secrets/pppoe-password")
	if len(files.writes) != 0 {
		t.Errorf("the host was written to anyway: %v", files.writes)
	}
}

// The ruleset carries no uplink address, so something has to put the addresses where the
// hairpin rules look for them. The firewall phase does it: right after the table, it
// seeds each uplink's set with what the link is holding (ADR 0015).
func TestTheFirewallPhaseSeedsTheUplinkSets(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	host.Links.(fakeLinks)["pppoe0"] = addrs(t, "192.0.2.10")

	mustApply(t, engine, load(t, hostFixture+forwardResource))

	if !ranNFTWith(runner.ran, "add element inet regied uplink4_pppoe0 { 192.0.2.10 }") {
		t.Errorf("nothing put the uplink's address into its set:\n%s", strings.Join(runner.commands(), "\n"))
	}
	// What is recorded is the text, and the text is a function of the configuration
	// alone: the address is in the kernel, not in the ruleset.
	recorded, _ := files.content("/var/lib/regied/applied/ruleset.nft")
	if strings.Contains(recorded, "192.0.2.10") {
		t.Errorf("the recorded ruleset carries the uplink's address:\n%s", recorded)
	}
	if !strings.Contains(recorded, "@uplink4_pppoe0") {
		t.Errorf("the recorded ruleset does not match on the uplink's set:\n%s", recorded)
	}
}

// A session started in the process phase dials seconds later, which used to mean a second
// rendering and a second install. It means nothing to the ruleset now: the table went in
// once, with the set the hook is about to fill (ADR 0015).
func TestTheLineComingUpDuringAnApplyChangesNoRuleset(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	links := host.Links.(fakeLinks)
	cfg := load(t, hostFixture+forwardResource)
	runner.onRun = func(cmd Command) {
		if strings.Contains(cmd.String(), "enable --now regied-pppoe@pppoe0") {
			links["pppoe0"] = addrs(t, "192.0.2.10")
		}
	}

	mustApply(t, engine, cfg)

	var applies int
	for _, cmd := range runner.ran {
		if cmd.String() == "nft -f -" && strings.Contains(cmd.Stdin, "table inet regied {") {
			applies++
		}
	}
	if applies != 1 {
		t.Errorf("the ruleset was installed %d times, want 1", applies)
	}

	// And the apply after the line came up has nothing to do at all.
	tablePresent(runner)
	if result := mustApply(t, engine, cfg); result.Changed {
		t.Errorf("the apply after the line came up reports a change: %s", result.Plan.Summary())
	}
}

// A rollback puts the previous table text back, and replacing a table empties its sets.
// Leaving them empty would take the hairpin rules away as surely as rendering them out
// would, so the rollback seeds them the way the apply does (ADR 0015).
func TestRollbackSeedsTheUplinkSetsAgain(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	host.Links.(fakeLinks)["pppoe0"] = addrs(t, "192.0.2.10")
	mustApply(t, engine, load(t, hostFixture+forwardResource))
	tablePresent(runner)

	// A second apply that changes the ruleset and dnsmasq's file, and fails at the
	// restart, after the firewall phase has already run.
	changed := load(t, strings.Replace(hostFixture, "192.168.10.127", "192.168.10.200", 1)+forwardResource+secondForward)
	runner.fail["systemctl restart regied-dnsmasq.service"] = errFake
	before := len(runner.ran)

	if _, err := engine.Apply(context.Background(), changed); err == nil {
		t.Fatal("the failing restart was not reported")
	}

	var restored, seeded bool
	for _, cmd := range runner.ran[before:] {
		if cmd.Name != "nft" {
			continue
		}
		if strings.Contains(cmd.Stdin, "table inet regied {") && !strings.Contains(cmd.Stdin, "PortForward/ssh") {
			restored = true
		}
		if restored && strings.Contains(cmd.Stdin, "add element inet regied uplink4_pppoe0 { 192.0.2.10 }") {
			seeded = true
		}
	}
	if !restored {
		t.Fatalf("the previous ruleset was not put back:\n%s", strings.Join(runner.commands(), "\n"))
	}
	if !seeded {
		t.Error("the restored table's uplink sets were left empty, so the hairpin rules match nothing")
	}
}

// The seeding is a step of its own, after the table. A failure at the table itself means
// the seeding was never attempted and is not among the steps the rollback undoes — and
// the rollback still replaces the table, which empties its sets. So putting the previous
// text back has to bring its elements with it, or a rollback that says it succeeded
// leaves the hairpin rules matching nothing (ADR 0005, ADR 0015).
func TestRollbackSeedsEvenWhenTheTableItselfFailed(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	host.Links.(fakeLinks)["pppoe0"] = addrs(t, "192.0.2.10")
	mustApply(t, engine, load(t, hostFixture+forwardResource))
	tablePresent(runner)

	changed := load(t, hostFixture+forwardResource+secondForward)
	runner.fail["nft -f -"] = errFake
	before := len(runner.ran)

	_, err := engine.Apply(context.Background(), changed)
	requireErrorContaining(t, err, "firewall")

	var restoredAndSeeded bool
	for _, cmd := range runner.ran[before:] {
		if cmd.Name != "nft" || strings.Contains(cmd.Stdin, "PortForward/ssh") {
			continue
		}
		if strings.Contains(cmd.Stdin, "table inet regied {") &&
			strings.Contains(cmd.Stdin, "add element inet regied uplink4_pppoe0 { 192.0.2.10 }") {
			restoredAndSeeded = true
		}
	}
	if !restoredAndSeeded {
		t.Errorf("the restored table was not seeded in the same transaction:\n%s", strings.Join(runner.commands(), "\n"))
	}
}

// A recorded ruleset from before the sets existed declares none, and telling nft to add
// elements to a set the text does not declare would make the whole restore fail. Only the
// sets the restored text declares are seeded.
func TestRollbackSeedsOnlyTheSetsTheRestoredTextDeclares(t *testing.T) {
	change := FirewallChange{
		Before:   "table inet regied {\n\tset uplink4_pppoe0 {\n\t\ttype ipv4_addr\n\t}\n}\n",
		Elements: []SetElements{{Set: "uplink4_pppoe0", Elements: []string{"192.0.2.10"}}, {Set: "uplink6_pppoe0", Elements: []string{"2001:db8::1"}}},
	}

	undo := firewallUndo(change)

	if !strings.Contains(undo.Command.Stdin, "add element inet regied uplink4_pppoe0 { 192.0.2.10 }") {
		t.Errorf("the set the restored text declares is not seeded:\n%s", undo.Command.Stdin)
	}
	if strings.Contains(undo.Command.Stdin, "uplink6_pppoe0") {
		t.Errorf("a set the restored text does not declare is seeded, which would make the restore fail:\n%s", undo.Command.Stdin)
	}
}

func TestApplyReportsARollbackThatFailedToo(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	// The reload fails, and the rollback re-runs it over the restored files, where it
	// fails for the same reason. An operator has to be told both.
	runner.fail["networkctl reload"] = errFake

	_, err := engine.Apply(context.Background(), load(t, hostFixture))
	requireErrorContaining(t, err, "networkctl reload")
	requireErrorContaining(t, err, "the rollback also failed")
}

// ranNFTWith is whether nft was handed a ruleset holding the given text.
func ranNFTWith(commands []Command, want string) bool {
	for _, cmd := range commands {
		if cmd.Name == "nft" && want != "" && strings.Contains(cmd.Stdin, want) {
			return true
		}
	}
	return false
}

func TestApplyChangesNothingTheSecondTime(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)
	tablePresent(runner)

	before := len(runner.ran)
	result := mustApply(t, engine, cfg)

	if result.Changed {
		t.Error("applying the same configuration twice reports a change")
	}
	// The only command the second apply is allowed is the probe that asks whether the
	// table is in the kernel.
	for _, cmd := range runner.ran[before:] {
		if cmd.String() != "nft list tables" {
			t.Errorf("an apply that changes nothing ran %q", cmd)
		}
	}
}
