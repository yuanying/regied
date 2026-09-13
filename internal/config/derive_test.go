package config_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// Two policies, and the uplinks they need.
const derivationBase = ifaceWAN + ifaceLAN + pppoe + dslite

const policyPPPoE = `    - kind: EgressRoutePolicy
      metadata: {name: upper-half-via-pppoe}
      spec:
        family: ipv4
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.168.10.128-192.168.10.255]
`

const policyDSLite = `    - kind: EgressRoutePolicy
      metadata: {name: rest-via-dslite}
      spec:
        family: ipv4
        priority: 20
        egressRef: dslite
        sourceRanges: [192.168.10.0/24]
`

func routing(t *testing.T, cfg *config.Config, name string) config.PolicyRouting {
	t.Helper()
	got, ok := cfg.PolicyRouting(name)
	if !ok {
		t.Fatalf("no routing derived for %q", name)
	}
	return got
}

// The operator does not write table numbers or firewall marks. regied allocates them,
// and the allocation has to be readable back out of the model the renderers use.
func TestDeriveAllocatesTablesAndMarks(t *testing.T) {
	cfg, problems := check(t, derivationBase+policyPPPoE+policyDSLite, secrets())
	if cfg == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}

	first := routing(t, cfg, "upper-half-via-pppoe")
	second := routing(t, cfg, "rest-via-dslite")

	if first.Table == 0 || second.Table == 0 {
		t.Fatalf("no table allocated: %+v %+v", first, second)
	}
	if first.Table == second.Table {
		t.Errorf("two policies share table %d", first.Table)
	}
	if first.Mark == second.Mark {
		t.Errorf("two policies share mark %d", first.Mark)
	}
	if first.TablePinned || first.MarkPinned {
		t.Errorf("nothing was pinned, but %+v says otherwise", first)
	}
	// The lower priority is allocated first, so the numbers read in the order the
	// policies are evaluated in.
	if first.Table > second.Table {
		t.Errorf("priority 10 got table %d, priority 20 got %d", first.Table, second.Table)
	}
}

// The same configuration has to produce the same numbers. A table number that moves when
// a resource is reordered would change the routing on a re-apply that changed nothing.
func TestDeriveIsStableAcrossResourceOrder(t *testing.T) {
	forward, problems := check(t, derivationBase+policyPPPoE+policyDSLite, secrets())
	if forward == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}
	reversed, problems := check(t, policyDSLite+policyPPPoE+dslite+pppoe+ifaceLAN+ifaceWAN, secrets())
	if reversed == nil {
		t.Fatalf("rejected the same document written in another order:\n%s", problems)
	}

	for _, name := range []string{"upper-half-via-pppoe", "rest-via-dslite"} {
		a, b := routing(t, forward, name), routing(t, reversed, name)
		if a != b {
			t.Errorf("%s: %+v in one order, %+v in the other", name, a, b)
		}
	}
}

// A host that shares its routing tables with something else pins them.
func TestDeriveRespectsPinnedValues(t *testing.T) {
	cfg, problems := check(t, derivationBase+`    - kind: EgressRoutePolicy
      metadata: {name: pinned}
      spec:
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.168.10.128-192.168.10.255]
        table: 42
        mark: 4660
`+policyDSLite, secrets())
	if cfg == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}

	pinned := routing(t, cfg, "pinned")
	if pinned.Table != 42 || !pinned.TablePinned {
		t.Errorf("pinned table: %+v", pinned)
	}
	if pinned.Mark != 4660 || !pinned.MarkPinned {
		t.Errorf("pinned mark: %+v", pinned)
	}

	derived := routing(t, cfg, "rest-via-dslite")
	if derived.Table == 42 || derived.Mark == 4660 {
		t.Errorf("allocation collided with a pinned value: %+v", derived)
	}
}

// A pinned value that happens to be the one the allocator would have chosen must not be
// handed out twice.
func TestDeriveSkipsAPinnedValueItWouldHaveChosen(t *testing.T) {
	first, problems := check(t, derivationBase+policyPPPoE+policyDSLite, secrets())
	if first == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}
	taken := routing(t, first, "upper-half-via-pppoe")

	cfg, problems := check(t, derivationBase+policyPPPoE+strings.Replace(policyDSLite,
		"        sourceRanges: [192.168.10.0/24]\n",
		"        sourceRanges: [192.168.10.0/24]\n        table: "+strconv.Itoa(taken.Table)+"\n        mark: "+strconv.Itoa(int(taken.Mark))+"\n", 1), secrets())
	if cfg == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}

	pinned := routing(t, cfg, "rest-via-dslite")
	unpinned := routing(t, cfg, "upper-half-via-pppoe")
	if pinned.Table != taken.Table || pinned.Mark != taken.Mark {
		t.Fatalf("the pin was not honoured: %+v", pinned)
	}
	if unpinned.Table == pinned.Table {
		t.Errorf("table %d handed out twice", pinned.Table)
	}
	if unpinned.Mark == pinned.Mark {
		t.Errorf("mark %d handed out twice", pinned.Mark)
	}
}

