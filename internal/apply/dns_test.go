package apply

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/ddns"
)

// recordSetResource keeps two names in one zone at the session's address.
const recordSetResource = `    - kind: DNSRecordSet
      metadata: {name: example-com}
      spec:
        provider: cloudflare
        zone: example.com
        apiTokenFile: /etc/regied/secrets/cloudflare-example-com
        records:
          - names: [example.com, "*.example.com"]
            egressRef: pppoe0
`

// fakeDNS is a provider that records what it was asked. Every record it is asked about
// is already there unless a case says otherwise, because that is the ordinary case on a
// host taking over from something else.
type fakeDNS struct {
	calls   []string
	tokens  []string
	written []ddns.Record
	absent  map[string]bool
	err     error
}

func newFakeDNS() *fakeDNS { return &fakeDNS{absent: make(map[string]bool)} }

func (f *fakeDNS) ZoneID(_ context.Context, token, zone string) (string, error) {
	f.calls = append(f.calls, "zone "+zone)
	f.tokens = append(f.tokens, token)
	if f.err != nil {
		return "", f.err
	}
	return "zone-1", nil
}

func (f *fakeDNS) RecordID(_ context.Context, token, _, name, recordType string) (string, bool, error) {
	f.calls = append(f.calls, "lookup "+name)
	f.tokens = append(f.tokens, token)
	if f.err != nil {
		return "", false, f.err
	}
	if f.absent[name] {
		return "", false, nil
	}
	return "record-" + name, true, nil
}

func (f *fakeDNS) Create(_ context.Context, token, _ string, record ddns.Record) (string, error) {
	f.calls = append(f.calls, "create "+record.Name)
	f.tokens = append(f.tokens, token)
	if f.err != nil {
		return "", f.err
	}
	f.written = append(f.written, record)
	return "record-" + record.Name, nil
}

func (f *fakeDNS) Update(_ context.Context, token, _, _ string, record ddns.Record) error {
	f.calls = append(f.calls, "update "+record.Name)
	f.tokens = append(f.tokens, token)
	if f.err != nil {
		return f.err
	}
	f.written = append(f.written, record)
	return nil
}

// dnsFixture is planFixture with a session that has dialled, a provider that answers, and
// the API token on disk.
func dnsFixture(t *testing.T) (*Engine, *fakeFiles, *fakeDNS) {
	t.Helper()
	host, files, runner := testHost()
	files.put("/etc/regied/secrets/pppoe-user-id", "account@example.net\n", 0o600)
	files.put("/etc/regied/secrets/pppoe-password", "hunter2\n", 0o600)
	files.put("/etc/regied/secrets/cloudflare-example-com", "super-secret-token\n", 0o600)
	tableAbsent(runner)
	sysctl := host.Sysctl.(*fakeSysctl)
	for _, key := range kernelSwitches(v1alpha1.Global{}) {
		sysctl.values[key.key] = "0"
	}
	host.Links = fakeLinks{"pppoe0": addrs(t, "192.0.2.1")}
	provider := newFakeDNS()
	host.DNS = provider
	return New(host, Options{}), files, provider
}

