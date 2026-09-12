package ddns_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuanying/regied/internal/ddns"
)

// fakeProvider stands in for Cloudflare. It records what it was asked and answers from
// what a case put in it, so that the writer's decisions are tested without a socket.
type fakeProvider struct {
	zoneIDs   map[string]string
	recordIDs map[string]string // name+type -> id; absent means the record is not there
	created   []ddns.Record
	updated   []ddns.Record
	updatedAt []string // the record identifier each update went to
	calls     []string
	tokens    []string

	zoneErr   error
	lookupErr error
	createErr error
	updateErr error
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		zoneIDs:   map[string]string{"example.com": "zone-1"},
		recordIDs: map[string]string{},
	}
}

func (f *fakeProvider) ZoneID(_ context.Context, token, zone string) (string, error) {
	f.calls = append(f.calls, "zone "+zone)
	f.tokens = append(f.tokens, token)
	if f.zoneErr != nil {
		return "", f.zoneErr
	}
	return f.zoneIDs[zone], nil
}

func (f *fakeProvider) RecordID(_ context.Context, token, zoneID, name, recordType string) (string, bool, error) {
	f.calls = append(f.calls, "lookup "+name+" "+recordType)
	f.tokens = append(f.tokens, token)
	if f.lookupErr != nil {
		return "", false, f.lookupErr
	}
	id, ok := f.recordIDs[name+" "+recordType]
	return id, ok, nil
}

func (f *fakeProvider) Create(_ context.Context, token, zoneID string, record ddns.Record) (string, error) {
	f.calls = append(f.calls, "create "+record.Name)
	f.tokens = append(f.tokens, token)
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, record)
	return "record-new", nil
}

func (f *fakeProvider) Update(_ context.Context, token, zoneID, recordID string, record ddns.Record) error {
	f.calls = append(f.calls, "update "+record.Name)
	f.tokens = append(f.tokens, token)
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = append(f.updated, record)
	f.updatedAt = append(f.updatedAt, recordID)
	return nil
}

func record(content string) ddns.Record {
	return ddns.Record{Zone: "example.com", Name: "example.com", Type: "A", Content: content}
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls: got %v, want %v", got, want)
		}
	}
}

