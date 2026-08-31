package config_test

import (
	"testing"

	"github.com/yuanying/regied/internal/config"
)

// The resources the validation cases build on. Each is valid on its own.
const (
	ifaceWAN = `    - kind: Interface
      metadata: {name: wan}
      spec:
        ifname: eth0
        dhcpv6:
          prefixDelegation:
            prefixLength: 56
            duidFile: /secrets/duid
`
	ifaceLAN = `    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses: [192.168.10.1/24]
`
	pppoe = `    - kind: PPPoESession
      metadata: {name: pppoe0}
      spec:
        interfaceRef: wan
        userIDFile: /secrets/user-id
        passwordFile: /secrets/password
`
	dslite = `    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        localAddressFrom: {interfaceRef: lan}
        aftrHost: aftr.example.net
`
	zones = `    - kind: FirewallZone
      metadata: {name: wan}
      spec:
        linkRefs: [wan, pppoe0]
    - kind: FirewallZone
      metadata: {name: lan}
      spec:
        linkRefs: [lan]
`
	zonesLAN = `    - kind: FirewallZone
      metadata: {name: lan}
      spec:
        linkRefs: [lan]
`
)

func secrets() secretFiles {
	return secretFiles{
		"/secrets/duid":     "00:03:00:01:00:00:5e:00:53:01",
		"/secrets/user-id":  "subscriber@example.net",
		"/secrets/password": "hunter2",
	}
}

// check parses and validates, and returns every problem the validation reported —
// warnings included, whether or not it succeeded.
func check(t *testing.T, resources string, files config.FileChecker) (*config.Config, config.Problems) {
	t.Helper()
	document, err := config.Parse(doc(resources))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg, err := config.Validate(document, config.WithSecretFiles(files))
	if err == nil {
		return cfg, cfg.Warnings()
	}
	var invalid *config.ValidationError
	if !asValidationError(err, &invalid) {
		t.Fatalf("want a *config.ValidationError, got %T: %v", err, err)
	}
	return nil, invalid.Problems
}

func TestValidateAcceptsACoherentDocument(t *testing.T) {
	cfg, problems := check(t, ifaceWAN+ifaceLAN+pppoe+dslite+zones, secrets())
	if cfg == nil {
		t.Fatalf("rejected a coherent document:\n%s", problems)
	}
	assertProblems(t, problems, nil)
}

