package config

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
)

// Validate says whether a parsed document is coherent, and returns the model that
// follows from it.
//
// It reports everything it finds in one pass. A document with six mistakes in it should
// take one run to see all six.
func Validate(document *v1alpha1.NetworkConfig, opts ...Option) (*Config, error) {
	v := &validator{document: document, files: resolveOptions(opts).files}

	v.buildIndex()
	v.checkDocument()
	v.checkSingletons()
	for _, resource := range v.order {
		v.checkResource(resource)
	}
	v.checkLinkNames()
	v.checkBridgeMembers()
	v.checkDerivationCycles()
	routing := v.deriveRouting()

	if v.problems.HasErrors() {
		return nil, &ValidationError{Problems: v.problems}
	}
	return &Config{
		document: document,
		order:    v.order,
		byKind:   v.byKind,
		index:    v.index,
		routing:  routing,
		warnings: v.problems.Warnings(),
	}, nil
}

type validator struct {
	document *v1alpha1.NetworkConfig
	files    FileChecker
	problems Problems

	order  []*v1alpha1.Resource
	byKind map[v1alpha1.ResourceKind][]*v1alpha1.Resource
	index  map[v1alpha1.ResourceKind]map[string]*v1alpha1.Resource
}

// buildIndex indexes the resources by kind and name, reporting a name used twice within
// a kind. The first of a pair keeps the name, so that everything referring to it still
// resolves and the run reports the duplicate rather than a cascade behind it.
func (v *validator) buildIndex() {
	v.byKind = make(map[v1alpha1.ResourceKind][]*v1alpha1.Resource)
	v.index = make(map[v1alpha1.ResourceKind]map[string]*v1alpha1.Resource)

	for i := range v.document.Spec.Resources {
		resource := &v.document.Spec.Resources[i]
		v.order = append(v.order, resource)
		v.byKind[resource.Kind] = append(v.byKind[resource.Kind], resource)

		name := resource.Metadata.Name
		if name == "" {
			v.errorf(resource, "metadata.name", "required")
			continue
		}
		byName := v.index[resource.Kind]
		if byName == nil {
			byName = make(map[string]*v1alpha1.Resource)
			v.index[resource.Kind] = byName
		}
		if _, taken := byName[name]; taken {
			v.errorf(resource, "metadata.name", "another %s is already named %q", resource.Kind, name)
			continue
		}
		byName[name] = resource
	}
}

func (v *validator) checkDocument() {
	if v.document.Metadata.Name == "" {
		v.errorf(nil, "metadata.name", "required")
	}
	// Policy routing makes return paths asymmetric by design, and strict reverse path
	// filtering drops exactly that traffic.
	if v.document.Spec.Global.SourceValidationEnabled() && len(v.byKind[v1alpha1.KindEgressRoutePolicy]) > 0 {
		v.errorf(nil, "spec.global.sourceValidation",
			"cannot be true while an EgressRoutePolicy is declared: policy routing makes return paths asymmetric, and reverse path filtering drops exactly that traffic")
	}
}

// checkSingletons refuses a second resource of a kind a host can only have one of.
//
// One dnsmasq serves the host, and it has one cache and one set of upstreams, so a
// second DNSForwarder asks the process for something it cannot do. The kind is shaped to
// need only one: listenOn, upstreams, conditional and staticHosts are all lists.
func (v *validator) checkSingletons() {
	forwarders := v.byKind[v1alpha1.KindDNSForwarder]
	if len(forwarders) < 2 {
		return
	}
	for _, resource := range forwarders[1:] {
		v.errorf(resource, "", "a host has at most one DNSForwarder, and %q is already declared: one dnsmasq serves the host, and it has one cache and one set of upstreams",
			forwarders[0].Metadata.Name)
	}
}

