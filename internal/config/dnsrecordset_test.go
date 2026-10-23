package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// A DNSRecordSet that is valid on its own, for the cases that need one beside them.
const recordSet = `    - kind: DNSRecordSet
      metadata: {name: example-com}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com, "*.example.com"]
            egressRef: pppoe0
`

// dnsSecrets is the credential files the DNS cases name, on top of the usual ones.
func dnsSecrets() secretFiles {
	files := secrets()
	files["/secrets/api-token"] = "a token"
	files["/secrets/other-token"] = "another token"
	return files
}

func TestDNSRecordSetDecodesWithItsDefaults(t *testing.T) {
	cfg, problems := check(t, ifaceWAN+ifaceLAN+pppoe+recordSet, dnsSecrets())
	if cfg == nil {
		t.Fatalf("rejected a coherent document:\n%s", problems)
	}
	assertProblems(t, problems, nil)

	sets := config.ResourcesOf[*v1alpha1.DNSRecordSetSpec](cfg)
	if len(sets) != 1 {
		t.Fatalf("got %d record sets, want 1", len(sets))
	}
	set := sets[0]
	if set.Name != "example-com" {
		t.Errorf("name: got %q", set.Name)
	}
	if set.Spec.Provider != v1alpha1.ProviderCloudflare {
		t.Errorf("provider: got %q", set.Spec.Provider)
	}
	if set.Spec.Zone != "example.com" || set.Spec.APITokenFile != "/secrets/api-token" {
		t.Errorf("zone and token file: got %q %q", set.Spec.Zone, set.Spec.APITokenFile)
	}
	if len(set.Spec.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(set.Spec.Records))
	}

	record := set.Spec.Records[0]
	if len(record.Names) != 2 || record.Names[0] != "example.com" || record.Names[1] != "*.example.com" {
		t.Errorf("names: got %v", record.Names)
	}
	// The three defaults the schema promises for a record that writes none of them.
	if got := record.TypeOrDefault(); got != v1alpha1.DNSRecordA {
		t.Errorf("type defaults to A, got %q", got)
	}
	if record.ProxiedEnabled() {
		t.Error("proxied defaults to false")
	}
	if got := record.TTL.String(); got != "automatic" {
		t.Errorf("an absent ttl is the provider's choice, got %q", got)
	}
}

func TestDNSRecordSetTakesAWrittenTTLAndProxyFlag(t *testing.T) {
	cfg, problems := check(t, ifaceWAN+ifaceLAN+pppoe+`    - kind: DNSRecordSet
      metadata: {name: example-com}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [a.example.com]
            type: A
            egressRef: pppoe0
            proxied: true
            ttl: automatic
          - names: [b.example.com]
            egressRef: pppoe0
            ttl: 5m
`, dnsSecrets())
	if cfg == nil {
		t.Fatalf("rejected a coherent document:\n%s", problems)
	}
	records := config.ResourcesOf[*v1alpha1.DNSRecordSetSpec](cfg)[0].Spec.Records
	if !records[0].ProxiedEnabled() {
		t.Error("proxied: true was written")
	}
	if got := records[0].TTL.String(); got != "automatic" {
		t.Errorf("a proxied record with no ttl is the provider's choice, got %q", got)
	}
	if records[1].ProxiedEnabled() {
		t.Error("proxied was not written on the second record")
	}
	if records[1].TTL.For != 5*time.Minute {
		t.Errorf("ttl: got %s, want 5m", records[1].TTL)
	}
}

