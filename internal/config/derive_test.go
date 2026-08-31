package config_test

import (
	"strconv"
	"strings"
	"testing"

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
