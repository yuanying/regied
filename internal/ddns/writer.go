package ddns

import (
	"context"
	"fmt"
	"time"
)

// Record is one record as a declaration asks for it, with the address filled in from the
// uplink it follows.
//
// It is comparable, which is what lets a writer answer whether it has already put exactly
// this at the provider: a record whose proxy flag or TTL moved is a record to write, even
// though its address did not.
type Record struct {
	Zone    string
	Name    string
	Type    string
	Content string

	Proxied bool
	// TTL is how long a resolver may cache the record. Zero is the provider's own
	// choice, which is what `automatic` in a declaration means.
	TTL time.Duration
}

// Key identifies a record at the provider. A zone holds at most one record for a given
// name and type, so the pair is what says whether two declarations are about one thing.
type Key struct {
	Zone string
	Name string
	Type string
}

func (r Record) Key() Key { return Key{Zone: r.Zone, Name: r.Name, Type: r.Type} }

func (r Record) String() string {
	return fmt.Sprintf("%s %s -> %s", r.Name, r.Type, r.Content)
}

// Provider is the DNS provider records are written at.
//
// It is an interface so that every decision in this package can be exercised without a
// socket. Cloudflare is the only implementation, and a second one is a decision nobody
// has made (ADR 0019).
type Provider interface {
	// ZoneID is the provider's identifier for a zone named by its domain name.
	ZoneID(ctx context.Context, token, zone string) (string, error)

	// RecordID is the provider's identifier for one record, and whether it is there at
	// all. Absent and "could not ask" are different answers: absent comes back as a
	// false second result with no error, and everything else is the error (ADR 0004).
	RecordID(ctx context.Context, token, zoneID, name, recordType string) (string, bool, error)

	// Create adds a record that is not there and returns its identifier.
	Create(ctx context.Context, token, zoneID string, record Record) (string, error)

	// Update changes a record's address and the properties the declaration carries,
	// leaving every other field of it as it is.
	Update(ctx context.Context, token, zoneID, recordID string, record Record) error
}

// Writer puts records at a provider and remembers what it put there.
//
// The memory is what decides whether a turn writes at all, and it is in the process
// rather than on disk. It is empty after any start, and empty is *not known* rather than
// *the same*, so the first turn after a start writes every record once. That is what
// recovers the level-triggered property without reading the provider on every turn
// (ADR 0019).
//
// A Writer is not safe for concurrent use. One turn runs at a time, under the turn lock
// (ADR 0016).
type Writer struct {
	provider Provider

	// What was resolved at the provider, so that it is asked once per process rather
	// than once per turn. This is a lookup, not a poll.
	zones   map[string]string
	records map[Key]string

	// written is what this process last put at the provider, in full.
	written map[Key]Record
}

func NewWriter(provider Provider) *Writer {
	return &Writer{
		provider: provider,
		zones:    make(map[string]string),
		records:  make(map[Key]string),
		written:  make(map[Key]Record),
	}
}

// Last is what this writer last put at the provider under the same name and type, and
// whether there is one. It is what lets a plan say why a record is being written — the
// address moved, a property changed, or nothing is known yet.
func (w *Writer) Last(record Record) (Record, bool) {
	written, ok := w.written[record.Key()]
	return written, ok
}

// Holds reports whether this writer has already put exactly this record at the provider.
// A writer that has written nothing holds nothing, whatever the provider may have.
func (w *Writer) Holds(record Record) bool {
	written, ok := w.Last(record)
	return ok && written == record
}

// Write puts one record at the provider, creating it if it is not there.
//
// The token is the content of the file the declaration named. It is passed in rather than
// read here: this package never touches the filesystem, so there is no path by which a
// credential could be logged from inside it (ADR 0003).
//
// Nothing is remembered from a write that failed, so the next turn tries the same thing
// again — which is what makes a per-target backoff converge once the provider comes back.
func (w *Writer) Write(ctx context.Context, token string, record Record) error {
	zoneID, known := w.zones[record.Zone]
	if !known {
		id, err := w.provider.ZoneID(ctx, token, record.Zone)
		if err != nil {
			return fmt.Errorf("cannot find the zone %s at the provider: %w", record.Zone, err)
		}
		w.zones[record.Zone] = id
		zoneID = id
	}

	key := record.Key()
	recordID, known := w.records[key]
	if !known {
		id, found, err := w.provider.RecordID(ctx, token, zoneID, record.Name, record.Type)
		if err != nil {
			return fmt.Errorf("cannot look up the %s record for %s: %w", record.Type, record.Name, err)
		}
		if !found {
			id, err := w.provider.Create(ctx, token, zoneID, record)
			if err != nil {
				return fmt.Errorf("cannot create the %s record for %s: %w", record.Type, record.Name, err)
			}
			w.records[key] = id
			w.written[key] = record
			return nil
		}
		w.records[key] = id
		recordID = id
	}

	if err := w.provider.Update(ctx, token, zoneID, recordID, record); err != nil {
		// The identifier may have gone with the record — somebody deleted it at the
		// provider — and updating one that is not there would fail for ever. Forgetting
		// it costs one lookup on a failure that was merely transient, and recovers on
		// one that was not.
		delete(w.records, key)
		return fmt.Errorf("cannot update the %s record for %s: %w", record.Type, record.Name, err)
	}
	w.written[key] = record
	return nil
}
