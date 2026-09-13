package v1alpha1

import (
	yaml "go.yaml.in/yaml/v3"
)

// InterfaceSpec is a link regied owns both ends of: a physical NIC, or a bridge over
// several of them, with everything that is a property of that link.
type InterfaceSpec struct {
	Ifname    string             `yaml:"ifname"`
	Bridge    *Bridge            `yaml:"bridge"`
	MTU       int                `yaml:"mtu"`
	WakeOnLan WakeOnLan          `yaml:"wakeOnLan"`
	Addresses []InterfaceAddress `yaml:"addresses"`
	Routes    []Route            `yaml:"routes"`
	DHCPv6    *DHCPv6Client      `yaml:"dhcpv6"`
	IPv6      *InterfaceIPv6     `yaml:"ipv6"`
}

func (*InterfaceSpec) ResourceKind() ResourceKind { return KindInterface }

// WakeOnLan is the modes a NIC wakes the host in, as networkd's WakeOnLan= takes them.
//
// Nil means the field was left out, and the NIC is left as it is; that is a different
// answer from off, which turns Wake-on-LAN off. A list written empty is neither, and is
// kept apart from nil so that validation can refuse it rather than read it as one of them.
type WakeOnLan []WakeOnLanMode

// WakeOnLanMode is one of networkd's WakeOnLan= words.
type WakeOnLanMode string

const (
	WakeOnLanOff       WakeOnLanMode = "off"
	WakeOnLanPhy       WakeOnLanMode = "phy"
	WakeOnLanUnicast   WakeOnLanMode = "unicast"
	WakeOnLanMulticast WakeOnLanMode = "multicast"
	WakeOnLanBroadcast WakeOnLanMode = "broadcast"
	WakeOnLanARP       WakeOnLanMode = "arp"
	WakeOnLanMagic     WakeOnLanMode = "magic"
	WakeOnLanSecureOn  WakeOnLanMode = "secureon"
)

func (m *WakeOnLanMode) UnmarshalYAML(node *yaml.Node) error {
	return enum(node, m, "a Wake-on-LAN mode", WakeOnLanOff, WakeOnLanPhy, WakeOnLanUnicast,
		WakeOnLanMulticast, WakeOnLanBroadcast, WakeOnLanARP, WakeOnLanMagic, WakeOnLanSecureOn)
}

// UnmarshalYAML accepts one mode or a list of them. The single form is what a host with
// one reason to be woken writes, and it means the same as a list holding that mode.
func (w *WakeOnLan) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var mode WakeOnLanMode
		if err := mode.UnmarshalYAML(node); err != nil {
			return err
		}
		*w = WakeOnLan{mode}
		return nil
	case yaml.SequenceNode:
		modes := make(WakeOnLan, 0, len(node.Content))
		for _, item := range node.Content {
			var mode WakeOnLanMode
			if err := mode.UnmarshalYAML(item); err != nil {
				return err
			}
			modes = append(modes, mode)
		}
		*w = modes
		return nil
	default:
		return typeErrorf(node, "expected a Wake-on-LAN mode or a list of them")
	}
}

// Bridge turns an Interface into a bridge over the kernel interfaces it names. The
// members are kernel interface names, not resource names. A bridge may name none: its
// ports are then attached by something else, such as a container runtime plugging veths
// in and out, and an empty mapping still makes the Interface a bridge.
type Bridge struct {
	Members []string `yaml:"members"`
}

// InterfaceAddress is one entry of an interface's addresses: a literal address with a
// prefix length, an address derived from a delegated prefix, or one derived from the
// prefix a router advertises. Exactly one of the three is set.
type InterfaceAddress struct {
	Literal                 Prefix                   // set when written as a string
	FromDelegatedPrefix     *DelegatedPrefixAddr     // set when written as a mapping
	FromRouterAdvertisement *RouterAdvertisementAddr // set when written as a mapping
}

// IsLiteral reports whether the address was written out rather than derived.
func (a InterfaceAddress) IsLiteral() bool { return a.Literal.IsValid() }

// DelegatedPrefixAddr derives an address from a prefix the upstream interface holds.
// Declaring the derivation rather than the result is what makes everything built on the
// address — a tunnel's local address, the advertised prefix, the address DNS listens on
// — follow a prefix change.
type DelegatedPrefixAddr struct {
	InterfaceRef string `yaml:"interfaceRef"`
	SubnetID     *int   `yaml:"subnetID"`
	Token        string `yaml:"token"`
}