func (v *validator) checkResource(resource *v1alpha1.Resource) {
	switch spec := resource.Spec.(type) {
	case *v1alpha1.InterfaceSpec:
		v.checkInterface(resource, spec)
	case *v1alpha1.PPPoESessionSpec:
		v.checkPPPoESession(resource, spec)
	case *v1alpha1.DSLiteTunnelSpec:
		v.checkDSLiteTunnel(resource, spec)
	case *v1alpha1.EgressRoutePolicySpec:
		v.checkEgressRoutePolicy(resource, spec)
	case *v1alpha1.IPAddressSetSpec:
		v.checkIPAddressSet(resource, spec)
	case *v1alpha1.FirewallZoneSpec:
		v.checkFirewallZone(resource, spec)
	case *v1alpha1.FirewallPolicySpec:
		v.checkFirewallPolicy(resource, spec)
	case *v1alpha1.SourceNATSpec:
		v.checkSourceNAT(resource, spec)
	case *v1alpha1.PortForwardSpec:
		v.checkPortForward(resource, spec)
	case *v1alpha1.DHCPServerSpec:
		v.checkDHCPServer(resource, spec)
	case *v1alpha1.DNSForwarderSpec:
		v.checkDNSForwarder(resource, spec)
	}
}

// --- the kinds -------------------------------------------------------------------

func (v *validator) checkInterface(resource *v1alpha1.Resource, spec *v1alpha1.InterfaceSpec) {
	v.required(resource, "spec.ifname", spec.Ifname != "")
	if spec.Bridge != nil {
		v.required(resource, "spec.bridge.members", len(spec.Bridge.Members) > 0)
	}

	for i, address := range spec.Addresses {
		field := fmt.Sprintf("spec.addresses[%d]", i)
		if address.IsLiteral() {
			continue
		}
		derived := address.FromDelegatedPrefix
		if !v.required(resource, field+".fromDelegatedPrefix.interfaceRef", derived.InterfaceRef != "") {
			continue
		}
		v.required(resource, field+".fromDelegatedPrefix.subnetID", derived.SubnetID != nil)

		upstream := v.resolveInterface(resource, field+".fromDelegatedPrefix.interfaceRef", derived.InterfaceRef)
		if upstream == nil {
			continue
		}
		// The address is a slice of a prefix that has to be asked for. An upstream that
		// asks for nothing hands out nothing.
		upstreamSpec := upstream.Spec.(*v1alpha1.InterfaceSpec)
		if upstreamSpec.DHCPv6 == nil || upstreamSpec.DHCPv6.PrefixDelegation == nil {
			v.errorf(resource, field+".fromDelegatedPrefix.interfaceRef",
				"the Interface %q has no prefix-delegation client", derived.InterfaceRef)
		}
	}

	v.checkRoutes(resource, "spec.routes", spec.Routes)

	if spec.DHCPv6 != nil && spec.DHCPv6.PrefixDelegation != nil {
		delegation := spec.DHCPv6.PrefixDelegation
		v.required(resource, "spec.dhcpv6.prefixDelegation.prefixLength", delegation.PrefixLength != nil)
		if delegation.DUIDFile != "" {
			v.checkFile(resource, "spec.dhcpv6.prefixDelegation.duidFile", delegation.DUIDFile)
		} else {
			// A line being brought up for the first time has no DUID to carry over, so
			// this is legitimate. A host replacing one that already holds a delegation
			// and omitting it is delegated a different prefix, quietly.
			v.warnf(resource, "spec.dhcpv6.prefixDelegation.duidFile",
				"not set; networkd will send a DUID of its own, and a host replacing one that already holds a delegation will be delegated a different prefix")
		}
	}

	if spec.IPv6 != nil && spec.IPv6.Advertise != nil {
		v.required(resource, "spec.ipv6.advertise.mode", spec.IPv6.Advertise.Mode != "")
	}
}

func (v *validator) checkPPPoESession(resource *v1alpha1.Resource, spec *v1alpha1.PPPoESessionSpec) {
	if v.required(resource, "spec.interfaceRef", spec.InterfaceRef != "") {
		v.resolveInterface(resource, "spec.interfaceRef", spec.InterfaceRef)
	}
	if v.required(resource, "spec.userIDFile", spec.UserIDFile != "") {
		v.checkFile(resource, "spec.userIDFile", spec.UserIDFile)
	}
	if v.required(resource, "spec.passwordFile", spec.PasswordFile != "") {
		v.checkFile(resource, "spec.passwordFile", spec.PasswordFile)
	}
	v.checkRoutes(resource, "spec.routes", spec.Routes)
}

