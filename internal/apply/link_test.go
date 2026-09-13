package apply

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// wakeFixture is a host below a router whose NIC is set to wake on the magic packet. The
// setting is the one thing in it that goes in a .link file, which udev reads rather than
// networkd (ADR 0020).
const wakeFixture = `  resources:
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: eno1
        wakeOnLan: magic
        addresses: [192.168.10.153/24]
`

const (
	linkFile       = "/etc/systemd/network/50-regied-lan.link"
	udevReload     = "udevadm control --reload"
	udevTriggerEno = "udevadm trigger --settle --action=add /sys/class/net/eno1"
)

// linkIsUp makes the host answer that a link is there.
func linkIsUp(host Host, ifname string) {
	host.Links.(fakeLinks)[ifname] = nil
}

func hasCommandWithPrefix(commands []string, prefix string) bool {
	return slices.ContainsFunc(commands, func(command string) bool {
		return strings.HasPrefix(command, prefix)
	})
}

// A .link file is not read when it is written, and networkd's reload does not read it
// either. udev is told to read it and to apply it to the link that is up, after networkd
// has been reloaded, in the networkd phase (ADR 0020).
func TestALinkFileIsAppliedToTheLinkThatIsUp(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	linkIsUp(host, "eno1")

	mustApply(t, engine, load(t, wakeFixture))

	if _, ok := files.content(linkFile); !ok {
		t.Fatalf("%s was not written", linkFile)
	}
	commands := runner.commands()
	networkctl := slices.Index(commands, "networkctl reload")
	reload := slices.Index(commands, udevReload)
	trigger := slices.Index(commands, udevTriggerEno)
	if networkctl < 0 || reload < 0 || trigger < 0 {
		t.Fatalf("not every command ran:\n%s", strings.Join(commands, "\n"))
	}
	if !(networkctl < reload && reload < trigger) {
		t.Errorf("the commands did not run in the order networkd, udev reload, udev trigger:\n%s", strings.Join(commands, "\n"))
	}
}

// A member of a bridge is the NIC that wakes the host, and its .link file is applied to it
// the same way. Being enslaved changes nothing about how udev reaches the device.
func TestALinkFileIsAppliedToAMemberOfABridge(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	linkIsUp(host, "br0")
	linkIsUp(host, "eno1")

	mustApply(t, engine, load(t, `  resources:
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br0
        bridge: {members: [eno1]}
        addresses: [192.168.10.153/24]
    - kind: Interface
      metadata: {name: lan-port}
      spec:
        ifname: eno1
        wakeOnLan: magic
`))

	const memberLinkFile = "/etc/systemd/network/50-regied-lan-port.link"
	if content, ok := files.content(memberLinkFile); !ok || !strings.Contains(content, "WakeOnLan=magic") {
		t.Fatalf("%s was not written with the mode:\n%s", memberLinkFile, content)
	}
	commands := runner.commands()
	if !slices.Contains(commands, udevReload) || !slices.Contains(commands, udevTriggerEno) {
		t.Errorf("udev was not asked to apply the member's .link file:\n%s", strings.Join(commands, "\n"))
	}
	if slices.Contains(commands, "udevadm trigger --settle --action=add /sys/class/net/br0") {
		t.Errorf("udev was asked to apply a .link file to the bridge, which has none:\n%s", strings.Join(commands, "\n"))
	}
}

// An apply that changes nothing runs nothing, and udev is no exception (ADR 0004).
func TestAnUnchangedLinkFileAsksUdevForNothing(t *testing.T) {
	engine, _, runner, host := planFixture(t)
	linkIsUp(host, "eno1")
	cfg := load(t, wakeFixture)
	mustApply(t, engine, cfg)
	tablePresent(runner)

	before := len(runner.ran)
	mustApply(t, engine, cfg)

	if since := commandsSince(runner, before); hasCommandWithPrefix(since, "udevadm") {
		t.Errorf("an apply that changed nothing asked udev for something:\n%s", strings.Join(since, "\n"))
	}
}

