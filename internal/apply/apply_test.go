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

func TestApplyStopsAtAFailedCommand(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	sysctl := host.Sysctl.(*fakeSysctl)
	sysctl.values["net.ipv4.ip_forward"] = "0"
	cfg := load(t, hostFixture)

	// Starting the session is where it fails: the firewall, the switches and the reload
	// have all been done, and every file is written.
	runner.fail["systemctl enable --now regied-pppoe@pppoe0.service"] = errFake

	_, err := engine.Apply(context.Background(), cfg)
	requireErrorContaining(t, err, "regied-pppoe@pppoe0.service")
	requireErrorContaining(t, err, "remains at the point")

	if _, ok := files.content("/etc/systemd/network/50-regied-wan.network"); !ok {
		t.Error("a file staged before the failure was lost")
	}
	if got := sysctl.values["net.ipv4.ip_forward"]; got != "1" {
		t.Errorf("net.ipv4.ip_forward was left at %q, want the value reached before the failure", got)
	}
	// The table was installed and has to come back off, because before this apply there
	// was none.
	for _, cmd := range runner.ran {
		if cmd.Stdin == "table inet regied\ndelete table inet regied\n" {
			t.Errorf("the failed turn tried to undo the firewall:\n%s", strings.Join(runner.commands(), "\n"))
		}
	}
	if _, ok := files.content("/var/lib/regied/applied/ruleset.nft"); ok {
		t.Error("a rolled-back apply recorded its ruleset as applied")
	}
}