func (v *validator) checkDSLiteTunnel(resource *v1alpha1.Resource, spec *v1alpha1.DSLiteTunnelSpec) {
	if v.required(resource, "spec.underlayRef", spec.UnderlayRef != "") {
		v.resolveInterface(resource, "spec.underlayRef", spec.UnderlayRef)
	}

	if v.exactlyOne(resource, "localAddressFrom", "localAddress", spec.LocalAddressFrom != nil, spec.LocalAddress != nil) &&
		spec.LocalAddressFrom != nil {
		if v.required(resource, "spec.localAddressFrom.interfaceRef", spec.LocalAddressFrom.InterfaceRef != "") {
			v.resolveInterface(resource, "spec.localAddressFrom.interfaceRef", spec.LocalAddressFrom.InterfaceRef)
		}
	}
	v.exactlyOne(resource, "aftrHost", "aftrAddress", spec.AFTRHost != "", spec.AFTRAddress != nil)

	v.checkRoutes(resource, "spec.routes", spec.Routes)
}

func (v *validator) checkEgressRoutePolicy(resource *v1alpha1.Resource, spec *v1alpha1.EgressRoutePolicySpec) {
	v.required(resource, "spec.priority", spec.Priority != nil)
	if v.required(resource, "spec.egressRef", spec.EgressRef != "") {
		v.resolveUplink(resource, "spec.egressRef", spec.EgressRef)
	}
	if len(spec.SourceRanges) == 0 && len(spec.SourceAddressSetRefs) == 0 {
		v.errorf(resource, "spec", "at least one of sourceRanges and sourceAddressSetRefs is required")
	}
	for i, ref := range spec.SourceAddressSetRefs {
		v.resolveAddressSet(resource, fmt.Sprintf("spec.sourceAddressSetRefs[%d]", i), ref, spec.FamilyOrDefault())
	}

	// Priority is what orders the routing policy rules within a family. Two policies
	// sharing one leaves the order to whatever the kernel does with a tie.
	if spec.Priority == nil {
		return
	}
	for _, other := range v.byKind[v1alpha1.KindEgressRoutePolicy] {
		if other == resource {
			break
		}
		otherSpec, ok := other.Spec.(*v1alpha1.EgressRoutePolicySpec)
		if !ok || otherSpec.Priority == nil {
			continue
		}
		if otherSpec.FamilyOrDefault() == spec.FamilyOrDefault() && *otherSpec.Priority == *spec.Priority {
			v.errorf(resource, "spec.priority", "the %s EgressRoutePolicy %q already has priority %d",
				spec.FamilyOrDefault(), other.Metadata.Name, *spec.Priority)
			break
		}
	}
}

func (v *validator) checkIPAddressSet(resource *v1alpha1.Resource, spec *v1alpha1.IPAddressSetSpec) {
	v.required(resource, "spec.family", spec.Family != "")
	if len(spec.Addresses) == 0 && len(spec.Networks) == 0 {
		v.errorf(resource, "spec", "at least one of addresses and networks is required")
	}
	if spec.Family == "" {
		return
	}
	for i, address := range spec.Addresses {
		if familyOf(address.Addr) != spec.Family {
			v.errorf(resource, fmt.Sprintf("spec.addresses[%d]", i), "%s is not %s", address, spec.Family)
		}
	}
	for i, network := range spec.Networks {
		if familyOf(network.Addr()) != spec.Family {
			v.errorf(resource, fmt.Sprintf("spec.networks[%d]", i), "%s is not %s", network, spec.Family)
		}
	}
}

