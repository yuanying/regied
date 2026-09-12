package ddns_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yuanying/regied/internal/ddns"
)

// exchange is one request the client made and the answer a case gave back.
type exchange struct {
	method string
	url    string
	header http.Header
	body   map[string]any
}

// transport answers requests from a queue of canned bodies and records what it was
// asked. No socket is opened: `make test` passes with nothing but the Go toolchain.
type transport struct {
	answers []answer
	seen    []exchange
}

type answer struct {
	status int
	body   string
	err    error
}

func (tr *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	seen := exchange{method: request.Method, url: request.URL.String(), header: request.Header.Clone()}
	if request.Body != nil {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &seen.body); err != nil {
				return nil, err
			}
		}
	}
	tr.seen = append(tr.seen, seen)

	if len(tr.answers) == 0 {
		tr.answers = []answer{{status: 500, body: `{"success":false,"errors":[{"code":0,"message":"the case gave no answer"}]}`}}
	}
	next := tr.answers[0]
	tr.answers = tr.answers[1:]
	if next.err != nil {
		return nil, next.err
	}
	return &http.Response{
		StatusCode: next.status,
		Body:       io.NopCloser(strings.NewReader(next.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func cloudflare(answers ...answer) (*ddns.Cloudflare, *transport) {
	tr := &transport{answers: answers}
	return ddns.NewCloudflare(&http.Client{Transport: tr}), tr
}

func TestCloudflareResolvesAZoneByName(t *testing.T) {
	client, tr := cloudflare(answer{status: 200, body: `{"success":true,"result":[{"id":"zone-1","name":"example.com"}]}`})

	id, err := client.ZoneID(context.Background(), "a-token", "example.com")
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	if id != "zone-1" {
		t.Errorf("id: got %q, want zone-1", id)
	}
	if got := tr.seen[0].url; got != "https://api.cloudflare.com/client/v4/zones?name=example.com" {
		t.Errorf("url: %s", got)
	}
	if got := tr.seen[0].header.Get("Authorization"); got != "Bearer a-token" {
		t.Errorf("the token is sent as a bearer credential, got %q", got)
	}
}

func TestCloudflareSaysSoWhenTheZoneIsNotThere(t *testing.T) {
	client, _ := cloudflare(answer{status: 200, body: `{"success":true,"result":[]}`})

	_, err := client.ZoneID(context.Background(), "a-token", "example.com")
	if err == nil {
		t.Fatal("a zone the account cannot see is an error, not an empty identifier")
	}
	if !strings.Contains(err.Error(), "example.com") {
		t.Errorf("the error names the zone: %v", err)
	}
}

func TestCloudflareFindsARecord(t *testing.T) {
	client, tr := cloudflare(answer{status: 200, body: `{"success":true,"result":[{"id":"record-7","content":"192.0.2.9"}]}`})

	id, found, err := client.RecordID(context.Background(), "a-token", "zone-1", "*.example.com", "A")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found || id != "record-7" {
		t.Errorf("got %q found=%v, want record-7 true", id, found)
	}
	url := tr.seen[0].url
	if !strings.HasPrefix(url, "https://api.cloudflare.com/client/v4/zones/zone-1/dns_records?") {
		t.Errorf("url: %s", url)
	}
	for _, want := range []string{"type=A", "name=%2A.example.com"} {
		if !strings.Contains(url, want) {
			t.Errorf("url %s does not carry %s", url, want)
		}
	}
}

// Absent and "could not ask" are different answers. A record that is not there comes
// back as not found with no error, so that the writer creates it (ADR 0004).
func TestCloudflareReportsAnAbsentRecordWithoutAnError(t *testing.T) {
	client, _ := cloudflare(answer{status: 200, body: `{"success":true,"result":[]}`})

	id, found, err := client.RecordID(context.Background(), "a-token", "zone-1", "example.com", "A")
	if err != nil {
		t.Fatalf("an absent record is not an error: %v", err)
	}
	if found || id != "" {
		t.Errorf("got %q found=%v, want \"\" false", id, found)
	}
}

func TestCloudflareCreatesARecord(t *testing.T) {
	client, tr := cloudflare(answer{status: 200, body: `{"success":true,"result":{"id":"record-new"}}`})

	id, err := client.Create(context.Background(), "a-token", "zone-1", ddns.Record{
		Zone: "example.com", Name: "example.com", Type: "A", Content: "192.0.2.1", Proxied: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id != "record-new" {
		t.Errorf("id: got %q, want record-new", id)
	}
	seen := tr.seen[0]
	if seen.method != http.MethodPost {
		t.Errorf("method: %s", seen.method)
	}
	if seen.url != "https://api.cloudflare.com/client/v4/zones/zone-1/dns_records" {
		t.Errorf("url: %s", seen.url)
	}
	want := map[string]any{"type": "A", "name": "example.com", "content": "192.0.2.1", "proxied": true, "ttl": float64(1)}
	for key, value := range want {
		if seen.body[key] != value {
			t.Errorf("body[%s]: got %v, want %v", key, seen.body[key], value)
		}
	}
	// Nothing writes a comment. It is a person's note about the record, and regied has
	// no business holding it (ADR 0019).
	if _, ok := seen.body["comment"]; ok {
		t.Errorf("the body carries a comment: %v", seen.body)
	}
}

// The update is partial on purpose: a comment a person or an earlier tool wrote on the
// record survives it (ADR 0019).
func TestCloudflareUpdatesOnlyWhatIsDeclared(t *testing.T) {
	client, tr := cloudflare(answer{status: 200, body: `{"success":true,"result":{"id":"record-7"}}`})

	err := client.Update(context.Background(), "a-token", "zone-1", "record-7", ddns.Record{
		Zone: "example.com", Name: "example.com", Type: "A", Content: "192.0.2.2", TTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	seen := tr.seen[0]
	if seen.method != http.MethodPatch {
		t.Errorf("method: got %s, want PATCH so that the rest of the record is left alone", seen.method)
	}
	if seen.url != "https://api.cloudflare.com/client/v4/zones/zone-1/dns_records/record-7" {
		t.Errorf("url: %s", seen.url)
	}
	if seen.body["content"] != "192.0.2.2" || seen.body["ttl"] != float64(300) || seen.body["proxied"] != false {
		t.Errorf("body: %v", seen.body)
	}
	for _, key := range []string{"comment", "name", "type"} {
		if _, ok := seen.body[key]; ok {
			t.Errorf("the update carries %s, which it does not change: %v", key, seen.body)
		}
	}
}

func TestCloudflareCarriesTheAPIsComplaint(t *testing.T) {
	client, _ := cloudflare(answer{status: 403, body: `{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`})

	_, err := client.ZoneID(context.Background(), "super-secret-token", "example.com")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "Invalid access token") {
		t.Errorf("the error says what the provider said: %v", err)
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("the token is in the error: %v", err)
	}
}

func TestCloudflareRefusesAnAnswerThatIsNotItsJSON(t *testing.T) {
	client, _ := cloudflare(answer{status: 200, body: `<html>a proxy answered instead</html>`})

	_, err := client.ZoneID(context.Background(), "a-token", "example.com")
	if err == nil {
		t.Fatal("an answer that is not the API's is an error")
	}
}