// RouterAdvertisementAddr takes the prefix from the router advertisement received on the
// link and combines it with a fixed interface identifier. A host that is not the router
// keeps only the part it chose, the token, so the prefix the router advertises is not
// written down a second time and a prefix change reaches the host from the wire.
type RouterAdvertisementAddr struct {
	Token string `yaml:"token"`
}

// UnmarshalYAML accepts any of the forms. It takes the unmarshal-function form so that the
// mapping half is still decoded strictly; see Resource.UnmarshalYAML.
func (a *InterfaceAddress) UnmarshalYAML(unmarshal func(any) error) error {
	var capture nodeCapture
	if err := unmarshal(&capture); err != nil {
		return err
	}
	node := &capture.node
	switch node.Kind {
	case yaml.ScalarNode:
		var literal Prefix
		if err := literal.UnmarshalYAML(node); err != nil {
			return err
		}
		a.Literal = literal
		return nil
	case yaml.MappingNode:
		var derived struct {
			FromDelegatedPrefix     *DelegatedPrefixAddr     `yaml:"fromDelegatedPrefix"`
			FromRouterAdvertisement *RouterAdvertisementAddr `yaml:"fromRouterAdvertisement"`
		}
		if err := unmarshal(&derived); err != nil {
			return err
		}
		// One entry is one address with one source. A mapping naming both would leave
		// which of them the address comes from to the reader.
		if (derived.FromDelegatedPrefix == nil) == (derived.FromRouterAdvertisement == nil) {
			return typeErrorf(node, "an address written as a mapping needs exactly one of fromDelegatedPrefix and fromRouterAdvertisement")
		}
		a.FromDelegatedPrefix = derived.FromDelegatedPrefix
		a.FromRouterAdvertisement = derived.FromRouterAdvertisement
		return nil
	default:
		return typeErrorf(node, "expected an address, or a fromDelegatedPrefix or fromRouterAdvertisement mapping")
	}
}

// Route is a static route that leaves by the link it is written on. There is no table
// field: the only extra tables regied creates are the ones an EgressRoutePolicy needs,
// and it fills them itself.
type Route struct {
	Destination Prefix `yaml:"destination"`
	Via         *Addr  `yaml:"via"`
	Metric      *int   `yaml:"metric"`
}

// DHCPv6Client is the DHCPv6 client on the upstream interface.
type DHCPv6Client struct {
	PrefixDelegation *PrefixDelegation `yaml:"prefixDelegation"`
	UseDNS           *bool             `yaml:"useDNS"`
}

func (c DHCPv6Client) UseDNSEnabled() bool { return boolOr(c.UseDNS, false) }

// PrefixDelegation asks the provider for a prefix.
//
// DUIDFile may be left out, and that is not an error: networkd then sends a DUID of its
// own. For a line being brought up for the first time that is right; for a host replacing
// one that already holds a delegation it silently changes the delegated prefix, which is
// why leaving it out is warned about.
//
// IAID is the other half of the identity some providers bind the delegation to, next to
// the DUID. It is a pointer because zero is the usual value carried over from a router
// being replaced, and it has to be told apart from "not declared", which leaves the
// value to networkd. networkd derives that value from the interface, so a replacing host
// never matches the replaced one by accident.
type PrefixDelegation struct {
	DUIDFile     string  `yaml:"duidFile"`
	PrefixLength *int    `yaml:"prefixLength"`
	RapidCommit  *bool   `yaml:"rapidCommit"`
	IAID         *uint32 `yaml:"iaid"`
}

func (d PrefixDelegation) RapidCommitEnabled() bool { return boolOr(d.RapidCommit, true) }

// InterfaceIPv6 groups the IPv6 settings of a link.
type InterfaceIPv6 struct {
	Advertise *RouterAdvertisement `yaml:"advertise"`
}

// RouterAdvertisement makes a downstream link advertise the prefix it holds. The prefix
// is not written here, so it cannot drift from the address that is actually configured.
type RouterAdvertisement struct {
	Mode              RAMode   `yaml:"mode"`
	OtherInformation  *bool    `yaml:"otherInformation"`
	DNSServers        []Addr   `yaml:"dnsServers"`
	ValidLifetime     Duration `yaml:"validLifetime"`
	PreferredLifetime Duration `yaml:"preferredLifetime"`
}

