package apply

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/render/nftables"
)

// derivedSets is what every IPAddressSet with an entry cut from the delegated prefix
// should hold: every entry it declares, the derived ones resolved in the delegation each
// client holds now. The ruleset declares these sets empty, and a turn writes them the way
// it writes an uplink's set (ADR 0021, ADR 0015).
//
// A derived entry that cannot be resolved is left out, and the rest of the set is
// written: the subnet it names does not exist at the moment, so a set without it is
// exactly what the declaration means. What it waits for is said, one line per entry, and
// every value that was resolved is said with the delegation it was cut from.
func derivedSets(cfg *config.Config, delegations config.Delegations) (sets []SetElements, resolved, waiting []string) {
	for _, set := range config.ResourcesOf[*v1alpha1.IPAddressSetSpec](cfg) {
		if !set.Spec.HasDerived() {
			continue
		}
		var elements []string
		leaveOut := func(field string, err error) {
			waiting = append(waiting, fmt.Sprintf("IPAddressSet/%s: waiting for %s; %s is left out of the set",
				set.Name, waitingFor(err), field))
		}

		for i, address := range set.Spec.Addresses {
			field := fmt.Sprintf("spec.addresses[%d]", i)
			if !address.IsDerived() {
				elements = append(elements, address.Literal.Addr.String())
				continue
			}
			value, err := delegations.Address(*address.FromDelegatedPrefix)
			if err != nil {
				leaveOut(field, err)
				continue
			}
			elements = append(elements, value.Address.String())
			resolved = append(resolved, fmt.Sprintf("IPAddressSet/%s: %s %s, in %s delegated on Interface/%s",
				set.Name, field, value.Address, value.Delegation, address.FromDelegatedPrefix.InterfaceRef))
		}
		for i, network := range set.Spec.Networks {
			field := fmt.Sprintf("spec.networks[%d]", i)
			if !network.IsDerived() {
				elements = append(elements, network.Literal.Masked().String())
				continue
			}
			subnet, err := delegations.Subnet(*network.FromDelegatedPrefix)
			if err != nil {
				leaveOut(field, err)
				continue
			}
			elements = append(elements, subnet.String())
			ref := network.FromDelegatedPrefix.InterfaceRef
			resolved = append(resolved, fmt.Sprintf("IPAddressSet/%s: %s %s, in %s delegated on Interface/%s",
				set.Name, field, subnet, delegations[ref][0].Masked(), ref))
		}

		// In the order a set read back from the kernel is compared in, and once each: two
		// entries resolving to one value are one element, and nft refuses it twice.
		slices.Sort(elements)
		sets = append(sets, SetElements{Set: nftables.AddressSetName(set.Name), Elements: slices.Compact(elements)})
	}
	slices.SortFunc(sets, func(a, b SetElements) int { return cmp.Compare(a.Set, b.Set) })
	return sets, resolved, waiting
}

// waitingFor is what a derived value waits for. Validation refuses everything that could
// fail for another reason, so anything else is said as it is.
func waitingFor(err error) string {
	var delegation *config.DelegationError
	if errors.As(err, &delegation) {
		return delegation.Waiting
	}
	return err.Error()
}
