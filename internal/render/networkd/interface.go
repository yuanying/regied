package networkd

import (
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// The lifetimes a router advertisement uses when the configuration leaves them out. The
// preferred lifetime defaults to half the valid one.
const defaultValidLifetime = 24 * time.Hour

func (r *renderer) renderInterface(iface config.Named[*v1alpha1.InterfaceSpec]) {
	spec := iface.Spec
	if spec.Bridge != nil {
		r.renderBridgeNetDev(iface)
	}

	// A link whose DUID has not been read gets no file at all. Written without the
	// DUID, the prefix delegation would send networkd's own identifier, and the
	// delegated prefix would change under a host that already holds one: that is not a
	// smaller version of what was declared but a different configuration, so nothing of
	// the file is written and the DUID is waited for (ADR 0004, ADR 0016). The bridge
	// above depends on nothing that is read at apply time and is rendered as usual.
	if path, unread := r.unreadDUID(iface); unread {
		r.omit(v1alpha1.KindInterface, iface.Name,
			"the DUID file "+path+" to be read",
			fileName(iface.Name, ".network"))
		return
	}

	u := newUnit(v1alpha1.KindInterface, iface.Name)
	u.section("Match").set("Name", spec.Ifname)

	if spec.MTU != 0 {
		u.section("Link").setInt("MTUBytes", spec.MTU)
	}

	delegated := r.delegatedAddress(iface)
	advertise := advertisementOf(spec)

	network := u.section("Network")
	for _, address := range spec.Addresses {
		if address.IsLiteral() {
			network.set("Address", address.Literal.String())
		}
	}
	if bridge, enslaved := r.enslavedBy[spec.Ifname]; enslaved {
		// A port of a bridge does no IP of its own. The addresses, and the name a
		// FirewallZone names, belong to the bridge.
		network.setBool("LinkLocalAddressing", false)
		network.set("Bridge", bridge.Spec.Ifname)
	}
	if spec.DHCPv6 != nil {
		network.set("DHCP", "ipv6")
	}
	// There is no field for accepting router advertisements, and the interface facing
	// the provider is the one that has to: it is where the default route and the
	// interface's own global address come from. Everywhere else an advertisement would
	// install routing nobody declared, so it is refused.
	network.setBool("IPv6AcceptRA", spec.DHCPv6 != nil)
	if advertise != nil {
		network.setBool("IPv6SendRA", true)
	}
	if delegated != nil {
		network.setBool("DHCPPrefixDelegation", true)
	}
	for _, tunnel := range r.tunnelOn[iface.Name] {
		network.set("Tunnel", tunnel)
	}

	r.renderDHCPv6Client(u, iface)
	if delegated != nil {
		r.renderPrefixDelegation(u, iface, delegated, advertise != nil)
	}
	r.renderAdvertisement(u, iface, advertise, delegated != nil)
	renderRoutes(u, spec.Routes)

	r.add(fileName(iface.Name, ".network"), u)
}

// advertisementOf is the interface's router advertisement, or nil.
func advertisementOf(spec *v1alpha1.InterfaceSpec) *v1alpha1.RouterAdvertisement {
	if spec.IPv6 == nil {
		return nil
	}
	return spec.IPv6.Advertise
}

// delegatedAddress is the one address of the interface derived from a delegated prefix.
//
// networkd takes one [DHCPPrefixDelegation] per link, so a second one has nowhere to go.
// The schema does not refuse it, so the backend that cannot express it says so.
func (r *renderer) delegatedAddress(iface config.Named[*v1alpha1.InterfaceSpec]) *v1alpha1.DelegatedPrefixAddr {
	var found *v1alpha1.DelegatedPrefixAddr
	for _, address := range iface.Spec.Addresses {
		if address.IsLiteral() {
			continue
		}
		if found != nil {
			r.errorf("Interface/%s: networkd assigns one address per link from a delegated prefix, and this one declares two",
				iface.Name)
			return found
		}
		found = address.FromDelegatedPrefix
	}
	return found
}

// renderDHCPv6Client writes the client that asks the provider for the prefix.
//
// It asks for the prefix and nothing else: UseAddress=no keeps it from taking an address
// nobody declared, and the interface's own global address comes from the router
// advertisement instead. That is also what makes a tunnel's Local=slaac mean something
// (ADR 0011).
func (r *renderer) renderDHCPv6Client(u *unit, iface config.Named[*v1alpha1.InterfaceSpec]) {
	client := iface.Spec.DHCPv6
	if client == nil {
		return
	}
	dhcpv6 := u.section("DHCPv6")
	if delegation := client.PrefixDelegation; delegation != nil {
		// Providers commonly advertise without the managed or other-configuration flag,
		// and the client would then never ask. This makes it ask.
		dhcpv6.set("WithoutRA", "solicit")
	}
	dhcpv6.setBool("UseAddress", false)
	dhcpv6.setBool("UseDNS", client.UseDNSEnabled())

	if delegation := client.PrefixDelegation; delegation != nil {
		dhcpv6.setBool("RapidCommit", delegation.RapidCommitEnabled())
		if delegation.PrefixLength != nil {
			dhcpv6.set("PrefixDelegationHint", "::/"+strconv.Itoa(*delegation.PrefixLength))
		}
		if delegation.DUIDFile != "" {
			r.renderDUID(dhcpv6, iface.Name, delegation.DUIDFile)
		}
	}

	// A provider's resolvers arrive by two roads, and the field means both.
	u.section("IPv6AcceptRA").setBool("UseDNS", client.UseDNSEnabled())
}

// unreadDUID is the DUID file an interface names whose contents were not supplied, if
// there is one. An interface that names no file has nothing to wait for.
func (r *renderer) unreadDUID(iface config.Named[*v1alpha1.InterfaceSpec]) (string, bool) {
	client := iface.Spec.DHCPv6
	if client == nil || client.PrefixDelegation == nil || client.PrefixDelegation.DUIDFile == "" {
		return "", false
	}
	path := client.PrefixDelegation.DUIDFile
	_, ok := r.rt.DUIDs[path]
	return path, !ok
}

func (r *renderer) renderDUID(dhcpv6 *section, ifaceName, path string) {
	raw, ok := r.rt.DUIDs[path]
	if !ok {
		// renderInterface omits the whole file before getting here.
		r.errorf("Interface/%s: nothing was read from the DUID file %s", ifaceName, path)
		return
	}
	kind, data, err := splitDUID(raw)
	if err != nil {
		r.errorf("Interface/%s: the DUID file %s holds %v", ifaceName, path, err)
		return
	}
	dhcpv6.set("DUIDType", kind)
	dhcpv6.set("DUIDRawData", data)
}

// renderPrefixDelegation assigns this link its slice of the delegated prefix.
func (r *renderer) renderPrefixDelegation(u *unit, iface config.Named[*v1alpha1.InterfaceSpec], derived *v1alpha1.DelegatedPrefixAddr, announce bool) {
	upstream, ok := r.interfaces[derived.InterfaceRef]
	if !ok {
		r.errorf("Interface/%s: the interface %q it takes a delegated prefix from is not declared", iface.Name, derived.InterfaceRef)
		return
	}
	pd := u.section("DHCPPrefixDelegation")
	pd.set("UplinkInterface", upstream.Spec.Ifname)
	if derived.SubnetID != nil {
		// networkd reads a subnet ID as an RFC 4291 subnet identifier, which is written
		// in hexadecimal; the 0x is what keeps the two readings of "10" apart.
		pd.set("SubnetId", "0x"+strconv.FormatInt(int64(*derived.SubnetID), 16))
	}
	if derived.Token != "" {
		pd.set("Token", "static:"+derived.Token)
	}
	pd.setBool("Assign", true)
	pd.setBool("Announce", announce)
}

// renderAdvertisement writes what this link tells the segment below it.
func (r *renderer) renderAdvertisement(u *unit, iface config.Named[*v1alpha1.InterfaceSpec], advertise *v1alpha1.RouterAdvertisement, delegated bool) {
	if advertise == nil {
		return
	}
	ra := u.section("IPv6SendRA")
	// Addresses come from the prefix, never from a DHCPv6 server: the schema has no
	// stateful mode. The other-configuration flag is what sends a client to DHCPv6 for
	// the rest, and a DHCPServer answers that.
	ra.setBool("Managed", false)
	ra.setBool("OtherInformation", advertise.OtherInformationEnabled())
	ra.setBool("EmitDNS", len(advertise.DNSServers) > 0)
	for _, server := range advertise.DNSServers {
		ra.set("DNS", server.String())
	}

	valid := advertise.ValidLifetime.OrDefault(defaultValidLifetime)
	preferred := advertise.PreferredLifetime.OrDefault(valid / 2)

	var prefixes int
	for _, address := range iface.Spec.Addresses {
		if !address.IsLiteral() || !isIPv6(address.Literal.Addr()) {
			continue
		}
		prefixes++
		prefix := u.section("IPv6Prefix")
		prefix.set("Prefix", address.Literal.Masked().String())
		prefix.setInt("PreferredLifetimeSec", int(preferred.Seconds()))
		prefix.setInt("ValidLifetimeSec", int(valid.Seconds()))
	}

	// A delegated prefix is announced by [DHCPPrefixDelegation], which takes the
	// lifetimes from the delegation itself: there is no directive to override them.
	// Writing one that goes nowhere is worth saying out loud.
	if prefixes == 0 && delegated && (advertise.ValidLifetime != 0 || advertise.PreferredLifetime != 0) {
		r.warnf("Interface/%s: ipv6.advertise validLifetime and preferredLifetime are not applied, because this interface advertises only its delegated prefix and networkd takes that prefix's lifetimes from the delegation",
			iface.Name)
	}
}

func isIPv6(addr netip.Addr) bool { return addr.Is6() && !addr.Is4In6() }

// renderBridgeNetDev creates the bridge itself.
//
// Spanning tree and VLAN filtering are off, and the remaining timers are the kernel's.
// Joining several ports into one segment is all the schema claims here; a bridge that
// has to do more than that is a different requirement.
func (r *renderer) renderBridgeNetDev(iface config.Named[*v1alpha1.InterfaceSpec]) {
	u := newUnit(v1alpha1.KindInterface, iface.Name)
	netdev := u.section("NetDev")
	netdev.set("Name", iface.Spec.Ifname)
	netdev.set("Kind", "bridge")

	bridge := u.section("Bridge")
	bridge.setBool("STP", false)
	bridge.setBool("VLANFiltering", false)

	r.add(fileName(iface.Name, ".netdev"), u)
}

// renderBridgeMembers enslaves the members that are not Interface resources of their
// own. One that is has been enslaved by its own file already.
func (r *renderer) renderBridgeMembers() {
	declared := make(map[string]bool, len(r.interfaces))
	for _, iface := range r.interfaces {
		declared[iface.Spec.Ifname] = true
	}

	for _, iface := range config.ResourcesOf[*v1alpha1.InterfaceSpec](r.cfg) {
		if iface.Spec.Bridge == nil {
			continue
		}
		for _, member := range iface.Spec.Bridge.Members {
			if declared[member] {
				continue
			}
			u := newUnit(v1alpha1.KindInterface, iface.Name)
			u.section("Match").set("Name", member)

			network := u.section("Network")
			network.setBool("LinkLocalAddressing", false)
			network.set("Bridge", iface.Spec.Ifname)
			network.setBool("IPv6AcceptRA", false)

			r.add(fileName(iface.Name+"-"+member, ".network"), u)
		}
	}
}

// renderRoutes writes the static routes that leave by a link. A route with no next hop
// is on-link through it.
func renderRoutes(u *unit, routes []v1alpha1.Route) {
	for _, r := range routes {
		route := u.section("Route")
		route.set("Destination", r.Destination.String())
		if r.Via != nil {
			route.set("Gateway", r.Via.String())
		}
		if r.Metric != nil {
			route.setInt("Metric", *r.Metric)
		}
	}
}

// splitDUID takes the DUID as it was written — colon-separated hex, type prefix included
// — and hands back the two halves networkd wants apart: the type by the name networkd
// knows it as, and the payload.
func splitDUID(raw string) (kind, data string, err error) {
	fields := strings.Split(strings.TrimSpace(raw), ":")
	if len(fields) < 3 {
		return "", "", errDUID("too few octets to be a DUID")
	}
	for _, field := range fields {
		if len(field) != 2 || strings.IndexFunc(field, notHex) >= 0 {
			return "", "", errDUID("something that is not colon-separated hexadecimal")
		}
	}
	number, convErr := strconv.ParseUint(fields[0]+fields[1], 16, 16)
	if convErr != nil {
		return "", "", errDUID("a type that is not a number")
	}
	return duidTypeName(uint16(number)), strings.Join(fields[2:], ":"), nil
}

func notHex(r rune) bool {
	return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F')
}

// duidTypeName is what networkd calls each DUID type. A type it has no name for is
// written as its number, which networkd also takes.
func duidTypeName(number uint16) string {
	switch number {
	case 1:
		return "link-layer-time"
	case 2:
		return "vendor"
	case 3:
		return "link-layer"
	case 4:
		return "uuid"
	default:
		return strconv.Itoa(int(number))
	}
}

type errDUID string

func (e errDUID) Error() string { return string(e) }
