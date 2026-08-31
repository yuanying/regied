package v1alpha1

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// typeErrorf reports a malformed value as a *yaml.TypeError. The decoder collects those
// instead of stopping at the first one, so a single run reports every malformed value in
// the file rather than sending the operator round the loop once per mistake.
func typeErrorf(node *yaml.Node, format string, args ...any) error {
	return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: %s", node.Line, fmt.Sprintf(format, args...))}}
}

func scalar(node *yaml.Node, want string) (string, error) {
	if node.Kind != yaml.ScalarNode {
		return "", typeErrorf(node, "expected %s", want)
	}
	return node.Value, nil
}

// enum decodes a string scalar that has to be one of a fixed set of values.
func enum[T ~string](node *yaml.Node, out *T, want string, allowed ...T) error {
	s, err := scalar(node, want)
	if err != nil {
		return err
	}
	for _, a := range allowed {
		if T(s) == a {
			*out = a
			return nil
		}
	}
	names := make([]string, len(allowed))
	for i, a := range allowed {
		names[i] = strconv.Quote(string(a))
	}
	return typeErrorf(node, "%q is not one of %s", s, strings.Join(names, ", "))
}

// nodeCapture takes a copy of the node a value was written as, without decoding it.
//
// It exists because the unmarshal function the decoder hands to a custom unmarshaler
// cannot fill a *yaml.Node directly — it decodes into the Node's own fields instead. A
// type that implements the node-based interface can, and this is the smallest one.
type nodeCapture struct{ node yaml.Node }

func (c *nodeCapture) UnmarshalYAML(node *yaml.Node) error {
	c.node = *node
	return nil
}

// Addr is a single IP address.
type Addr struct{ netip.Addr }

func (a *Addr) UnmarshalYAML(node *yaml.Node) error {
	s, err := scalar(node, "an IP address")
	if err != nil {
		return err
	}
	v, perr := netip.ParseAddr(s)
	if perr != nil {
		return typeErrorf(node, "%q is not an IP address", s)
	}
	a.Addr = v
	return nil
}

// Prefix is an address with a prefix length: a network, or an address configured on a
// link. Host bits are kept as written, because on a link they are the address.
type Prefix struct{ netip.Prefix }

func (p *Prefix) UnmarshalYAML(node *yaml.Node) error {
	s, err := scalar(node, "an address with a prefix length, such as 192.0.2.0/24")
	if err != nil {
		return err
	}
	v, perr := netip.ParsePrefix(s)
	if perr != nil {
		return typeErrorf(node, "%q is not an address with a prefix length", s)
	}
	p.Prefix = v
	return nil
}

// SourceRange is a set of source addresses, written either as a CIDR or as an inclusive
// range. Only EgressRoutePolicy takes the range form; every other source field is CIDRs.
type SourceRange struct {
	Prefix   netip.Prefix // set when the range was written as a CIDR
	From, To netip.Addr   // set when it was written as first-last
}

// IsPrefix reports whether the range was written as a CIDR.
func (r SourceRange) IsPrefix() bool { return r.Prefix.IsValid() }

func (r SourceRange) String() string {
	if r.IsPrefix() {
		return r.Prefix.String()
	}
	return r.From.String() + "-" + r.To.String()
}

func (r *SourceRange) UnmarshalYAML(node *yaml.Node) error {
	s, err := scalar(node, "a CIDR or an address range such as 192.0.2.130-192.0.2.255")
	if err != nil {
		return err
	}
	if strings.Contains(s, "/") {
		v, perr := netip.ParsePrefix(s)
		if perr != nil {
			return typeErrorf(node, "%q is not a CIDR", s)
		}
		r.Prefix = v
		return nil
	}
	first, last, ok := strings.Cut(s, "-")
	if !ok {
		return typeErrorf(node, "%q is neither a CIDR nor an address range", s)
	}
	from, ferr := netip.ParseAddr(strings.TrimSpace(first))
	to, terr := netip.ParseAddr(strings.TrimSpace(last))
	if ferr != nil || terr != nil {
		return typeErrorf(node, "%q is not an address range", s)
	}
	if from.BitLen() != to.BitLen() {
		return typeErrorf(node, "%q mixes address families", s)
	}
	if to.Less(from) {
		return typeErrorf(node, "%q ends before it starts", s)
	}
	r.From, r.To = from, to
	return nil
}

// Port is a single port number.
type Port uint16

func (p *Port) UnmarshalYAML(node *yaml.Node) error {
	s, err := scalar(node, "a port number")
	if err != nil {
		return err
	}
	v, perr := strconv.ParseUint(s, 10, 16)
	if perr != nil || v == 0 {
		return typeErrorf(node, "%q is not a port number", s)
	}
	*p = Port(v)
	return nil
}

// PortSpec is a single port or an inclusive range of them, as ports are written in
// firewall rules and in a port forward.
type PortSpec struct{ From, To Port }

func (p PortSpec) String() string {
	if p.From == p.To {
		return strconv.Itoa(int(p.From))
	}
	return strconv.Itoa(int(p.From)) + "-" + strconv.Itoa(int(p.To))
}

// Width is how many ports the spec covers.
func (p PortSpec) Width() int { return int(p.To) - int(p.From) + 1 }

