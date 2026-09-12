package ddns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// cloudflareAPI is the root of Cloudflare's v4 API. It is a constant rather than a field:
// there is one provider, and a configurable endpoint would be a place for a declaration
// to send a credential somewhere nobody meant it to go.
const cloudflareAPI = "https://api.cloudflare.com/client/v4"

// cloudflareAutomaticTTL is how Cloudflare spells "the provider decides".
const cloudflareAutomaticTTL = 1

// Cloudflare writes records through Cloudflare's v4 API.
//
// It is the only thing in regied that opens a connection to something that is not on this
// host, which is why the platform needs CA certificates (ADR 0011). Everything it sends
// is what the declaration says plus the address the kernel holds; nothing is inferred
// from the request, because the request leaves by whichever route this host's own traffic
// takes and that is not necessarily the uplink the record follows (ADR 0019).
type Cloudflare struct {
	client *http.Client
}

// NewCloudflare builds the client. A nil http.Client gets one with a timeout, so that a
// provider that accepts a connection and then says nothing cannot hold a turn open.
func NewCloudflare(client *http.Client) *Cloudflare {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Cloudflare{client: client}
}

func (c *Cloudflare) ZoneID(ctx context.Context, token, zone string) (string, error) {
	var zones []struct {
		ID string `json:"id"`
	}
	query := url.Values{"name": []string{zone}}
	if err := c.call(ctx, token, http.MethodGet, "/zones?"+query.Encode(), nil, &zones); err != nil {
		return "", err
	}
	if len(zones) == 0 {
		// Not a missing record but a missing zone: either the name is wrong or the
		// token cannot see it. Both are the operator's to fix, and neither is something
		// a later turn recovers from on its own.
		return "", fmt.Errorf("no zone named %s is visible to this token", zone)
	}
	return zones[0].ID, nil
}

func (c *Cloudflare) RecordID(ctx context.Context, token, zoneID, name, recordType string) (string, bool, error) {
	var records []struct {
		ID string `json:"id"`
	}
	query := url.Values{"type": []string{recordType}, "name": []string{name}}
	path := "/zones/" + url.PathEscape(zoneID) + "/dns_records?" + query.Encode()
	if err := c.call(ctx, token, http.MethodGet, path, nil, &records); err != nil {
		return "", false, err
	}
	if len(records) == 0 {
		return "", false, nil
	}
	return records[0].ID, true, nil
}

func (c *Cloudflare) Create(ctx context.Context, token, zoneID string, record Record) (string, error) {
	body := map[string]any{
		"type":    record.Type,
		"name":    record.Name,
		"content": record.Content,
		"proxied": record.Proxied,
		"ttl":     cloudflareTTL(record),
	}
	var created struct {
		ID string `json:"id"`
	}
	path := "/zones/" + url.PathEscape(zoneID) + "/dns_records"
	if err := c.call(ctx, token, http.MethodPost, path, body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// Update sends only what the declaration decides. The request is a PATCH rather than a
// PUT so that everything else the record carries — a comment somebody wrote on it — is
// left exactly as it is (ADR 0019).
func (c *Cloudflare) Update(ctx context.Context, token, zoneID, recordID string, record Record) error {
	body := map[string]any{
		"content": record.Content,
		"proxied": record.Proxied,
		"ttl":     cloudflareTTL(record),
	}
	path := "/zones/" + url.PathEscape(zoneID) + "/dns_records/" + url.PathEscape(recordID)
	return c.call(ctx, token, http.MethodPatch, path, body, nil)
}

// cloudflareTTL is the record's TTL in the seconds Cloudflare takes, with the provider's
// own choice written as the value it reserves for it.
func cloudflareTTL(record Record) int {
	if record.TTL <= 0 {
		return cloudflareAutomaticTTL
	}
	return int(record.TTL.Seconds())
}

// envelope is what every v4 answer is wrapped in. The result is decoded a second time,
// into whatever the caller wanted, because its shape differs per endpoint.
type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// call makes one request and decodes the answer.
//
// The token is put in the header and nowhere else. No error this function builds carries
// the request body or the header, so a failure cannot be the moment a credential reaches
// a log (ADR 0003).
func (c *Cloudflare) call(ctx context.Context, token, method, path string, body any, result any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("cannot build the request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, cloudflareAPI+path, payload)
	if err != nil {
		return fmt.Errorf("cannot build the request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("the provider could not be reached: %w", err)
	}
	defer response.Body.Close()

	// The body is bounded: an answer from something that is not the API — a captive
	// portal, a proxy — should not be read into memory without limit.
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("the provider's answer could not be read: %w", err)
	}

	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("the provider answered %s with something that is not its JSON", response.Status)
	}
	if !envelope.Success {
		return fmt.Errorf("the provider refused with %s: %s", response.Status, describeErrors(envelope.Errors))
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("the provider's answer could not be read: %w", err)
	}
	return nil
}

func describeErrors(errs []apiError) string {
	if len(errs) == 0 {
		return "no reason given"
	}
	lines := make([]string, len(errs))
	for i, item := range errs {
		lines[i] = fmt.Sprintf("%s (code %d)", item.Message, item.Code)
	}
	return strings.Join(lines, "; ")
}