// An AAAA record holds an address cut from the delegation, and it may share its names
// with A records: a name with both is two records at the provider (ADR 0024).
func TestDNSRecordSetTakesAnAAAARecordCutFromTheDelegation(t *testing.T) {
	cfg, problems := check(t, ifaceWAN+ifaceLAN+pppoe+`    - kind: DNSRecordSet
      metadata: {name: example-com}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com, "*.example.com"]
            egressRef: pppoe0
          - names: [example.com, "*.example.com"]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::200"}
            ttl: 5m
`, dnsSecrets())
	if cfg == nil {
		t.Fatalf("rejected a coherent document:\n%s", problems)
	}
	assertProblems(t, problems, nil)

	records := config.ResourcesOf[*v1alpha1.DNSRecordSetSpec](cfg)[0].Spec.Records
	record := records[1]
	if got := record.TypeOrDefault(); got != v1alpha1.DNSRecordAAAA {
		t.Errorf("type: got %q, want AAAA", got)
	}
	if record.Address == nil || record.Address.FromDelegatedPrefix == nil {
		t.Fatalf("address: got %+v", record.Address)
	}
	derived := record.Address.FromDelegatedPrefix
	if derived.InterfaceRef != "wan" || derived.SubnetID == nil || *derived.SubnetID != 1 || derived.Token != "::200" {
		t.Errorf("fromDelegatedPrefix: got %+v", derived)
	}
	if record.EgressRef != "" {
		t.Errorf("egressRef: got %q", record.EgressRef)
	}
	if records[0].Address != nil {
		t.Errorf("the A record has no address: %+v", records[0].Address)
	}
}

// An AAAA record's address is always derived. A literal one is something regied would
// have nothing to keep up to date, and it is refused where it is read, with the reason
// (ADR 0024).
func TestAnAAAARecordWithALiteralAddressIsRefused(t *testing.T) {
	for _, address := range []string{
		`            address: "2001:db8::200"`,
		`            address: {}`,
		`            address: {literal: "2001:db8::200"}`,
		`            address: [2001:db8::200]`,
	} {
		_, err := config.Parse(doc(ifaceWAN + `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            type: AAAA
` + address + "\n"))
		if err == nil {
			t.Errorf("%s: parsed, want an error", address)
		}
	}

	_, err := config.Parse(doc(ifaceWAN + `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            type: AAAA
            address: "2001:db8::200"
`))
	if err == nil || !strings.Contains(err.Error(), "derived") {
		t.Errorf("the refusal says the address is derived: %v", err)
	}
}

func TestDNSRecordSetRefusals(t *testing.T) {
	cases := []struct {
		name      string
		resources string
		want      []string
	}{
		{
			name: "a provider there is no implementation for",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: aardvark
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            egressRef: pppoe0
`,
			want: []string{`"aardvark" is not a provider regied writes to`},
		},
		{
			name: "no provider at all",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            egressRef: pppoe0
`,
			want: []string{"spec.provider: required"},
		},
		{
			// An uplink's own IPv6 address is not what the outside world reaches over
			// IPv6, and an AAAA record never follows it (ADR 0019, ADR 0024).
			name: "an AAAA record following an uplink",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            type: AAAA
            egressRef: pppoe0
            address:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::200"}
`,
			want: []string{"spec.records[0].egressRef: an AAAA record holds an address cut from the delegation"},
		},
		{
			name: "an AAAA record with no address",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            type: AAAA
`,
			want: []string{"spec.records[0].address: required"},
		},
		{
			// A record holds an address, so the token is the host half it cannot do
			// without (ADR 0021).
			name: "an AAAA record cut from the delegation without a token",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1}
`,
			want: []string{"spec.records[0].address.fromDelegatedPrefix.token: required"},
		},
		{
			name: "an AAAA record whose token is not an interface identifier",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "2001:db8::200"}
`,
			want: []string{`spec.records[0].address.fromDelegatedPrefix.token: "2001:db8::200" is not an interface identifier`},
		},
		{
			name: "an AAAA record with no subnet ID",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: wan, token: "::200"}
`,
			want: []string{"spec.records[0].address.fromDelegatedPrefix.subnetID: required"},
		},
		{
			// The prefix has to be one this host was delegated: an interface that asks
			// for nothing is handed nothing.
			name: "an AAAA record cut from an interface with no prefix-delegation client",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: lan, subnetID: 1, token: "::200"}
`,
			want: []string{`spec.records[0].address.fromDelegatedPrefix.interfaceRef: the Interface "lan" has no prefix-delegation client`},
		},
		{
			name: "an AAAA record cut from an interface that is not declared",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: nowhere, subnetID: 1, token: "::200"}