func (v *validator) checkFirewallZone(resource *v1alpha1.Resource, spec *v1alpha1.FirewallZoneSpec) {
	if resource.Metadata.Name == v1alpha1.SelfZone {
		v.errorf(resource, "metadata.name", "%q is reserved for the host itself", v1alpha1.SelfZone)
	}
	if !v.required(resource, "spec.linkRefs", len(spec.LinkRefs) > 0) {
		return
	}
	for i, ref := range spec.LinkRefs {
		v.resolveLink(resource, fmt.Sprintf("spec.linkRefs[%d]", i), ref)
	}
}

func (v *validator) checkFirewallPolicy(resource *v1alpha1.Resource, spec *v1alpha1.FirewallPolicySpec) {
	if v.required(resource, "spec.from", spec.From != "") {
		if spec.From == v1alpha1.SelfZone {
			// The netfilter hook follows from the pair, and nothing here filters what
			// the host itself sends.
			v.errorf(resource, "spec.from", "%q cannot be a policy's source: nothing here filters what the host itself sends", v1alpha1.SelfZone)
		} else {
			v.resolveZone(resource, "spec.from", spec.From)
		}
	}
	if v.required(resource, "spec.to", spec.To != "") && spec.To != v1alpha1.SelfZone {
		v.resolveZone(resource, "spec.to", spec.To)
	}
	v.required(resource, "spec.defaultAction", spec.DefaultAction != "")

	for i, rule := range spec.Rules {
		field := fmt.Sprintf("spec.rules[%d]", i)
		v.required(resource, field+".name", rule.Name != "")
		v.required(resource, field+".action", rule.Action != "")
		for j, ref := range rule.SourceAddressSetRefs {
			v.resolveAddressSet(resource, fmt.Sprintf("%s.sourceAddressSetRefs[%d]", field, j), ref, rule.Family)
		}
		for j, ref := range rule.DestinationAddressSetRefs {
			v.resolveAddressSet(resource, fmt.Sprintf("%s.destinationAddressSetRefs[%d]", field, j), ref, rule.Family)
		}
	}

	// Only one policy may exist for a given pair, or the two would both be dispatched to
	// and the second would never be reached.
	for _, other := range v.byKind[v1alpha1.KindFirewallPolicy] {
		if other == resource {
			break
		}
		otherSpec, ok := other.Spec.(*v1alpha1.FirewallPolicySpec)
		if ok && otherSpec.From == spec.From && otherSpec.To == spec.To {
			v.errorf(resource, "spec", "the FirewallPolicy %q already covers %s to %s", other.Metadata.Name, spec.From, spec.To)
			break
		}
	}
}

func (v *validator) checkSourceNAT(resource *v1alpha1.Resource, spec *v1alpha1.SourceNATSpec) {
	if !v.required(resource, "spec.egressRef", spec.EgressRef != "") {
		return
	}
	uplink := v.resolveUplink(resource, "spec.egressRef", spec.EgressRef)
	if uplink != nil && uplink.Kind == v1alpha1.KindDSLiteTunnel {
		// The AFTR translates at the far end; a masquerade here would translate twice.
		v.errorf(resource, "spec.egressRef", "the DSLiteTunnel %q is translated by the AFTR, so translating here would translate twice", spec.EgressRef)
	}
}

