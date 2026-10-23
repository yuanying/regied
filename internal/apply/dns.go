package apply

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/ddns"
)

// DNSChange is one record this turn is about, and what it would do with it.
//
// It carries no credential: the token is named by the path of the file holding it, and
// the content is read by the step that writes and dropped. A plan can therefore be
// printed without anybody having to remember to be careful (ADR 0003, ADR 0006).
type DNSChange struct {
	// Resource is the DNSRecordSet the record was declared in, for a diagnostic that
	// points at the file.
	Resource string
	// Follows is what the record's address comes from: the uplink whose address an A
	// record holds, or the delegation an AAAA record's address is cut from.
	Follows string
	Record  ddns.Record
	// TokenFile is where the API token is, not what it is.
	TokenFile string

	// Write is whether this turn would write the record, and Reason says why, or why
	// there is nothing to do.
	Write  bool
	Reason string
}

func (c DNSChange) describe() string {
	return fmt.Sprintf("set %s %s to %s in %s", c.Record.Name, c.Record.Type, c.Record.Content, c.Record.Zone)
}

// records is every record the declaration asks for, with the address it should hold,
// and what a turn would do with each; and every AAAA address resolved, with the
// delegation it was cut from, for a dry run to show beside the other derived values.
//
// Nothing here contacts the provider. What decides whether to write is what this process
// last wrote — which is in hand — and the address: the one the kernel holds on the
// uplink, which the runtime already read for the uplink sets, or the one cut from the
// delegation networkd holds (ADR 0015, ADR 0019, ADR 0024).
//
// A rendering has read no host, so it has no uplink address to show. It says what each
// record follows and leaves the address empty, rather than inventing one. An AAAA address
// is shown when the rendering was given the delegation it is cut from.
func (e *Engine) records(cfg *config.Config, addresses map[string][]netip.Addr, delegations config.Delegations, rendered bool) (changes []DNSChange, resolved, waiting []string) {
	for _, set := range config.ResourcesOf[*v1alpha1.DNSRecordSetSpec](cfg) {
		for i, record := range set.Spec.Records {
			resource := "DNSRecordSet/" + set.Name
			change := func(name, follows, content string) DNSChange {
				return DNSChange{
					Resource:  resource,
					Follows:   follows,
					Record:    desiredRecord(set.Spec, record, name, content),
					TokenFile: set.Spec.APITokenFile,
				}
			}

			follows, address, err := recordAddress(cfg, record, addresses, delegations)
			if record.Address != nil && err == nil {
				resolved = append(resolved, fmt.Sprintf("%s: spec.records[%d] %s, in %s delegated on Interface/%s",
					resource, i, address.Address, address.Delegation, record.Address.FromDelegatedPrefix.InterfaceRef))
			}
			if rendered {
				reason := "the address is the uplink's, and nothing was read from this host"
				content := ""
				if record.Address != nil {
					if err != nil {
						reason = "the address is cut from the delegation, and the rendering waits for " + waitingFor(err)
					} else {
						reason = "cut from the delegation that was given; a rendering writes nothing"
						content = address.Address.String()
					}
				}
				for _, name := range record.Names {
					rendering := change(name, follows, content)
					rendering.Reason = reason
					changes = append(changes, rendering)
				}
				continue
			}

			if err != nil {
				// While there is no address to hold — the session is redialling, no
				// delegation is held — the records are left exactly as they are. An
				// empty record is not a smaller version of the one declared, and one
				// pointing at the previous prefix for a minute is better than none
				// (ADR 0016, ADR 0024).
				waiting = append(waiting, fmt.Sprintf("%s: waiting for %s; %s left as they are",
					resource, waitingFor(err), strings.Join(record.Names, ", ")))
				continue
			}
			for _, name := range record.Names {
				write := change(name, follows, address.Address.String())
				write.Write, write.Reason = writeReason(e.dnsWriter, write.Record)
				changes = append(changes, write)
			}
		}
	}
	slices.SortFunc(changes, func(a, b DNSChange) int {
		if by := cmp.Compare(a.Record.Zone, b.Record.Zone); by != 0 {
			return by
		}
		if by := cmp.Compare(a.Record.Name, b.Record.Name); by != 0 {
			return by
		}
		return cmp.Compare(a.Record.Type, b.Record.Type)
	})
	return changes, resolved, waiting
}

// writeReason says whether a record has to be written, and why.
//
// A writer that has not written this record knows nothing about it, and "not known" is
// never "the same": that is what makes the first turn after a start write every record
// once, and it is the whole of how the level-triggered property is recovered without
// reading the provider every turn (ADR 0004, ADR 0019).
func writeReason(writer *ddns.Writer, record ddns.Record) (bool, string) {
	last, known := writer.Last(record)
	switch {
	case !known:
		return true, "nothing has been written here since this process started, so what it holds is not known"
	case last.Content != record.Content:
		return true, "the address moved from " + last.Content
	case last != record:
		return true, "a declared property changed"
	}
	return false, "this process wrote it, and nothing has moved since"
}

func desiredRecord(set *v1alpha1.DNSRecordSetSpec, record v1alpha1.DNSRecord, name, content string) ddns.Record {
	return ddns.Record{
		Zone:    set.Zone,
		Name:    name,
		Type:    string(record.TypeOrDefault()),
		Content: content,
		Proxied: record.ProxiedEnabled(),
		TTL:     record.TTL.For,
	}
}

// recordAddress is what a record follows, as a diagnostic names it, and the address it
// should hold now. An error is what the record waits for.
//
// An A record holds the IPv4 address its uplink is holding; the runtime has already
// dropped everything that is not globally reachable. An AAAA record holds the address cut
// from the delegation its interface holds (ADR 0024).
func recordAddress(cfg *config.Config, record v1alpha1.DNSRecord, addresses map[string][]netip.Addr, delegations config.Delegations) (string, config.Resolved, error) {
	if record.Address != nil {
		derived := *record.Address.FromDelegatedPrefix
		address, err := delegations.Address(derived)
		return "the delegation on Interface/" + derived.InterfaceRef, address, err
	}
	uplink := uplinkRef(cfg, record.EgressRef)
	for _, address := range addresses[record.EgressRef] {
		if address.Is4() {
			return uplink, config.Resolved{Address: address}, nil
		}
	}
	return uplink, config.Resolved{}, fmt.Errorf("an address on %s", uplink)
}

// uplinkRef is a record's egressRef as a resource reference, for a diagnostic. A
// declaration that got here has been validated, so the reference resolves.
func uplinkRef(cfg *config.Config, name string) string {
	for _, kind := range []v1alpha1.ResourceKind{v1alpha1.KindPPPoESession, v1alpha1.KindDSLiteTunnel} {
		if resource := cfg.Lookup(kind, name); resource != nil {
			return resource.Ref()
		}
	}
	return name
}

// writeRecord is what a DNS step does: read the token, write the record, drop the token.
//
// The token is read here and nowhere else, so a host whose records all hold what they
// should reads no token at all, and a dry run — which runs no step — never reads one.
// Nothing this function returns carries the content it read (ADR 0003).
func (e *Engine) writeRecord(ctx context.Context, change DNSChange) error {
	data, _, err := e.host.Files.ReadFile(change.TokenFile)
	if err != nil {
		return fmt.Errorf("the API token %s cannot be read: %w", change.TokenFile, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return fmt.Errorf("the API token %s is empty", change.TokenFile)
	}
	return e.dnsWriter.Write(ctx, token, change.Record)
}
