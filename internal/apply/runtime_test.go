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
	if got := rt.UplinkAddresses["pppoe0"]; !equalAddrs(got, want) {
		t.Errorf("the PPPoE uplink holds %v, want %v", got, want)
	}
	// An Interface is never an egressRef, so nothing in the ruleset can depend on its
	// address, and it is not asked (ADR 0013).
	if got, ok := rt.UplinkAddresses["wan"]; ok {
		t.Errorf("the WAN interface was asked for its address and holds %v; only uplinks are asked", got)
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
	if len(rt.UplinkAddresses) != 0 {
		t.Errorf("expected no uplink addresses, got %v", rt.UplinkAddresses)
	}
	if len(rt.Notes) == 0 {
		t.Error("expected a note saying which links could not be read, got none")
	}
}

// An AFTR name that resolves only to IPv4 is no address to build the tunnel on: the tunnel
// is what would carry IPv4. It is not a failure either. The tunnel is left out for this
// turn, the host is told what it could not answer, and a later turn asks again
// (ADR 0016).
func TestCollectRuntimeNotesAnAFTRThatIsNotReachableOverIPv6(t *testing.T) {
	cfg := load(t, uplinkFixture)
	host, files, _ := testHost()
	putSecrets(files)
	host.Resolver = fakeResolver{"aftr.example.net": addrs(t, "192.0.2.53")}

	rt, err := CollectRuntime(context.Background(), cfg, host)
	if err != nil {
		t.Fatalf("an AFTR with no IPv6 address is not a failure, but: %v", err)
	}
	if _, ok := rt.Networkd.AFTRAddresses["dslite"]; ok {
		t.Error("an IPv4 answer was taken as the AFTR's address")
	}
	notes := strings.Join(rt.Notes, "\n")
	if !strings.Contains(notes, "aftr.example.net") || !strings.Contains(notes, "IPv6") {
		t.Errorf("the notes do not say what the host could not answer and why: %v", rt.Notes)
	}
}

// A name that does not resolve is the ordinary state of a host that has just booted, before
// its resolver is reachable. The tunnel waits for it; nothing fails (ADR 0016).
func TestCollectRuntimeNotesAnAFTRThatDoesNotResolve(t *testing.T) {
	cfg := load(t, uplinkFixture)
	host, files, _ := testHost()
	putSecrets(files)

	rt, err := CollectRuntime(context.Background(), cfg, host)
	if err != nil {
		t.Fatalf("a name that does not resolve is not a failure, but: %v", err)
	}
	if _, ok := rt.Networkd.AFTRAddresses["dslite"]; ok {
		t.Error("an address was invented for a name that did not resolve")
	}
	if !strings.Contains(strings.Join(rt.Notes, "\n"), "aftr.example.net") {
		t.Errorf("the notes do not name the AFTR that could not be resolved: %v", rt.Notes)
	}
}

// A credential that cannot be read still stops the turn: bringing a line up without
// authentication is not a degraded success, and there is no smaller version of a
// credentials file to write (ADR 0003, ADR 0016).
func TestCollectRuntimeRefusesAnUnreadableCredential(t *testing.T) {
	for _, missing := range []string{
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

// The DUID is different. The file that names it is left out whole and waited for, so that
// networkd never sends an identifier of its own in its place; the host says which file it
// could not read (ADR 0016).
func TestCollectRuntimeNotesAnUnreadableDUID(t *testing.T) {
	cfg := load(t, uplinkFixture)
	host, files, _ := testHost()
	putSecrets(files)
	delete(files.files, "/etc/regied/secrets/dhcpv6-duid")
	host.Resolver = fakeResolver{"aftr.example.net": addrs(t, "2001:db8:53::1")}

	rt, err := CollectRuntime(context.Background(), cfg, host)
	if err != nil {
		t.Fatalf("a DUID that cannot be read is not a failure, but: %v", err)
	}
	if _, ok := rt.Networkd.DUIDs["/etc/regied/secrets/dhcpv6-duid"]; ok {
		t.Error("a DUID was invented for a file that could not be read")
	}
	if !strings.Contains(strings.Join(rt.Notes, "\n"), "/etc/regied/secrets/dhcpv6-duid") {
		t.Errorf("the notes do not name the DUID file that could not be read: %v", rt.Notes)
	}
}

func TestCollectRuntimeReportsEveryProblemAtOnce(t *testing.T) {
	cfg := load(t, uplinkFixture)
	host, _, _ := testHost()
	// Nothing exists: no secrets, no resolver entry. An operator setting a host up for
	// the first time should be told all of it, not one thing per run.

	_, err := CollectRuntime(context.Background(), cfg, host)
	requireErrorContaining(t, err, "/etc/regied/secrets/pppoe-user-id")
	requireErrorContaining(t, err, "/etc/regied/secrets/pppoe-password")
	// The DUID and the AFTR are waited for, not failed over, so they are not in the
	// failure; the turn does not get as far as noting them.
	if strings.Contains(err.Error(), "aftr.example.net") {
		t.Errorf("a name that is waited for is reported as a failure:\n%v", err)
	}
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