func TestWriterCreatesARecordThatIsNotThere(t *testing.T) {
	provider := newFakeProvider()
	writer := ddns.NewWriter(provider)

	if err := writer.Write(context.Background(), "a-token", record("192.0.2.1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	assertCalls(t, provider.calls, []string{"zone example.com", "lookup example.com A", "create example.com"})
	if len(provider.created) != 1 || provider.created[0].Content != "192.0.2.1" {
		t.Errorf("created: %+v", provider.created)
	}
	if len(provider.updated) != 0 {
		t.Errorf("nothing was there to update: %+v", provider.updated)
	}
}

func TestWriterUpdatesARecordThatIsThere(t *testing.T) {
	provider := newFakeProvider()
	provider.recordIDs["example.com A"] = "record-7"
	writer := ddns.NewWriter(provider)

	if err := writer.Write(context.Background(), "a-token", record("192.0.2.1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	assertCalls(t, provider.calls, []string{"zone example.com", "lookup example.com A", "update example.com"})
	if len(provider.updatedAt) != 1 || provider.updatedAt[0] != "record-7" {
		t.Errorf("the update went to %v, want record-7", provider.updatedAt)
	}
	if len(provider.created) != 0 {
		t.Errorf("a record that is there is not created: %+v", provider.created)
	}
}

// The zone and the record are resolved once per process, not once per turn. That is the
// whole difference between a lookup and a poll (ADR 0019).
func TestWriterResolvesTheZoneAndTheRecordOnce(t *testing.T) {
	provider := newFakeProvider()
	provider.recordIDs["example.com A"] = "record-7"
	writer := ddns.NewWriter(provider)

	for _, content := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
		if err := writer.Write(context.Background(), "a-token", record(content)); err != nil {
			t.Fatalf("write %s: %v", content, err)
		}
	}
	assertCalls(t, provider.calls, []string{
		"zone example.com", "lookup example.com A",
		"update example.com", "update example.com", "update example.com",
	})
}

func TestWriterKnowsWhatItHasWritten(t *testing.T) {
	provider := newFakeProvider()
	writer := ddns.NewWriter(provider)

	// A writer that has written nothing knows nothing. Empty is "not known", never
	// "the same", which is what makes the first turn after a start write.
	if writer.Holds(record("192.0.2.1")) {
		t.Error("a writer that has written nothing holds nothing")
	}
	if err := writer.Write(context.Background(), "a-token", record("192.0.2.1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !writer.Holds(record("192.0.2.1")) {
		t.Error("what was just written is held")
	}
	if writer.Holds(record("192.0.2.2")) {
		t.Error("a different address is not what was written")
	}

	// A declared property that moved is a record to write, even though the address did
	// not: the write carries the properties too.
	proxied := record("192.0.2.1")
	proxied.Proxied = true
	if writer.Holds(proxied) {
		t.Error("a record whose proxy flag moved is not what was written")
	}
	retimed := record("192.0.2.1")
	retimed.TTL = 5 * time.Minute
	if writer.Holds(retimed) {
		t.Error("a record whose TTL moved is not what was written")
	}
}

func TestWriterRemembersNothingFromAFailedWrite(t *testing.T) {
	provider := newFakeProvider()
	provider.recordIDs["example.com A"] = "record-7"
	provider.updateErr = errors.New("the provider said no")
	writer := ddns.NewWriter(provider)

	err := writer.Write(context.Background(), "a-token", record("192.0.2.1"))
	if err == nil {
		t.Fatal("a write the provider refused is an error")
	}
	if writer.Holds(record("192.0.2.1")) {
		t.Error("a write that failed put nothing at the provider, so nothing is held")
	}

	// The identifier is looked up again: an update that failed may have failed because
	// the record is no longer there, and updating an identifier that has gone would
	// fail for ever.
	provider.updateErr = nil
	provider.calls = nil
	if err := writer.Write(context.Background(), "a-token", record("192.0.2.1")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	assertCalls(t, provider.calls, []string{"lookup example.com A", "update example.com"})
}

func TestWriterReportsWhatWentWrongWithoutTheToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(*fakeProvider)
	}{
		{"the zone could not be resolved", func(f *fakeProvider) { f.zoneErr = errors.New("zone refused") }},
		{"the record could not be looked up", func(f *fakeProvider) { f.lookupErr = errors.New("lookup refused") }},
		{"the record could not be created", func(f *fakeProvider) { f.createErr = errors.New("create refused") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := newFakeProvider()
			tc.spoil(provider)
			writer := ddns.NewWriter(provider)

			err := writer.Write(context.Background(), "super-secret-token", record("192.0.2.1"))
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "super-secret-token") {
				t.Errorf("the token is in the error: %v", err)
			}
			if writer.Holds(record("192.0.2.1")) {
				t.Error("nothing was written, so nothing is held")
			}
		})
	}
}

// A zone whose records are written with one token and another with a different one: the
// writer hands each record the token it was given, and does not carry one over.
func TestWriterUsesTheTokenItWasGiven(t *testing.T) {
	provider := newFakeProvider()
	provider.zoneIDs["example.net"] = "zone-2"
	writer := ddns.NewWriter(provider)

	if err := writer.Write(context.Background(), "token-com", record("192.0.2.1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	other := ddns.Record{Zone: "example.net", Name: "example.net", Type: "A", Content: "192.0.2.1"}
	if err := writer.Write(context.Background(), "token-net", other); err != nil {
		t.Fatalf("write: %v", err)
	}
	for i, token := range provider.tokens {
		want := "token-com"
		if i >= 3 {
			want = "token-net"
		}
		if token != want {
			t.Errorf("call %d (%s) used %q, want %q", i, provider.calls[i], token, want)
		}
	}
}
