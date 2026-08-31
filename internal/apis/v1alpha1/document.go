package v1alpha1

import (
	yaml "go.yaml.in/yaml/v3"
)

// APIVersion and DocumentKind are what every regied configuration document carries.
//
// The apiGroup deliberately does not contain the project's name: renaming the binary
// must not invalidate a configuration file. The document kind deliberately carries no
// role either — what a host does is expressed by which resources it lists (ADR 0002).
const (
	APIVersion   = "net.unstable.cloud/v1alpha1"
	DocumentKind = "NetworkConfig"
)

// NetworkConfig is one host's network configuration: the whole document.
type NetworkConfig struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

// Metadata names the document, or a resource within it.
type Metadata struct {
	Name string `yaml:"name"`
}

// Spec is the body of the document.
type Spec struct {
	Global    Global     `yaml:"global"`
	Resources []Resource `yaml:"resources"`
}

// Global holds the host-wide switches. They are not resources: there is exactly one of
// each per host, and nothing refers to them.
type Global struct {
	IPForwarding     *bool    `yaml:"ipForwarding"`
	SynCookies       *bool    `yaml:"synCookies"`
	LogMartians      *bool    `yaml:"logMartians"`
	SendRedirects    *bool    `yaml:"sendRedirects"`
	ReceiveRedirects *bool    `yaml:"receiveRedirects"`
	SourceValidation *bool    `yaml:"sourceValidation"`
	MSSClamp         MSSClamp `yaml:"mssClamp"`
}

// The defaults for the switches that have one other than false.
func (g Global) IPForwardingEnabled() bool     { return boolOr(g.IPForwarding, false) }
func (g Global) SynCookiesEnabled() bool       { return boolOr(g.SynCookies, true) }
func (g Global) LogMartiansEnabled() bool      { return boolOr(g.LogMartians, false) }
func (g Global) SendRedirectsEnabled() bool    { return boolOr(g.SendRedirects, false) }
func (g Global) ReceiveRedirectsEnabled() bool { return boolOr(g.ReceiveRedirects, false) }
func (g Global) SourceValidationEnabled() bool { return boolOr(g.SourceValidation, false) }

// ResourceKind names one of the eleven kinds.
type ResourceKind string

const (
	KindInterface         ResourceKind = "Interface"
	KindPPPoESession      ResourceKind = "PPPoESession"
	KindDSLiteTunnel      ResourceKind = "DSLiteTunnel"
	KindEgressRoutePolicy ResourceKind = "EgressRoutePolicy"
	KindIPAddressSet      ResourceKind = "IPAddressSet"
	KindFirewallZone      ResourceKind = "FirewallZone"
	KindFirewallPolicy    ResourceKind = "FirewallPolicy"
	KindSourceNAT         ResourceKind = "SourceNAT"
	KindPortForward       ResourceKind = "PortForward"
	KindDHCPServer        ResourceKind = "DHCPServer"
	KindDNSForwarder      ResourceKind = "DNSForwarder"
)

// Kinds is every resource kind, in the order docs/spec/kinds.md lists them.
var Kinds = []ResourceKind{
	KindInterface, KindPPPoESession, KindDSLiteTunnel, KindEgressRoutePolicy,
	KindIPAddressSet, KindFirewallZone, KindFirewallPolicy, KindSourceNAT,
	KindPortForward, KindDHCPServer, KindDNSForwarder,
}

// SelfZone is the reserved zone name denoting the host itself. A FirewallPolicy may name
// it as its `to`; no FirewallZone may be called it.
const SelfZone = "self"

// LoopbackLink is the reserved name a DNSForwarder may list in listenOn alongside real
// link resources.
const LoopbackLink = "loopback"

// ResourceSpec is the body of a resource. The concrete type follows from the kind, and a
// renderer recovers it with a type assertion or with config.ResourcesOf.
type ResourceSpec interface {
	// ResourceKind is the kind this spec belongs to.
	ResourceKind() ResourceKind
}

