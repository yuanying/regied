package apply

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// moveDelegation makes the client on eth0 hold another prefix, the way a line that
// solicited again does.
func moveDelegation(delegations fakeDelegations, prefix string) {
	delegations["eth0"] = []netip.Prefix{netip.MustParsePrefix(prefix)}
}

func advance(engine *Engine, by time.Duration) {
	clock := engine.host.Clock.(*fakeClock)
	clock.now = clock.now.Add(by)
}

func countCommand(commands []string, command string) int {
	var n int
	for _, c := range commands {
		if c == command {
			n++
		}
	}
	return n
}

// The delegation moves twice inside a second. The first turn writes the tunnel and
// reloads networkd; the second writes it again and its reload is held back by the
// backoff. The third finds the files as they should be, and networkd still holds the
// version of the first turn: the reload the second turn wrote the files for is still
// owed, and the third turn runs it. Backoff suppresses the command, not the comparison,
// and not what the comparison already found (ADR 0016).
func TestAReloadHeldBackByTheBackoffRunsOnALaterTurn(t *testing.T) {
	engine, files, runner, delegations := delegationHost(t, "2001:db8:1000::/56")
	mustSubmit(t, engine, delegationFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	moveDelegation(delegations, "2001:db8:2000::/56")
	before := len(runner.ran)
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatalf("turn A failed: %v", err)
	}
	if !slices.Contains(commandsSince(runner, before), "networkctl reload") {
		t.Fatalf("turn A did not reload networkd; ran %q", commandsSince(runner, before))
	}

	moveDelegation(delegations, "2001:db8:3000::/56")
	before = len(runner.ran)
	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("turn B failed: %v", err)
	}
	if slices.Contains(commandsSince(runner, before), "networkctl reload") {
		t.Fatal("turn B was not backed off, so the test proves nothing")
	}
	if netdev, _ := files.content("/etc/systemd/network/50-regied-dslite.netdev"); !strings.Contains(netdev, "Local=2001:db8:3000:1::1\n") {
		t.Fatalf("turn B did not write the tunnel for the new delegation:\n%s", netdev)
	}
	if result.State != StateFailing || !strings.Contains(strings.Join(result.Plan.Failing, "\n"), "networkctl reload: retry backoff") {
		t.Errorf("turn B does not say the reload was held back: state=%s failing=%v", result.State, result.Plan.Failing)
	}

	advance(engine, 2*time.Second)
	before = len(runner.ran)
	result, err = engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("turn C failed: %v", err)
	}
	if !slices.Contains(commandsSince(runner, before), "networkctl reload") {
		t.Errorf("turn C did not run the reload turn B held back; ran %q", commandsSince(runner, before))
	}
	if result.State != StateConverged {
		t.Errorf("turn C says %q, want converged; failing %v", result.State, result.Plan.Failing)
	}

	// Once it has run, it is owed no more.
	advance(engine, 10*time.Minute)
	before = len(runner.ran)
	result, err = engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("turn D failed: %v", err)
	}
	if slices.Contains(commandsSince(runner, before), "networkctl reload") {
		t.Error("turn D reloaded networkd again, after the owed reload had run")
	}
	if result.Changed {
		t.Errorf("turn D did something: %v", result.Plan.Steps)
	}
}

// What is owed is written under the state directory, so a process that is restarted
// between the turn that held the reload back and the next one still runs it.
func TestAnOwedReloadSurvivesARestartOfTheProcess(t *testing.T) {
	engine, _, runner, delegations := delegationHost(t, "2001:db8:1000::/56")
	mustSubmit(t, engine, delegationFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	moveDelegation(delegations, "2001:db8:2000::/56")
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatal(err)
	}
	moveDelegation(delegations, "2001:db8:3000::/56")
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatal(err)
	}

	restarted := New(engine.host, engine.opts)
	before := len(runner.ran)
	if _, err := restarted.ReconcileUnattended(context.Background()); err != nil {
		t.Fatalf("the first turn of the restarted process failed: %v", err)
	}
	if got := countCommand(commandsSince(runner, before), "networkctl reload"); got != 1 {
		t.Errorf("the restarted process reloaded networkd %d times, want once; ran %q", got, commandsSince(runner, before))
	}
}

// A turn that stopped before its reload ran — here, the reload itself failed — leaves it
// owed, and the next turn runs it although the files are already as they should be.
func TestAReloadThatFailedRunsOnTheNextTurn(t *testing.T) {
	engine, _, runner, delegations := delegationHost(t, "2001:db8:1000::/56")
	mustSubmit(t, engine, delegationFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	moveDelegation(delegations, "2001:db8:2000::/56")
	runner.fail["networkctl reload"] = errFake
	if _, err := engine.ReconcileUnattended(context.Background()); err == nil {
		t.Fatal("the turn whose reload failed did not fail")
	}

	delete(runner.fail, "networkctl reload")
	advance(engine, 2*time.Second)
	before := len(runner.ran)
	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("the next turn failed: %v", err)
	}
	if !slices.Contains(commandsSince(runner, before), "networkctl reload") {
		t.Errorf("the next turn did not run the reload that failed; ran %q", commandsSince(runner, before))
	}
	if result.State != StateConverged {
		t.Errorf("the next turn says %q, want converged; failing %v", result.State, result.Plan.Failing)
	}
}