func TestValidateResolvesReferences(t *testing.T) {
	cases := []struct {
		name      string
		resources string
		want      []string
	}{
		{
			name: "interfaceRef",
			resources: ifaceWAN + `    - kind: PPPoESession
      metadata: {name: pppoe0}
      spec:
        interfaceRef: wan0
        userIDFile: /secrets/user-id
        passwordFile: /secrets/password
`,
			want: []string{`spec.interfaceRef: no Interface named "wan0"`},
		},
		{
			name: "underlayRef",
			resources: ifaceLAN + `    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        localAddressFrom: {interfaceRef: lan}
        aftrHost: aftr.example.net
`,
			want: []string{`spec.underlayRef: no Interface named "wan"`},
		},
		{
			name: "egressRef",
			resources: ifaceWAN + ifaceLAN + `    - kind: SourceNAT
      metadata: {name: masq}
      spec:
        egressRef: pppoe1
`,
			want: []string{`spec.egressRef: no PPPoESession or DSLiteTunnel named "pppoe1"`},
		},
		{
			name: "egressRef naming something that is not an uplink",
			resources: ifaceWAN + ifaceLAN + `    - kind: SourceNAT
      metadata: {name: masq}
      spec:
        egressRef: lan
`,
			want: []string{`spec.egressRef: "lan" is an Interface, not an uplink`},
		},
		{
			name: "linkRefs",
			resources: ifaceWAN + ifaceLAN + `    - kind: FirewallZone
      metadata: {name: dmz}
      spec:
        linkRefs: [lan, dmz0]
`,
			want: []string{`spec.linkRefs[1]: no Interface, PPPoESession or DSLiteTunnel named "dmz0"`},
		},
		{
			name: "linkRefs naming something that is not a link",
			resources: ifaceLAN + `    - kind: IPAddressSet
      metadata: {name: friends}
      spec:
        family: ipv4
        addresses: [192.0.2.1]
    - kind: FirewallZone
      metadata: {name: dmz}
      spec:
        linkRefs: [friends]
`,
			want: []string{`spec.linkRefs[0]: "friends" is an IPAddressSet, not a link`},
		},
		{
			name: "addressSetRefs on a rule",
			resources: ifaceLAN + zonesLAN + `    - kind: FirewallPolicy
      metadata: {name: lan-to-self}
      spec:
        from: lan
        to: self
        defaultAction: drop
        rules:
          - name: published
            action: accept
            destinationAddressSetRefs: [absent]
`,
			want: []string{`spec.rules[0].destinationAddressSetRefs[0]: no IPAddressSet named "absent"`},
		},
		{
			name: "sourceAddressSetRefs on a policy",
			resources: ifaceWAN + ifaceLAN + pppoe + `    - kind: EgressRoutePolicy
      metadata: {name: p}
      spec:
        priority: 10
        egressRef: pppoe0
        sourceAddressSetRefs: [absent]
`,
			want: []string{`spec.sourceAddressSetRefs[0]: no IPAddressSet named "absent"`},
		},
		{
			name: "firewall zones",
			resources: ifaceLAN + `    - kind: FirewallPolicy
      metadata: {name: dmz-to-lan}
      spec:
        from: dmz
        to: lan
        defaultAction: drop
`,
			want: []string{
				`spec.from: no FirewallZone named "dmz"`,
				`spec.to: "lan" is an Interface, not a FirewallZone`,
			},
		},
		{
			name: "listenOn",
			resources: ifaceLAN + `    - kind: DNSForwarder
      metadata: {name: lan}
      spec:
        listenOn: [lan, loopback, absent]
        upstreams: [192.0.2.53]
`,
			want: []string{`spec.listenOn[2]: no Interface, PPPoESession or DSLiteTunnel named "absent"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, problems := check(t, tc.resources, secrets())
			if cfg != nil {
				t.Fatal("accepted a document with an unresolved reference")
			}
			assertProblems(t, problems, tc.want)
		})
	}
}

// "self" is a policy's destination, never a zone's name and never a policy's source.
func TestValidateSelfZone(t *testing.T) {
	t.Run("accepted as a destination", func(t *testing.T) {
		cfg, problems := check(t, ifaceWAN+ifaceLAN+pppoe+zones+`    - kind: FirewallPolicy
      metadata: {name: lan-to-self}
      spec:
        from: lan
        to: self
        defaultAction: accept
`, secrets())
		if cfg == nil {
			t.Fatalf("rejected a policy to self:\n%s", problems)
		}
	})

	t.Run("rejected as a zone name", func(t *testing.T) {
		_, problems := check(t, ifaceLAN+`    - kind: FirewallZone
      metadata: {name: self}
      spec:
        linkRefs: [lan]
`, secrets())
		assertProblems(t, problems, []string{`metadata.name: "self" is reserved for the host itself`})
	})

	t.Run("rejected as a policy source", func(t *testing.T) {
		_, problems := check(t, ifaceLAN+zonesLAN+`    - kind: FirewallPolicy
      metadata: {name: self-to-lan}
      spec:
        from: self
        to: lan
        defaultAction: accept
`, secrets())
		assertProblems(t, problems, []string{`spec.from: "self" cannot be a policy's source`})
	})
}

func TestValidateNameUniqueness(t *testing.T) {
	t.Run("rejects a duplicate within a kind", func(t *testing.T) {
		_, problems := check(t, ifaceLAN+`    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan2
`, secrets())
		assertProblems(t, problems, []string{`metadata.name: another Interface is already named "lan"`})
	})

	t.Run("accepts the same name in different kinds", func(t *testing.T) {
		cfg, problems := check(t, ifaceLAN+zonesLAN+`    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
        subnet: 192.168.10.0/24
        pool: {start: 192.168.10.64, end: 192.168.10.127}
`, secrets())
		if cfg == nil {
			t.Fatalf("rejected the same name in three kinds:\n%s", problems)
		}
	})

	t.Run("rejects a resource with no name", func(t *testing.T) {
		_, problems := check(t, `    - kind: Interface
      metadata: {}
      spec:
        ifname: br-lan
`, secrets())
		assertProblems(t, problems, []string{"metadata.name: required"})
	})
}

func TestValidateRequiredFields(t *testing.T) {
	cases := []struct {
		name      string
		resources string
		want      []string
	}{
		{
			name: "Interface without an ifname",
			resources: `    - kind: Interface
      metadata: {name: lan}
      spec: {mtu: 1500}
`,
			want: []string{"spec.ifname: required"},
		},
		{
			name: "PPPoESession without credentials",
			resources: ifaceWAN + `    - kind: PPPoESession
      metadata: {name: pppoe0}
      spec:
        interfaceRef: wan
`,
			want: []string{"spec.userIDFile: required", "spec.passwordFile: required"},
		},
		{
			name: "EgressRoutePolicy without a priority",
			resources: ifaceWAN + pppoe + `    - kind: EgressRoutePolicy
      metadata: {name: p}
      spec:
        egressRef: pppoe0
        sourceRanges: [192.168.10.0/24]
`,
			want: []string{"spec.priority: required"},
		},
		{
			name: "FirewallPolicy without a default action",
			resources: ifaceLAN + zonesLAN + `    - kind: FirewallPolicy
      metadata: {name: lan-to-self}
      spec:
        from: lan
        to: self
`,
			want: []string{"spec.defaultAction: required"},
		},
		{
			name: "a firewall rule without a name or an action",
			resources: ifaceLAN + zonesLAN + `    - kind: FirewallPolicy
      metadata: {name: lan-to-self}
      spec:
        from: lan
        to: self
        defaultAction: drop
        rules:
          - {protocol: tcp}
`,
			want: []string{"spec.rules[0].name: required", "spec.rules[0].action: required"},
		},
		{
			name: "FirewallZone without links",
			resources: `    - kind: FirewallZone
      metadata: {name: dmz}
      spec: {}
`,
			want: []string{"spec.linkRefs: required"},
		},
		{
			name: "PortForward without a target",
			resources: ifaceWAN + pppoe + `    - kind: PortForward
      metadata: {name: https}
      spec:
        egressRef: pppoe0
        protocol: tcp
        port: 443
`,
			want: []string{"spec.target.address: required"},
		},
		{
			name: "DHCPServer without a subnet or a pool",
			resources: ifaceLAN + `    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
`,
			want: []string{"spec.subnet: required", "spec.pool: required"},
		},
		{
			name: "DNSForwarder without upstreams",
			resources: ifaceLAN + `    - kind: DNSForwarder
      metadata: {name: lan}
      spec:
        listenOn: [lan]
`,
			want: []string{"spec.upstreams: required"},
		},
		{
			name: "an EgressRoutePolicy with nothing to match on",
			resources: ifaceWAN + pppoe + `    - kind: EgressRoutePolicy
      metadata: {name: p}
      spec:
        priority: 10
        egressRef: pppoe0
`,
			want: []string{"spec: at least one of sourceRanges and sourceAddressSetRefs is required"},
		},
		{
			name: "an empty IPAddressSet",
			resources: `    - kind: IPAddressSet
      metadata: {name: empty}
      spec:
        family: ipv4
`,
			want: []string{"spec: at least one of addresses and networks is required"},
		},
		{
			name: "an IPAddressSet without a family",
			resources: `    - kind: IPAddressSet
      metadata: {name: friends}
      spec:
        addresses: [192.0.2.1]
`,
			want: []string{"spec.family: required"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, problems := check(t, tc.resources, secrets())
			if cfg != nil {
				t.Fatal("accepted a document with a missing required field")
			}
			assertProblems(t, problems, tc.want)
		})
	}
}

// A pair of fields where exactly one has to be written needs both halves checked. A
// document with neither is the one that is easy to miss.
func TestValidateExclusivePairs(t *testing.T) {
	cases := []struct {
		name      string
		resources string
		want      []string
	}{
		{
			name: "a tunnel with both AFTR forms",
			resources: ifaceWAN + ifaceLAN + `    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        localAddressFrom: {interfaceRef: lan}
        aftrHost: aftr.example.net
        aftrAddress: 2001:db8:ffff::1
`,
			want: []string{"spec: exactly one of aftrHost and aftrAddress is required; both are set"},
		},
		{
			name: "a tunnel with neither AFTR form",
			resources: ifaceWAN + ifaceLAN + `    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        localAddressFrom: {interfaceRef: lan}
`,
			want: []string{"spec: exactly one of aftrHost and aftrAddress is required; neither is set"},
		},
		{
			name: "a tunnel with both local address forms",
			resources: ifaceWAN + ifaceLAN + `    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        localAddressFrom: {interfaceRef: lan}
        localAddress: 2001:db8:0:1::1
        aftrHost: aftr.example.net
`,
			want: []string{"spec: exactly one of localAddressFrom and localAddress is required; both are set"},
		},
		{
			name: "a tunnel with neither local address form",
			resources: ifaceWAN + `    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        aftrHost: aftr.example.net
`,
			want: []string{"spec: exactly one of localAddressFrom and localAddress is required; neither is set"},
		},
		{
			name: "a port forward with both port forms",
			resources: ifaceWAN + pppoe + `    - kind: PortForward
      metadata: {name: https}
      spec:
        egressRef: pppoe0
        protocol: tcp
        port: 443
        portRange: 443-444
        target: {address: 192.168.10.20}
`,
			want: []string{"spec: exactly one of port and portRange is required; both are set"},
		},
		{
			name: "a port forward with neither port form",
			resources: ifaceWAN + pppoe + `    - kind: PortForward
      metadata: {name: https}
      spec:
        egressRef: pppoe0
        protocol: tcp
        target: {address: 192.168.10.20}
`,
			want: []string{"spec: exactly one of port and portRange is required; neither is set"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, problems := check(t, tc.resources, secrets())
			if cfg != nil {
				t.Fatal("accepted a document with a broken exclusive pair")
			}
			assertProblems(t, problems, tc.want)
		})
	}
}

// The list docs/spec/configuration.md gives under "Validation".
func TestValidateRejectsWhatTheSpecSaysItRejects(t *testing.T) {
	cases := []struct {
		name      string
		global    string
		resources string
		want      []string
	}{
		{
			name:   "reverse path filtering together with policy routing",
			global: "    sourceValidation: true\n",
			resources: ifaceWAN + ifaceLAN + pppoe + `    - kind: EgressRoutePolicy
      metadata: {name: p}
      spec:
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.168.10.0/24]
`,
			want: []string{"spec.global.sourceValidation: cannot be true while an EgressRoutePolicy is declared"},
		},
		{
			name: "two policies with the same priority in the same family",
			resources: ifaceWAN + ifaceLAN + pppoe + `    - kind: EgressRoutePolicy
      metadata: {name: first}
      spec:
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.168.10.0/24]
    - kind: EgressRoutePolicy
      metadata: {name: second}
      spec:
        family: ipv4
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.168.11.0/24]
`,
			want: []string{`spec.priority: the ipv4 EgressRoutePolicy "first" already has priority 10`},
		},
		{
			name: "two policies with the same from and to",
			resources: ifaceWAN + ifaceLAN + pppoe + zones + `    - kind: FirewallPolicy
      metadata: {name: first}
      spec:
        from: lan
        to: wan
        defaultAction: accept
    - kind: FirewallPolicy
      metadata: {name: second}
      spec:
        from: lan
        to: wan
        defaultAction: drop
`,
			want: []string{`spec: the FirewallPolicy "first" already covers lan to wan`},
		},
		{
			name: "a bridge member that carries an address",
			resources: `    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        bridge: {members: [eth1, eth2]}
        addresses: [192.168.10.1/24]
    - kind: Interface
      metadata: {name: port1}
      spec:
        ifname: eth1
        mtu: 9000
        addresses: [192.168.11.1/24]
`,
			want: []string{`spec.addresses: "eth1" is a member of the bridge "br-lan" and cannot carry addresses of its own`},
		},
		{
			name: "an address derived from an interface that asks for no prefix",
			resources: `    - kind: Interface
      metadata: {name: wan}
      spec:
        ifname: eth0
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses:
          - fromDelegatedPrefix: {interfaceRef: wan, subnetID: 1}
`,
			want: []string{`spec.addresses[0].fromDelegatedPrefix.interfaceRef: the Interface "wan" has no prefix-delegation client`},
		},
		{
			name: "a port forward through a DS-Lite tunnel",
			resources: ifaceWAN + ifaceLAN + dslite + `    - kind: PortForward
      metadata: {name: https}
      spec:
        egressRef: dslite
        protocol: tcp
        port: 443
        target: {address: 192.168.10.20}
`,
			want: []string{`spec.egressRef: nothing can be published through the DSLiteTunnel "dslite"`},
		},
		{
			name: "a port forward whose protocol is written as a number",
			resources: ifaceWAN + pppoe + `    - kind: PortForward
      metadata: {name: mosh}
      spec:
        egressRef: pppoe0
        protocol: 17
        portRange: 60000-60010
        target: {address: 192.168.10.30}
`,
			want: []string{"spec.protocol: a port forward is tcp or udp, not 17"},
		},
		{
			name: "source NAT on a DS-Lite tunnel",
			resources: ifaceWAN + ifaceLAN + dslite + `    - kind: SourceNAT
      metadata: {name: masq}
      spec:
        egressRef: dslite
`,
			want: []string{`spec.egressRef: the DSLiteTunnel "dslite" is translated by the AFTR`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			global := tc.global
			if global == "" {
				global = "    ipForwarding: true\n"
			}
			document, err := config.Parse([]byte("apiVersion: net.unstable.cloud/v1alpha1\nkind: NetworkConfig\nmetadata:\n  name: test\nspec:\n  global:\n" + global + "  resources:\n" + tc.resources))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			cfg, err := config.Validate(document, config.WithSecretFiles(secrets()))
			if err == nil {
				t.Fatalf("accepted what the spec says to reject; warnings: %s", cfg.Warnings())
			}
			var invalid *config.ValidationError
			if !asValidationError(err, &invalid) {
				t.Fatalf("want a *config.ValidationError, got %T", err)
			}
			assertProblems(t, invalid.Problems, tc.want)
		})
	}
}

// Bringing an uplink up without authentication is not a degraded success (ADR 0003).
func TestValidateSecretFiles(t *testing.T) {
	cases := []struct {
		name  string
		files secretFiles
		want  []string
	}{
		{
			name:  "missing",
			files: secretFiles{"/secrets/duid": "x", "/secrets/user-id": "u"},
			want:  []string{"spec.passwordFile: /secrets/password: file is missing"},
		},
		{
			name:  "empty",
			files: secretFiles{"/secrets/duid": "x", "/secrets/user-id": "u", "/secrets/password": ""},
			want:  []string{"spec.passwordFile: /secrets/password: file is empty"},
		},
		{
			name:  "a missing DUID file",
			files: secretFiles{"/secrets/user-id": "u", "/secrets/password": "p"},
			want:  []string{"spec.dhcpv6.prefixDelegation.duidFile: /secrets/duid: file is missing"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, problems := check(t, ifaceWAN+ifaceLAN+pppoe, tc.files)
			if cfg != nil {
				t.Fatal("accepted a document naming an unusable secret file")
			}
			assertProblems(t, problems, tc.want)
		})
	}
}

// Two things are worth saying out loud without refusing the document.
func TestValidateWarns(t *testing.T) {
	t.Run("prefix delegation with no DUID file", func(t *testing.T) {
		cfg, problems := check(t, `    - kind: Interface
      metadata: {name: wan}
      spec:
        ifname: eth0
        dhcpv6:
          prefixDelegation: {prefixLength: 56}
`, secrets())
		if cfg == nil {
			t.Fatalf("a missing duidFile is a warning, not an error:\n%s", problems)
		}
		assertProblems(t, problems, []string{"spec.dhcpv6.prefixDelegation.duidFile: not set"})
	})

	t.Run("stateless DHCPv6 nothing will ask for", func(t *testing.T) {
		cfg, problems := check(t, `    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses: [192.168.10.1/24]
        ipv6:
          advertise: {mode: slaac}
    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
        subnet: 192.168.10.0/24
        pool: {start: 192.168.10.64, end: 192.168.10.127}
        ipv6: {mode: stateless}
`, secrets())
		if cfg == nil {
			t.Fatalf("this is a warning, not an error:\n%s", problems)
		}
		assertProblems(t, problems, []string{`spec.ipv6: the Interface "lan" does not advertise otherInformation`})
	})
}

func TestValidateAddressSetFamilies(t *testing.T) {
	t.Run("a set holding an address of the other family", func(t *testing.T) {
		_, problems := check(t, `    - kind: IPAddressSet
      metadata: {name: mixed}
      spec:
        family: ipv4
        addresses: [192.0.2.1, 2001:db8::1]
        networks: [2001:db8:0:2::/64]
`, secrets())
		assertProblems(t, problems, []string{
			"spec.addresses[1]: 2001:db8::1 is not ipv4",
			"spec.networks[0]: 2001:db8:0:2::/64 is not ipv4",
		})
	})

	t.Run("a rule naming a set of the other family", func(t *testing.T) {
		_, problems := check(t, ifaceLAN+zonesLAN+`    - kind: IPAddressSet
      metadata: {name: published-v6}
      spec:
        family: ipv6
        addresses: [2001:db8:0:1::20]
    - kind: FirewallPolicy
      metadata: {name: lan-to-self}
      spec:
        from: lan
        to: self
        defaultAction: drop
        rules:
          - name: published
            action: accept
            family: ipv4
            destinationAddressSetRefs: [published-v6]
`, secrets())
		assertProblems(t, problems, []string{`spec.rules[0].destinationAddressSetRefs[0]: the IPAddressSet "published-v6" is ipv6, but the rule is ipv4`})
	})
}

// A forward that translates a range has to translate it onto a range of the same width.
// Where it does not, which outside port lands on which inside one is decided by the
// kernel rather than by the file, and reading the configuration no longer tells you what
// the host does.
func TestValidatePortForwardRangeWidths(t *testing.T) {
	forward := func(listen, target string) string {
		return ifaceWAN + pppoe + `    - kind: PortForward
      metadata: {name: published}
      spec:
        egressRef: pppoe0
        protocol: udp
        ` + listen + `
        target:
          address: 192.168.10.30
` + target
	}

	cases := []struct {
		name   string
		listen string
		target string
		want   []string
	}{
		{
			name:   "a target range narrower than what is listened on",
			listen: "portRange: 60000-60010",
			target: "          portRange: 8080-8081\n",
			want:   []string{"spec.target.portRange: 8080-8081 covers 2 ports, but the forward listens on 60000-60010, which covers 11"},
		},
		{
			name:   "a single target port under a listened-on range",
			listen: "portRange: 60000-60010",
			target: "          port: 8080\n",
			want:   []string{"spec.target.port: 8080 covers 1 port, but the forward listens on 60000-60010, which covers 11"},
		},
		{
			name:   "a target range under a single listened-on port",
			listen: "port: 443",
			target: "          portRange: 8080-8090\n",
			want:   []string{"spec.target.portRange: 8080-8090 covers 11 ports, but the forward listens on 443, which covers 1"},
		},
		{
			name:   "a target range of the same width",
			listen: "portRange: 60000-60010",
			target: "          portRange: 8080-8090\n",
		},
		{
			name:   "no target port at all, which keeps the range it listens on",
			listen: "portRange: 60000-60010",
			target: "",
		},
		{
			name:   "a single port onto a different single port",
			listen: "port: 443",
			target: "          port: 8443\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, problems := check(t, forward(tc.listen, tc.target), secrets())
			if len(tc.want) == 0 {
				if cfg == nil {
					t.Fatalf("rejected a forward whose widths agree:\n%s", problems)
				}
				return
			}
			if cfg != nil {
				t.Fatal("accepted a forward whose widths disagree")
			}
			assertProblems(t, problems, tc.want)
		})
	}
}

