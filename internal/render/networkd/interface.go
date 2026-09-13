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
	if spec.WakeOnLan != nil {
		r.renderLinkFile(iface)
	}

	// A link whose DUID has not been read gets no .network at all. Written without the
	// DUID, the prefix delegation would send networkd's own identifier, and the
	// delegated prefix would change under a host that already holds one: that is not a
	// smaller version of what was declared but a different configuration, so nothing of
	// the file is written and the DUID is waited for (ADR 0004, ADR 0016). The bridge and
	// the .link above depend on nothing that is read at apply time and are rendered as
	// usual.
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
	advertised := advertisedAddress(spec)
	advertise := advertisementOf(spec)

	network := u.section("Network")
	if spec.Bridge != nil {
		// A bridge has no carrier until a member does, and networkd would otherwise
		// leave it unconfigured until then. Its address is the segment's gateway and
		// the address the host's own services answer at; that must not wait for the
		// first client, nor go when the last one is unplugged (ADR 0018).
		network.setBool("ConfigureWithoutCarrier", true)
	}
	for _, address := range spec.Addresses {
		if address.IsLiteral() {
			network.set("Address", address.Literal.String())
		}
	}
	if r.carriesHostResolver(iface) {
		// The host asks its own dnsmasq. resolved reaches a loopback server through
		// the loopback link whichever link named it, and DNSDefaultRoute sends it
		// every name no other link claims (ADR 0018).
		network.set("DNS", "127.0.0.1")
		network.setBool("DNSDefaultRoute", true)
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
	// There is no field for accepting router advertisements. Two declarations need them:
	// the interface facing the provider, where the default route and the interface's own
	// global address come from, and an address taken from the advertisement, which exists
	// to take both from the router above. Everywhere else an advertisement would install
	// routing nobody declared, so it is refused.
	network.setBool("IPv6AcceptRA", spec.DHCPv6 != nil || advertised != nil)
	if advertise != nil {
		network.setBool("IPv6SendRA", true)
	}
	if delegated != nil {
		network.setBool("DHCPPrefixDelegation", true)
	}
	for _, tunnel := range r.tunnelOn[iface.Name] {
		network.set("Tunnel", tunnel)
	}

	acceptRA := r.renderDHCPv6Client(u, iface)
	if advertised != nil {
		// Only the token is written. The default route the advertisement carries, and the
		// DHCPv6 information request its other-configuration flag asks for, are
		// networkd's defaults, and no configuration has needed them otherwise.
		if acceptRA == nil {
			acceptRA = u.section("IPv6AcceptRA")
		}
		acceptRA.set("Token", "static:"+advertised.Token)
	}
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
		if address.FromDelegatedPrefix == nil {
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

// advertisedAddress is the interface's address taken from the router advertisement, or
// nil. The validation allows at most one.
func advertisedAddress(spec *v1alpha1.InterfaceSpec) *v1alpha1.RouterAdvertisementAddr {
	for _, address := range spec.Addresses {
		if address.FromRouterAdvertisement != nil {
			return address.FromRouterAdvertisement
		}
	}
	return nil
}

// renderDHCPv6Client writes the client that asks the provider for the prefix, and hands
// back the [IPv6AcceptRA] section it opened, or nil when there is no client.
//
// It asks for an address (IA_NA) alongside the prefix (IA_PD). Some providers never
// answer a Solicit that carries only IA_PD, and the usual answer to the IA_NA is "no
// address" while the prefix arrives in the IA_PD. networkd ties requesting the address
// to using it (UseAddress= drives sd_dhcp6_client_set_address_request), so the address,
// on the rare line that hands one out, lands on the upstream link. The interface's own
// global address still comes from the router advertisement, which is what makes a
// tunnel's Local=slaac mean something (ADR 0011). UseAddress=yes is networkd's default,
// written out so the intent is visible in the file (ADR 0012).
func (r *renderer) renderDHCPv6Client(u *unit, iface config.Named[*v1alpha1.InterfaceSpec]) *section {
	client := iface.Spec.DHCPv6
	if client == nil {
		return nil
	}
	dhcpv6 := u.section("DHCPv6")
	if delegation := client.PrefixDelegation; delegation != nil {
		// A line that advertises nothing would otherwise leave the client waiting for an
		// invitation that never comes. This makes it ask anyway.
		dhcpv6.set("WithoutRA", "solicit")
	}
	dhcpv6.setBool("UseAddress", true)
	dhcpv6.setBool("UseDNS", client.UseDNSEnabled())

	if delegation := client.PrefixDelegation; delegation != nil {
		dhcpv6.setBool("RapidCommit", delegation.RapidCommitEnabled())
		if delegation.PrefixLength != nil {
			dhcpv6.set("PrefixDelegationHint", "::/"+strconv.Itoa(*delegation.PrefixLength))
		}
		if delegation.DUIDFile != "" {
			r.renderDUID(dhcpv6, iface.Name, delegation.DUIDFile)
		}
		if delegation.IAID != nil {
			// The delegation can be bound to the DUID and the IAID together, and networkd's
			// own IAID is derived from the interface. A declared one is carried over as is;
			// an undeclared one is left to networkd (ADR 0012).
			dhcpv6.set("IAID", strconv.FormatUint(uint64(*delegation.IAID), 10))
		}
	}

	acceptRA := u.section("IPv6AcceptRA")
	if client.PrefixDelegation != nil {
		// WithoutRA covers the line that advertises nothing. This covers the line that
		// does advertise but sets neither the managed nor the other-configuration flag,
		// which networkd otherwise reads as "do not start the client". A provider can
		// delegate prefixes without ever setting M, so the delegation must not hinge on it.
		acceptRA.set("DHCPv6Client", "always")
	}
	// A provider's resolvers arrive by two roads, and the field means both.
	acceptRA.setBool("UseDNS", client.UseDNSEnabled())
	return acceptRA
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

// renderLinkFile writes what udev sets on the NIC itself. It is a .link file because
// networkd takes WakeOnLan= nowhere else, and udev, not networkd, applies it.
//
// It matches the declared name as OriginalName=. At the add event a NIC still has the name
// the kernel gave it and this file does not match, so the distribution's default file names
// the device; the rename sends a move event, the device then has the declared name, and this
// file is the first match. Names are given on add only, so nothing the default gave is lost
// (ADR 0020). The declaration holds no MAC, and there is no Name= match in a .link file.
func (r *renderer) renderLinkFile(iface config.Named[*v1alpha1.InterfaceSpec]) {
	u := newUnit(v1alpha1.KindInterface, iface.Name)
	u.section("Match").set("OriginalName", iface.Spec.Ifname)

	modes := make([]string, len(iface.Spec.WakeOnLan))
	for i, mode := range iface.Spec.WakeOnLan {
		modes[i] = string(mode)
	}
	u.section("Link").set("WakeOnLan", strings.Join(modes, " "))

	name := fileName(iface.Name, ".link")
	r.add(name, u)
	r.linkFiles = append(r.linkFiles, LinkFile{Name: name, Ifname: iface.Spec.Ifname})
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
