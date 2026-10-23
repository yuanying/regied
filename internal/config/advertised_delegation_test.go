package config_test

import (
	"math"
	"net/netip"
	"slices"
	"testing"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

func advertised(address string, preferred uint64) config.AdvertisedAddress {
	return config.AdvertisedAddress{Address: netip.MustParseAddr(address), PreferredUntil: preferred}
}

func prefixes(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(values))
	for i, value := range values {
		out[i] = netip.MustParsePrefix(value)
	}
	return out
}

// A downstream host knows the delegated prefix as well as the router does: the /64
// advertised to it, read from the address it took with its token, cut back to the
// declared length (ADR 0023).
func TestTheAdvertisedSubnetIsCutBackToTheDeclaredLength(t *testing.T) {
	held, passedOver := config.DelegationFromAdvertisement([]config.AdvertisedAddress{
		advertised("2001:db8:0:101::151", 1000),
	}, "::151", 56)
	if !slices.Equal(held, prefixes("2001:db8:0:100::/56")) {
		t.Errorf("held %v, want 2001:db8:0:100::/56", held)
	}
	if len(passedOver) != 0 {
		t.Errorf("passed over %v with one candidate", passedOver)
	}
}

// Only the address carrying the declared token is the one networkd took from the
// advertisement for this declaration. A temporary address, a link-local one, and a ULA
// advertised beside the global prefix are not the delegation (ADR 0023).
func TestOnlyAGlobalAddressWithTheTokenIsACandidate(t *testing.T) {
	held, _ := config.DelegationFromAdvertisement([]config.AdvertisedAddress{
		advertised("fe80::151", math.MaxUint64),
		advertised("fd00:0:0:1::151", math.MaxUint64),
		advertised("2001:db8:0:101:aaaa:bbbb:cccc:dddd", math.MaxUint64),
		advertised("2001:db8:0:101::151", 1000),
	}, "::151", 56)
	if !slices.Equal(held, prefixes("2001:db8:0:100::/56")) {
		t.Errorf("held %v, want only the global address with the token", held)
	}
}

// With nothing advertised yet there is no delegation, which is a reason to wait.
func TestNoAdvertisedAddressIsNoDelegation(t *testing.T) {
	held, passedOver := config.DelegationFromAdvertisement([]config.AdvertisedAddress{
		advertised("fd00:0:0:1::151", math.MaxUint64),
	}, "::151", 56)
	if len(held) != 0 || len(passedOver) != 0 {
		t.Errorf("held %v, passed over %v, want nothing", held, passedOver)
	}
}

// During a change two prefixes are advertised for a while. The new one is preferred for
// longer as soon as it has arrived, so it is taken, and the other is reported as passed
// over (ADR 0023).
func TestDuringAChangeThePrefixPreferredLongestIsTaken(t *testing.T) {
	held, passedOver := config.DelegationFromAdvertisement([]config.AdvertisedAddress{
		advertised("2001:db8:0:101::151", 1000),
		advertised("2001:db8:0:ab01::151", 5000),
	}, "::151", 56)
	if !slices.Equal(held, prefixes("2001:db8:0:ab00::/56")) {
		t.Errorf("held %v, want the one preferred longest", held)
	}
	if !slices.Equal(passedOver, prefixes("2001:db8:0:100::/56")) {
		t.Errorf("passed over %v, want the old delegation", passedOver)
	}
}

// An address networkd describes with no preferred lifetime is preferred for ever.
func TestAnAddressWithoutALifetimeIsPreferredLongest(t *testing.T) {
	held, _ := config.DelegationFromAdvertisement([]config.AdvertisedAddress{
		advertised("2001:db8:0:101::151", 5000),
		advertised("2001:db8:0:ab01::151", math.MaxUint64),
	}, "::151", 56)
	if !slices.Equal(held, prefixes("2001:db8:0:ab00::/56")) {
		t.Errorf("held %v, want the one with no lifetime", held)
	}
}

// Two subnets of one delegation advertised on the same LAN are not two delegations.
func TestTwoSubnetsOfOneDelegationAreOneDelegation(t *testing.T) {
	held, passedOver := config.DelegationFromAdvertisement([]config.AdvertisedAddress{
		advertised("2001:db8:0:101::151", 1000),
		advertised("2001:db8:0:102::151", 1000),
	}, "::151", 56)
	if !slices.Equal(held, prefixes("2001:db8:0:100::/56")) || len(passedOver) != 0 {
		t.Errorf("held %v, passed over %v, want one delegation", held, passedOver)
	}
}

