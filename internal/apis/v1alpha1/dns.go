package v1alpha1

import (
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// DNSProvider is where a set's records live. There is one implementation and no
// interface admitting a second, but the declaration names it anyway: a file that says
// what it was talking to stays valid on the day there is a second provider, and adding
// that provider is then a new value rather than an edit to every file
// (ADR 0019).
type DNSProvider string

// ProviderCloudflare is the only provider regied writes records at.
const ProviderCloudflare DNSProvider = "cloudflare"

// DNSRecordType is the type of a record. A is the only one regied writes: a record here
// follows an address this host's uplink holds, and that is the IPv4 address. The field
// exists so that widening it later adds a value rather than a field (ADR 0019).
type DNSRecordType string

const DNSRecordA DNSRecordType = "A"

// DNSRecordTTL is how long a resolver may cache a record.
//
// Zero is the provider's own choice. That is what `automatic` decodes to and what an
// absent field means, and the two are deliberately the same value: they ask for the same
// thing, and a distinction nothing acts on is one more state to keep in step.
type DNSRecordTTL struct {
	For time.Duration
}

func (t DNSRecordTTL) String() string {
	if t.For == 0 {
		return "automatic"
	}
	return t.For.String()
}

func (t *DNSRecordTTL) UnmarshalYAML(node *yaml.Node) error {
	s, err := scalar(node, `"automatic" or a duration such as 5m`)
	if err != nil {
		return err
	}
	if s == "automatic" {
		t.For = 0
		return nil
	}
	v, perr := time.ParseDuration(s)
	if perr != nil {
		return typeErrorf(node, `%q is not "automatic" or a duration`, s)
	}
	if v <= 0 {
		return typeErrorf(node, "%q is not a positive duration", s)
	}
	t.For = v
	return nil
}

// DNSRecordSetSpec is the records regied keeps in one DNS zone, so that a name published
// through a PortForward resolves to the address the uplink is holding now.
//
// It is a set of records *in* a zone and not the zone itself: regied writes what is
// listed here, creates what is not there, and deletes nothing (ADR 0009, ADR 0019). The
// resource is per zone because the API token is what binds to a zone, and a token scoped
// to one zone is the smaller credential.
type DNSRecordSetSpec struct {
	Provider     DNSProvider `yaml:"provider"`
	Zone         string      `yaml:"zone"`
	APITokenFile string      `yaml:"apiTokenFile"`
	Records      []DNSRecord `yaml:"records"`
}

func (*DNSRecordSetSpec) ResourceKind() ResourceKind { return KindDNSRecordSet }

// DNSRecord is one set of properties over several names. The names follow the same
// uplink and want the same flags, which is how a zone's apex and its wildcard are
// written; each name is its own record at the provider.
//
// There is no field for the address. It is the uplink's, exactly as it is for SourceNAT
// and PortForward, and writing it down produces a configuration that works until the
// address changes (ADR 0002, ADR 0019).
type DNSRecord struct {
	Names     []string      `yaml:"names"`
	Type      DNSRecordType `yaml:"type"`
	EgressRef string        `yaml:"egressRef"`
	Proxied   *bool         `yaml:"proxied"`
	TTL       DNSRecordTTL  `yaml:"ttl"`
}

// TypeOrDefault is the record type, which is A unless something else was written.
func (r DNSRecord) TypeOrDefault() DNSRecordType {
	if r.Type == "" {
		return DNSRecordA
	}
	return r.Type
}

// ProxiedEnabled is Cloudflare's proxy flag. It is declared rather than left to whatever
// the record already carries, because a record being created has no existing value to
// defer to (ADR 0019).
func (r DNSRecord) ProxiedEnabled() bool { return boolOr(r.Proxied, false) }