func TestDeriveRejectsCollidingPins(t *testing.T) {
	_, problems := check(t, derivationBase+`    - kind: EgressRoutePolicy
      metadata: {name: first}
      spec:
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.168.10.128-192.168.10.255]
        table: 42
        mark: 4660
    - kind: EgressRoutePolicy
      metadata: {name: second}
      spec:
        priority: 20
        egressRef: dslite
        sourceRanges: [192.168.10.0/24]
        table: 42
        mark: 4660
`, secrets())
	assertProblems(t, problems, []string{
		`spec.table: the EgressRoutePolicy "first" already uses table 42`,
		`spec.mark: the EgressRoutePolicy "first" already uses mark 4660`,
	})
}

// The tables the kernel reserves are not regied's to hand out.
func TestDeriveRejectsAReservedTable(t *testing.T) {
	_, problems := check(t, derivationBase+`    - kind: EgressRoutePolicy
      metadata: {name: pinned}
      spec:
        priority: 10
        egressRef: pppoe0
        sourceRanges: [192.168.10.0/24]
        table: 254
`, secrets())
	assertProblems(t, problems, []string{"spec.table: 254 is reserved by the kernel"})
}

// A forward published on an uplink, and one on the other family of the same uplink.
const forwardV4 = `    - kind: PortForward
      metadata: {name: https}
      spec:
        egressRef: pppoe0
        protocol: tcp
        port: 443
        target: {address: 192.168.10.20}
`

const forwardV6 = `    - kind: PortForward
      metadata: {name: https-v6}
      spec:
        egressRef: pppoe0
        protocol: tcp
        port: 443
        target: {address: "2001:db8:0:1::20"}
`

func forwardReturn(t *testing.T, cfg *config.Config, uplink string, family v1alpha1.Family) config.ForwardReturn {
	t.Helper()
	got, ok := cfg.ForwardReturn(uplink, family)
	if !ok {
		t.Fatalf("no return routing derived for %s %s", uplink, family)
	}
	return got
}

// An uplink a port forward is published on gets a table and a mark of its own, per
// family, so that the reply to a connection that arrived there can be sent back the same
// way. They come after the policies' numbers, so that adding a forward moves no policy.
func TestDeriveAllocatesAReturnTableAndMarkPerUplinkAndFamily(t *testing.T) {
	cfg, problems := check(t, derivationBase+policyPPPoE+policyDSLite+forwardV6+forwardV4, secrets())
	if cfg == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}

	last := routing(t, cfg, "rest-via-dslite")
	v4 := forwardReturn(t, cfg, "pppoe0", v1alpha1.FamilyIPv4)
	v6 := forwardReturn(t, cfg, "pppoe0", v1alpha1.FamilyIPv6)

	if v4.Table <= last.Table || v4.Mark <= last.Mark {
		t.Errorf("the uplink's numbers %+v were not allocated after the last policy's %+v", v4, last)
	}
	if v6.Table <= v4.Table || v6.Mark <= v4.Mark {
		t.Errorf("IPv6 %+v was not allocated after IPv4 %+v", v6, v4)
	}
	if v4.Uplink != "pppoe0" || v4.Family != v1alpha1.FamilyIPv4 {
		t.Errorf("the entry does not say what it is for: %+v", v4)
	}

	all := cfg.ForwardReturns()
	if len(all) != 2 || all[0] != v4 || all[1] != v6 {
		t.Errorf("ForwardReturns is %+v, want the IPv4 entry then the IPv6 one", all)
	}
}

