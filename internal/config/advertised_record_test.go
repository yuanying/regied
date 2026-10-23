package config_test

import (
	"math"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// recordsOn is a DNSRecordSet in example.com holding the given records.
func recordsOn(records string) string {
	return `    - kind: DNSRecordSet
      metadata: {name: example-com}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
` + records
}

// A downstream host publishes its own LAN address by naming the interface that takes it
// from the advertisement. The token is that interface's, and is not written again
// (ADR 0024).
func TestDNSRecordSetTakesAnAAAARecordHoldingTheAdvertisedAddress(t *testing.T) {
	cfg, problems := check(t, downstreamLAN+recordsOn(`          - names: [host.example.com]
            type: AAAA
            address:
              fromRouterAdvertisement: {interfaceRef: lan}
          - names: [web.example.com]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2, token: "::151"}
`), dnsSecrets())
	if cfg == nil {
		t.Fatalf("rejected a coherent document:\n%s", problems)
	}
	assertProblems(t, problems, nil)

	records := config.ResourcesOf[*v1alpha1.DNSRecordSetSpec](cfg)[0].Spec.Records
	address := records[0].Address
	if address == nil || address.FromRouterAdvertisement == nil || address.FromDelegatedPrefix != nil {
		t.Fatalf("address: got %+v", address)
	}
	if got := address.FromRouterAdvertisement.InterfaceRef; got != "lan" {
		t.Errorf("fromRouterAdvertisement.interfaceRef: got %q", got)
	}
	if records[1].Address == nil || records[1].Address.FromDelegatedPrefix == nil || records[1].Address.FromRouterAdvertisement != nil {
		t.Errorf("the second record is cut from the delegation: %+v", records[1].Address)
	}
}

// The address a record takes from the advertisement does not need the interface to know
// its delegation: holding the address is enough.
func TestAnAdvertisedAddressRecordNeedsNoAdvertisedDelegation(t *testing.T) {
	_, problems := check(t, `    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: eth0
        addresses:
          - fromRouterAdvertisement: {token: "::151"}
`+recordsOn(`          - names: [host.example.com]
            type: AAAA
            address:
              fromRouterAdvertisement: {interfaceRef: lan}
`), dnsSecrets())
	assertProblems(t, problems, nil)
}

func TestAnAdvertisedAddressRecordRefusals(t *testing.T) {
	cases := []struct {
		name      string
		resources string
		want      []string
	}{
		{
			// The token is the interface's; without an address from the advertisement
			// there is nothing for the record to hold.
			name: "an interface holding no address from the advertisement",
			resources: downstreamLAN + `    - kind: Interface
      metadata: {name: containers}
      spec:
        ifname: br-v6net
        bridge: {}
        addresses:
          - fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2, token: "::1"}
` + recordsOn(`          - names: [host.example.com]
            type: AAAA
            address:
              fromRouterAdvertisement: {interfaceRef: containers}
`),
			want: []string{`spec.records[0].address.fromRouterAdvertisement.interfaceRef: the Interface "containers" takes no address from the router advertisement; give it a fromRouterAdvertisement address, or cut this record from the delegation with fromDelegatedPrefix`},
		},
		{
			name: "an interface that is not declared",
			resources: downstreamLAN + recordsOn(`          - names: [host.example.com]
            type: AAAA
            address:
              fromRouterAdvertisement: {interfaceRef: nowhere}
`),
			want: []string{`spec.records[0].address.fromRouterAdvertisement.interfaceRef: no Interface named "nowhere"`},
		},
		{
			name: "no interface named",
			resources: downstreamLAN + recordsOn(`          - names: [host.example.com]
            type: AAAA
            address:
              fromRouterAdvertisement: {}
`),
			want: []string{"spec.records[0].address.fromRouterAdvertisement.interfaceRef: required"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := check(t, tc.resources, dnsSecrets())
			assertProblems(t, problems, tc.want)
		})
	}
}

// The token is not written twice. A record that tries is refused where it is read, rather
// than one of the two being ignored.
func TestATokenOnTheAdvertisedAddressRecordIsRefused(t *testing.T) {
	_, err := config.Parse(doc(downstreamLAN + recordsOn(`          - names: [host.example.com]
            type: AAAA
            address:
              fromRouterAdvertisement: {interfaceRef: lan, token: "::151"}
`)))
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("parsed a token on the record, want an error naming it: %v", err)
	}
}

// A record's address is one form. Both is a declaration of two addresses for one record.
func TestARecordAddressWithBothFormsIsRefused(t *testing.T) {
	_, err := config.Parse(doc(downstreamLAN + recordsOn(`          - names: [host.example.com]
            type: AAAA
            address:
              fromRouterAdvertisement: {interfaceRef: lan}
              fromDelegatedPrefix: {interfaceRef: lan, subnetID: 2, token: "::151"}
`)))
	if err == nil {
		t.Error("parsed an address with both forms, want an error")
	}
}

// The address a record holds is the one the interface took from the advertisement with
// its token, chosen as the delegation is: the one preferred longest, so that a record
// moves to the new prefix as soon as it has arrived (ADR 0023, ADR 0024).
func TestTheAdvertisedAddressIsTheOneWithTheTokenPreferredLongest(t *testing.T) {
	held, passedOver := config.AddressFromAdvertisement([]config.AdvertisedAddress{
		advertised("fe80::151", math.MaxUint64),
		advertised("fd00:0:0:1::151", math.MaxUint64),
		advertised("2001:db8:0:101:aaaa:bbbb:cccc:dddd", math.MaxUint64),
		advertised("2001:db8:0:101::151", 0),
		advertised("2001:db8:1:201::151", 1000),
	}, "::151")
	if !slices.Equal(held, []netip.Addr{netip.MustParseAddr("2001:db8:1:201::151")}) {
		t.Errorf("held %v, want the new address", held)
	}
	if !slices.Equal(passedOver, []netip.Addr{netip.MustParseAddr("2001:db8:0:101::151")}) {
		t.Errorf("passed over %v, want the old address", passedOver)
	}
}

func TestNoAdvertisedAddressWithTheTokenIsNone(t *testing.T) {
	held, passedOver := config.AddressFromAdvertisement([]config.AdvertisedAddress{
		advertised("2001:db8:0:101::152", 1000),
	}, "::151")
	if len(held) != 0 || len(passedOver) != 0 {
		t.Errorf("held %v, passed over %v; want none", held, passedOver)
	}
}

// Two addresses preferred equally long cannot be told apart, and both are held, so the
// record waits and says why.
func TestTwoAdvertisedAddressesPreferredEquallyLongAreBothHeld(t *testing.T) {
	held, passedOver := config.AddressFromAdvertisement([]config.AdvertisedAddress{
		advertised("2001:db8:1:201::151", 1000),
		advertised("2001:db8:0:101::151", 1000),
	}, "::151")
	want := []netip.Addr{netip.MustParseAddr("2001:db8:0:101::151"), netip.MustParseAddr("2001:db8:1:201::151")}
	if !slices.Equal(held, want) || len(passedOver) != 0 {
		t.Errorf("held %v, passed over %v; want both held", held, passedOver)
	}

	_, err := config.Advertised{"lan": held}.Address("lan")
	if err == nil {
		t.Error("an ambiguous advertisement resolved to an address")
	}
	if _, err := (config.Advertised{}).Address("lan"); err == nil {
		t.Error("nothing advertised resolved to an address")
	}
	got, err := config.Advertised{"lan": want[:1]}.Address("lan")
	if err != nil || got != want[0] {
		t.Errorf("one address resolves to itself: %v, %v", got, err)
	}
}
