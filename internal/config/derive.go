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
// them.
//
// The allocation is a function of the policies alone, not of where they appear in the
// file. A table number that moved when a resource was reordered would change the routing
// on an apply that changed nothing.
func (v *validator) deriveRouting() map[string]PolicyRouting {
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
	return routing
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