func TestValidateDHCPPools(t *testing.T) {
	base := ifaceLAN + `    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
        subnet: 192.168.10.0/24
        pool: {start: 192.168.10.64, end: 192.168.10.127}
        staticMappings:
`
	t.Run("a mapping outside the subnet", func(t *testing.T) {
		_, problems := check(t, base+"          - {name: h, macAddress: '00:00:5e:00:53:01', address: 192.168.11.20}\n", secrets())
		assertProblems(t, problems, []string{"spec.staticMappings[0].address: 192.168.11.20 is not inside 192.168.10.0/24"})
	})

	t.Run("a mapping inside the pool", func(t *testing.T) {
		_, problems := check(t, base+"          - {name: h, macAddress: '00:00:5e:00:53:01', address: 192.168.10.70}\n", secrets())
		assertProblems(t, problems, []string{"spec.staticMappings[0].address: 192.168.10.70 is inside the pool 192.168.10.64-192.168.10.127"})
	})

	t.Run("a pool that ends before it starts", func(t *testing.T) {
		_, problems := check(t, ifaceLAN+`    - kind: DHCPServer
      metadata: {name: lan}
      spec:
        interfaceRef: lan
        subnet: 192.168.10.0/24
        pool: {start: 192.168.10.127, end: 192.168.10.64}
`, secrets())
		assertProblems(t, problems, []string{"spec.pool: 192.168.10.127-192.168.10.64 ends before it starts"})
	})
}

