package v1alpha1

// SourceNATSpec rewrites the source address of traffic on its way out.
//
// masquerade takes the address from the outgoing link at the moment the packet leaves, so
// a dynamically addressed uplink needs nothing written down and nothing re-applied when
// the address changes. There is no field for an address for the same reason.
type SourceNATSpec struct {
	Type                NATType  `yaml:"type"`
	EgressRef           string   `yaml:"egressRef"`
	SourceRanges        []Prefix `yaml:"sourceRanges"`
	ExcludeDestinations []Prefix `yaml:"excludeDestinations"`
}

func (*SourceNATSpec) ResourceKind() ResourceKind { return KindSourceNAT }

// TypeOrDefault is the translation to perform. masquerade is the only one.
func (s SourceNATSpec) TypeOrDefault() NATType {
	if s.Type == "" {
		return NATMasquerade
	}
	return s.Type
}

// PortForwardSpec rewrites the destination of traffic arriving on an uplink, so that a
// host inside can be reached from outside.
//
// Exactly one of Port and PortRange is required. There is no field for the address to
// listen on: it is the uplink's, and writing it down produces a configuration that works
// until the address changes and then fails in a way that looks like something else.
type PortForwardSpec struct {
	EgressRef    string         `yaml:"egressRef"`
	Protocol     Protocol       `yaml:"protocol"`
	Port         *Port          `yaml:"port"`
	PortRange    *PortSpec      `yaml:"portRange"`
	Target       *ForwardTarget `yaml:"target"`
	Hairpin      *bool          `yaml:"hairpin"`
	OpenFirewall *bool          `yaml:"openFirewall"`
}

func (*PortForwardSpec) ResourceKind() ResourceKind { return KindPortForward }

// HairpinEnabled covers a host on the inside reaching the service through the uplink's
// global address, which is what happens when an internal client resolves the same public
// name as an external one.
func (s PortForwardSpec) HairpinEnabled() bool { return boolOr(s.Hairpin, true) }

// OpenFirewallEnabled adds the accept that matches this forward in the path the
// translated traffic takes. It is on by default because a port forward with default-drop
// firewalling and no opening is a configuration that is wrong in every case.
func (s PortForwardSpec) OpenFirewallEnabled() bool { return boolOr(s.OpenFirewall, true) }

// Ports is what the forward listens on, whichever way it was written.
func (s PortForwardSpec) Ports() PortSpec {
	switch {
	case s.Port != nil:
		return PortSpec{From: *s.Port, To: *s.Port}
	case s.PortRange != nil:
		return *s.PortRange
	}
	return PortSpec{}
}

// ForwardTarget is the host inside, and optionally the port or range to translate to.
type ForwardTarget struct {
	Address   Addr      `yaml:"address"`
	Port      *Port     `yaml:"port"`
	PortRange *PortSpec `yaml:"portRange"`
}

// Ports is what the traffic is translated to, defaulting to the same port or range the
// forward listens on.
func (s PortForwardSpec) TargetPorts() PortSpec {
	if s.Target != nil {
		switch {
		case s.Target.Port != nil:
			return PortSpec{From: *s.Target.Port, To: *s.Target.Port}
		case s.Target.PortRange != nil:
			return *s.Target.PortRange
		}
	}
	return s.Ports()
}