// recordsOf is the plan's DNS changes as text, so that a case can say what it expects
// without walking the structure.
func recordsOf(plan *Plan) []string {
	out := make([]string, 0, len(plan.Records))
	for _, change := range plan.Records {
		verb := "hold"
		if change.Write {
			verb = "write"
		}
		out = append(out, verb+" "+change.Record.Name+" "+change.Record.Type+" "+change.Record.Content)
	}
	return out
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestPlanWritesEveryRecordOnTheFirstTurn(t *testing.T) {
	engine, _, provider := dnsFixture(t)
	plan := mustPlan(t, engine, load(t, hostFixture+recordSetResource))

	assertLines(t, recordsOf(plan), []string{
		`write *.example.com A 192.0.2.1`,
		`write example.com A 192.0.2.1`,
	})
	if len(plan.Waiting) != 0 {
		t.Errorf("nothing is waited for: %v", plan.Waiting)
	}
	// Planning contacts nothing. A dry run is what an operator types when they are not
	// sure, and it must not spend somebody's rate limit to answer (ADR 0006, ADR 0019).
	if len(provider.calls) != 0 {
		t.Errorf("the provider was asked during planning: %v", provider.calls)
	}
}

func TestPlanLeavesRecordsAloneWhileTheUplinkHasNoAddress(t *testing.T) {
	engine, _, _ := dnsFixture(t)
	engine.host.Links = fakeLinks{}

	plan := mustPlan(t, engine, load(t, hostFixture+recordSetResource))
	if len(plan.Records) != 0 {
		t.Errorf("a record with no address to hold is not a record to write: %v", recordsOf(plan))
	}
	if len(plan.Waiting) == 0 {
		t.Fatal("the turn says what it waits for")
	}
	found := false
	for _, line := range plan.Waiting {
		if strings.Contains(line, "DNSRecordSet/example-com") && strings.Contains(line, "pppoe0") {
			found = true
		}
	}
	if !found {
		t.Errorf("the waiting line names the set and the uplink: %v", plan.Waiting)
	}
	if stateOf(plan, nil) != StateWaiting {
		t.Errorf("a turn that left a record out is waiting, got %s", stateOf(plan, nil))
	}
}

func TestApplyWritesTheRecordsAndThenHoldsStill(t *testing.T) {
	engine, _, provider := dnsFixture(t)
	provider.absent["example.com"] = true
	cfg := load(t, hostFixture+recordSetResource)

	mustApply(t, engine, cfg)
	assertLines(t, provider.calls, []string{
		"zone example.com", "lookup *.example.com", "update *.example.com",
		"lookup example.com", "create example.com",
	})
	for _, record := range provider.written {
		if record.Content != "192.0.2.1" {
			t.Errorf("%s was written with %s", record.Name, record.Content)
		}
	}

	// The second turn asks the provider nothing: what this process wrote is what the
	// records hold, and the address has not moved (ADR 0019).
	provider.calls = nil
	plan := mustPlan(t, engine, cfg)
	assertLines(t, recordsOf(plan), []string{
		`hold *.example.com A 192.0.2.1`,
		`hold example.com A 192.0.2.1`,
	})
	result := mustApply(t, engine, cfg)
	if len(provider.calls) != 0 {
		t.Errorf("the provider was asked again: %v", provider.calls)
	}
	if result.State != StateConverged {
		t.Errorf("state: got %s, want converged", result.State)
	}
}

func TestApplyWritesAgainWhenTheAddressMoves(t *testing.T) {
	engine, _, provider := dnsFixture(t)
	cfg := load(t, hostFixture+recordSetResource)
	mustApply(t, engine, cfg)

	engine.host.Links = fakeLinks{"pppoe0": addrs(t, "192.0.2.2")}
	provider.calls = nil
	provider.written = nil
	mustApply(t, engine, cfg)

	// The zone and the records were resolved on the first turn and are not resolved
	// again: this is a lookup, not a poll.
	assertLines(t, provider.calls, []string{"update *.example.com", "update example.com"})
	for _, record := range provider.written {
		if record.Content != "192.0.2.2" {
			t.Errorf("%s was written with %s", record.Name, record.Content)
		}
	}
}

// A provider that is unreachable makes that record failing and holds up nothing else.
func TestARecordThatCannotBeWrittenDoesNotStopTheTurn(t *testing.T) {
	engine, _, provider := dnsFixture(t)
	provider.err = errors.New("the provider is unreachable")
	cfg := load(t, hostFixture+recordSetResource)

	result, err := engine.Apply(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a record that could not be written is not a failed turn: %v", err)
	}
	if result.State != StateFailing {
		t.Errorf("state: got %s, want failing", result.State)
	}
	if len(result.Plan.Failing) != 2 {
		t.Fatalf("both records are failing: %v", result.Plan.Failing)
	}
	for _, line := range result.Plan.Failing {
		if !strings.Contains(line, "unreachable") {
			t.Errorf("the failure says what went wrong: %s", line)
		}
	}
	// Everything the configuration asked of this host is on it: the ruleset went in and
	// the files were written before the records were tried.
	if !slicesContains(engine.host.Runner.(*fakeRunner).commands(), "nft -f -") {
		t.Errorf("the firewall phase ran: %v", engine.host.Runner.(*fakeRunner).commands())
	}
}

func TestARecordIsRetriedAfterAFailure(t *testing.T) {
	engine, _, provider := dnsFixture(t)
	provider.err = errors.New("the provider is unreachable")
	cfg := load(t, hostFixture+recordSetResource)
	mustApply(t, engine, cfg)

	provider.err = nil
	provider.calls = nil
	result := mustApply(t, engine, cfg)
	if len(provider.calls) == 0 {
		t.Fatal("a write that failed is tried again")
	}
	if result.State != StateConverged {
		t.Errorf("state: got %s, want converged", result.State)
	}
}

// The dry run has to say why it would write, because "would write every record" is also
// what a fresh process says about records that are perfectly fine.
func TestThePlanSaysWhyARecordIsWritten(t *testing.T) {
	engine, _, _ := dnsFixture(t)
	cfg := load(t, hostFixture+recordSetResource)

	first := mustPlan(t, engine, cfg)
	for _, change := range first.Records {
		if !strings.Contains(change.Reason, "since this process started") {
			t.Errorf("a record nothing is known about: %q", change.Reason)
		}
	}

	mustApply(t, engine, cfg)
	engine.host.Links = fakeLinks{"pppoe0": addrs(t, "192.0.2.2")}
	moved := mustPlan(t, engine, cfg)
	for _, change := range moved.Records {
		if change.Reason != "the address moved from 192.0.2.1" {
			t.Errorf("an address that moved: %q", change.Reason)
		}
	}

	mustApply(t, engine, cfg)
	settled := mustPlan(t, engine, cfg)
	for _, change := range settled.Records {
		if change.Write || !strings.Contains(change.Reason, "nothing has moved") {
			t.Errorf("a record that is where it should be: %+v", change)
		}
	}
}

// The resident loop is what updates records, and an unattended turn is what it runs.
// Writing what the kernel says into a record takes down nothing that is up (ADR 0019).
func TestAnUnattendedTurnWritesRecordsAndBacksOffAFailure(t *testing.T) {
	engine, files, provider := dnsFixture(t)
	declaration := Declaration{Bytes: []byte(documentHeader + hostFixture + recordSetResource), Source: "test"}
	if _, err := engine.SubmitDeclaration(context.Background(), declaration); err != nil {
		t.Fatalf("submit: %v", err)
	}
	files.put(engine.opts.acceptedDeclaration(), string(declaration.Bytes), 0o644)

	provider.err = errors.New("the provider is unreachable")
	provider.calls = nil
	// Force the records to be written again by moving the address the kernel holds.
	engine.host.Links = fakeLinks{"pppoe0": addrs(t, "192.0.2.9")}

	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("an unreachable provider is not a failed turn: %v", err)
	}
	if result.State != StateFailing {
		t.Errorf("state: got %s, want failing", result.State)
	}
	if len(provider.calls) == 0 {
		t.Error("an unattended turn writes records")
	}

	// The next unattended turn holds the records back rather than hammering a provider
	// that just refused. The backoff is per record, which is what ADR 0019 calls a
	// per-target backoff.
	provider.calls = nil
	result, err = engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if len(provider.calls) != 0 {
		t.Errorf("the provider was asked again inside the backoff: %v", provider.calls)
	}
	var heldBack []string
	for _, line := range result.Plan.Failing {
		if strings.Contains(line, "example.com A") {
			heldBack = append(heldBack, line)
		}
	}
	if len(heldBack) != 2 {
		t.Fatalf("both records say they are held back: %v", result.Plan.Failing)
	}
	for _, line := range heldBack {
		if !strings.Contains(line, "backoff") {
			t.Errorf("the line says it is a backoff: %s", line)
		}
	}
}