// A submitted turn is not rate-limited, and it runs what an unattended turn left owed
// even when the declaration it submits changes no file.
func TestASubmittedTurnRunsWhatIsOwed(t *testing.T) {
	engine, _, runner, delegations := delegationHost(t, "2001:db8:1000::/56")
	mustSubmit(t, engine, delegationFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	moveDelegation(delegations, "2001:db8:2000::/56")
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatal(err)
	}
	moveDelegation(delegations, "2001:db8:3000::/56")
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatal(err)
	}

	before := len(runner.ran)
	mustSubmit(t, engine, delegationFixture, "/etc/regied/config.yaml")
	if !slices.Contains(commandsSince(runner, before), "networkctl reload") {
		t.Errorf("the submission did not run the owed reload; ran %q", commandsSince(runner, before))
	}
}

// The dry-run shows what is owed, and why, before anything runs (ADR 0006).
func TestAPlanShowsWhatIsOwed(t *testing.T) {
	engine, _, runner, delegations := delegationHost(t, "2001:db8:1000::/56")
	mustSubmit(t, engine, delegationFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	moveDelegation(delegations, "2001:db8:2000::/56")
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatal(err)
	}
	moveDelegation(delegations, "2001:db8:3000::/56")
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatal(err)
	}

	plan := mustPlan(t, engine, load(t, delegationFixture))
	var found bool
	for _, step := range plan.Steps {
		if step.describe() == "networkctl reload" {
			found = true
			if !strings.Contains(step.Reason, "an earlier turn") {
				t.Errorf("the owed reload does not say where it comes from: %q", step.Reason)
			}
		}
	}
	if !found {
		t.Errorf("the plan does not show the owed reload: %v", plan.Steps)
	}
}

// The .link file is put back and udev's commands fail, so applying it is owed. The next
// turn finds the file in place and still applies it; and once the link has gone, there
// is nothing to apply it to and nothing is owed any more (ADR 0020).
func TestApplyingALinkFileIsOwedUntilItRunsOrTheLinkIsGone(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	linkIsUp(host, "eno1")
	mustSubmit(t, engine, wakeFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	delete(files.files, linkFile)
	runner.fail[udevReload] = errFake
	if _, err := engine.ReconcileUnattended(context.Background()); err == nil {
		t.Fatal("the turn whose udev reload failed did not fail")
	}

	delete(runner.fail, udevReload)
	runner.fail[udevTriggerEno] = errFake
	advance(engine, 2*time.Second)
	before := len(runner.ran)
	if _, err := engine.ReconcileUnattended(context.Background()); err == nil {
		t.Fatal("the turn whose udev trigger failed did not fail")
	}
	since := commandsSince(runner, before)
	if !slices.Contains(since, udevReload) || !slices.Contains(since, udevTriggerEno) {
		t.Fatalf("the next turn did not run what was owed:\n%s", strings.Join(since, "\n"))
	}

	delete(host.Links.(fakeLinks), "eno1")
	advance(engine, 10*time.Minute)
	before = len(runner.ran)
	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("the turn after the link went away failed: %v", err)
	}
	if since := commandsSince(runner, before); slices.Contains(since, udevTriggerEno) {
		t.Errorf("a link that is gone was given an event:\n%s", strings.Join(since, "\n"))
	}
	if result.Changed {
		t.Errorf("the turn after the link went away did something: %v", result.Plan.Steps)
	}
	if _, ok := files.content("/var/lib/regied/turn/owed.yaml"); ok {
		t.Error("something is still written down as owed")
	}
}

// The file is read back as commands to run, so nothing in it runs that regied would not
// have asked for itself.
func TestOnlyWhatATurnCanOweIsRunFromTheFile(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")
	tablePresent(runner)
	files.put("/var/lib/regied/turn/owed.yaml", `- phase: 3
  reason: planted
  command: rm
  args: ["-rf", "/"]
- phase: 3
  reason: planted
  command: udevadm
  args: ["trigger", "--settle", "--action=add", "/sys/class/net/../../etc"]
`, 0o644)

	before := len(runner.ran)
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, command := range commandsSince(runner, before) {
		if strings.HasPrefix(command, "rm ") || strings.HasPrefix(command, "udevadm") {
			t.Errorf("a command regied would not owe was run from the file: %q", command)
		}
	}
}
