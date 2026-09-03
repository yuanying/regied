package apply

import "fmt"

// The units regied writes. pppd and dnsmasq are long-running processes regied
// configures but does not host: it does not fork them, does not hold them as children,
// and does not write a restart loop. It writes a unit for each and asks systemd to start
// it (ADR 0004).
//
// They are generated here rather than in a renderer because what they say is how the
// processes are run, which is the apply model's subject and not a backend's
// configuration.
const (
	// unitPrefix is on the name of every unit regied writes, so that reclaiming can
	// tell them from everybody else's in /etc/systemd/system.
	unitPrefix = "regied-"

	pppoeTemplateUnit = unitPrefix + "pppoe@.service"
	dnsmasqUnit       = unitPrefix + "dnsmasq.service"
)

// pppoeUnit is the unit that runs one session. The instance name is the resource's name,
// which is also the name of the link pppd puts on the host, so `systemctl status`
// answers in the vocabulary the configuration was written in.
func pppoeUnit(session string) string {
	return unitPrefix + "pppoe@" + session + ".service"
}

// units is the systemd units this configuration needs. A host with no session gets no
// PPPoE template, and a host with no address handout and no DNS gets no dnsmasq unit;
// both are then reclaimed like any other file regied stopped needing.
func (e *Engine) units(rendered *rendering) []artifact {
	var out []artifact
	if len(rendered.sessions) > 0 {
		out = append(out, artifact{
			Path:    e.opts.UnitDir + "/" + pppoeTemplateUnit,
			Mode:    0o644,
			DirMode: 0o755,
			Content: pppoeUnitFile(e.opts.Root),
		})
	}
	if rendered.dnsmasq {
		out = append(out, artifact{
			Path:    e.opts.UnitDir + "/" + dnsmasqUnit,
			Mode:    0o644,
			DirMode: 0o755,
			Content: dnsmasqUnitFile(e.opts.Root),
		})
	}
	return out
}

// pppoeUnitFile is one template unit for every session. The instance name is the
// session's name, and it is what selects both options files.
//
// Redialling is not systemd's: pppd's own persist and the LCP echoes beside it are what
// bring a dropped session back, and they are in the peer file (ADR 0014). Restart= is
// the backstop for pppd itself dying.
//
// The unit is ordered after networkd rather than after network-pre.target, because the
// Ethernet it dials over is networkd's to configure.
func pppoeUnitFile(root string) string {
	return fmt.Sprintf(`%s
#
# One PPPoE session. %%i is the PPPoESession's name, which is also the name of the
# link pppd puts on the host.

[Unit]
Description=regied PPPoE session %%i
Documentation=man:pppd(8)
# The Ethernet this dials over is configured by systemd-networkd, and the apply model
# puts networkd ahead of the sessions for that reason (ADR 0004). Ordering against
# network-pre.target would do the opposite: that target is reached before networkd has
# configured anything, so pppd would start ahead of its own underlay.
Wants=systemd-networkd.service
After=systemd-networkd.service

[Service]
Type=exec
ExecStart=/usr/sbin/pppd file %s/ppp/peers/%%i.conf file %s/ppp/credentials/%%i.conf nodetach
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, ownershipMarker, root, root)
}

// dnsmasqUnitFile runs regied's own dnsmasq.
//
// It is deliberately not the distribution's dnsmasq.service: that one reads a file
// regied did not write and belongs to whoever installed it (ADR 0009). A host may run
// both, and this unit's dnsmasq answers only on the links the configuration named,
// because the generated file says bind-dynamic and lists them.
//
// It offers no reload, because dnsmasq has none for its configuration file.
func dnsmasqUnitFile(root string) string {
	return fmt.Sprintf(`%s
#
# regied's own dnsmasq: DHCP and DNS for the links the configuration names. The
# distribution's dnsmasq.service, if there is one, is left alone.

[Unit]
Description=regied dnsmasq (address handout and DNS)
Documentation=man:dnsmasq(8)
# It binds to the addresses of the links networkd configures.
Wants=systemd-networkd.service
After=systemd-networkd.service

[Service]
Type=exec
# There is no ExecReload. dnsmasq re-reads /etc/hosts, its lease file and resolv.conf on
# SIGHUP, and does not re-read its configuration file, so offering a reload would let a
# changed configuration go unapplied. regied restarts it instead.
ExecStart=/usr/sbin/dnsmasq --keep-in-foreground --conf-file=%s/dnsmasq/dnsmasq.conf
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, ownershipMarker, root)
}
