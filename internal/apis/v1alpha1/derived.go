package v1alpha1

import (
	yaml "go.yaml.in/yaml/v3"
)

// DerivableAddr is an address written out, or cut from the delegated prefix: a route's
// next hop, an element of an address set. Exactly one of the two is set.
//
// It is the same derivation an interface's address takes, accepted wherever an address
// inside the delegated prefix has to be written, so that a prefix change reaches it
// without an edit (ADR 0021). Cut from the prefix, it needs the token, which is the host
// half: a field that wants an address cannot be given a subnet.
type DerivableAddr struct {
	Literal             Addr                 // set when written as a string
	FromDelegatedPrefix *DelegatedPrefixAddr // set when written as a mapping
}

// IsDerived reports whether the address is cut from the delegated prefix.
func (a DerivableAddr) IsDerived() bool { return a.FromDelegatedPrefix != nil }

func (a *DerivableAddr) UnmarshalYAML(unmarshal func(any) error) error {
	derived, node, err := decodeDerivable(unmarshal)
	if err != nil {
		return err
	}
	if derived != nil {
		a.FromDelegatedPrefix = derived
		return nil
	}
	return a.Literal.UnmarshalYAML(node)
}

// DerivablePrefix is a network written out, or a subnet of the delegated prefix: a
// route's destination, a network in an address set. Exactly one of the two is set.
//
// A subnet of the delegation is always a /64, and is written without a token: a field
// that wants a subnet cannot be given an address (ADR 0021).
type DerivablePrefix struct {
	Literal             Prefix               // set when written as a string
	FromDelegatedPrefix *DelegatedPrefixAddr // set when written as a mapping
}

// IsDerived reports whether the network is a subnet of the delegated prefix.
func (p DerivablePrefix) IsDerived() bool { return p.FromDelegatedPrefix != nil }

func (p *DerivablePrefix) UnmarshalYAML(unmarshal func(any) error) error {
	derived, node, err := decodeDerivable(unmarshal)
	if err != nil {
		return err
	}
	if derived != nil {
		p.FromDelegatedPrefix = derived
		return nil
	}
	return p.Literal.UnmarshalYAML(node)
}

// decodeDerivable reads the mapping form, and hands back the node when the value was
// written as a scalar, for the literal to be read out of. It takes the unmarshal-function
// form so that the mapping is still decoded strictly; see Resource.UnmarshalYAML.
func decodeDerivable(unmarshal func(any) error) (*DelegatedPrefixAddr, *yaml.Node, error) {
	var capture nodeCapture
	if err := unmarshal(&capture); err != nil {
		return nil, nil, err
	}
	node := &capture.node
	switch node.Kind {
	case yaml.ScalarNode:
		return nil, node, nil
	case yaml.MappingNode:
		var derived struct {
			FromDelegatedPrefix *DelegatedPrefixAddr `yaml:"fromDelegatedPrefix"`
		}
		if err := unmarshal(&derived); err != nil {
			return nil, nil, err
		}
		if derived.FromDelegatedPrefix == nil {
			return nil, nil, typeErrorf(node, "a value written as a mapping needs fromDelegatedPrefix")
		}
		return derived.FromDelegatedPrefix, nil, nil
	default:
		return nil, nil, typeErrorf(node, "expected a literal, or a fromDelegatedPrefix mapping")
	}
}