// Without a forward there is no reply to put back, and an uplink gets nothing.
func TestDeriveGivesNoReturnRoutingToAnUplinkWithoutAForward(t *testing.T) {
	cfg, problems := check(t, derivationBase+policyPPPoE+policyDSLite+forwardV4, secrets())
	if cfg == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}
	if _, ok := cfg.ForwardReturn("dslite", v1alpha1.FamilyIPv4); ok {
		t.Error("the tunnel got return routing with no forward published on it")
	}
	if _, ok := cfg.ForwardReturn("pppoe0", v1alpha1.FamilyIPv6); ok {
		t.Error("pppoe0 got IPv6 return routing with only an IPv4 forward")
	}
	if all := cfg.ForwardReturns(); len(all) != 1 {
		t.Errorf("ForwardReturns is %+v, want one entry", all)
	}
}

// The return routing does not depend on a policy being there. A host with one uplink and
// a forward gets it too, and it is harmless there.
func TestDeriveReturnRoutingWithoutAnyPolicy(t *testing.T) {
	cfg, problems := check(t, derivationBase+forwardV4, secrets())
	if cfg == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}
	got := forwardReturn(t, cfg, "pppoe0", v1alpha1.FamilyIPv4)
	if got.Table == 0 || got.Mark == 0 {
		t.Errorf("nothing allocated: %+v", got)
	}
}

// Two forwards on the same uplink and family share the one entry: the mark says which
// uplink the connection arrived on, not which forward readdressed it.
func TestDeriveReturnRoutingIsPerUplinkNotPerForward(t *testing.T) {
	cfg, problems := check(t, derivationBase+forwardV4+`    - kind: PortForward
      metadata: {name: ssh}
      spec:
        egressRef: pppoe0
        protocol: tcp
        port: 10022
        target: {address: 192.168.10.30, port: 22}
`, secrets())
	if cfg == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}
	if all := cfg.ForwardReturns(); len(all) != 1 {
		t.Errorf("ForwardReturns is %+v, want one entry for the one uplink", all)
	}
}

// A pinned policy value the allocator would otherwise have reached is skipped for the
// uplink as it is for a policy.
func TestDeriveReturnRoutingAvoidsPinnedValues(t *testing.T) {
	first, problems := check(t, derivationBase+policyPPPoE+policyDSLite+forwardV4, secrets())
	if first == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}
	taken := forwardReturn(t, first, "pppoe0", v1alpha1.FamilyIPv4)

	cfg, problems := check(t, derivationBase+policyPPPoE+strings.Replace(policyDSLite,
		"        sourceRanges: [192.168.10.0/24]\n",
		"        sourceRanges: [192.168.10.0/24]\n        table: "+strconv.Itoa(taken.Table)+"\n        mark: "+strconv.Itoa(int(taken.Mark))+"\n", 1)+forwardV4, secrets())
	if cfg == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}
	pinned := routing(t, cfg, "rest-via-dslite")
	got := forwardReturn(t, cfg, "pppoe0", v1alpha1.FamilyIPv4)
	if got.Table == pinned.Table {
		t.Errorf("table %d handed out twice", got.Table)
	}
	if got.Mark == pinned.Mark {
		t.Errorf("mark %d handed out twice", got.Mark)
	}
}

// The same document in another order derives the same numbers for the uplink, as it does
// for the policies.
func TestDeriveReturnRoutingIsStableAcrossResourceOrder(t *testing.T) {
	forward, problems := check(t, derivationBase+policyPPPoE+policyDSLite+forwardV4+forwardV6, secrets())
	if forward == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}
	reversed, problems := check(t, forwardV6+forwardV4+policyDSLite+policyPPPoE+dslite+pppoe+ifaceLAN+ifaceWAN, secrets())
	if reversed == nil {
		t.Fatalf("rejected the same document written in another order:\n%s", problems)
	}
	for _, family := range []v1alpha1.Family{v1alpha1.FamilyIPv4, v1alpha1.FamilyIPv6} {
		a, b := forwardReturn(t, forward, "pppoe0", family), forwardReturn(t, reversed, "pppoe0", family)
		if a != b {
			t.Errorf("%s: %+v in one order, %+v in the other", family, a, b)
		}
	}
}

// A target outside every policy's range used to be warned about. The reply now leaves by
// the uplink it arrived on whatever the policies say, so there is nothing to warn about.
func TestValidateDoesNotWarnAboutAForwardTargetOutsideThePolicyRanges(t *testing.T) {
	cfg, problems := check(t, derivationBase+policyPPPoE+policyDSLite+forwardV4, secrets())
	if cfg == nil {
		t.Fatalf("rejected a valid document:\n%s", problems)
	}
	assertProblems(t, problems, nil)
}
