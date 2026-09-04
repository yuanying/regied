package apply

import (
	"strings"
	"testing"
)

// B-1. dnsmasq does not re-read its configuration on SIGHUP — what it re-reads is
// /etc/hosts, the lease file and resolv.conf. A unit that offers a reload systemd would
// use is a unit that quietly leaves a changed configuration unapplied.
func TestTheDNSMasqUnitOffersNoReload(t *testing.T) {
	unit := dnsmasqUnitFile("/etc/regied")

	if hasDirective(unit, "ExecReload=") {
		t.Errorf("the unit offers a reload dnsmasq cannot honour for its configuration:\n%s", unit)
	}
	if !strings.Contains(unit, "--conf-file=/etc/regied/dnsmasq/dnsmasq.conf") {
		t.Errorf("the unit does not read regied's own configuration:\n%s", unit)
	}
}

func TestAChangedDNSMasqConfigurationRestartsIt(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixture))
	tablePresent(runner)

	changed := load(t, strings.Replace(hostFixture, "192.168.10.127", "192.168.10.200", 1))
	plan := mustPlan(t, engine, changed)

	commands := stepCommands(plan)
	for _, command := range commands {
		if strings.Contains(command, "reload-or-restart") {
			t.Errorf("a changed dnsmasq configuration is reloaded rather than restarted: %q", command)
		}
	}
	if !contains(commands, "systemctl restart regied-dnsmasq.service") {
		t.Errorf("a changed dnsmasq configuration does not restart it:\n%s", strings.Join(commands, "\n"))
	}
}

// B-3. network-pre.target is before networkd configures anything, so ordering a session
// after it puts pppd ahead of the Ethernet it dials over. ADR 0004 puts networkd first
// for exactly this reason.
func TestTheSessionUnitStartsAfterTheLinkItDialsOverIsConfigured(t *testing.T) {
	unit := pppoeUnitFile("/etc/regied")

	for _, directive := range directives(unit) {
		if strings.HasPrefix(directive, "After=") && strings.Contains(directive, "network-pre.target") {
			t.Errorf("the session is ordered against a target that is before its own underlay: %q", directive)
		}
	}
	if !hasDirective(unit, "After=systemd-networkd.service") {
		t.Errorf("the session is not ordered after the thing that configures its underlay:\n%s", unit)
	}
}

func TestTheDNSMasqUnitStartsAfterTheLinksItBindsTo(t *testing.T) {
	unit := dnsmasqUnitFile("/etc/regied")

	if !hasDirective(unit, "After=systemd-networkd.service") {
		t.Errorf("dnsmasq is not ordered after the links it binds to are configured:\n%s", unit)
	}
}

// directives is the lines of a unit file that systemd acts on: not the comments, which
// are there to say why the directives are what they are.
func directives(unit string) []string {
	var out []string
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func hasDirective(unit, prefix string) bool {
	for _, directive := range directives(unit) {
		if strings.HasPrefix(directive, prefix) {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
