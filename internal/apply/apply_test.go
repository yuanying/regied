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
	delete(runner.fail, "nft list table inet regied")

	before, _ := files.content("/etc/regied/dnsmasq/dnsmasq.conf")
	beforeRuleset, _ := files.content("/var/lib/regied/applied/ruleset.nft")

	// A second apply that changes the handout, and fails while reloading networkd.
	changed := load(t, strings.Replace(hostFixture, "192.168.10.127", "192.168.10.200", 1)+forwardResource)
	runner.fail["systemctl reload-or-restart regied-dnsmasq.service"] = errFake

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

func TestApplyReappliesTheFirewallWhenTheLineComesUp(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	links := host.Links.(fakeLinks)
	cfg := load(t, hostFixture+forwardResource)

	// The session dials while the apply is running, which is the ordinary case: the
	// ruleset written in the first phase was rendered without an address.
	runner.onRun = func(cmd Command) {
		if strings.Contains(cmd.String(), "enable --now regied-pppoe@pppoe0") {
			links["pppoe0"] = addrs(t, "192.0.2.10")
		}
	}

	result := mustApply(t, engine, cfg)

	var applies int
	for _, cmd := range runner.ran {
		if cmd.String() == "nft -f -" {
			applies++
		}
	}
	if applies != 2 {
		t.Errorf("the ruleset was applied %d times, want 2: once without the address and once with it", applies)
	}
	if !result.FirewallReapplied {
		t.Error("the result does not say the firewall was re-applied")
	}
}

func TestApplyDoesNotReapplyTheFirewallWhenNothingMoved(t *testing.T) {
	engine, _, runner, _ := planFixture(t)

	result := mustApply(t, engine, load(t, hostFixture))

	var applies int
	for _, cmd := range runner.ran {
		if cmd.String() == "nft -f -" {
			applies++
		}
	}
	if applies != 1 {
		t.Errorf("the ruleset was applied %d times, want 1", applies)
	}
	if result.FirewallReapplied {
		t.Error("the result claims the firewall was re-applied when no address moved")
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
	delete(runner.fail, "nft list table inet regied")

	before := len(runner.ran)
	result := mustApply(t, engine, cfg)

	if result.Changed {
		t.Error("applying the same configuration twice reports a change")
	}
	// The only command the second apply is allowed is the probe that asks whether the
	// table is in the kernel.
	for _, cmd := range runner.ran[before:] {
		if cmd.String() != "nft list table inet regied" {
			t.Errorf("an apply that changes nothing ran %q", cmd)
		}
	}
}
