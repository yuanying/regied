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
	Addresses []InterfaceAddress `yaml:"addresses"`
	Routes    []Route            `yaml:"routes"`
	DHCPv6    *DHCPv6Client      `yaml:"dhcpv6"`
	IPv6      *InterfaceIPv6     `yaml:"ipv6"`
}

func (*InterfaceSpec) ResourceKind() ResourceKind { return KindInterface }

// Bridge turns an Interface into a bridge over the kernel interfaces it names. The
// members are kernel interface names, not resource names.
type Bridge struct {
	Members []string `yaml:"members"`
}

// InterfaceAddress is one entry of an interface's addresses: either a literal address
// with a prefix length, or an address derived from a delegated prefix.
type InterfaceAddress struct {
	Literal             Prefix               // set when written as a string
	FromDelegatedPrefix *DelegatedPrefixAddr // set when written as a mapping
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

// UnmarshalYAML accepts either form. It takes the unmarshal-function form so that the
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
			FromDelegatedPrefix *DelegatedPrefixAddr `yaml:"fromDelegatedPrefix"`
		}
		if err := unmarshal(&derived); err != nil {
			return err
		}
		if derived.FromDelegatedPrefix == nil {
			return typeErrorf(node, "an address written as a mapping needs fromDelegatedPrefix")
		}
		a.FromDelegatedPrefix = derived.FromDelegatedPrefix
		return nil
	default:
		return typeErrorf(node, "expected an address or a fromDelegatedPrefix mapping")
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
type PrefixDelegation struct {
	DUIDFile     string `yaml:"duidFile"`
	PrefixLength *int   `yaml:"prefixLength"`
	RapidCommit  *bool  `yaml:"rapidCommit"`
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