// A turn whose only work is a record still has to say what it did. The summary is what
// `regied apply` prints above "Applied."
func TestTheSummaryNamesTheRecords(t *testing.T) {
	engine, _, _ := dnsFixture(t)
	cfg := load(t, hostFixture+recordSetResource)
	mustApply(t, engine, cfg)

	engine.host.Links = fakeLinks{"pppoe0": addrs(t, "192.0.2.2")}
	plan := mustPlan(t, engine, cfg)
	if plan.Empty() {
		t.Fatal("a record to write is something to do")
	}
	summary := plan.Summary()
	for _, want := range []string{"set example.com A to 192.0.2.2", "set *.example.com A to 192.0.2.2"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary does not say %q:\n%s", want, summary)
		}
	}
}

// The report is read by somebody who was not watching, so it must not list a write that
// failed among the things the turn did. The failure is said once, where failures are.
func TestAFailedRecordIsNotReportedAsSomethingTheTurnDid(t *testing.T) {
	engine, _, provider := dnsFixture(t)
	provider.err = errors.New("the provider is unreachable")
	cfg := load(t, hostFixture+recordSetResource)

	result, err := engine.Apply(context.Background(), cfg)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	report, err := engine.ReportTurn("sha256:test", "test", result.Plan, nil)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	for _, phase := range report.Phases {
		if strings.Contains(phase, "example.com") {
			t.Errorf("a write that failed is reported as done: %s", phase)
		}
	}
	if len(report.Failing) != 2 {
		t.Errorf("the failures are said where failures are: %v", report.Failing)
	}
}

