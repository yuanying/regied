package config

import (
	"cmp"
	"slices"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
)

// PolicyRouting is what an EgressRoutePolicy becomes below the schema: a routing table to
// put the uplink's default route in, and a firewall mark that selects it.
//
// The operator does not write either. Nothing outside regied should depend on the values;
// they are an implementation detail of how a match becomes a route, and `regied render`
// is where to read them.
type PolicyRouting struct {
	Table       int
	Mark        uint32
	TablePinned bool
	MarkPinned  bool
}

// ForwardReturn is what an uplink a PortForward is published on gets below the schema:
// a routing table holding the uplink's default route, and a firewall mark that selects
// it. The mark is saved on every connection a forward readdresses and restored on the
// reply, which is what puts the reply back on the uplink it arrived on whatever an
// EgressRoutePolicy would say about the answering host's address (ADR 0020).
//
// There is one per uplink and family, not one per forward: the mark says where the
// connection arrived, and every forward on that uplink says the same thing. Neither
// value can be pinned; nothing outside regied should depend on them.
type ForwardReturn struct {
	Uplink string
	Family v1alpha1.Family
	Table  int
	Mark   uint32
}

// forwardReturnKey is what one ForwardReturn is looked up by.
type forwardReturnKey struct {
	uplink string
	family v1alpha1.Family
}

// derived is everything deriveRouting hands out: the policies' numbers by policy name,
// and the uplinks' return routing in the order it was allocated in.
type derived struct {
	policies map[string]PolicyRouting
	returns  []ForwardReturn
}

// Where allocation starts. Both ranges are regied's own: low table numbers are where
// other tools that write routing tables by hand tend to sit, and a mark well away from
// the table numbers keeps the two apart in `nft list ruleset` output.
const (
	firstDerivedTable = 100
	firstDerivedMark  = 0x100
)

// The tables the kernel keeps for itself. Handing one out would replace the host's own
// routing.
var reservedTables = map[int]string{
	0:   "unspecified",
	253: "default",
	254: "main",
	255: "local",
}

// deriveRouting allocates a table and a mark for every EgressRoutePolicy that did not pin
// them, and then for every uplink and family a PortForward is published on.
//
// The allocation is a function of the policies and forwards alone, not of where they
// appear in the file. A table number that moved when a resource was reordered would
// change the routing on an apply that changed nothing.
//
// The uplinks come after the policies. Every pinned value is reserved before anything is
// allocated, so an uplink's numbers cannot collide with a pin either.
func (v *validator) deriveRouting() derived {
	policies := v.byKind[v1alpha1.KindEgressRoutePolicy]
	routing := make(map[string]PolicyRouting, len(policies))

	tables := newAllocator(firstDerivedTable)
	marks := newAllocator(firstDerivedMark)
	tableOwner := make(map[int]string)
	markOwner := make(map[uint32]string)

	var unpinned []*v1alpha1.Resource
	for _, resource := range policies {
		spec, ok := resource.Spec.(*v1alpha1.EgressRoutePolicySpec)
		if !ok {
			continue
		}
		name := resource.Metadata.Name
		var derived PolicyRouting

		if spec.Table != nil {
			table := *spec.Table
			if reason, reserved := reservedTables[table]; reserved {
				v.errorf(resource, "spec.table", "%d is reserved by the kernel for the %s table", table, reason)
			} else if owner, taken := tableOwner[table]; taken {
				v.errorf(resource, "spec.table", "the EgressRoutePolicy %q already uses table %d", owner, table)
			} else {
				tableOwner[table] = name
				tables.reserve(table)
				derived.Table, derived.TablePinned = table, true
			}
		}
		if spec.Mark != nil {
			mark := *spec.Mark
			if mark == 0 {
				v.errorf(resource, "spec.mark", "0 is not a mark: it is what an unmarked packet carries")
			} else if owner, taken := markOwner[mark]; taken {
				v.errorf(resource, "spec.mark", "the EgressRoutePolicy %q already uses mark %d", owner, mark)
			} else {
				markOwner[mark] = name
				marks.reserve(int(mark))
				derived.Mark, derived.MarkPinned = mark, true
			}
		}

		routing[name] = derived
		if !derived.TablePinned || !derived.MarkPinned {
			unpinned = append(unpinned, resource)
		}
	}

	// Allocate in the order the policies are evaluated in, so that the numbers read the
	// way the file does, and so that the result does not depend on the file's order.
	slices.SortStableFunc(unpinned, func(a, b *v1alpha1.Resource) int {
		specA := a.Spec.(*v1alpha1.EgressRoutePolicySpec)
		specB := b.Spec.(*v1alpha1.EgressRoutePolicySpec)
		if order := cmp.Compare(specA.FamilyOrDefault(), specB.FamilyOrDefault()); order != 0 {
			return order
		}
		if order := cmp.Compare(priorityOf(specA), priorityOf(specB)); order != 0 {
			return order
		}
		return cmp.Compare(a.Metadata.Name, b.Metadata.Name)
	})

	for _, resource := range unpinned {
		derived := routing[resource.Metadata.Name]
		if !derived.TablePinned {
			derived.Table = tables.next(func(n int) bool {
				_, reserved := reservedTables[n]
				return !reserved
			})
		}
		if !derived.MarkPinned {
			derived.Mark = uint32(marks.next(func(int) bool { return true }))
		}
		routing[resource.Metadata.Name] = derived
	}

	return derived{policies: routing, returns: v.deriveForwardReturns(tables, marks)}
}