// Two delegations preferred equally long cannot be told apart. Both are held, which is
// what makes every value cut from them wait and say why (ADR 0021, ADR 0023).
func TestTwoDelegationsPreferredEquallyLongAreAnAmbiguity(t *testing.T) {
	held, passedOver := config.DelegationFromAdvertisement([]config.AdvertisedAddress{
		advertised("2001:db8:0:ab01::151", 1000),
		advertised("2001:db8:0:101::151", 1000),
	}, "::151", 56)
	if !slices.Equal(held, prefixes("2001:db8:0:100::/56", "2001:db8:0:ab00::/56")) {
		t.Errorf("held %v, want both, in order", held)
	}
	if len(passedOver) != 0 {
		t.Errorf("passed over %v when nothing was chosen", passedOver)
	}
	two := 2
	if _, err := (config.Delegations{"lan": held}).Subnet(v1alpha1.DelegatedPrefixAddr{InterfaceRef: "lan", SubnetID: &two}); err == nil {
		t.Error("a value cut from an ambiguous delegation resolved")
	}
}

// The resources the validation cases of a downstream host build on.
const downstreamLAN = `    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: eth0
        addresses:
          - fromRouterAdvertisement: {token: "::151"}
        ipv6:
          advertisedDelegation: {prefixLength: 56}
`

// A downstream host's declaration holds no literal prefix: its bridge, its routes and its
// sets are cut from the delegation the LAN interface is advertised (ADR 0023).
func TestValidateAcceptsADownstreamHost(t *testing.T) {
	cfg, problems := check(t, downstreamLAN+`    - kind: Interface
      metadata: {name: containers}
      spec:
        ifname: br-v6net
        bridge: {}
        addresses:
          - fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2, token: "::1"}
        routes:
          - destination:
              fromDelegatedPrefix: {interfaceRef: lan, subnetID: 3}
            via:
              fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2, token: "::10"}
        ipv6:
          advertise: {mode: slaac}
    - kind: IPAddressSet
      metadata: {name: web}
      spec:
        family: ipv6
        addresses:
          - fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2, token: "::151"}
        networks:
          - fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2}
`, secrets())
	if cfg == nil {
		t.Fatalf("rejected a downstream host:\n%s", problems)
	}
	assertProblems(t, problems, nil)
}

func TestValidateRejectsAnAdvertisedDelegationItCannotRead(t *testing.T) {
	cases := []struct {
		name      string
		resources string
		want      []string
	}{
		{
			name: "no length",
			resources: `    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: eth0
        addresses:
          - fromRouterAdvertisement: {token: "::151"}
        ipv6:
          advertisedDelegation: {}
`,
			want: []string{"spec.ipv6.advertisedDelegation.prefixLength: required"},
		},
		{
			name: "a length longer than a subnet",
			resources: `    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: eth0
        addresses:
          - fromRouterAdvertisement: {token: "::151"}
        ipv6:
          advertisedDelegation: {prefixLength: 65}
`,
			want: []string{"spec.ipv6.advertisedDelegation.prefixLength: 65 is not the length of a delegation; it is between 1 and 64"},
		},
		{
			name: "no address taken from the advertisement",
			resources: `    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: eth0
        addresses: [192.168.10.2/24]
        ipv6:
          advertisedDelegation: {prefixLength: 56}
`,
			want: []string{"spec.ipv6.advertisedDelegation: the advertised prefix is read from the address taken from the router advertisement, and this Interface declares none; add a fromRouterAdvertisement address"},
		},
		{
			name: "a prefix-delegation client as well",
			resources: `    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: eth0
        addresses:
          - fromRouterAdvertisement: {token: "::151"}
        dhcpv6:
          prefixDelegation:
            prefixLength: 56
            duidFile: /secrets/duid
        ipv6:
          advertisedDelegation: {prefixLength: 56}
`,
			want: []string{"spec.ipv6.advertisedDelegation: this Interface runs a prefix-delegation client, which is where its delegation comes from; declare one or the other"},
		},
		{
			name: "an interface address cut from the advertisement without a token",
			resources: downstreamLAN + `    - kind: Interface
      metadata: {name: containers}
      spec:
        ifname: br-v6net
        bridge: {}
        addresses:
          - fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2}
`,
			want: []string{`spec.addresses[0].fromDelegatedPrefix.token: the Interface "lan" is advertised its prefix rather than delegated one, so this address is written as the value it resolves to and needs a token`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := check(t, tc.resources, secrets())
			assertProblems(t, problems, tc.want)
		})
	}
}

// An AAAA record names an address cut from the delegation through the same check as every
// other derived value, so a downstream host's record may name its advertised delegation
// (ADR 0023, ADR 0024).
func TestValidateAcceptsAnAAAARecordCutFromTheAdvertisedDelegation(t *testing.T) {
	cfg, problems := check(t, downstreamLAN+`    - kind: DNSRecordSet
      metadata: {name: example-com}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [web.example.com]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2, token: "::151"}
`, dnsSecrets())
	if cfg == nil {
		t.Fatalf("rejected an AAAA record cut from the advertised delegation:\n%s", problems)
	}
	assertProblems(t, problems, nil)
}