// A record that was written is reported as something the turn did.
func TestAWrittenRecordIsReportedAsSomethingTheTurnDid(t *testing.T) {
	engine, _, _ := dnsFixture(t)
	result := mustApply(t, engine, load(t, hostFixture+recordSetResource))

	report, err := engine.ReportTurn("sha256:test", "test", result.Plan, nil)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	found := false
	for _, phase := range report.Phases {
		if strings.HasPrefix(phase, "dns records: ") && strings.Contains(phase, "set example.com A to 192.0.2.1") {
			found = true
		}
	}
	if !found {
		t.Errorf("the phases do not name the records written: %v", report.Phases)
	}
}

func TestTheTokenReachesTheProviderAndNothingElse(t *testing.T) {
	engine, _, provider := dnsFixture(t)
	cfg := load(t, hostFixture+recordSetResource)

	plan := mustPlan(t, engine, cfg)
	var out bytes.Buffer
	Report(&out, plan)
	if strings.Contains(out.String(), "super-secret-token") {
		t.Error("the dry run printed the token")
	}
	// The path is not a secret and is useful during a diagnosis; the content is.
	if !strings.Contains(out.String(), "example.com") {
		t.Errorf("the dry run names the records:\n%s", out.String())
	}

	mustApply(t, engine, cfg)
	for _, token := range provider.tokens {
		if token != "super-secret-token" {
			t.Errorf("the provider got %q, want the file's content with its newline trimmed", token)
		}
	}
}

func TestATokenThatCannotBeReadFailsOnlyThatRecord(t *testing.T) {
	engine, files, provider := dnsFixture(t)
	delete(files.files, "/etc/regied/secrets/cloudflare-example-com")

	result, err := engine.Apply(context.Background(), load(t, hostFixture+recordSetResource))
	if err != nil {
		t.Fatalf("an unreadable token is not a failed turn: %v", err)
	}
	if result.State != StateFailing {
		t.Errorf("state: got %s, want failing", result.State)
	}
	if len(provider.calls) != 0 {
		t.Errorf("nothing was sent without a token: %v", provider.calls)
	}
	for _, line := range result.Plan.Failing {
		if !strings.Contains(line, "/etc/regied/secrets/cloudflare-example-com") {
			t.Errorf("the failure names the file it could not read: %s", line)
		}
	}
}

// `regied render` answers what a configuration means without reading any host, so it has
// no address to show and says so rather than inventing one.
func TestRenderShowsTheRecordsWithoutAnAddress(t *testing.T) {
	engine, _, provider := dnsFixture(t)
	plan, err := engine.Render(load(t, hostFixture+recordSetResource), nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(plan.Records) != 2 {
		t.Fatalf("both records are shown: %v", recordsOf(plan))
	}
	for _, change := range plan.Records {
		if change.Write {
			t.Errorf("a rendering writes nothing: %+v", change)
		}
		if change.Record.Content != "" {
			t.Errorf("nothing was read, so there is no address: %+v", change)
		}
		if change.Follows != "PPPoESession/pppoe0" {
			t.Errorf("the rendering says which uplink the record follows: %+v", change)
		}
	}
	if len(provider.calls) != 0 {
		t.Errorf("a rendering contacts nothing: %v", provider.calls)
	}
}

func TestRecordsCarryTheirDeclaredProperties(t *testing.T) {
	engine, files, provider := dnsFixture(t)
	files.put("/etc/regied/secrets/cloudflare-example-net", "another-token\n", 0o600)
	cfg := load(t, hostFixture+`    - kind: DNSRecordSet
      metadata: {name: example-net}
      spec:
        provider: cloudflare
        zone: example.net
        apiTokenFile: /etc/regied/secrets/cloudflare-example-net
        records:
          - names: [example.net]
            egressRef: pppoe0
            proxied: true
          - names: [slow.example.net]
            egressRef: pppoe0
            ttl: 5m
`)
	mustApply(t, engine, cfg)

	byName := make(map[string]ddns.Record, len(provider.written))
	for _, record := range provider.written {
		byName[record.Name] = record
	}
	if got := byName["example.net"]; !got.Proxied || got.TTL != 0 {
		t.Errorf("the proxied record: %+v", got)
	}
	if got := byName["slow.example.net"]; got.Proxied || got.TTL.String() != "5m0s" {
		t.Errorf("the record with a TTL: %+v", got)
	}
	if got := byName["example.net"].Zone; got != "example.net" {
		t.Errorf("zone: %q", got)
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