func (v *validator) checkPortForward(resource *v1alpha1.Resource, spec *v1alpha1.PortForwardSpec) {
	if v.required(resource, "spec.egressRef", spec.EgressRef != "") {
		uplink := v.resolveUplink(resource, "spec.egressRef", spec.EgressRef)
		if uplink != nil && uplink.Kind == v1alpha1.KindDSLiteTunnel {
			// The AFTR holds the address the outside world would have to reach.
			v.errorf(resource, "spec.egressRef", "nothing can be published through the DSLiteTunnel %q: it is translated at the far end", spec.EgressRef)
		}
	}
	if v.required(resource, "spec.protocol", !spec.Protocol.IsZero()) &&
		spec.Protocol.Name != "tcp" && spec.Protocol.Name != "udp" {
		v.errorf(resource, "spec.protocol", "a port forward is tcp or udp, not %s", spec.Protocol)
	}
	listens := v.exactlyOne(resource, "port", "portRange", spec.Port != nil, spec.PortRange != nil)

	if spec.Target == nil || !spec.Target.Address.IsValid() {
		v.errorf(resource, "spec.target.address", "required")
		return
	}
	v.exclusive(resource, "spec.target", "port", "portRange", spec.Target.Port != nil, spec.Target.PortRange != nil)

	// A range has to be translated onto a range of the same width. Where the widths
	// differ, which outside port lands on which inside one is decided by the kernel and
	// written down nowhere, so reading the configuration stops telling you what the host
	// does. Leaving the target ports out keeps the width by construction.
	if !listens {
		return
	}
	listen, target := spec.Ports(), spec.TargetPorts()
	if listen.Width() == target.Width() {
		return
	}
	field := "spec.target.port"
	if spec.Target.PortRange != nil {
		field = "spec.target.portRange"
	}
	v.errorf(resource, field, "%s covers %s, but the forward listens on %s, which covers %d",
		target, ports(target.Width()), listen, listen.Width())
}

func ports(n int) string {
	if n == 1 {
		return "1 port"
	}
	return fmt.Sprintf("%d ports", n)
}

func (v *validator) checkDHCPServer(resource *v1alpha1.Resource, spec *v1alpha1.DHCPServerSpec) {
	var iface *v1alpha1.Resource
	if v.required(resource, "spec.interfaceRef", spec.InterfaceRef != "") {
		iface = v.resolveInterface(resource, "spec.interfaceRef", spec.InterfaceRef)
	}
	hasSubnet := v.required(resource, "spec.subnet", spec.Subnet.IsValid())

	hasPool := v.required(resource, "spec.pool", spec.Pool != nil)
	if hasPool {
		hasPool = v.required(resource, "spec.pool.start", spec.Pool.Start.IsValid()) &&
			v.required(resource, "spec.pool.end", spec.Pool.End.IsValid())
	}
	if hasPool && spec.Pool.End.Less(spec.Pool.Start.Addr) {
		v.errorf(resource, "spec.pool", "%s-%s ends before it starts", spec.Pool.Start, spec.Pool.End)
		hasPool = false
	}

	for i, mapping := range spec.StaticMappings {
		field := fmt.Sprintf("spec.staticMappings[%d]", i)
		v.required(resource, field+".name", mapping.Name != "")
		v.required(resource, field+".macAddress", len(mapping.MACAddress.HardwareAddr) > 0)
		if !v.required(resource, field+".address", mapping.Address.IsValid()) {
			continue
		}
		// Which side of the pool boundary a mapping falls on is a routing decision where
		// policy routing selects an uplink by source range, so it is worth being exact.
		if hasSubnet && !spec.Subnet.Masked().Contains(mapping.Address.Addr) {
			v.errorf(resource, field+".address", "%s is not inside %s", mapping.Address, spec.Subnet)
		}
		if hasPool && !mapping.Address.Less(spec.Pool.Start.Addr) && !spec.Pool.End.Less(mapping.Address.Addr) {
			v.errorf(resource, field+".address", "%s is inside the pool %s-%s", mapping.Address, spec.Pool.Start, spec.Pool.End)
		}
	}

	if spec.IPv6 == nil {
		return
	}
	v.required(resource, "spec.ipv6.mode", spec.IPv6.Mode != "")
	if iface == nil {
		return
	}
	// The advertisement is what tells a client to ask DHCPv6 for the rest. Without the
	// O flag, nothing ever asks.
	ifaceSpec := iface.Spec.(*v1alpha1.InterfaceSpec)
	advertises := ifaceSpec.IPv6 != nil && ifaceSpec.IPv6.Advertise != nil &&
		ifaceSpec.IPv6.Advertise.OtherInformationEnabled()
	if !advertises {
		v.warnf(resource, "spec.ipv6", "the Interface %q does not advertise otherInformation, so nothing will ask for this", spec.InterfaceRef)
	}
}

