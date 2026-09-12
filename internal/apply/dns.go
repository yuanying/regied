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
	// Follows is the uplink whose address the record holds.
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

// records is every record the declaration asks for, with the address of the uplink it
// follows, and what a turn would do with each.
//
// Nothing here contacts the provider. What decides whether to write is what this process
// last wrote — which is in hand — and the address the kernel is holding, which the
// runtime already read for the uplink sets (ADR 0015, ADR 0019).
//
// A rendering has read no host, so it has no address to show. It says which uplink each
// record follows and leaves the address empty, rather than inventing one.
func (e *Engine) records(cfg *config.Config, addresses map[string][]netip.Addr, rendered bool) (changes []DNSChange, waiting []string) {
	for _, set := range config.ResourcesOf[*v1alpha1.DNSRecordSetSpec](cfg) {
		for _, record := range set.Spec.Records {
			uplink := uplinkRef(cfg, record.EgressRef)
			if rendered {
				for _, name := range record.Names {
					changes = append(changes, DNSChange{
						Resource:  "DNSRecordSet/" + set.Name,
						Follows:   uplink,
						Record:    desiredRecord(set.Spec, record, name, ""),
						TokenFile: set.Spec.APITokenFile,
						Reason:    "the address is the uplink's, and nothing was read from this host",
					})
				}
				continue
			}

			address, held := recordAddress(addresses[record.EgressRef], record.TypeOrDefault())
			if !held {
				// While the uplink holds no address — the session is redialling, the
				// line is down — the records are left exactly as they are. An empty
				// record is not a smaller version of the one declared (ADR 0016).
				waiting = append(waiting, fmt.Sprintf(
					"DNSRecordSet/%s: waiting for an address on %s; %s left as they are",
					set.Name, uplink, strings.Join(record.Names, ", ")))
				continue
			}
			for _, name := range record.Names {
				change := DNSChange{
					Resource:  "DNSRecordSet/" + set.Name,
					Follows:   uplink,
					Record:    desiredRecord(set.Spec, record, name, address.String()),
					TokenFile: set.Spec.APITokenFile,
				}
				change.Write, change.Reason = writeReason(e.dnsWriter, change.Record)
				changes = append(changes, change)
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
	return changes, waiting
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

// recordAddress is the address a record of this type should hold, out of what its uplink
// is holding. The runtime has already dropped everything that is not globally reachable.
func recordAddress(held []netip.Addr, recordType v1alpha1.DNSRecordType) (netip.Addr, bool) {
	for _, address := range held {
		if recordType == v1alpha1.DNSRecordA && address.Is4() {
			return address, true
		}
	}
	return netip.Addr{}, false
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