// Resource is one entry of spec.resources.
type Resource struct {
	Kind     ResourceKind
	Metadata Metadata
	Spec     ResourceSpec

	// Line is where the resource starts in the file it was read from, so that a problem
	// found long after parsing can still point at it.
	Line int
}

// Ref is how a resource is named in a diagnostic: kind and name together, because names
// are only unique within a kind.
func (r Resource) Ref() string { return string(r.Kind) + "/" + r.Metadata.Name }

// IsLink reports whether the kind puts a link on the host: the kinds a FirewallZone's
// linkRefs and a DNSForwarder's listenOn may name.
func (k ResourceKind) IsLink() bool {
	return k == KindInterface || k == KindPPPoESession || k == KindDSLiteTunnel
}

// IsUplink reports whether the kind is one that leads outward: the kinds an egressRef
// may name.
func (k ResourceKind) IsUplink() bool {
	return k == KindPPPoESession || k == KindDSLiteTunnel
}

// resourceOf is the shape a resource has once its kind is known. Decoding into it a
// second time is what makes the spec strict: the decoder checks the fields of a concrete
// spec type, which it cannot do on the first pass because the kind is not known yet.
type resourceOf[S any] struct {
	Kind     ResourceKind `yaml:"kind"`
	Metadata Metadata     `yaml:"metadata"`
	Spec     S            `yaml:"spec"`
}

// UnmarshalYAML decodes a resource in two passes over the same node.
//
// It takes the obsolete unmarshal-function form on purpose. That form hands back the
// decoder that is already running, so both passes reject unknown fields; the newer
// Node-based form would start a fresh decoder, and a misspelt field inside a spec would
// be silently dropped — the worst way this kind of configuration can break.
func (r *Resource) UnmarshalYAML(unmarshal func(any) error) error {
	var capture nodeCapture
	if err := unmarshal(&capture); err != nil {
		return err
	}
	node := &capture.node
	r.Line = node.Line

	var head struct {
		Kind     ResourceKind `yaml:"kind"`
		Metadata Metadata     `yaml:"metadata"`
		Spec     yaml.Node    `yaml:"spec"`
	}
	if err := unmarshal(&head); err != nil {
		return err
	}
	r.Kind, r.Metadata = head.Kind, head.Metadata

	switch head.Kind {
	case KindInterface:
		return decodeSpec[InterfaceSpec](r, unmarshal)
	case KindPPPoESession:
		return decodeSpec[PPPoESessionSpec](r, unmarshal)
	case KindDSLiteTunnel:
		return decodeSpec[DSLiteTunnelSpec](r, unmarshal)
	case KindEgressRoutePolicy:
		return decodeSpec[EgressRoutePolicySpec](r, unmarshal)
	case KindIPAddressSet:
		return decodeSpec[IPAddressSetSpec](r, unmarshal)
	case KindFirewallZone:
		return decodeSpec[FirewallZoneSpec](r, unmarshal)
	case KindFirewallPolicy:
		return decodeSpec[FirewallPolicySpec](r, unmarshal)
	case KindSourceNAT:
		return decodeSpec[SourceNATSpec](r, unmarshal)
	case KindPortForward:
		return decodeSpec[PortForwardSpec](r, unmarshal)
	case KindDHCPServer:
		return decodeSpec[DHCPServerSpec](r, unmarshal)
	case KindDNSForwarder:
		return decodeSpec[DNSForwarderSpec](r, unmarshal)
	case "":
		return typeErrorf(node, "resource has no kind")
	default:
		return typeErrorf(node, "unknown resource kind %q", head.Kind)
	}
}

func decodeSpec[S any](r *Resource, unmarshal func(any) error) error {
	var typed resourceOf[S]
	if err := unmarshal(&typed); err != nil {
		return err
	}
	spec, ok := any(&typed.Spec).(ResourceSpec)
	if !ok {
		// Every spec type implements ResourceSpec; reaching here is a programming error.
		panic("v1alpha1: spec type does not implement ResourceSpec")
	}
	r.Spec = spec
	return nil
}