func (v *validator) checkDNSForwarder(resource *v1alpha1.Resource, spec *v1alpha1.DNSForwarderSpec) {
	if v.required(resource, "spec.listenOn", len(spec.ListenOn) > 0) {
		for i, ref := range spec.ListenOn {
			if ref == v1alpha1.LoopbackLink {
				continue
			}
			v.resolveLink(resource, fmt.Sprintf("spec.listenOn[%d]", i), ref)
		}
	}
	v.required(resource, "spec.upstreams", len(spec.Upstreams) > 0)

	for i, conditional := range spec.Conditional {
		field := fmt.Sprintf("spec.conditional[%d]", i)
		v.required(resource, field+".domain", conditional.Domain != "")
		v.required(resource, field+".servers", len(conditional.Servers) > 0)
	}
	for i, host := range spec.StaticHosts {
		field := fmt.Sprintf("spec.staticHosts[%d]", i)
		v.required(resource, field+".name", host.Name != "")
		v.required(resource, field+".address", host.Address.IsValid())
	}
}

func (v *validator) checkRoutes(resource *v1alpha1.Resource, field string, routes []v1alpha1.Route) {
	for i, route := range routes {
		v.required(resource, fmt.Sprintf("%s[%d].destination", field, i), route.Destination.IsValid())
	}
}

// --- names that reach the kernel -------------------------------------------------

// MaxLinkNameLength is the longest name the kernel accepts for a link. IFNAMSIZ is 16
// bytes and the last one is the terminator.
const MaxLinkNameLength = 15

// checkLinkNames refuses a name that would become a link the kernel cannot name.
//
// Which kinds put a link on the host is what ResourceKind.IsLink says. An Interface
// carries the kernel name in spec.ifname, and names the bridge members it enslaves the
// same way; a PPPoESession and a DSLiteTunnel are named after the resource, so that the
// firewall sees a stable name across redials.
//
// Without this the name is refused by the kernel partway through an apply, where what
// fails is a link that other resources are written against rather than the name that was
// too long.
func (v *validator) checkLinkNames() {
	for _, resource := range v.order {
		if !resource.Kind.IsLink() {
			continue
		}
		spec, isInterface := resource.Spec.(*v1alpha1.InterfaceSpec)
		if !isInterface {
			v.checkLinkName(resource, "metadata.name", resource.Metadata.Name,
				"the link this puts on the host is named after it, and ")
			continue
		}
		v.checkLinkName(resource, "spec.ifname", spec.Ifname, "")
		if spec.Bridge == nil {
			continue
		}
		for i, member := range spec.Bridge.Members {
			v.checkLinkName(resource, fmt.Sprintf("spec.bridge.members[%d]", i), member, "")
		}
	}
}

func (v *validator) checkLinkName(resource *v1alpha1.Resource, field, name, because string) {
	if len(name) <= MaxLinkNameLength {
		return
	}
	v.errorf(resource, field, "%q is %d characters; %sa link name the kernel takes is at most %d",
		name, len(name), because, MaxLinkNameLength)
}

// --- cross-resource checks -------------------------------------------------------

// checkBridgeMembers refuses an Interface that describes a port of a bridge and also
// carries an address. The addresses belong to the bridge, and so does the name a
// FirewallZone names.
func (v *validator) checkBridgeMembers() {
	memberOf := make(map[string]*v1alpha1.Resource)
	for _, resource := range v.byKind[v1alpha1.KindInterface] {
		spec, ok := resource.Spec.(*v1alpha1.InterfaceSpec)
		if !ok || spec.Bridge == nil {
			continue
		}
		for _, member := range spec.Bridge.Members {
			memberOf[member] = resource
		}
	}
	for _, resource := range v.byKind[v1alpha1.KindInterface] {
		spec, ok := resource.Spec.(*v1alpha1.InterfaceSpec)
		if !ok || len(spec.Addresses) == 0 {
			continue
		}
		bridge, isMember := memberOf[spec.Ifname]
		if !isMember {
			continue
		}
		bridgeSpec := bridge.Spec.(*v1alpha1.InterfaceSpec)
		v.errorf(resource, "spec.addresses", "%q is a member of the bridge %q and cannot carry addresses of its own", spec.Ifname, bridgeSpec.Ifname)
	}
}

