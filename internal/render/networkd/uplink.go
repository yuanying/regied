package networkd

import (
	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// The default route of an uplink, and of the table a policy routes into, by family.
const (
	defaultRouteV4 = "0.0.0.0/0"
	defaultRouteV6 = "::/0"
)

// renderDSLiteTunnel writes the ip6tnl and the link configuration that goes with it.
func (r *renderer) renderDSLiteTunnel(tunnel config.Named[*v1alpha1.DSLiteTunnelSpec]) {
	r.renderTunnelNetDev(tunnel)

	spec := tunnel.Spec
	u := newUnit(v1alpha1.KindDSLiteTunnel, tunnel.Name)
	u.section("Match").set("Name", tunnel.Name)

	network := u.section("Network")
	// The tunnel carries IPv4 and nothing else, so it wants no IPv6 of its own and has
	// no business listening to advertisements.
	network.setBool("LinkLocalAddressing", false)
	network.setBool("IPv6AcceptRA", false)

	if spec.DefaultRoute.InstallEnabled() {
		route := u.section("Route")
		route.set("Destination", defaultRouteV4)
		route.setInt("Metric", spec.DefaultRoute.MetricOrDefault())
	}
	renderRoutes(u, spec.Routes)
	r.renderPolicyRouting(u, tunnel.Name)

	r.add(fileName(tunnel.Name, ".network"), u)
}

func (r *renderer) renderTunnelNetDev(tunnel config.Named[*v1alpha1.DSLiteTunnelSpec]) {
	spec := tunnel.Spec
	u := newUnit(v1alpha1.KindDSLiteTunnel, tunnel.Name)

	netdev := u.section("NetDev")
	netdev.set("Name", tunnel.Name)
	netdev.set("Kind", "ip6tnl")
	netdev.setInt("MTUBytes", spec.MTUOrDefault())

	t := u.section("Tunnel")
	t.set("Mode", "ipip6")
	if local, ok := r.tunnelLocal(tunnel); ok {
		t.set("Local", local)
	}
	if remote, ok := r.tunnelRemote(tunnel); ok {
		t.set("Remote", remote)
	}
	t.setInt("TTL", spec.TTLOrDefault())
	// The encapsulation limit is a destination-options header on every encapsulated
	// packet. It costs eight bytes of an already reduced MTU and buys nothing here,
	// because the far end is a single AFTR rather than a chain of tunnels.
	t.set("EncapsulationLimit", "none")

	r.add(fileName(tunnel.Name, ".netdev"), u)
}

// tunnelLocal is the address the tunnel sends from.
//
// A literal address is written out. A localAddressFrom becomes networkd's slaac, which
// is the address of the link the tunnel is stacked on — the underlay. systemd 257 has no
// way to take it from a delegated prefix, so a localAddressFrom naming any other
// interface cannot be rendered as it was written (ADR 0011).
func (r *renderer) tunnelLocal(tunnel config.Named[*v1alpha1.DSLiteTunnelSpec]) (string, bool) {
	spec := tunnel.Spec
	if spec.LocalAddress != nil {
		return spec.LocalAddress.String(), true
	}
	if spec.LocalAddressFrom == nil {
		return "", false
	}
	if spec.LocalAddressFrom.InterfaceRef != spec.UnderlayRef {
		r.warnf("DSLiteTunnel/%s: localAddressFrom names Interface/%s, but systemd 257 can only take a tunnel's local address from the interface it is stacked on, so the tunnel sends from the global address of the underlay Interface/%s (ADR 0011)",
			tunnel.Name, spec.LocalAddressFrom.InterfaceRef, spec.UnderlayRef)
	}
	return "slaac", true
}

// tunnelRemote is the AFTR. A name is resolved by the apply engine and arrives already
// resolved: an ip6tnl takes its remote when the link is created, so the address is what
// the tunnel is built with and it is reported in dry-run output.
func (r *renderer) tunnelRemote(tunnel config.Named[*v1alpha1.DSLiteTunnelSpec]) (string, bool) {
	spec := tunnel.Spec
	if spec.AFTRAddress != nil {
		return spec.AFTRAddress.String(), true
	}
	if spec.AFTRHost == "" {
		return "", false
	}
	address, ok := r.rt.AFTRAddresses[tunnel.Name]
	if !ok {
		r.errorf("DSLiteTunnel/%s: the AFTR %s has not been resolved to an address", tunnel.Name, spec.AFTRHost)
		return "", false
	}
	return address.String(), true
}

// renderPPPoESession writes what the routing needs from a link networkd does not own.
//
// pppd creates the link, names it and addresses it; networkd is here only because a
// policy's table needs a default route through it, and a route has to live on the link
// it leaves by. A session no policy names gets no file at all, and its link stays
// unmanaged.
func (r *renderer) renderPPPoESession(session config.Named[*v1alpha1.PPPoESessionSpec]) {
	if len(r.policies[session.Name]) == 0 {
		return
	}
	u := newUnit(v1alpha1.KindPPPoESession, session.Name)
	u.header += `#
# pppd creates this link, names it and addresses it. regied writes this file only so
# that the routing an EgressRoutePolicy needs follows the link up and down, and
# KeepConfiguration keeps networkd from dropping what pppd installed.
`
	u.section("Match").set("Name", session.Name)
	// Everything on this link — the address, the peer, the default route in the main
	// table — was put there by pppd. networkd is told to leave all of it alone.
	u.section("Network").setBool("KeepConfiguration", true)

	r.renderPolicyRouting(u, session.Name)

	r.add(fileName(session.Name, ".network"), u)
}

// renderPolicyRouting writes both halves of every policy that leaves by this uplink: the
// default route in the policy's own table, and the rule that selects the table by mark.
//
// The rule's priority is the table number. Both are derived together and are unique by
// construction, and the range they are allocated from sits between the rule the kernel
// keeps for the local table and the one it keeps for main, which is where a rule has to
// be to have any effect.
func (r *renderer) renderPolicyRouting(u *unit, egress string) {
	for _, p := range r.policies[egress] {
		destination := defaultRouteV4
		if p.spec.FamilyOrDefault() == v1alpha1.FamilyIPv6 {
			destination = defaultRouteV6
		}

		route := u.section("Route")
		route.set("Destination", destination)
		route.setInt("Table", p.routing.Table)

		rule := u.section("RoutingPolicyRule")
		rule.set("Family", string(p.spec.FamilyOrDefault()))
		rule.setInt("FirewallMark", int(p.routing.Mark))
		rule.setInt("Table", p.routing.Table)
		rule.setInt("Priority", p.routing.Table)
	}
}
