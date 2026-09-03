package apply

import (
	"context"
	"io/fs"
	"net/netip"
	"strings"
	"testing"
)

// uplinkFixture is a host with both uplinks: a PPPoE session over a NIC, and a DS-Lite
// tunnel whose AFTR is named rather than addressed.
const uplinkFixture = `  resources:
    - kind: Interface
      metadata: {name: wan}
      spec:
        ifname: eth0
        dhcpv6:
          prefixDelegation:
            prefixLength: 56
            duidFile: /etc/regied/secrets/dhcpv6-duid
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses: [192.168.10.1/24]
    - kind: PPPoESession
      metadata: {name: pppoe0}
      spec:
        interfaceRef: wan
        userIDFile: /etc/regied/secrets/pppoe-user-id
        passwordFile: /etc/regied/secrets/pppoe-password
    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        localAddressFrom: {interfaceRef: lan}
        aftrHost: aftr.example.net
`

// putSecrets fills in the three files the fixture names.
func putSecrets(files *fakeFiles) {
	files.put("/etc/regied/secrets/dhcpv6-duid", "00:03:00:01:00:00:5e:00:53:01\n", 0o644)
	files.put("/etc/regied/secrets/pppoe-user-id", "account@example.net\n", 0o600)
	files.put("/etc/regied/secrets/pppoe-password", "hunter2\n", 0o600)
}

func TestCollectRuntimeReadsWhatOnlyTheHostKnows(t *testing.T) {
	cfg := load(t, uplinkFixture)
	host, files, _ := testHost()
	putSecrets(files)
	host.Resolver = fakeResolver{"aftr.example.net": addrs(t, "2001:db8:53::1")}
	host.Links = fakeLinks{
		"pppoe0": addrs(t, "192.0.2.10", "fe80::1"),
		"dslite": addrs(t, "192.0.2.20"),
		"eth0":   addrs(t, "2001:db8:1::1"),
		"br-lan": addrs(t, "192.168.10.1"),
	}

	rt, err := CollectRuntime(context.Background(), cfg, host)
	if err != nil {
		t.Fatalf("collecting the runtime values failed: %v", err)
	}

	if got, want := rt.Networkd.AFTRAddresses["dslite"], netip.MustParseAddr("2001:db8:53::1"); got != want {
		t.Errorf("the AFTR address is %v, want %v", got, want)
	}
	if got, want := rt.Networkd.DUIDs["/etc/regied/secrets/dhcpv6-duid"], "00:03:00:01:00:00:5e:00:53:01\n"; got != want {
		t.Errorf("the DUID is %q, want %q", got, want)
	}
	if got, want := rt.Credentials["pppoe0"].UserID, "account@example.net\n"; got != want {
		t.Errorf("the user ID is %q, want %q", got, want)
	}
	if got, want := rt.Credentials["pppoe0"].Password, "hunter2\n"; got != want {
		t.Errorf("the password is %q, want %q", got, want)
	}

	// A link-local address is not an address an uplink is reachable at, so it is not
	// one a hairpin rule could ever match on.
	want := addrs(t, "192.0.2.10")
	if got := rt.NFTables.UplinkAddresses["pppoe0"]; !equalAddrs(got, want) {
		t.Errorf("the PPPoE uplink holds %v, want %v", got, want)
	}
	if got, want := rt.NFTables.UplinkAddresses["wan"], addrs(t, "2001:db8:1::1"); !equalAddrs(got, want) {
		t.Errorf("the WAN interface holds %v, want %v", got, want)
	}
}

func TestCollectRuntimeAcceptsAnUplinkThatIsNotUp(t *testing.T) {
	cfg := load(t, uplinkFixture)
	host, files, _ := testHost()
	putSecrets(files)
	host.Resolver = fakeResolver{"aftr.example.net": addrs(t, "2001:db8:53::1")}
	// Nothing is up: the fake knows no links at all.

	rt, err := CollectRuntime(context.Background(), cfg, host)
	if err != nil {
		t.Fatalf("a line that is not up is not an error, but: %v", err)
	}
	if len(rt.NFTables.UplinkAddresses) != 0 {
		t.Errorf("expected no uplink addresses, got %v", rt.NFTables.UplinkAddresses)
	}
	if len(rt.Notes) == 0 {
		t.Error("expected a note saying which links could not be read, got none")
	}
}