// checkDerivationCycles refuses a value derived, directly or through others, from itself.
// The edges are the ones that take a value from another resource rather than merely name
// it: an address sliced out of a delegated prefix, and a tunnel's local address.
func (v *validator) checkDerivationCycles() {
	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := make(map[*v1alpha1.Resource]int)
	var stack []*v1alpha1.Resource

	var walk func(resource *v1alpha1.Resource) bool
	walk = func(resource *v1alpha1.Resource) bool {
		if state[resource] == done {
			return false
		}
		state[resource] = onStack
		stack = append(stack, resource)
		defer func() {
			stack = stack[:len(stack)-1]
			state[resource] = done
		}()

		for _, edge := range v.derivationEdges(resource) {
			if state[edge.to] == onStack {
				v.errorf(resource, edge.field, "reference cycle: %s", cyclePath(stack, edge.to))
				return true
			}
			if walk(edge.to) {
				return true
			}
		}
		return false
	}

	for _, resource := range v.order {
		walk(resource)
	}
}

type derivationEdge struct {
	to    *v1alpha1.Resource
	field string
}

func (v *validator) derivationEdges(resource *v1alpha1.Resource) []derivationEdge {
	var edges []derivationEdge
	add := func(name, field string) {
		if target := v.index[v1alpha1.KindInterface][name]; target != nil {
			edges = append(edges, derivationEdge{to: target, field: field})
		}
	}
	switch spec := resource.Spec.(type) {
	case *v1alpha1.InterfaceSpec:
		for i, address := range spec.Addresses {
			if address.FromDelegatedPrefix != nil {
				add(address.FromDelegatedPrefix.InterfaceRef, fmt.Sprintf("spec.addresses[%d].fromDelegatedPrefix.interfaceRef", i))
			}
		}
	case *v1alpha1.DSLiteTunnelSpec:
		if spec.LocalAddressFrom != nil {
			add(spec.LocalAddressFrom.InterfaceRef, "spec.localAddressFrom.interfaceRef")
		}
	}
	return edges
}

func cyclePath(stack []*v1alpha1.Resource, back *v1alpha1.Resource) string {
	start := 0
	for i, resource := range stack {
		if resource == back {
			start = i
			break
		}
	}
	names := make([]string, 0, len(stack)-start+1)
	for _, resource := range stack[start:] {
		names = append(names, resource.Ref())
	}
	return strings.Join(append(names, back.Ref()), " -> ")
}

// --- reference resolution --------------------------------------------------------

func (v *validator) resolveInterface(resource *v1alpha1.Resource, field, name string) *v1alpha1.Resource {
	return v.resolve(resource, field, name, []v1alpha1.ResourceKind{v1alpha1.KindInterface}, "an Interface")
}

func (v *validator) resolveZone(resource *v1alpha1.Resource, field, name string) *v1alpha1.Resource {
	return v.resolve(resource, field, name, []v1alpha1.ResourceKind{v1alpha1.KindFirewallZone}, "a FirewallZone")
}

func (v *validator) resolveUplink(resource *v1alpha1.Resource, field, name string) *v1alpha1.Resource {
	return v.resolve(resource, field, name,
		[]v1alpha1.ResourceKind{v1alpha1.KindPPPoESession, v1alpha1.KindDSLiteTunnel}, "an uplink")
}

func (v *validator) resolveLink(resource *v1alpha1.Resource, field, name string) *v1alpha1.Resource {
	return v.resolve(resource, field, name,
		[]v1alpha1.ResourceKind{v1alpha1.KindInterface, v1alpha1.KindPPPoESession, v1alpha1.KindDSLiteTunnel}, "a link")
}

