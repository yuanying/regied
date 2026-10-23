package config_test

import (
	"testing"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// A route and an address set name a subnet or an address inside the delegated prefix the
// way an interface's address does: with fromDelegatedPrefix, in place of the literal
// (ADR 0021). Either form decodes into the same field.
func TestRoutesAndSetsDecodeBothForms(t *testing.T) {
	cfg, problems := check(t, ifaceWAN+`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses: [192.168.10.1/24]
        routes:
          - destination: 172.16.0.0/16
            via: 192.168.10.10
          - destination:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 2}
            via:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::10"}
    - kind: IPAddressSet
      metadata: {name: published}
      spec:
        family: ipv6
        addresses:
          - 2001:db8:0:1::200
          - fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::210"}
        networks:
          - 2001:db8:0:3::/64
          - fromDelegatedPrefix: {interfaceRef: wan, subnetID: 2}
`, secrets())
	if cfg == nil {
		t.Fatalf("rejected routes and a set derived from the delegation:\n%s", problems)
	}

	lan := config.ResourcesOf[*v1alpha1.InterfaceSpec](cfg)[1].Spec
	if got := lan.Routes[0]; got.Destination.IsDerived() || got.Destination.Literal.String() != "172.16.0.0/16" ||
		got.Via.IsDerived() || got.Via.Literal.String() != "192.168.10.10" {
		t.Errorf("the literal route decoded as %+v", got)
	}
	derived := lan.Routes[1]
	if !derived.Destination.IsDerived() || derived.Destination.FromDelegatedPrefix.InterfaceRef != "wan" ||
		*derived.Destination.FromDelegatedPrefix.SubnetID != 2 {
		t.Errorf("the derived destination decoded as %+v", derived.Destination)
	}
	if !derived.Via.IsDerived() || derived.Via.FromDelegatedPrefix.Token != "::10" {
		t.Errorf("the derived next hop decoded as %+v", derived.Via)
	}

	set := config.ResourcesOf[*v1alpha1.IPAddressSetSpec](cfg)[0].Spec
	if set.Addresses[0].IsDerived() || set.Addresses[0].Literal.String() != "2001:db8:0:1::200" {
		t.Errorf("the literal address decoded as %+v", set.Addresses[0])
	}
	if !set.Addresses[1].IsDerived() || set.Addresses[1].FromDelegatedPrefix.Token != "::210" {
		t.Errorf("the derived address decoded as %+v", set.Addresses[1])
	}
	if set.Networks[0].IsDerived() || !set.Networks[1].IsDerived() {
		t.Errorf("the networks decoded as %+v", set.Networks)
	}
	if !set.HasDerived() {
		t.Error("a set with a derived entry does not say so")
	}
}

func TestADerivedValueMappingMustNameItsSource(t *testing.T) {
	for _, body := range []string{
		`          - destination: {fromRouterAdvertisement: {token: "::1"}}`,
		`          - destination: {}`,
		`          - destination: [2001:db8::/64]`,
	} {
		_, err := config.Parse(doc(`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        routes:
` + body + "\n"))
		if err == nil {
			t.Errorf("%s: parsed, want an error", body)
		}
	}
}

// A field that wants an address needs the token, which is the host half; a field that
// wants a subnet must not have one, so that one cannot be given for the other. The
// subnet ID is always required, and the interface named must ask for a delegation
// (ADR 0021).
func TestValidateRejectsDerivedRoutesAndSetsThatAreNotStated(t *testing.T) {
	cases := []struct {
		name      string
		resources string
		want      []string
	}{
		{
			name: "a destination with a token",
			resources: `        routes:
          - destination:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 2, token: "::1"}
`,
			want: []string{`spec.routes[0].destination.fromDelegatedPrefix.token: a destination is a /64 subnet of the delegation`},
		},
		{
			name: "a next hop without a token",
			resources: `        routes:
          - destination: 2001:db8:0:2::/64
            via:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1}
`,
			want: []string{`spec.routes[0].via.fromDelegatedPrefix.token: required`},
		},
		{
			name: "a next hop whose token is not an interface identifier",
			resources: `        routes:
          - destination: 2001:db8:0:2::/64
            via:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "2001:db8::10"}
`,
			want: []string{`spec.routes[0].via.fromDelegatedPrefix.token: "2001:db8::10" is not an interface identifier`},
		},
		{
			name: "a destination without a subnet ID",
			resources: `        routes:
          - destination:
              fromDelegatedPrefix: {interfaceRef: wan}
`,
			want: []string{`spec.routes[0].destination.fromDelegatedPrefix.subnetID: required`},
		},
		{
			name: "a destination cut from an interface that asks for no prefix",
			resources: `        routes:
          - destination:
              fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2}
`,
			want: []string{`spec.routes[0].destination.fromDelegatedPrefix.interfaceRef: the Interface "lan" has no prefix-delegation client`},
		},
		{
			name: "a destination cut from an interface that is not declared",
			resources: `        routes:
          - destination:
              fromDelegatedPrefix: {interfaceRef: nowhere, subnetID: 2}
`,
			want: []string{`spec.routes[0].destination.fromDelegatedPrefix.interfaceRef: no Interface named "nowhere"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, problems := check(t, ifaceWAN+`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses: [192.168.10.1/24]
`+tc.resources, secrets())
			if cfg != nil {
				t.Fatal("accepted a derived route that is not stated")
			}
			assertProblems(t, problems, tc.want)
		})
	}
}

func TestValidateRejectsDerivedSetEntriesThatAreNotStated(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want []string
	}{
		{
			name: "an address without a token",
			spec: `        family: ipv6
        addresses:
          - fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1}
`,
			want: []string{`spec.addresses[0].fromDelegatedPrefix.token: required`},
		},
		{
			name: "a network with a token",
			spec: `        family: ipv6
        networks:
          - fromDelegatedPrefix: {interfaceRef: wan, subnetID: 2, token: "::1"}
`,
			want: []string{`spec.networks[0].fromDelegatedPrefix.token: a network is a /64 subnet of the delegation`},
		},
		{
			name: "a derived entry in an IPv4 set",
			spec: `        family: ipv4
        addresses:
          - fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::1"}
`,
			want: []string{`spec.addresses[0]: an address cut from the delegated prefix is ipv6, and the set is ipv4`},
		},
		{
			name: "a derived entry from an interface that asks for no prefix",
			spec: `        family: ipv6
        networks:
          - fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2}
`,
			want: []string{`spec.networks[0].fromDelegatedPrefix.interfaceRef: the Interface "lan" has no prefix-delegation client`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, problems := check(t, ifaceWAN+ifaceLAN+`    - kind: IPAddressSet
      metadata: {name: published}
      spec:
`+tc.spec, secrets())
			if cfg != nil {
				t.Fatal("accepted a derived set entry that is not stated")
			}
			assertProblems(t, problems, tc.want)
		})
	}
}

// Only an Interface's routes take the derived form. An uplink's routes lead outward, and
// nothing outward is inside the prefix the provider delegated to this host.
func TestValidateRejectsDerivedRoutesOnAnUplink(t *testing.T) {
	cfg, problems := check(t, ifaceWAN+`    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        localAddressFrom: {interfaceRef: wan}
        aftrHost: aftr.example.net
        routes:
          - destination:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 2}
`, secrets())
	if cfg != nil {
		t.Fatal("accepted a derived route on a tunnel")
	}
	assertProblems(t, problems, []string{
		`spec.routes[0].destination: only an Interface's routes may be cut from the delegated prefix`,
	})
}