func (r RouterAdvertisement) OtherInformationEnabled() bool {
	return boolOr(r.OtherInformation, false)
}

// PPPoESessionSpec is a PPPoE uplink. systemd-networkd has no PPPoE, so regied generates
// pppd's configuration and supervises the process.
//
// The link is named after the resource, so other resources and the firewall see a stable
// name across redials.
type PPPoESessionSpec struct {
	InterfaceRef string        `yaml:"interfaceRef"`
	UserIDFile   string        `yaml:"userIDFile"`
	PasswordFile string        `yaml:"passwordFile"`
	MTU          int           `yaml:"mtu"`
	Persist      *bool         `yaml:"persist"`
	Holdoff      Duration      `yaml:"holdoff"`
	UseDNS       *bool         `yaml:"useDNS"`
	DefaultRoute *DefaultRoute `yaml:"defaultRoute"`
	Routes       []Route       `yaml:"routes"`
}

func (*PPPoESessionSpec) ResourceKind() ResourceKind { return KindPPPoESession }

// DefaultPPPoEMTU is the most PPPoE over Ethernet allows.
const DefaultPPPoEMTU = 1492

func (s PPPoESessionSpec) MTUOrDefault() int {
	if s.MTU == 0 {
		return DefaultPPPoEMTU
	}
	return s.MTU
}

func (s PPPoESessionSpec) PersistEnabled() bool { return boolOr(s.Persist, true) }
func (s PPPoESessionSpec) UseDNSEnabled() bool  { return boolOr(s.UseDNS, false) }

// DefaultPPPoEHoldoff is how long pppd waits before redialling.
const DefaultPPPoEHoldoffSeconds = 5

// DSLiteTunnelSpec is the IPv4-over-IPv6 uplink: the B4 side of RFC 6333.
//
// Exactly one of LocalAddressFrom and LocalAddress is required, and exactly one of
// AFTRHost and AFTRAddress. The reference forms are the ones to reach for: a tunnel whose
// local address follows a delegated prefix does not go dark when the prefix changes, and
// a provider's AFTR name is what the provider publishes while the addresses behind it are
// theirs to change.
type DSLiteTunnelSpec struct {
	UnderlayRef      string            `yaml:"underlayRef"`
	LocalAddressFrom *LocalAddressFrom `yaml:"localAddressFrom"`
	LocalAddress     *Addr             `yaml:"localAddress"`
	AFTRHost         string            `yaml:"aftrHost"`
	AFTRAddress      *Addr             `yaml:"aftrAddress"`
	MTU              int               `yaml:"mtu"`
	TTL              *int              `yaml:"ttl"`
	DefaultRoute     *DefaultRoute     `yaml:"defaultRoute"`
	Routes           []Route           `yaml:"routes"`
}

func (*DSLiteTunnelSpec) ResourceKind() ResourceKind { return KindDSLiteTunnel }

// The DS-Lite defaults: an MTU that leaves room for the outer IPv6 header, and the usual
// hop limit.
const (
	DefaultDSLiteMTU = 1454
	DefaultDSLiteTTL = 64
)

func (s DSLiteTunnelSpec) MTUOrDefault() int {
	if s.MTU == 0 {
		return DefaultDSLiteMTU
	}
	return s.MTU
}

func (s DSLiteTunnelSpec) TTLOrDefault() int {
	if s.TTL == nil {
		return DefaultDSLiteTTL
	}
	return *s.TTL
}

// LocalAddressFrom takes the tunnel's local address from an interface's IPv6 address.
type LocalAddressFrom struct {
	InterfaceRef string `yaml:"interfaceRef"`
}

// DefaultRoute is whether an uplink installs a default route in the main table, and with
// what metric. The metric is how a host with two uplinks says which one its own traffic
// uses: traffic originating on the host is not subject to policy routing.
type DefaultRoute struct {
	Install *bool `yaml:"install"`
	Metric  *int  `yaml:"metric"`
}

// InstallEnabled is whether to install the route, for a DefaultRoute that may be nil.
func (r *DefaultRoute) InstallEnabled() bool {
	if r == nil {
		return true
	}
	return boolOr(r.Install, true)
}

// MetricOrDefault is the route's metric, for a DefaultRoute that may be nil.
func (r *DefaultRoute) MetricOrDefault() int {
	if r == nil || r.Metric == nil {
		return 0
	}
	return *r.Metric
}