func TestCollectRuntimeRefusesAnAFTRThatIsNotReachableOverIPv6(t *testing.T) {
	cfg := load(t, uplinkFixture)
	host, files, _ := testHost()
	putSecrets(files)
	// The name resolves, but only to IPv4. The tunnel being configured is what would
	// carry that IPv4, so there is no address here to build it on.
	host.Resolver = fakeResolver{"aftr.example.net": addrs(t, "192.0.2.53")}

	_, err := CollectRuntime(context.Background(), cfg, host)
	requireErrorContaining(t, err, "aftr.example.net")
	requireErrorContaining(t, err, "IPv6")
}

func TestCollectRuntimeRefusesAnAFTRThatDoesNotResolve(t *testing.T) {
	cfg := load(t, uplinkFixture)
	host, files, _ := testHost()
	putSecrets(files)

	_, err := CollectRuntime(context.Background(), cfg, host)
	requireErrorContaining(t, err, "aftr.example.net")
}

func TestCollectRuntimeRefusesAnUnreadableSecret(t *testing.T) {
	for _, missing := range []string{
		"/etc/regied/secrets/dhcpv6-duid",
		"/etc/regied/secrets/pppoe-user-id",
		"/etc/regied/secrets/pppoe-password",
	} {
		t.Run(missing, func(t *testing.T) {
			cfg := load(t, uplinkFixture)
			host, files, _ := testHost()
			putSecrets(files)
			delete(files.files, missing)
			host.Resolver = fakeResolver{"aftr.example.net": addrs(t, "2001:db8:53::1")}

			_, err := CollectRuntime(context.Background(), cfg, host)
			requireErrorContaining(t, err, missing)
		})
	}
}

func TestCollectRuntimeReportsEveryProblemAtOnce(t *testing.T) {
	cfg := load(t, uplinkFixture)
	host, _, _ := testHost()
	// Nothing exists: no secrets, no resolver entry. An operator setting a host up for
	// the first time should be told all of it, not one thing per run.

	_, err := CollectRuntime(context.Background(), cfg, host)
	requireErrorContaining(t, err, "/etc/regied/secrets/dhcpv6-duid")
	requireErrorContaining(t, err, "/etc/regied/secrets/pppoe-password")
	requireErrorContaining(t, err, "aftr.example.net")
}

func TestCollectRuntimeDoesNotResolveATunnelWrittenWithAnAddress(t *testing.T) {
	cfg := load(t, `  resources:
    - kind: Interface
      metadata: {name: wan}
      spec: {ifname: eth0}
    - kind: Interface
      metadata: {name: lan}
      spec:
        ifname: br-lan
        addresses: [192.168.10.1/24]
    - kind: DSLiteTunnel
      metadata: {name: dslite}
      spec:
        underlayRef: wan
        localAddressFrom: {interfaceRef: lan}
        aftrAddress: 2001:db8:53::1
`)
	host, _, _ := testHost()
	// The resolver knows nothing. A tunnel that was written with an address must not
	// ask it anything.

	rt, err := CollectRuntime(context.Background(), cfg, host)
	if err != nil {
		t.Fatalf("collecting the runtime values failed: %v", err)
	}
	if len(rt.Networkd.AFTRAddresses) != 0 {
		t.Errorf("expected no resolved AFTR addresses, got %v", rt.Networkd.AFTRAddresses)
	}
}

func TestCollectRuntimeKeepsCredentialsOutOfEverythingItPrints(t *testing.T) {
	cfg := load(t, uplinkFixture)
	host, files, _ := testHost()
	putSecrets(files)
	host.Resolver = fakeResolver{"aftr.example.net": addrs(t, "2001:db8:53::1")}
	files.readErr["/etc/regied/secrets/pppoe-password"] = fs.ErrPermission

	_, err := CollectRuntime(context.Background(), cfg, host)
	requireErrorContaining(t, err, "/etc/regied/secrets/pppoe-password")
	if got := err.Error(); strings.Contains(got, "hunter2") {
		t.Errorf("the error carries the credential:\n%v", got)
	}
}

func equalAddrs(got, want []netip.Addr) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