func (p *PortSpec) UnmarshalYAML(node *yaml.Node) error {
	s, err := scalar(node, "a port or a port range such as 60000-60010")
	if err != nil {
		return err
	}
	first, last, isRange := strings.Cut(s, "-")
	from, ferr := strconv.ParseUint(strings.TrimSpace(first), 10, 16)
	if ferr != nil || from == 0 {
		return typeErrorf(node, "%q is not a port or a port range", s)
	}
	if !isRange {
		p.From, p.To = Port(from), Port(from)
		return nil
	}
	to, terr := strconv.ParseUint(strings.TrimSpace(last), 10, 16)
	if terr != nil || to == 0 {
		return typeErrorf(node, "%q is not a port range", s)
	}
	if to < from {
		return typeErrorf(node, "%q ends before it starts", s)
	}
	p.From, p.To = Port(from), Port(to)
	return nil
}

// MAC is a hardware address.
type MAC struct{ net.HardwareAddr }

func (m *MAC) UnmarshalYAML(node *yaml.Node) error {
	s, err := scalar(node, "a MAC address")
	if err != nil {
		return err
	}
	v, perr := net.ParseMAC(s)
	if perr != nil {
		return typeErrorf(node, "%q is not a MAC address", s)
	}
	m.HardwareAddr = v
	return nil
}

// Duration is a lease time, a lifetime, or a holdoff. Zero means the field was left out
// and the kind's default applies.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

// OrDefault is the value, or def when the field was left out.
func (d Duration) OrDefault(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	s, err := scalar(node, "a duration such as 24h")
	if err != nil {
		return err
	}
	v, perr := time.ParseDuration(s)
	if perr != nil {
		return typeErrorf(node, "%q is not a duration", s)
	}
	if v <= 0 {
		return typeErrorf(node, "%q is not a positive duration", s)
	}
	*d = Duration(v)
	return nil
}

// Family is an address family. Empty means the field was left out.
type Family string

const (
	FamilyIPv4 Family = "ipv4"
	FamilyIPv6 Family = "ipv6"
)

func (f *Family) UnmarshalYAML(node *yaml.Node) error {
	return enum(node, f, "an address family", FamilyIPv4, FamilyIPv6)
}

// Action is what a firewall rule does with the traffic it matched.
type Action string

const (
	ActionAccept Action = "accept"
	ActionDrop   Action = "drop"
	ActionReject Action = "reject"
)

func (a *Action) UnmarshalYAML(node *yaml.Node) error {
	return enum(node, a, "a firewall action", ActionAccept, ActionDrop, ActionReject)
}

// NATType is the kind of source translation. masquerade is the only one.
type NATType string

const NATMasquerade NATType = "masquerade"

func (t *NATType) UnmarshalYAML(node *yaml.Node) error {
	return enum(node, t, "a NAT type", NATMasquerade)
}

// RAMode is what a router advertisement offers.
type RAMode string

const RASLAAC RAMode = "slaac"

func (m *RAMode) UnmarshalYAML(node *yaml.Node) error {
	return enum(node, m, "a router advertisement mode", RASLAAC)
}

// DHCPv6Mode is how the DHCPv6 server answers. Addresses come from SLAAC, so stateless
// is the only mode.
type DHCPv6Mode string

const DHCPv6Stateless DHCPv6Mode = "stateless"

func (m *DHCPv6Mode) UnmarshalYAML(node *yaml.Node) error {
	return enum(node, m, "a DHCPv6 mode", DHCPv6Stateless)
}

// Protocol is a transport or encapsulation protocol, written by name or by number.
type Protocol struct {
	Name   string // one of the names below; empty when written as a number
	Number int    // set when written as a number
}

// The names the schema accepts. A protocol not in this list is written as its number.
var protocolNames = []string{"tcp", "udp", "icmp", "icmpv6", "ipip", "esp"}

// IsZero reports whether the field was left out.
func (p Protocol) IsZero() bool { return p.Name == "" && p.Number == 0 }

func (p Protocol) String() string {
	if p.Name != "" {
		return p.Name
	}
	return strconv.Itoa(p.Number)
}

func (p *Protocol) UnmarshalYAML(node *yaml.Node) error {
	s, err := scalar(node, "a protocol name or number")
	if err != nil {
		return err
	}
	for _, name := range protocolNames {
		if s == name {
			p.Name = name
			return nil
		}
	}
	n, perr := strconv.Atoi(s)
	if perr != nil || n < 0 || n > 255 {
		return typeErrorf(node, "%q is not a protocol name or number", s)
	}
	p.Number = n
	return nil
}

// MSSClampMode is how spec.global.mssClamp was written.
type MSSClampMode string

const (
	MSSClampAuto  MSSClampMode = "auto"  // clamp to the path MTU wherever it is lower
	MSSClampOff   MSSClampMode = "off"   // do not clamp
	MSSClampFixed MSSClampMode = "fixed" // clamp to MSSClamp.Value
)

// MSSClamp is spec.global.mssClamp: auto, off, or a fixed segment size.
type MSSClamp struct {
	Mode  MSSClampMode
	Value int // set when Mode is MSSClampFixed
}

// Resolved is the mode, filling in the default for a field that was left out.
func (c MSSClamp) Resolved() MSSClamp {
	if c.Mode == "" {
		return MSSClamp{Mode: MSSClampAuto}
	}
	return c
}

func (c *MSSClamp) UnmarshalYAML(node *yaml.Node) error {
	s, err := scalar(node, `"auto", "off", or a segment size`)
	if err != nil {
		return err
	}
	switch s {
	case string(MSSClampAuto), string(MSSClampOff):
		c.Mode = MSSClampMode(s)
		return nil
	}
	n, perr := strconv.Atoi(s)
	if perr != nil || n <= 0 {
		return typeErrorf(node, `%q is not "auto", "off", or a segment size`, s)
	}
	c.Mode, c.Value = MSSClampFixed, n
	return nil
}

// boolOr is the value of an optional boolean, or def when it was left out.
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}