// deriveForwardReturns allocates the return routing of every uplink and family at least
// one PortForward is published on, by family and then by the uplink's name.
//
// The family is the target's: a forward readdresses within one family, and the reply it
// puts back is of that family. Whether the uplink may be published on at all is
// validation's question, asked elsewhere; a forward that failed it never reaches a
// Config, so nothing here has to know.
func (v *validator) deriveForwardReturns(tables, marks *allocator) []ForwardReturn {
	seen := make(map[forwardReturnKey]bool)
	var keys []forwardReturnKey
	for _, resource := range v.byKind[v1alpha1.KindPortForward] {
		spec, ok := resource.Spec.(*v1alpha1.PortForwardSpec)
		if !ok || spec.EgressRef == "" || spec.Target == nil || !spec.Target.Address.IsValid() {
			continue
		}
		key := forwardReturnKey{uplink: spec.EgressRef, family: familyOf(spec.Target.Address.Addr)}
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b forwardReturnKey) int {
		if order := cmp.Compare(a.family, b.family); order != 0 {
			return order
		}
		return cmp.Compare(a.uplink, b.uplink)
	})

	returns := make([]ForwardReturn, 0, len(keys))
	for _, key := range keys {
		returns = append(returns, ForwardReturn{
			Uplink: key.uplink,
			Family: key.family,
			Table: tables.next(func(n int) bool {
				_, reserved := reservedTables[n]
				return !reserved
			}),
			Mark: uint32(marks.next(func(int) bool { return true })),
		})
	}
	return returns
}

// priorityOf sorts a policy whose priority is missing last. It has already been reported
// as an error; ordering it consistently keeps the rest of the allocation readable.
func priorityOf(spec *v1alpha1.EgressRoutePolicySpec) int {
	if spec.Priority == nil {
		return 1 << 30
	}
	return *spec.Priority
}

// allocator hands out ascending numbers, skipping the ones already spoken for.
type allocator struct {
	next_ int
	taken map[int]bool
}

func newAllocator(first int) *allocator {
	return &allocator{next_: first, taken: make(map[int]bool)}
}

func (a *allocator) reserve(n int) { a.taken[n] = true }

func (a *allocator) next(usable func(int) bool) int {
	for a.taken[a.next_] || !usable(a.next_) {
		a.next_++
	}
	n := a.next_
	a.taken[n] = true
	a.next_++
	return n
}