`,
			want: []string{`spec.records[0].address.fromDelegatedPrefix.interfaceRef`},
		},
		{
			name: "an A record carrying an address",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            egressRef: pppoe0
            address:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::200"}
`,
			want: []string{"spec.records[0].address: an A record follows an uplink"},
		},
		{
			name: "a record type regied does not write",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            type: MX
            egressRef: pppoe0
`,
			want: []string{`spec.records[0].type: "MX" is not a record type regied writes`},
		},
		{
			name: "the same AAAA name across two sets",
			resources: `    - kind: DNSRecordSet
      metadata: {name: first}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [www.example.com]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1, token: "::200"}
    - kind: DNSRecordSet
      metadata: {name: second}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/other-token
        records:
          - names: [WWW.example.com.]
            type: AAAA
            address:
              fromDelegatedPrefix: {interfaceRef: wan, subnetID: 2, token: "::200"}
`,
			want: []string{`the AAAA record for "WWW.example.com." is already declared by DNSRecordSet/first`},
		},
		{
			name: "a name outside the zone",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com, www.example.org]
            egressRef: pppoe0
`,
			want: []string{`spec.records[0].names[1]: "www.example.org" is not inside the zone "example.com"`},
		},
		{
			// A near miss the suffix has to refuse: notexample.com ends in the zone's
			// text but is a different zone.
			name: "a name that merely ends in the zone's text",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [notexample.com]
            egressRef: pppoe0
`,
			want: []string{`spec.records[0].names[0]: "notexample.com" is not inside the zone`},
		},
		{
			name: "a record following the tunnel",
			resources: dslite + `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            egressRef: dslite
`,
			want: []string{`nothing can be published through the DSLiteTunnel "dslite"`},
		},
		{
			name: "a record following a zone rather than an uplink",
			resources: zonesLAN + `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            egressRef: lan
`,
			want: []string{`"lan" is an Interface, not an uplink`},
		},
		{
			name: "the same name and type twice in one set",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            egressRef: pppoe0
          - names: [example.com]
            egressRef: pppoe0
            proxied: true
`,
			want: []string{`the A record for "example.com" is already declared by spec.records[0].names[0]`},
		},
		{
			name: "the same name and type across two sets",
			resources: recordSet + `    - kind: DNSRecordSet
      metadata: {name: other}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/other-token
        records:
          - names: ["*.example.com"]
            egressRef: pppoe0
`,
			want: []string{`the A record for "*.example.com" is already declared by DNSRecordSet/example-com`},
		},
		{
			name: "a TTL shorter than the provider accepts",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            egressRef: pppoe0
            ttl: 30s
`,
			want: []string{"spec.records[0].ttl: 30s is outside what the provider accepts"},
		},
		{
			name: "a TTL longer than the provider accepts",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            egressRef: pppoe0
            ttl: 48h
`,
			want: []string{"spec.records[0].ttl: 48h0m0s is outside what the provider accepts"},
		},
		{
			name: "a TTL on a proxied record",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - names: [example.com]
            egressRef: pppoe0
            proxied: true
            ttl: 5m
`,
			want: []string{"spec.records[0].ttl: a proxied record's TTL is the provider's to decide"},
		},
		{
			name: "no records",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
`,
			want: []string{"spec.records: required"},
		},
		{
			name: "a record with no names",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/api-token
        records:
          - egressRef: pppoe0
`,
			want: []string{"spec.records[0].names: required"},
		},
		{
			name: "an API token file that is not there",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /secrets/gone
        records:
          - names: [example.com]
            egressRef: pppoe0
`,
			want: []string{"spec.apiTokenFile: /secrets/gone: file is missing"},
		},
		{
			name: "no zone and no token file",
			resources: `    - kind: DNSRecordSet
      metadata: {name: set}
      spec:
        provider: cloudflare
        records:
          - names: [example.com]
            egressRef: pppoe0
`,
			// The name check needs a zone, and says nothing when there is none: one
			// missing field should not report twice.
			want: []string{"spec.zone: required", "spec.apiTokenFile: required"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, problems := check(t, ifaceWAN+ifaceLAN+pppoe+tc.resources, dnsSecrets())
			if cfg != nil {
				t.Fatalf("accepted %s", tc.name)
			}
			assertProblems(t, problems, tc.want)
		})
	}
}