func TestApplyLeavesTheNewGenerationAtTheFailurePoint(t *testing.T) {
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

	if got, _ := files.content("/etc/regied/dnsmasq/dnsmasq.conf"); got == before {
		t.Error("the staged dnsmasq configuration was unexpectedly undone")
	}
	if got, _ := files.content("/var/lib/regied/applied/ruleset.nft"); got != beforeRuleset {
		t.Error("the recorded ruleset moved on even though the apply failed")
	}
	if ranNFTWith(runner.ran[sinceSecondApply:], beforeRuleset) {
		t.Error("the failed turn reinstalled the previous ruleset")
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

	// And the apply after the line came up — its hook having filled the set — has nothing
	// to do at all.
	tablePresent(runner)
	setsHold(runner, map[string][]string{"uplink4_pppoe0": {"192.0.2.10"}, "uplink6_pppoe0": {}})
	if result := mustApply(t, engine, cfg); result.Changed {
		t.Errorf("the apply after the line came up reports a change: %s", result.Plan.Summary())
	}
}

// --- 差し戻し 1. The seeding is not tied to the table being replaced -------------------
//
// An apply is the one general way a host is put right, and the sets are kernel state
// like the table is. So the apply asks the kernel what they hold, compares that with what
// the links hold, and writes the sets that differ — whether or not the ruleset changed.
// Everything that follows is that rule from a different side (ADR 0015, ADR 0004).

// The motivating case: an IPv6 address that arrived through networkd after the apply.
// No hook covers it, the ruleset did not change, and the next apply is what puts it in.
func TestAnApplyThatChangesNoRulesetStillSeedsASetThatIsWrong(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	links := host.Links.(fakeLinks)
	links["pppoe0"] = addrs(t, "192.0.2.10")
	cfg := load(t, hostFixture+forwardResource)
	mustApply(t, engine, cfg)
	tablePresent(runner)

	// The line has a global IPv6 address now, and the kernel's set does not.
	links["pppoe0"] = addrs(t, "192.0.2.10", "2001:db8::1")
	setsHold(runner, map[string][]string{"uplink4_pppoe0": {"192.0.2.10"}, "uplink6_pppoe0": {}})
	before := len(runner.ran)

	result := mustApply(t, engine, cfg)

	if !result.Changed {
		t.Fatal("the apply says there was nothing to do while a set is wrong")
	}
	var seeded, replaced bool
	for _, cmd := range runner.ran[before:] {
		if cmd.Name != "nft" || cmd.Args[0] != "-f" {
			continue
		}
		if strings.Contains(cmd.Stdin, "table inet regied {") {
			replaced = true
		}
		if strings.Contains(cmd.Stdin, "uplink6_pppoe0 { 2001:db8::1 }") {
			seeded = true
		}
	}
	if !seeded {
		t.Errorf("the IPv6 address was not put into its set:\n%s", strings.Join(runner.commands(), "\n"))
	}
	if replaced {
		t.Error("the table was replaced to fix a set, which an unchanged ruleset must never do")
	}
	// Only the set that was wrong is written. The one that was right is left alone.
	for _, cmd := range runner.ran[before:] {
		if cmd.Name == "nft" && cmd.Args[0] == "-f" && strings.Contains(cmd.Stdin, "uplink4_pppoe0") {
			t.Errorf("a set that already held what the link holds was written again:\n%s", cmd.Stdin)
		}
	}
}

// The guard on the other side: an apply that finds everything right runs nothing that
// changes anything, so it is safe to run from a timer (ADR 0004).
func TestAnApplyThatFindsTheSetsRightRunsNothing(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	host.Links.(fakeLinks)["pppoe0"] = addrs(t, "192.0.2.10")
	cfg := load(t, hostFixture+forwardResource)
	mustApply(t, engine, cfg)
	tablePresent(runner)
	setsHold(runner, map[string][]string{"uplink4_pppoe0": {"192.0.2.10"}, "uplink6_pppoe0": {}})
	before := len(runner.ran)

	result := mustApply(t, engine, cfg)

	if result.Changed {
		t.Errorf("an apply that found the sets right reports a change: %s", result.Plan.Summary())
	}
	for _, cmd := range runner.ran[before:] {
		if cmd.Args[0] == "-f" {
			t.Errorf("an apply with nothing to fix ran %q", cmd)
		}
	}
}

// What a set could not be read from is not what an empty set says. Reading a failed
// probe as "empty" would seed on every apply on which nft misbehaved, which is the trap
// the table probe already avoids (ADR 0006).
func TestASetThatCannotBeReadIsNotTakenForEmpty(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	host.Links.(fakeLinks)["pppoe0"] = addrs(t, "192.0.2.10")
	cfg := load(t, hostFixture+forwardResource)
	mustApply(t, engine, cfg)
	tablePresent(runner)
	runner.fail[listTableCommand().String()] = errFake

	plan := mustPlan(t, engine, cfg)

	if !plan.Empty() {
		t.Errorf("a set that could not be read is being seeded: %s", plan.Summary())
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "uplink sets") {
		t.Errorf("nothing says the sets could not be read: %v", plan.Notes)
	}
}

// A set holding an address the link no longer holds claims something false, and a hook
// that missed its ip-down is how it happens. The apply puts the set right in one
// transaction, so that an element the hook adds in between is neither lost nor doubled
// and a delete of something already gone cannot fail the apply.
func TestAStaleElementIsTakenOutWithTheRightOnePutIn(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	host.Links.(fakeLinks)["pppoe0"] = addrs(t, "192.0.2.10")
	cfg := load(t, hostFixture+forwardResource)
	mustApply(t, engine, cfg)
	tablePresent(runner)
	setsHold(runner, map[string][]string{"uplink4_pppoe0": {"192.0.2.99"}, "uplink6_pppoe0": {}})
	before := len(runner.ran)

	mustApply(t, engine, cfg)

	var written bool
	for _, cmd := range runner.ran[before:] {
		if cmd.Name != "nft" || cmd.Args[0] != "-f" {
			continue
		}
		written = true
		if !strings.Contains(cmd.Stdin, "flush set inet regied uplink4_pppoe0\n") ||
			!strings.Contains(cmd.Stdin, "add element inet regied uplink4_pppoe0 { 192.0.2.10 }") {
			t.Errorf("the set was not emptied and refilled in one transaction:\n%s", cmd.Stdin)
		}
		if strings.Contains(cmd.Stdin, "delete element") {
			t.Errorf("a delete of one element races the hook and fails when it is already gone:\n%s", cmd.Stdin)
		}
	}
	if !written {
		t.Errorf("the stale set was left as it was:\n%s", strings.Join(runner.commands(), "\n"))
	}
}

// A set nobody should have removed. The table is regied's and it is only put back when
// the ruleset changes (ADR 0004), so the apply cannot fix this one; it has to say so
// rather than fail on an add to a set that is not there.
func TestASetMissingFromTheTableIsReportedNotSeeded(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	host.Links.(fakeLinks)["pppoe0"] = addrs(t, "192.0.2.10")
	cfg := load(t, hostFixture+forwardResource)
	mustApply(t, engine, cfg)
	tablePresent(runner)
	setsHold(runner, map[string][]string{"uplink6_pppoe0": {}})

	plan := mustPlan(t, engine, cfg)

	if !plan.Empty() {
		t.Errorf("a set that is not in the table is being written to: %s", plan.Summary())
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "uplink4_pppoe0") {
		t.Errorf("nothing names the set that is missing: %v", plan.Notes)
	}
}

func TestApplyReportsWhereACommandFailed(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	// The reload fails, and the rollback re-runs it over the restored files, where it
	// fails for the same reason. An operator has to be told both.
	runner.fail["networkctl reload"] = errFake

	_, err := engine.Apply(context.Background(), load(t, hostFixture))
	requireErrorContaining(t, err, "networkctl reload")
	requireErrorContaining(t, err, "remains at the point")
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
	// The only commands the second apply is allowed are the two probes, both of which
	// change nothing: whether the table is in the kernel, and what its sets hold.
	for _, cmd := range runner.ran[before:] {
		if cmd.String() != "nft list tables" && cmd.String() != listTableCommand().String() {
			t.Errorf("an apply that changes nothing ran %q", cmd)
		}
	}
}
