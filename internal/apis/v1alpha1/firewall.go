package v1alpha1

// EgressRoutePolicySpec says which uplink a class of traffic leaves by. One resource
// covers both halves: the match that identifies the traffic, and the routing that follows
// from it.
//
// Table and Mark are the two numbers the operator does not have to keep consistent by
// hand. Left out, regied allocates them; see internal/config's derivation.
type EgressRoutePolicySpec struct {
	Family               Family        `yaml:"family"`
	Priority             *int          `yaml:"priority"`
	EgressRef            string        `yaml:"egressRef"`
	SourceRanges         []SourceRange `yaml:"sourceRanges"`
	SourceAddressSetRefs []string      `yaml:"sourceAddressSetRefs"`
	ExcludeDestinations  []Prefix      `yaml:"excludeDestinations"`
	Table                *int          `yaml:"table"`
	Mark                 *uint32       `yaml:"mark"`
}

func (*EgressRoutePolicySpec) ResourceKind() ResourceKind { return KindEgressRoutePolicy }

// FamilyOrDefault is the family the policy matches on.
func (s EgressRoutePolicySpec) FamilyOrDefault() Family {
	if s.Family == "" {
		return FamilyIPv4
	}
	return s.Family
}

// IPAddressSetSpec is a named set of addresses or prefixes, so that a group of hosts can
// be written once and referred to from several rules.
type IPAddressSetSpec struct {
	Family    Family   `yaml:"family"`
	Addresses []Addr   `yaml:"addresses"`
	Networks  []Prefix `yaml:"networks"`
}

func (*IPAddressSetSpec) ResourceKind() ResourceKind { return KindIPAddressSet }

// FirewallZoneSpec is a named set of links. Zones are what policies are written between.
//
// "wan" and "lan" are ordinary names for ordinary zones, not concepts the schema knows
// (ADR 0009). The name "self" is reserved and denotes the host itself.
type FirewallZoneSpec struct {
	LinkRefs []string `yaml:"linkRefs"`
}

func (*FirewallZoneSpec) ResourceKind() ResourceKind { return KindFirewallZone }

// FirewallPolicySpec is the rules that apply to traffic travelling from one zone to
// another, or from a zone to the host.
//
// The netfilter hook follows from the pair: a To of "self" is input, zone to zone is
// forward. There is no policy for output. Traffic between a pair of zones with no policy
// is dropped: a pair is not implicitly open because nobody wrote it down.
type FirewallPolicySpec struct {
	From          string         `yaml:"from"`
	To            string         `yaml:"to"`
	DefaultAction Action         `yaml:"defaultAction"`
	LogDefault    *bool          `yaml:"logDefault"`
	Stateful      *bool          `yaml:"stateful"`
	Rules         []FirewallRule `yaml:"rules"`
}

func (*FirewallPolicySpec) ResourceKind() ResourceKind { return KindFirewallPolicy }

// LogDefaultEnabled is whether traffic that reached the default action is logged. It
// defaults to true unless the default action is accept, where logging every accepted
// packet is not what anybody wants.
func (s FirewallPolicySpec) LogDefaultEnabled() bool {
	return boolOr(s.LogDefault, s.DefaultAction != ActionAccept)
}

// StatefulEnabled is whether the two rules every chain needs — accept established and
// related, drop invalid — are put at the top. Writing them by hand in every policy is how
// one gets forgotten.
func (s FirewallPolicySpec) StatefulEnabled() bool { return boolOr(s.Stateful, true) }

// FirewallRule is one rule of a policy. Rules are evaluated in order and the first match
// wins.
type FirewallRule struct {
	Name                      string     `yaml:"name"`
	Action                    Action     `yaml:"action"`
	Family                    Family     `yaml:"family"`
	Protocol                  *Protocol  `yaml:"protocol"`
	SourceCIDRs               []Prefix   `yaml:"sourceCIDRs"`
	SourceAddressSetRefs      []string   `yaml:"sourceAddressSetRefs"`
	SourcePorts               []PortSpec `yaml:"sourcePorts"`
	DestinationCIDRs          []Prefix   `yaml:"destinationCIDRs"`
	DestinationAddressSetRefs []string   `yaml:"destinationAddressSetRefs"`
	DestinationPorts          []PortSpec `yaml:"destinationPorts"`
	Log                       *bool      `yaml:"log"`
}

func (r FirewallRule) LogEnabled() bool { return boolOr(r.Log, false) }