// networkd does not read a .link file, so a change to one alone is not a reason to reload
// it. udev is the one told.
func TestAChangedLinkFileAloneDoesNotReloadNetworkd(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	linkIsUp(host, "eno1")
	mustApply(t, engine, load(t, wakeFixture))
	tablePresent(runner)

	before := len(runner.ran)
	mustApply(t, engine, load(t, strings.Replace(wakeFixture, "wakeOnLan: magic", "wakeOnLan: [magic, unicast]", 1)))

	since := commandsSince(runner, before)
	if slices.Contains(since, "networkctl reload") {
		t.Errorf("networkd was reloaded although only a .link file changed:\n%s", strings.Join(since, "\n"))
	}
	if !slices.Contains(since, udevReload) || !slices.Contains(since, udevTriggerEno) {
		t.Errorf("udev was not told about the changed .link file:\n%s", strings.Join(since, "\n"))
	}
	if content, _ := files.content(linkFile); !strings.Contains(content, "WakeOnLan=magic unicast") {
		t.Errorf("%s does not hold the new modes:\n%s", linkFile, content)
	}
}

// A link that is not on the host has no device to give the event to, and asking would fail
// the turn. The file is in place and udev applies it when the link appears; the plan says
// so, and that is not something the turn waits for.
func TestALinkThatIsNotOnTheHostIsNotTriggered(t *testing.T) {
	engine, files, runner, _ := planFixture(t)

	result := mustApply(t, engine, load(t, wakeFixture))

	if _, ok := files.content(linkFile); !ok {
		t.Fatalf("%s was not written", linkFile)
	}
	commands := runner.commands()
	if hasCommandWithPrefix(commands, "udevadm trigger") {
		t.Errorf("udev was asked to apply the file to a link that is not there:\n%s", strings.Join(commands, "\n"))
	}
	if !slices.Contains(commands, udevReload) {
		t.Errorf("udev was not told to read the file it will apply when the link appears:\n%s", strings.Join(commands, "\n"))
	}
	if !slices.ContainsFunc(result.Plan.Notes, func(note string) bool {
		return strings.Contains(note, "eno1") && strings.Contains(note, "not on this host")
	}) {
		t.Errorf("the plan does not say the link is not on this host: %q", result.Plan.Notes)
	}
	if result.State != StateConverged {
		t.Errorf("the turn says %q, want converged", result.State)
	}
}

// Taking the field away reclaims the file and turns nothing off: what the NIC holds stays
// until the device is added again. So udev is reloaded, so that it does not apply the old
// file, and no link is triggered (ADR 0020).
func TestTakingWakeOnLanAwayReclaimsTheFileAndTriggersNothing(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	linkIsUp(host, "eno1")
	mustApply(t, engine, load(t, wakeFixture))
	tablePresent(runner)

	before := len(runner.ran)
	mustApply(t, engine, load(t, strings.Replace(wakeFixture, "        wakeOnLan: magic\n", "", 1)))

	if _, ok := files.content(linkFile); ok {
		t.Errorf("%s was not reclaimed", linkFile)
	}
	since := commandsSince(runner, before)
	if !slices.Contains(since, udevReload) {
		t.Errorf("udev was not told the file went away:\n%s", strings.Join(since, "\n"))
	}
	if hasCommandWithPrefix(since, "udevadm trigger") || slices.Contains(since, "networkctl reload") {
		t.Errorf("reclaiming the .link file ran more than the udev reload:\n%s", strings.Join(since, "\n"))
	}
}

// Applying a .link file takes nothing down, so a turn nobody asked for does it: a file
// somebody deleted is put back and applied again (ADR 0016, ADR 0020).
func TestAnUnattendedTurnPutsBackALinkFileAndAppliesIt(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	linkIsUp(host, "eno1")
	mustSubmit(t, engine, wakeFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	delete(files.files, linkFile)
	before := len(runner.ran)

	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("the unattended turn failed: %v", err)
	}
	if _, ok := files.content(linkFile); !ok {
		t.Errorf("the unattended turn did not put %s back", linkFile)
	}
	if since := commandsSince(runner, before); !slices.Contains(since, udevTriggerEno) {
		t.Errorf("the unattended turn did not apply the file it put back:\n%s", strings.Join(since, "\n"))
	}
	if len(result.Plan.Failing) > 0 {
		t.Errorf("the unattended turn held something back: %q", result.Plan.Failing)
	}
}
