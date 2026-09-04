package apply

import (
	"context"
	"strings"
	"testing"
)

// deleteOnlyRuleset is the script that takes regied's table off and puts nothing back.
// It is the whole of the undo for an apply that found no table, and it must not be run
// for any other reason.
const deleteOnlyRuleset = "table inet regied\ndelete table inet regied\n"

// A rollback puts back what was there. Taking the table off when regied has no record of
// what was there before is not putting anything back: it is taking the firewall off a
// host that had one, to recover from a missing note (ADR 0005).
func TestRollbackLeavesATableItCannotRestore(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)
	tablePresent(runner)

	// The state the previous round made reachable: the ruleset is in the kernel, and
	// the note saying what it is could not be written.
	delete(files.files, "/var/lib/regied/applied/ruleset.nft")

	changed := load(t, strings.Replace(hostFixture, "192.168.10.127", "192.168.10.200", 1))
	runner.fail["systemctl restart regied-dnsmasq.service"] = errFake

	_, err := engine.Apply(context.Background(), changed)
	if err == nil {
		t.Fatal("the failing restart was not reported")
	}

	for _, cmd := range runner.ran {
		if cmd.Name == "nft" && cmd.Stdin == deleteOnlyRuleset {
			t.Fatal("the rollback took regied's table off a host that was running it")
		}
	}
	requireErrorContaining(t, err, "no record")
}

// The same table, on a host that genuinely had none before, is still taken off: there
// the removal is the restore.
func TestRollbackTakesOffATableThisApplyPutThere(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	runner.fail["systemctl enable --now regied-dnsmasq.service"] = errFake

	_, err := engine.Apply(context.Background(), load(t, hostFixture))
	if err == nil {
		t.Fatal("the failing start was not reported")
	}

	var undone bool
	for _, cmd := range runner.ran {
		if cmd.Name == "nft" && cmd.Stdin == deleteOnlyRuleset {
			undone = true
		}
	}
	if !undone {
		t.Errorf("a table this apply installed was left behind:\n%s", strings.Join(runner.commands(), "\n"))
	}
}

// The rule the apply order already follows, on the way back: systemctl resolves an
// instance through its template, so a rollback may not take the template away and then
// ask for a stop (ADR 0004).
func TestRollbackStopsWhatRunsFromAUnitBeforeTakingTheUnitAway(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	template := "/etc/systemd/system/regied-pppoe@.service"
	unit := "/etc/systemd/system/regied-dnsmasq.service"

	var unitsAtStop []string
	runner.onRun = func(cmd Command) {
		if !strings.Contains(cmd.String(), "disable --now") {
			return
		}
		for _, path := range []string{template, unit} {
			if _, ok := files.content(path); ok {
				unitsAtStop = append(unitsAtStop, path)
			}
		}
	}
	// The first apply, which creates both units, fails at the last thing it does.
	runner.fail["systemctl enable --now regied-dnsmasq.service"] = errFake

	if _, err := engine.Apply(context.Background(), load(t, hostFixture)); err == nil {
		t.Fatal("the failing start was not reported")
	}

	if !contains(unitsAtStop, template) {
		t.Error("the PPPoE template was already gone when the rollback asked systemctl to stop the session")
	}
	if !contains(unitsAtStop, unit) {
		t.Error("the dnsmasq unit was already gone when the rollback asked systemctl to stop it")
	}
	// And they are gone once the stops are done: the host is back as it was.
	for _, path := range []string{template, unit} {
		if _, ok := files.content(path); ok {
			t.Errorf("%s was created by a rolled-back apply and left behind", path)
		}
	}
}

// Round 3. A unit the rollback takes back off is one systemd was told about. Taking the
// file away and not saying so leaves systemd able to start a unit that no longer exists.
func TestRollbackTellsSystemdOnceTheUnitsItCreatedAreGone(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	template := "/etc/systemd/system/regied-pppoe@.service"

	var templateAtReload []bool
	runner.onRun = func(cmd Command) {
		if cmd.String() == "systemctl daemon-reload" {
			_, ok := files.content(template)
			templateAtReload = append(templateAtReload, ok)
		}
	}
	runner.fail["systemctl enable --now regied-dnsmasq.service"] = errFake

	if _, err := engine.Apply(context.Background(), load(t, hostFixture)); err == nil {
		t.Fatal("the failing start was not reported")
	}

	if len(templateAtReload) == 0 || templateAtReload[len(templateAtReload)-1] {
		t.Errorf("systemd was not reloaded after the units were taken away; reloads saw the template: %v", templateAtReload)
	}
}
