package v1alpha1

import "time"

// DHCPServerSpec is address handout on a downstream link. Every DHCPServer and
// DNSForwarder renders into one dnsmasq configuration and one supervised process.
type DHCPServerSpec struct {
	InterfaceRef   string          `yaml:"interfaceRef"`
	Subnet         Prefix          `yaml:"subnet"`
	Pool           *Pool           `yaml:"pool"`
	LeaseTime      Duration        `yaml:"leaseTime"`
	Gateway        *Addr           `yaml:"gateway"`
	DNSServers     []Addr          `yaml:"dnsServers"`
	Domain         string          `yaml:"domain"`
	StaticMappings []StaticMapping `yaml:"staticMappings"`
	IPv6           *DHCPServerIPv6 `yaml:"ipv6"`
}

func (*DHCPServerSpec) ResourceKind() ResourceKind { return KindDHCPServer }

// DefaultLeaseTime is how long a lease lasts when the field is left out.
const DefaultLeaseTime = 24 * time.Hour

func (s DHCPServerSpec) LeaseTimeOrDefault() time.Duration {
	return s.LeaseTime.OrDefault(DefaultLeaseTime)
}

// Pool is the inclusive range addresses are handed out from. It may cover only part of
// the subnet; the rest is left for static mappings and for addresses assigned by hand.
type Pool struct {
	Start Addr `yaml:"start"`
	End   Addr `yaml:"end"`
}

// StaticMapping pins one host's address.
//
// Where policy routing selects an uplink by source range, which side of that boundary a
// mapping falls on decides which uplink that host uses. Moving a mapping across the
// boundary is a routing change, not a cosmetic one.
type StaticMapping struct {
	Name       string `yaml:"name"`
	MACAddress MAC    `yaml:"macAddress"`
	Address    Addr   `yaml:"address"`
}

// DHCPServerIPv6 is the stateless half of IPv6 configuration: what a client asks DHCPv6
// for after the router advertisement told it to. The prefix and the advertisement itself
// come from the interface.
type DHCPServerIPv6 struct {
	Mode                   DHCPv6Mode `yaml:"mode"`
	DNSServers             []Addr     `yaml:"dnsServers"`
	InformationRefreshTime Duration   `yaml:"informationRefreshTime"`
}

// DefaultInformationRefreshTime is how often a client rechecks the information it was
// given, when the field is left out.
const DefaultInformationRefreshTime = 6 * time.Hour

func (s DHCPServerIPv6) InformationRefreshTimeOrDefault() time.Duration {
	return s.InformationRefreshTime.OrDefault(DefaultInformationRefreshTime)
}

// DNSForwarderSpec is recursive resolution for the segments below, with conditional
// forwarding and name overrides.
//
// ListenOn names links, not addresses, so an interface whose address comes from a
// delegated prefix keeps being listened on after the prefix changes.
type DNSForwarderSpec struct {
	ListenOn    []string             `yaml:"listenOn"`
	CacheSize   *int                 `yaml:"cacheSize"`
	Upstreams   []Addr               `yaml:"upstreams"`
	Conditional []ConditionalForward `yaml:"conditional"`
	StaticHosts []StaticHost         `yaml:"staticHosts"`
}

func (*DNSForwarderSpec) ResourceKind() ResourceKind { return KindDNSForwarder }

// DefaultCacheSize is how many entries dnsmasq caches when the field is left out.
const DefaultCacheSize = 150

func (s DNSForwarderSpec) CacheSizeOrDefault() int {
	if s.CacheSize == nil {
		return DefaultCacheSize
	}
	return *s.CacheSize
}

// ConditionalForward diverts one zone to a resolver that knows about it, while everything
// else goes upstream.
type ConditionalForward struct {
	Domain  string `yaml:"domain"`
	Servers []Addr `yaml:"servers"`
}

// StaticHost overrides one name. The usual reason is a public name that should resolve to
// an internal address for clients inside.
type StaticHost struct {
	Name    string `yaml:"name"`
	Address Addr   `yaml:"address"`
}