// A derivation that points back at itself would never resolve.
func TestValidateRejectsReferenceCycles(t *testing.T) {
	_, problems := check(t, `    - kind: Interface
      metadata: {name: a}
      spec:
        ifname: eth0
        dhcpv6: {prefixDelegation: {prefixLength: 56, duidFile: /secrets/duid}}
        addresses:
          - fromDelegatedPrefix: {interfaceRef: b, subnetID: 1}
    - kind: Interface
      metadata: {name: b}
      spec:
        ifname: eth1
        dhcpv6: {prefixDelegation: {prefixLength: 56, duidFile: /secrets/duid}}
        addresses:
          - fromDelegatedPrefix: {interfaceRef: a, subnetID: 2}
`, secrets())
	assertProblems(t, problems, []string{"reference cycle"})
}

// One run of the validator reports everything it found, so that a broken file is fixed
// once rather than once per mistake.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	_, problems := check(t, `    - kind: Interface
      metadata: {name: lan}
      spec:
        mtu: 1500
    - kind: PPPoESession
      metadata: {name: pppoe0}
      spec:
        interfaceRef: absent
        userIDFile: /secrets/user-id
        passwordFile: /secrets/nowhere
    - kind: FirewallZone
      metadata: {name: self}
      spec:
        linkRefs: [lan]
`, secrets())
	assertProblems(t, problems, []string{
		"spec.ifname: required",
		`spec.interfaceRef: no Interface named "absent"`,
		"spec.passwordFile: /secrets/nowhere: file is missing",
		`metadata.name: "self" is reserved for the host itself`,
	})
}