// resolveAddressSet resolves a set and checks that it holds the family of the rule that
// names it. A rule with no family covers both, and any set suits it.
func (v *validator) resolveAddressSet(resource *v1alpha1.Resource, field, name string, family v1alpha1.Family) *v1alpha1.Resource {
	set := v.resolve(resource, field, name, []v1alpha1.ResourceKind{v1alpha1.KindIPAddressSet}, "an IPAddressSet")
	if set == nil || family == "" {
		return set
	}
	setSpec, ok := set.Spec.(*v1alpha1.IPAddressSetSpec)
	if ok && setSpec.Family != "" && setSpec.Family != family {
		v.errorf(resource, field, "the IPAddressSet %q is %s, but the rule is %s", name, setSpec.Family, family)
	}
	return set
}

// resolve finds a resource of one of the allowed kinds. A name that exists under another
// kind is reported as the wrong kind rather than as missing, because "no Interface named
// lan" is unhelpful when there is a FirewallZone called lan two screens up.
func (v *validator) resolve(resource *v1alpha1.Resource, field, name string, allowed []v1alpha1.ResourceKind, category string) *v1alpha1.Resource {
	for _, kind := range allowed {
		if found := v.index[kind][name]; found != nil {
			return found
		}
	}
	for _, kind := range v1alpha1.Kinds {
		if found := v.index[kind][name]; found != nil {
			v.errorf(resource, field, "%q is %s %s, not %s", name, article(string(kind)), kind, category)
			return nil
		}
	}
	v.errorf(resource, field, "no %s named %q", joinKinds(allowed), name)
	return nil
}

func joinKinds(kinds []v1alpha1.ResourceKind) string {
	names := make([]string, len(kinds))
	for i, kind := range kinds {
		names[i] = string(kind)
	}
	switch len(names) {
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
	}
}

func article(word string) string {
	if word == "" {
		return "a"
	}
	if strings.ContainsRune("AEIOU", rune(word[0])) {
		return "an"
	}
	return "a"
}

// --- small helpers ---------------------------------------------------------------

func (v *validator) required(resource *v1alpha1.Resource, field string, present bool) bool {
	if !present {
		v.errorf(resource, field, "required")
	}
	return present
}

// exactlyOne checks a pair of fields where the schema says one of them has to be written.
// Both halves are checked separately: a document with neither is the one that is easy to
// write by accident.
func (v *validator) exactlyOne(resource *v1alpha1.Resource, first, second string, hasFirst, hasSecond bool) bool {
	switch {
	case hasFirst && hasSecond:
		v.errorf(resource, "spec", "exactly one of %s and %s is required; both are set", first, second)
	case !hasFirst && !hasSecond:
		v.errorf(resource, "spec", "exactly one of %s and %s is required; neither is set", first, second)
	default:
		return true
	}
	return false
}

// exclusive checks a pair where neither is required but both together are meaningless.
func (v *validator) exclusive(resource *v1alpha1.Resource, field, first, second string, hasFirst, hasSecond bool) {
	if hasFirst && hasSecond {
		v.errorf(resource, field, "%s and %s cannot both be set", first, second)
	}
}

func (v *validator) checkFile(resource *v1alpha1.Resource, field, path string) {
	if err := v.files.CheckSecretFile(path); err != nil {
		v.errorf(resource, field, "%s: %v", path, err)
	}
}

func (v *validator) errorf(resource *v1alpha1.Resource, field, format string, args ...any) {
	v.report(SeverityError, resource, field, format, args...)
}

func (v *validator) warnf(resource *v1alpha1.Resource, field, format string, args ...any) {
	v.report(SeverityWarning, resource, field, format, args...)
}

func (v *validator) report(severity Severity, resource *v1alpha1.Resource, field, format string, args ...any) {
	problem := Problem{Severity: severity, Field: field, Message: fmt.Sprintf(format, args...)}
	if resource != nil {
		problem.Resource, problem.Line = resource.Ref(), resource.Line
	}
	v.problems = append(v.problems, problem)
}

func familyOf(address netip.Addr) v1alpha1.Family {
	if address.Is4() {
		return v1alpha1.FamilyIPv4
	}
	return v1alpha1.FamilyIPv6
}
