package nftables

import (
	"cmp"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// The names regied gives the things it owns. A prefix per kind keeps a zone and an
// address set that happen to share a name apart, and puts a letter at the front of every
// identifier whatever the resource was called.
const (
	zoneSetPrefix     = "zone_"
	addressSetPrefix  = "addrset_"
	policyChainPrefix = "policy_"
)

// The base chains.
//
// prerouting_mark is what makes policy routing work, and where it sits is the whole
// point: priority filter is after nat prerouting, so a packet that a port forward
// readdressed to a host inside is already carrying that address when the policies'
// destination exclusions are considered. That is what keeps a hairpinned connection
// local without anybody writing the uplink's global address down.
const (
	chainMark       = "prerouting_mark"
	chainDNAT       = "prerouting_nat"
	chainSNAT       = "postrouting_nat"
	chainMSS        = "forward_mss"
	chainInput      = "input"
	chainForward    = "forward"
	logPrefixMarker = "regied "
)

// Runtime is what a ruleset needs and a configuration cannot hold: the addresses an
// uplink is holding at the moment it is rendered.
//
// A masquerading uplink needs none of this — the address is taken from the link as the
// packet leaves — but the hairpin half of a port forward has to match on the address the
// clients inside resolved, and only the running host knows what that is. Keeping it in
// an argument is what leaves this package a pure function: the apply engine reads the
// link and fills this in, and a test fills it in with whatever it wants to render.
type Runtime struct {
	// UplinkAddresses is the global addresses each uplink holds, keyed by the uplink
	// resource's name. An uplink that is not up yet is absent, which is not an error:
	// what depends on the address is left out and says so.
	UplinkAddresses map[string][]netip.Addr
}

// Error is what Render returns for a configuration it cannot turn into a ruleset. It
// reports everything it found rather than the first thing, as validation does.
type Error struct{ Problems []string }

func (e *Error) Error() string {
	return "cannot render the nftables ruleset:\n  " + strings.Join(e.Problems, "\n  ")
}

// Render builds the table regied owns out of a validated configuration.
func Render(cfg *config.Config, runtime Runtime) (*Ruleset, error) {
	r := &renderer{cfg: cfg, runtime: runtime}
	ruleset := &Ruleset{Family: TableFamily, Table: TableName}

	r.renderSets(ruleset)
	r.renderPolicyChains(ruleset)
	r.renderMarkChain(ruleset)
	r.renderNATChains(ruleset)
	r.renderMSSChain(ruleset)
	r.renderFilterChains(ruleset)

	if len(r.problems) > 0 {
		return nil, &Error{Problems: r.problems}
	}
	return ruleset, nil
}

type renderer struct {
	cfg      *config.Config
	runtime  Runtime
	problems []string
}

func (r *renderer) failf(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

// --- sets ------------------------------------------------------------------------

func (r *renderer) renderSets(ruleset *Ruleset) {
	zones := config.ResourcesOf[*v1alpha1.FirewallZoneSpec](r.cfg)
	slices.SortFunc(zones, func(a, b config.Named[*v1alpha1.FirewallZoneSpec]) int {
		return cmp.Compare(a.Name, b.Name)
	})
	for _, zone := range zones {
		elements := make([]string, 0, len(zone.Spec.LinkRefs))
		for _, ref := range zone.Spec.LinkRefs {
			name, ok := r.linkName(ref)
			if !ok {
				r.failf("the FirewallZone %q names %q, which is not a link", zone.Name, ref)
				continue
			}
			elements = append(elements, strconv.Quote(name))
		}
		slices.Sort(elements)
		ruleset.Sets = append(ruleset.Sets, Set{
			Name:     r.identifier(zoneSetPrefix, "FirewallZone", zone.Name),
			Type:     "ifname",
			Elements: slices.Compact(elements),
			Comment:  fmt.Sprintf("FirewallZone %q", zone.Name),
		})
	}

	sets := config.ResourcesOf[*v1alpha1.IPAddressSetSpec](r.cfg)
	slices.SortFunc(sets, func(a, b config.Named[*v1alpha1.IPAddressSetSpec]) int {
		return cmp.Compare(a.Name, b.Name)
	})
	for _, set := range sets {
		addresses := slices.Clone(set.Spec.Addresses)
		slices.SortFunc(addresses, func(a, b v1alpha1.Addr) int { return a.Addr.Compare(b.Addr) })
		networks := slices.Clone(set.Spec.Networks)
		slices.SortFunc(networks, comparePrefixes)

		elements := make([]string, 0, len(addresses)+len(networks))
		for _, address := range addresses {
			elements = append(elements, address.String())
		}
		for _, network := range networks {
			elements = append(elements, network.String())
		}
		// A set holding prefixes is an interval set. A single address is a /128 or a /32
		// inside one, so the two forms live together.
		var flags []string
		if len(networks) > 0 {
			flags = []string{"interval"}
		}
		ruleset.Sets = append(ruleset.Sets, Set{
			Name:     r.identifier(addressSetPrefix, "IPAddressSet", set.Name),
			Type:     addressType(set.Spec.Family),
			Flags:    flags,
			Elements: elements,
			Comment:  fmt.Sprintf("IPAddressSet %q", set.Name),
		})
	}
}

// --- firewall policies -----------------------------------------------------------

// policies is every FirewallPolicy in the order the base chains dispatch to them: by the
// pair they are written between, so that the file reads the same whatever order the
// document happens to list them in.
func (r *renderer) policies() []config.Named[*v1alpha1.FirewallPolicySpec] {
	policies := config.ResourcesOf[*v1alpha1.FirewallPolicySpec](r.cfg)
	slices.SortFunc(policies, func(a, b config.Named[*v1alpha1.FirewallPolicySpec]) int {
		if order := cmp.Compare(a.Spec.From, b.Spec.From); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Spec.To, b.Spec.To); order != 0 {
			return order
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return policies
}

func (r *renderer) renderPolicyChains(ruleset *Ruleset) {
	for _, policy := range r.policies() {
		spec := policy.Spec
		var rules []Rule

		// The two rules every chain needs. Writing them by hand in every policy is how
		// one gets forgotten, so stateful is on unless it was turned off.
		if spec.StatefulEnabled() {
			rules = append(rules,
				Rule{Text: "ct state established,related counter accept"},
				Rule{Text: "ct state invalid counter drop"})
		}
		for _, rule := range spec.Rules {
			for _, text := range r.firewallRule(policy.Name, rule) {
				rules = append(rules, Rule{Text: text})
			}
		}

		// What happens to traffic no rule matched.
		parts := newParts()
		if spec.LogDefaultEnabled() {
			parts.addf("log prefix %q", logPrefixMarker+policy.Name+" default ")
		}
		parts.add("counter", string(spec.DefaultAction))
		parts.addf("comment %q", policy.Name+" default")
		rules = append(rules, Rule{Text: parts.String()})

		ruleset.Chains = append(ruleset.Chains, Chain{
			Name:    r.identifier(policyChainPrefix, "FirewallPolicy", policy.Name),
			Rules:   rules,
			Comment: fmt.Sprintf("FirewallPolicy %q: %s to %s", policy.Name, spec.From, spec.To),
		})
	}
}

// firewallRule is one rule of a policy, as the one or more nftables rules it takes.
//
// A rule that names both CIDRs and address sets means either of them, and a rule with no
// family whose CIDRs are of both means either family. Neither can be said in one
// nftables rule, so the alternatives are spelt out. First match wins either way, and the
// alternatives sit where the one rule would have.
func (r *renderer) firewallRule(policyName string, rule v1alpha1.FirewallRule) []string {
	sources := r.addressAlternatives("saddr", rule.SourceCIDRs, rule.SourceAddressSetRefs)
	destinations := r.addressAlternatives("daddr", rule.DestinationCIDRs, rule.DestinationAddressSetRefs)

	var out []string
	for _, source := range sources {
		for _, destination := range destinations {
			if _, ok := agree(rule.Family, source.family, destination.family); !ok {
				// The two halves are of different families, so this combination
				// matches nothing. The others still do.
				continue
			}
			parts := newParts()
			if rule.Family != "" {
				parts.addf("meta nfproto %s", rule.Family)
			}
			parts.add(r.protocolMatch(rule.Protocol, len(rule.SourcePorts)+len(rule.DestinationPorts) > 0))
			parts.add(source.text, destination.text)
			parts.add(portMatch(rule.Protocol, "sport", rule.SourcePorts))
			parts.add(portMatch(rule.Protocol, "dport", rule.DestinationPorts))
			if rule.LogEnabled() {
				parts.addf("log prefix %q", logPrefixMarker+policyName+" "+rule.Name+" ")
			}
			parts.add("counter", string(rule.Action))
			parts.addf("comment %q", rule.Name)
			out = append(out, parts.String())
		}
	}
	return out
}

// --- the matching half of policy routing -----------------------------------------

func (r *renderer) renderMarkChain(ruleset *Ruleset) {
	policies := config.ResourcesOf[*v1alpha1.EgressRoutePolicySpec](r.cfg)
	if len(policies) == 0 {
		return
	}
	// In the order the policies are evaluated in — by family, then by priority — which
	// is the order internal/config allocated their marks in.
	slices.SortFunc(policies, func(a, b config.Named[*v1alpha1.EgressRoutePolicySpec]) int {
		if order := cmp.Compare(a.Spec.FamilyOrDefault(), b.Spec.FamilyOrDefault()); order != 0 {
			return order
		}
		if order := cmp.Compare(priorityOf(a.Spec), priorityOf(b.Spec)); order != 0 {
			return order
		}
		return cmp.Compare(a.Name, b.Name)
	})

	var rules []Rule
	for _, policy := range policies {
		family := policy.Spec.FamilyOrDefault()
		routing, ok := r.cfg.PolicyRouting(policy.Name)
		if !ok {
			r.failf("no mark was derived for the EgressRoutePolicy %q", policy.Name)
			continue
		}
		exclusion := r.exclusionMatch(family, policy.Spec.ExcludeDestinations)

		for _, source := range r.sourceAlternatives(family, policy.Spec.SourceRanges, policy.Spec.SourceAddressSetRefs) {
			parts := newParts()
			parts.add(source, exclusion)
			// The mark, and then nothing else: a packet one policy claimed is not
			// offered to the ones behind it.
			parts.addf("meta mark set 0x%x", routing.Mark)
			parts.add("return")
			parts.addf("comment %q", "EgressRoutePolicy/"+policy.Name)
			rules = append(rules, Rule{Text: parts.String()})
		}
	}
	if len(rules) == 0 {
		return
	}
	ruleset.Chains = append(ruleset.Chains, Chain{
		Name:  chainMark,
		Base:  &BaseChain{Type: "filter", Hook: "prerouting", Priority: "filter", Policy: "accept"},
		Rules: rules,
		Comment: "the matching half of policy routing: which uplink a class of traffic leaves by.\n" +
			"It runs after nat prerouting, so a packet a port forward readdressed to a host\n" +
			"inside already carries that address when the exclusions below are considered,\n" +
			"and stays local. Nothing here has to know the uplink's global address",
	})
}

// sourceAlternatives is the source match of a policy routing rule, which is written
// either as ranges or as address sets, and may be both.
func (r *renderer) sourceAlternatives(family v1alpha1.Family, ranges []v1alpha1.SourceRange, refs []string) []string {
	header := familyHeader(family)
	var out []string

	values := make([]string, 0, len(ranges))
	sorted := slices.Clone(ranges)
	slices.SortFunc(sorted, compareSourceRanges)
	for _, source := range sorted {
		if familyOf(firstAddressOf(source)) != family {
			continue
		}
		values = append(values, source.String())
	}
	if len(values) > 0 {
		out = append(out, header+" saddr "+setOf(values))
	}
	for _, ref := range refs {
		out = append(out, header+" saddr @"+addressSetPrefix+ref)
	}
	return out
}

// exclusionMatch is what keeps traffic that must not leave by an uplink from being
// marked for it: the destinations of the family in hand, negated.
func (r *renderer) exclusionMatch(family v1alpha1.Family, destinations []v1alpha1.Prefix) string {
	values := prefixValues(family, destinations)
	if len(values) == 0 {
		return ""
	}
	return familyHeader(family) + " daddr != " + setOf(values)
}

// --- NAT -------------------------------------------------------------------------

func (r *renderer) renderNATChains(ruleset *Ruleset) {
	forwards := config.ResourcesOf[*v1alpha1.PortForwardSpec](r.cfg)
	slices.SortFunc(forwards, func(a, b config.Named[*v1alpha1.PortForwardSpec]) int {
		return cmp.Compare(a.Name, b.Name)
	})
	sources := config.ResourcesOf[*v1alpha1.SourceNATSpec](r.cfg)
	slices.SortFunc(sources, func(a, b config.Named[*v1alpha1.SourceNATSpec]) int {
		return cmp.Compare(a.Name, b.Name)
	})

	var dnat, snat []Rule
	for _, forward := range forwards {
		dnat = append(dnat, r.forwardDNAT(forward)...)
		snat = append(snat, r.forwardHairpinSNAT(forward)...)
	}
	for _, source := range sources {
		snat = append(snat, r.sourceNAT(source)...)
	}

	if len(dnat) > 0 {
		ruleset.Chains = append(ruleset.Chains, Chain{
			Name:    chainDNAT,
			Base:    &BaseChain{Type: "nat", Hook: "prerouting", Priority: "dstnat", Policy: "accept"},
			Rules:   dnat,
			Comment: "port forwards. This runs before the chain that marks, which is what lets a\nhairpinned connection fall out of a policy's local exclusion",
		})
	}
	if len(snat) > 0 {
		ruleset.Chains = append(ruleset.Chains, Chain{
			Name:    chainSNAT,
			Base:    &BaseChain{Type: "nat", Hook: "postrouting", Priority: "srcnat", Policy: "accept"},
			Rules:   snat,
			Comment: "translation on the way out: the hairpin half of a port forward, then the\nsource translation each uplink asked for",
		})
	}
}

// forwardDNAT is the translation itself: the one that catches the traffic arriving on
// the uplink, and the hairpin one that catches a host inside reaching the same service
// through the address the uplink holds.
func (r *renderer) forwardDNAT(forward config.Named[*v1alpha1.PortForwardSpec]) []Rule {
	spec := forward.Spec
	comment := "PortForward/" + forward.Name
	link, ok := r.linkName(spec.EgressRef)
	if !ok {
		r.failf("the PortForward %q leaves by %q, which is not a link", forward.Name, spec.EgressRef)
		return nil
	}
	if spec.Target == nil {
		r.failf("the PortForward %q has no target", forward.Name)
		return nil
	}
	family := familyOf(spec.Target.Address.Addr)
	translation := fmt.Sprintf("dnat %s to %s", familyHeader(family), targetOf(*spec.Target, spec.TargetPorts()))

	head := newParts()
	head.addf("iifname %q", link)
	head.addf("meta nfproto %s", family)
	head.add(portMatch(&spec.Protocol, "dport", []v1alpha1.PortSpec{spec.Ports()}))
	head.add(translation)
	head.addf("comment %q", comment)
	rules := []Rule{{Text: head.String()}}

	if !spec.HairpinEnabled() {
		return rules
	}
	// The address is the uplink's, and only the running host knows it. Without it the
	// external path still works; saying so is better than a rule built on a guess.
	addresses := r.uplinkAddresses(spec.EgressRef, family)
	if len(addresses) == 0 {
		return append(rules, Rule{Comment: fmt.Sprintf(
			"the hairpin translation for %s is not here: no address is known for the uplink %q",
			comment, spec.EgressRef)})
	}
	for _, address := range addresses {
		parts := newParts()
		parts.addf("iifname != %q", link)
		parts.addf("meta nfproto %s", family)
		parts.addf("%s daddr %s", familyHeader(family), address)
		parts.add(portMatch(&spec.Protocol, "dport", []v1alpha1.PortSpec{spec.Ports()}))
		parts.add(translation)
		parts.addf("comment %q", comment)
		rules = append(rules, Rule{Text: parts.String()})
	}
	return rules
}

// forwardHairpinSNAT is the other half of hairpin. Without it the target answers the
// client inside directly, the reply does not pass back through the translation, and the
// client discards it.
//
// The source address is not written down: masquerade takes it from the link the packet
// leaves by, which is the one the target is on. ct status dnat is what keeps this off a
// connection that was addressed to the target directly, and iifname != the uplink is
// what keeps it off a client outside, whose address the target should go on seeing.
func (r *renderer) forwardHairpinSNAT(forward config.Named[*v1alpha1.PortForwardSpec]) []Rule {
	spec := forward.Spec
	if !spec.HairpinEnabled() || spec.Target == nil {
		return nil
	}
	link, ok := r.linkName(spec.EgressRef)
	if !ok {
		return nil // already reported by forwardDNAT
	}
	family := familyOf(spec.Target.Address.Addr)

	parts := newParts()
	parts.addf("iifname != %q", link)
	parts.add("ct status dnat")
	parts.addf("meta nfproto %s", family)
	parts.addf("%s daddr %s", familyHeader(family), spec.Target.Address)
	parts.add(portMatch(&spec.Protocol, "dport", []v1alpha1.PortSpec{spec.TargetPorts()}))
	parts.add("masquerade")
	parts.addf("comment %q", "PortForward/"+forward.Name)
	return []Rule{{Text: parts.String()}}
}

// sourceNAT is the translation on the way out.
//
// masquerade carries no flags. random and fully-random would spread one host's
// connections over different external ports, which is the thing that stops a NAT
// mapping being endpoint-independent, and endpoint-independent is what everything
// behind it needs.
func (r *renderer) sourceNAT(source config.Named[*v1alpha1.SourceNATSpec]) []Rule {
	spec := source.Spec
	link, ok := r.linkName(spec.EgressRef)
	if !ok {
		r.failf("the SourceNAT %q leaves by %q, which is not a link", source.Name, spec.EgressRef)
		return nil
	}
	comment := "SourceNAT/" + source.Name

	// The families this resource named are the families it translates. Naming none
	// translates everything, which is what an omitted sourceRanges says.
	families := familiesOf(spec.SourceRanges, spec.ExcludeDestinations)
	if len(families) == 0 {
		parts := newParts()
		parts.addf("oifname %q", link)
		parts.add("masquerade")
		parts.addf("comment %q", comment)
		return []Rule{{Text: parts.String()}}
	}

	var rules []Rule
	for _, family := range families {
		parts := newParts()
		parts.addf("oifname %q", link)
		if values := prefixValues(family, spec.SourceRanges); len(values) > 0 {
			parts.addf("%s saddr %s", familyHeader(family), setOf(values))
		}
		if values := prefixValues(family, spec.ExcludeDestinations); len(values) > 0 {
			parts.addf("%s daddr != %s", familyHeader(family), setOf(values))
		}
		parts.add("masquerade")
		parts.addf("comment %q", comment)
		rules = append(rules, Rule{Text: parts.String()})
	}
	return rules
}

// --- MSS clamping ----------------------------------------------------------------

func (r *renderer) renderMSSChain(ruleset *Ruleset) {
	clamp := r.cfg.Global().MSSClamp.Resolved()
	var size string
	switch clamp.Mode {
	case v1alpha1.MSSClampOff:
		return
	case v1alpha1.MSSClampFixed:
		size = strconv.Itoa(clamp.Value)
	default:
		// The path's own MTU, so a tunnel is clamped as much as a PPPoE link is and
		// neither has to be named.
		size = "rt mtu"
	}
	ruleset.Chains = append(ruleset.Chains, Chain{
		Name: chainMSS,
		Base: &BaseChain{Type: "filter", Hook: "forward", Priority: "mangle", Policy: "accept"},
		Rules: []Rule{{
			Text: "tcp flags syn / syn,rst tcp option maxseg size set " + size,
		}},
		Comment: "clamp the segment size on every path whose MTU is lower. The masked flags match\n" +
			"the SYN and the SYN-ACK, so both ends are told",
	})
}

// --- the hook chains -------------------------------------------------------------

// renderFilterChains puts the input and forward chains in, but only for a host that
// declared a firewall. A configuration with no FirewallPolicy asked for none, and a
// default-drop chain it never asked for would take the host off the network (ADR 0009).
// Once one policy exists the pairs nobody wrote down are dropped, which is what the
// schema says they are.
func (r *renderer) renderFilterChains(ruleset *Ruleset) {
	policies := r.policies()
	if len(policies) == 0 {
		return
	}

	input := []Rule{{Text: `iif "lo" counter accept`, Comment: "the host talking to itself"}}
	var forward []Rule

	// The openings a port forward brings with it. They sit ahead of the zone dispatch
	// because what they let through is exactly what the pair's policy would otherwise
	// have to be widened for, and a hairpinned connection travels between two links of
	// the same zone, a pair nobody writes a policy for.
	forwards := config.ResourcesOf[*v1alpha1.PortForwardSpec](r.cfg)
	slices.SortFunc(forwards, func(a, b config.Named[*v1alpha1.PortForwardSpec]) int {
		return cmp.Compare(a.Name, b.Name)
	})
	for _, entry := range forwards {
		forward = append(forward, r.forwardOpening(entry)...)
	}

	for _, policy := range policies {
		chain := r.identifier(policyChainPrefix, "FirewallPolicy", policy.Name)
		from, ok := r.zoneSet(policy.Spec.From)
		if !ok {
			r.failf("the FirewallPolicy %q comes from %q, which is not a FirewallZone", policy.Name, policy.Spec.From)
			continue
		}
		if policy.Spec.To == v1alpha1.SelfZone {
			input = append(input, Rule{Text: fmt.Sprintf("iifname @%s jump %s", from, chain)})
			continue
		}
		to, ok := r.zoneSet(policy.Spec.To)
		if !ok {
			r.failf("the FirewallPolicy %q goes to %q, which is not a FirewallZone", policy.Name, policy.Spec.To)
			continue
		}
		forward = append(forward, Rule{Text: fmt.Sprintf("iifname @%s oifname @%s jump %s", from, to, chain)})
	}

	ruleset.Chains = append(ruleset.Chains,
		Chain{
			Name:    chainInput,
			Base:    &BaseChain{Type: "filter", Hook: "input", Priority: "filter", Policy: "drop"},
			Rules:   input,
			Comment: "traffic addressed to the host. A zone with no policy to self is dropped",
		},
		Chain{
			Name:    chainForward,
			Base:    &BaseChain{Type: "filter", Hook: "forward", Priority: "filter", Policy: "drop"},
			Rules:   forward,
			Comment: "traffic passing through. A pair of zones with no policy is dropped",
		})
}

// forwardOpening is the accept that lets a port forward's traffic through, in both
// directions. The reply needs one of its own: a hairpinned connection is answered to a
// client on the same zone as the target, and nobody writes a policy for that pair.
func (r *renderer) forwardOpening(forward config.Named[*v1alpha1.PortForwardSpec]) []Rule {
	spec := forward.Spec
	if !spec.OpenFirewallEnabled() || spec.Target == nil {
		return nil
	}
	family := familyOf(spec.Target.Address.Addr)
	comment := "PortForward/" + forward.Name
	ports := []v1alpha1.PortSpec{spec.TargetPorts()}

	var rules []Rule
	for _, side := range []struct{ address, port string }{{"daddr", "dport"}, {"saddr", "sport"}} {
		parts := newParts()
		parts.add("ct status dnat")
		parts.addf("meta nfproto %s", family)
		parts.addf("%s %s %s", familyHeader(family), side.address, spec.Target.Address)
		parts.add(portMatch(&spec.Protocol, side.port, ports))
		parts.add("counter", "accept")
		parts.addf("comment %q", comment)
		rules = append(rules, Rule{Text: parts.String()})
	}
	return rules
}

// --- names -----------------------------------------------------------------------

// What nftables reads back as one identifier. The prefix this package puts in front
// supplies the leading letter, so only the characters have to hold.
var nftIdentifier = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

func (r *renderer) identifier(prefix, kind, name string) string {
	if !nftIdentifier.MatchString(name) {
		r.failf("the %s named %q cannot be used as an nftables name: letters, digits and _ . / - only", kind, name)
	}
	return prefix + name
}

func (r *renderer) zoneSet(name string) (string, bool) {
	if r.cfg.Lookup(v1alpha1.KindFirewallZone, name) == nil {
		return "", false
	}
	return r.identifier(zoneSetPrefix, "FirewallZone", name), true
}

// linkName is the kernel interface name a link resource puts on the host. An Interface
// says which one; a PPPoE session and a DS-Lite tunnel are named after the resource, so
// that the firewall sees a stable name across redials.
//
// The kinds are tried in the order internal/config resolves a link reference in.
func (r *renderer) linkName(ref string) (string, bool) {
	if resource := r.cfg.Lookup(v1alpha1.KindInterface, ref); resource != nil {
		if spec, ok := resource.Spec.(*v1alpha1.InterfaceSpec); ok {
			return spec.Ifname, true
		}
	}
	for _, kind := range []v1alpha1.ResourceKind{v1alpha1.KindPPPoESession, v1alpha1.KindDSLiteTunnel} {
		if r.cfg.Lookup(kind, ref) != nil {
			return ref, true
		}
	}
	return "", false
}

func (r *renderer) uplinkAddresses(uplink string, family v1alpha1.Family) []netip.Addr {
	var out []netip.Addr
	for _, address := range r.runtime.UplinkAddresses[uplink] {
		if familyOf(address) == family {
			out = append(out, address)
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return out
}

// --- matches ---------------------------------------------------------------------

// addressMatch is one way a rule's source or destination was written, and the family
// that follows from it. A rule that named several is several rules.
type addressMatch struct {
	family v1alpha1.Family
	text   string
}

func (r *renderer) addressAlternatives(side string, cidrs []v1alpha1.Prefix, refs []string) []addressMatch {
	var out []addressMatch
	for _, family := range []v1alpha1.Family{v1alpha1.FamilyIPv4, v1alpha1.FamilyIPv6} {
		values := prefixValues(family, cidrs)
		if len(values) == 0 {
			continue
		}
		out = append(out, addressMatch{family: family, text: familyHeader(family) + " " + side + " " + setOf(values)})
	}
	for _, ref := range refs {
		set := r.cfg.Lookup(v1alpha1.KindIPAddressSet, ref)
		if set == nil {
			r.failf("no IPAddressSet named %q", ref)
			continue
		}
		spec, ok := set.Spec.(*v1alpha1.IPAddressSetSpec)
		if !ok {
			continue
		}
		out = append(out, addressMatch{
			family: spec.Family,
			text:   familyHeader(spec.Family) + " " + side + " @" + addressSetPrefix + ref,
		})
	}
	if len(out) == 0 {
		out = []addressMatch{{}}
	}
	return out
}

// protocolMatch is the transport a rule is about. It is written out even where a port
// match would imply it, so that every rule says which protocol it is on.
func (r *renderer) protocolMatch(protocol *v1alpha1.Protocol, hasPorts bool) string {
	if protocol == nil || protocol.IsZero() {
		if hasPorts {
			// Ports with no protocol are the ports of either transport that has them.
			return "meta l4proto { tcp, udp }"
		}
		return ""
	}
	return "meta l4proto " + protocolKeyword(*protocol)
}

// protocolKeyword is how nftables reads a protocol back.
//
// icmpv6 and ipip are the two the schema spells differently from netfilter. 58 is
// ipv6-icmp there, and 4 has no name nftables reads, so it goes in as the number the
// reference implementation uses.
func protocolKeyword(protocol v1alpha1.Protocol) string {
	switch protocol.Name {
	case "":
		return strconv.Itoa(protocol.Number)
	case "icmpv6":
		return "ipv6-icmp"
	case "ipip":
		return "4"
	default:
		return protocol.Name
	}
}

func portMatch(protocol *v1alpha1.Protocol, side string, ports []v1alpha1.PortSpec) string {
	if len(ports) == 0 {
		return ""
	}
	header := "th"
	if protocol != nil && (protocol.Name == "tcp" || protocol.Name == "udp") {
		header = protocol.Name
	}
	values := make([]string, 0, len(ports))
	for _, port := range ports {
		values = append(values, port.String())
	}
	return header + " " + side + " " + setOf(values)
}

// targetOf is where a port forward sends the traffic, in the form nftables takes an
// address and a port in. An IPv6 address goes in brackets, or the colon before the port
// would be read as part of it.
func targetOf(target v1alpha1.ForwardTarget, ports v1alpha1.PortSpec) string {
	address := target.Address.String()
	if target.Address.Is6() {
		address = "[" + address + "]"
	}
	return address + ":" + ports.String()
}

// --- small helpers ---------------------------------------------------------------

// parts assembles a rule out of the pieces that are there, in the order a rule reads:
// which link, which connection, which family, which protocol, which addresses, which
// ports, and then what to do about it.
type parts struct{ pieces []string }

func newParts() *parts { return &parts{} }

func (p *parts) add(pieces ...string) {
	for _, piece := range pieces {
		if piece != "" {
			p.pieces = append(p.pieces, piece)
		}
	}
}

func (p *parts) addf(format string, args ...any) { p.add(fmt.Sprintf(format, args...)) }

func (p *parts) String() string { return strings.Join(p.pieces, " ") }

// setOf is one value on its own, or several as an anonymous set.
func setOf(values []string) string {
	if len(values) == 1 {
		return values[0]
	}
	return "{ " + strings.Join(values, ", ") + " }"
}

func familyOf(address netip.Addr) v1alpha1.Family {
	if address.Is4() {
		return v1alpha1.FamilyIPv4
	}
	return v1alpha1.FamilyIPv6
}

// familyHeader is the header a match of that family is read out of.
func familyHeader(family v1alpha1.Family) string {
	if family == v1alpha1.FamilyIPv6 {
		return "ip6"
	}
	return "ip"
}

func addressType(family v1alpha1.Family) string {
	if family == v1alpha1.FamilyIPv6 {
		return "ipv6_addr"
	}
	return "ipv4_addr"
}

// agree is the family a rule is on, given what the rule said and what its two address
// matches imply. It is not ok when two of them disagree: that combination matches
// nothing, and writing it out would be a rule nft refuses.
func agree(families ...v1alpha1.Family) (v1alpha1.Family, bool) {
	var settled v1alpha1.Family
	for _, family := range families {
		if family == "" {
			continue
		}
		if settled != "" && settled != family {
			return "", false
		}
		settled = family
	}
	return settled, true
}

// familiesOf is the families a pair of prefix lists mentions, in a fixed order.
func familiesOf(lists ...[]v1alpha1.Prefix) []v1alpha1.Family {
	seen := map[v1alpha1.Family]bool{}
	for _, list := range lists {
		for _, prefix := range list {
			seen[familyOf(prefix.Addr())] = true
		}
	}
	var out []v1alpha1.Family
	for _, family := range []v1alpha1.Family{v1alpha1.FamilyIPv4, v1alpha1.FamilyIPv6} {
		if seen[family] {
			out = append(out, family)
		}
	}
	return out
}

// prefixValues is the prefixes of one family, in a fixed order.
func prefixValues(family v1alpha1.Family, prefixes []v1alpha1.Prefix) []string {
	sorted := make([]v1alpha1.Prefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		if familyOf(prefix.Addr()) == family {
			sorted = append(sorted, prefix)
		}
	}
	slices.SortFunc(sorted, comparePrefixes)
	values := make([]string, 0, len(sorted))
	for _, prefix := range sorted {
		values = append(values, prefix.String())
	}
	return values
}

func comparePrefixes(a, b v1alpha1.Prefix) int {
	if order := a.Addr().Compare(b.Addr()); order != 0 {
		return order
	}
	return cmp.Compare(a.Bits(), b.Bits())
}

func compareSourceRanges(a, b v1alpha1.SourceRange) int {
	if order := firstAddressOf(a).Compare(firstAddressOf(b)); order != 0 {
		return order
	}
	return cmp.Compare(a.String(), b.String())
}

func firstAddressOf(source v1alpha1.SourceRange) netip.Addr {
	if source.IsPrefix() {
		return source.Prefix.Addr()
	}
	return source.From
}

// priorityOf sorts a policy whose priority is missing last, as the derivation does.
func priorityOf(spec *v1alpha1.EgressRoutePolicySpec) int {
	if spec.Priority == nil {
		return 1 << 30
	}
	return *spec.Priority
}
