package apply

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// declarationOf is the bytes a test submits: the same document load builds its Config
// from, so that the record can be compared with what was validated.
func declarationOf(body string) []byte { return []byte(documentHeader + body) }

// mustSubmit plans and submits a declaration the way `regied apply` does.
func mustSubmit(t *testing.T, engine *Engine, body, source string) *Result {
	t.Helper()
	cfg := load(t, body)
	plan := mustPlan(t, engine, cfg)
	result, err := engine.Submit(context.Background(), plan, Declaration{Bytes: declarationOf(body), Source: source})
	if err != nil {
		t.Fatalf("submitting failed: %v", err)
	}
	return result
}

// --- The record is the declaration, written at submission ---------------------------

// The record is written once the declaration has validated and staged, before the first
// command runs: from that moment the declaration is the spec, whether or not the commands
// that follow succeed (ADR 0016).
func TestSubmitRecordsTheDeclarationBeforeTheFirstCommand(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	var recordedAtFirstCommand string
	var seen bool
	runner.onRun = func(cmd Command) {
		if seen || cmd.String() == "nft list tables" || cmd.String() == "nft --check -f -" {
			return
		}
		seen = true
		recordedAtFirstCommand, _ = files.content("/var/lib/regied/accepted/declaration.yaml")
	}

	result := mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")

	if !seen {
		t.Fatal("the fixture ran no command, so the test proves nothing")
	}
	if recordedAtFirstCommand != string(declarationOf(hostFixture)) {
		t.Errorf("at the first command the record held:\n%q\nwant the declaration as submitted", recordedAtFirstCommand)
	}
	if result.Revision != Revision(declarationOf(hostFixture)) {
		t.Errorf("the result reports revision %q, want the digest of the declaration", result.Revision)
	}
}

// The revision is the digest of the bytes and nothing else, so anyone holding a copy of
// the file can compute it without regied (ADR 0007).
func TestRevisionIsTheDigestOfTheBytes(t *testing.T) {
	if got, want := Revision([]byte("abc")), "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Errorf("Revision(abc) = %q, want %q", got, want)
	}
}

// A submission whose commands fail leaves the declaration in the record. The record
// holds what was asked for, not what last worked; the host is wherever the failure left
// it, and the report says so (ADR 0016).
//
// No inverse is run. The record and staged files keep the new declaration, while the
// report identifies the command at which the turn stopped.
func TestAFailedSubmissionKeepsTheDeclarationItWasAsked(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	changed := strings.Replace(hostFixture, "192.168.10.127", "192.168.10.200", 1)
	runner.fail["systemctl restart regied-dnsmasq.service"] = errFake
	plan := mustPlan(t, engine, load(t, changed))
	_, err := engine.Submit(context.Background(), plan, Declaration{Bytes: declarationOf(changed), Source: "/etc/regied/config.yaml"})
	requireErrorContaining(t, err, "regied-dnsmasq.service")

	recorded, _ := files.content("/var/lib/regied/accepted/declaration.yaml")
	if recorded != string(declarationOf(changed)) {
		t.Error("the record does not hold the declaration that was submitted")
	}
	report := mustLastTurn(t, engine)
	if report.State != StateFailing {
		t.Errorf("the report says %q, want failing", report.State)
	}
	if !strings.Contains(strings.Join(report.Failing, "\n"), "regied-dnsmasq.service") {
		t.Errorf("the report does not name what failed: %v", report.Failing)
	}
	if report.Revision != Revision(declarationOf(changed)) {
		t.Error("the report is about a different revision from the one recorded")
	}

	if got, _ := files.content("/etc/regied/dnsmasq/dnsmasq.conf"); !strings.Contains(got, "192.168.10.200") {
		t.Error("the staged file was rolled back after the failed submission")
	}
}

// A declaration that does not stage is not accepted, and nothing records it.
func TestASubmissionThatFailsStagingIsNotRecorded(t *testing.T) {
	engine, files, _, _ := planFixture(t)
	files.writeErr["/etc/regied/dnsmasq/dnsmasq.conf"] = errFake

	plan := mustPlan(t, engine, load(t, hostFixture))
	_, err := engine.Submit(context.Background(), plan, Declaration{Bytes: declarationOf(hostFixture)})
	if err == nil {
		t.Fatal("the failing write was not reported")
	}
	if _, ok := files.content("/var/lib/regied/accepted/declaration.yaml"); ok {
		t.Error("a declaration that did not stage was recorded as accepted")
	}
}

// A declaration that cannot be recorded is not applied either. A host running a
// declaration its record does not hold is exactly the drift the record exists to rule
// out, and nothing has run yet, so refusing costs nothing (ADR 0016).
func TestASubmissionRunsNothingWhenTheDeclarationCannotBeRecorded(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	files.writeErr["/var/lib/regied/accepted/declaration.yaml"] = errFake

	plan := mustPlan(t, engine, load(t, hostFixture))
	_, err := engine.Submit(context.Background(), plan, Declaration{Bytes: declarationOf(hostFixture)})
	requireErrorContaining(t, err, "/var/lib/regied/accepted/declaration.yaml")

	for _, command := range runner.commands() {
		if command != "nft list tables" && command != "nft --check -f -" {
			t.Errorf("a submission that could not be recorded still ran %q", command)
		}
	}
	if _, ok := files.content("/etc/regied/dnsmasq/dnsmasq.conf"); !ok {
		t.Error("the staged files disappeared even though no inverse is run")
	}
}

// --- The report says where the turn left the host ----------------------------------

func TestASubmissionThatDidEverythingReportsConverged(t *testing.T) {
	engine, _, _, _ := planFixture(t)

	result := mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")

	if result.State != StateConverged {
		t.Errorf("the result says %q, want converged", result.State)
	}
	report := mustLastTurn(t, engine)
	if report.State != StateConverged {
		t.Errorf("the report says %q, want converged", report.State)
	}
	if report.Revision != Revision(declarationOf(hostFixture)) {
		t.Errorf("the report is about %q, want the submitted revision", report.Revision)
	}
	if report.Source != "/etc/regied/config.yaml" {
		t.Errorf("the report names %q as the source, want the file that was submitted", report.Source)
	}
	if report.Since.IsZero() {
		t.Error("the report does not say when the state was entered")
	}
}

// A turn that left something out for want of a value has not converged, and the report
// names what it waits for. It is not a failure: the apply exits 0 (ADR 0016).
func TestASubmissionThatWaitsReportsWhatItWaitsFor(t *testing.T) {
	engine, files, _, _ := planFixture(t)
	putSecrets(files)
	// No resolver entry: the AFTR name does not resolve.

	result := mustSubmit(t, engine, uplinkFixture, "/etc/regied/config.yaml")

	if result.State != StateWaiting {
		t.Errorf("the result says %q, want waiting", result.State)
	}
	report := mustLastTurn(t, engine)
	if report.State != StateWaiting {
		t.Errorf("the report says %q, want waiting", report.State)
	}
	if !strings.Contains(strings.Join(report.Waiting, "\n"), "aftr.example.net") {
		t.Errorf("the report does not name what is waited for: %v", report.Waiting)
	}
}

// The report is rewritten only when what it says changes, the rule ADR 0004 gives every
// file regied writes. A turn that confirms the same state writes nothing (ADR 0016).
func TestTheReportIsWrittenOnlyWhenItChanges(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	clock := host.Clock.(*fakeClock)
	mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")
	// The kernel now answers both probes the way it would after the apply, so that the
	// turns that follow have nothing to do.
	tablePresent(runner)
	setsHold(runner, map[string][]string{"uplink4_pppoe0": {}, "uplink6_pppoe0": {}})
	report := "/var/lib/regied/turn/report.yaml"

	// The turn after the submission does say something new — the submission applied, this
	// one found nothing to do — so it writes. It is the one after that which must not.
	clock.now = clock.now.Add(time.Minute)
	if _, err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("the first reconcile failed: %v", err)
	}
	writes := countOf(files.writes, report)

	clock.now = clock.now.Add(time.Minute)
	if _, err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("the second reconcile failed: %v", err)
	}

	if got := countOf(files.writes, report); got != writes {
		t.Errorf("a turn that changed nothing and said nothing new rewrote the report (%d writes, then %d)", writes, got)
	}
}

// What the report records is when the state was entered, not when it was last confirmed.
// A report carrying the time of the last turn would be rewritten every minute to say
// nothing new (ADR 0016).
func TestTheReportKeepsTheTimeTheStateWasEntered(t *testing.T) {
	engine, files, runner, host := planFixture(t)
	putSecrets(files)
	clock := host.Clock.(*fakeClock)
	resolver := host.Resolver.(fakeResolver)
	resolver["aftr.example.net"] = addrs(t, "2001:db8:53::1")

	entered := clock.now
	mustSubmit(t, engine, uplinkFixture, "/etc/regied/config.yaml")
	tablePresent(runner)
	if got := mustLastTurn(t, engine); got.State != StateConverged || !got.Since.Equal(entered) {
		t.Fatalf("after the submission the report says %s since %v, want converged since %v", got.State, got.Since, entered)
	}

	// A later turn that finds the same state leaves the time alone.
	clock.now = entered.Add(time.Minute)
	if _, err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("the reconcile failed: %v", err)
	}
	if got := mustLastTurn(t, engine); !got.Since.Equal(entered) {
		t.Errorf("a turn that confirmed the state moved the time to %v", got.Since)
	}

	// The resolver goes away: the host is waiting from now.
	delete(resolver, "aftr.example.net")
	waitingFrom := entered.Add(2 * time.Minute)
	clock.now = waitingFrom
	if _, err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("the reconcile failed: %v", err)
	}
	if got := mustLastTurn(t, engine); got.State != StateWaiting || !got.Since.Equal(waitingFrom) {
		t.Errorf("the report says %s since %v, want waiting since %v", got.State, got.Since, waitingFrom)
	}
	// And the source, which no reconcile knows, is carried with the revision.
	if got := mustLastTurn(t, engine); got.Source != "/etc/regied/config.yaml" {
		t.Errorf("the reconcile lost the source: %q", got.Source)
	}
}

// --- Reconcile: one turn toward the record, reading nothing else ---------------------

// A reconcile takes no file. It reads the record, runs one turn toward it and stops,
// which is what a boot unit runs and what an operator types to put a host back where it
// should be without submitting anything (ADR 0016).
func TestReconcileConvergesToTheRecord(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	// Somebody deleted a file regied wrote.
	network := "/etc/systemd/network/50-regied-wan.network"
	delete(files.files, network)
	before := len(runner.ran)

	result, err := engine.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("the reconcile failed: %v", err)
	}
	if !result.Changed {
		t.Error("the reconcile found nothing to do while a file was missing")
	}
	if _, ok := files.content(network); !ok {
		t.Error("the reconcile did not put the file back")
	}
	if !slices.Contains(commandsSince(runner, before), "networkctl reload") {
		t.Errorf("the reconcile did not reload networkd after putting its file back:\n%s", strings.Join(commandsSince(runner, before), "\n"))
	}
	if result.State != StateConverged {
		t.Errorf("the reconcile says %q, want converged", result.State)
	}
}

func TestUnattendedTurnReportsButDoesNotRestartARunningProcess(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")
	tablePresent(runner)

	changed := strings.Replace(hostFixture, "192.168.10.127", "192.168.10.200", 1)
	files.put("/var/lib/regied/accepted/declaration.yaml", string(declarationOf(changed)), 0o644)
	before := len(runner.ran)
	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("unattended turn failed: %v", err)
	}
	if slices.Contains(commandsSince(runner, before), "systemctl restart regied-dnsmasq.service") {
		t.Fatal("an unattended turn restarted a running process")
	}
	if result.State != StateFailing || !strings.Contains(strings.Join(result.Plan.Failing, "\n"), "does not take down") {
		t.Errorf("unsafe drift was not reported as failing: state=%s failing=%v", result.State, result.Plan.Failing)
	}
	if got, _ := files.content("/etc/regied/dnsmasq/dnsmasq.conf"); !strings.Contains(got, "192.168.10.200") {
		t.Error("the unattended turn did not write the owned file")
	}
}

func TestUnattendedTurnBacksOffACommandWhileStillComparing(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")
	tableAbsent(runner)

	before := len(runner.ran)
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatalf("first unattended turn failed: %v", err)
	}
	first := commandsSince(runner, before)
	if !slices.Contains(first, "nft -f -") {
		t.Fatal("the first repair attempt was rate limited")
	}

	before = len(runner.ran)
	result, err := engine.ReconcileUnattended(context.Background())
	if err != nil {
		t.Fatalf("backed-off turn failed: %v", err)
	}
	if slices.Contains(commandsSince(runner, before), "nft -f -") {
		t.Fatal("the repeated repair command was not backed off")
	}
	if result.State != StateFailing || !strings.Contains(strings.Join(result.Plan.Failing, "\n"), "backoff level") {
		t.Errorf("backoff was not visible in the result: state=%s failing=%v", result.State, result.Plan.Failing)
	}
}

type fixedUnits map[string]bool

func (u fixedUnits) Active(_ context.Context, unit string) (bool, error) { return u[unit], nil }

func TestUnattendedTurnStartsADeclaredInactiveUnit(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")
	tablePresent(runner)
	engine.host.Units = fixedUnits{
		"regied-pppoe@pppoe0.service": true,
		"regied-dnsmasq.service":      false,
	}

	before := len(runner.ran)
	if _, err := engine.ReconcileUnattended(context.Background()); err != nil {
		t.Fatalf("unattended turn failed: %v", err)
	}
	commands := commandsSince(runner, before)
	if !slices.Contains(commands, "systemctl start regied-dnsmasq.service") {
		t.Errorf("the inactive declared unit was not started:\n%s", strings.Join(commands, "\n"))
	}
	for _, command := range commands {
		if strings.Contains(command, "restart") || strings.Contains(command, "disable --now") {
			t.Errorf("the unattended turn crossed its safety line with %q", command)
		}
	}
}

// A host with no record is left alone and told so. Converging on nothing would take the
// firewall off a running router over a missing file (ADR 0016).
func TestReconcileWithNoRecordDoesNothingAndSaysSo(t *testing.T) {
	engine, files, runner, _ := planFixture(t)

	_, err := engine.Reconcile(context.Background())

	if !errors.Is(err, ErrNoRecord) {
		t.Fatalf("a host with no record reports %v, want ErrNoRecord", err)
	}
	if len(files.writes) != 0 || len(files.removes) != 0 {
		t.Errorf("a reconcile with no record touched the host: wrote %v, removed %v", files.writes, files.removes)
	}
	if len(runner.ran) != 0 {
		t.Errorf("a reconcile with no record ran %v", runner.commands())
	}
}

// A record this version of regied no longer accepts is left alone too. Converging on a
// declaration half understood is worse than not converging, and the operator is left with
// a running router and a message (ADR 0016).
func TestReconcileWithARecordThatDoesNotValidateDoesNothingAndSaysSo(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	// An egressRef naming nothing: the document parses and does not validate.
	invalid := declarationOf(hostFixture + `    - kind: SourceNAT
      metadata: {name: masquerade}
      spec:
        type: masquerade
        egressRef: nowhere
        sourceRanges: [192.168.10.0/24]
`)
	files.put("/var/lib/regied/accepted/declaration.yaml", string(invalid), 0o644)

	_, err := engine.Reconcile(context.Background())

	var invalidRecord *RecordError
	if !errors.As(err, &invalidRecord) {
		t.Fatalf("an invalid record reports %v, want a RecordError", err)
	}
	requireErrorContaining(t, err, "nowhere")
	for _, path := range files.writes {
		if path != "/var/lib/regied/turn/report.yaml" {
			t.Errorf("a reconcile over an invalid record wrote %s", path)
		}
	}
	if len(runner.ran) != 0 {
		t.Errorf("a reconcile over an invalid record ran %v", runner.commands())
	}
	report := mustLastTurn(t, engine)
	if report.State != StateFailing || !strings.Contains(strings.Join(report.Failing, "\n"), "nowhere") {
		t.Errorf("the report does not say the record failed validation and why: %+v", report)
	}
	if report.Revision != Revision(invalid) {
		t.Error("the report is not about the record it could not accept")
	}
}

// A credential that cannot be read fails the turn before anything runs, on every turn
// that needs it: the rule does not change under the loop (ADR 0003, ADR 0016).
func TestReconcileFailsBeforeTheFirstCommandWhenACredentialCannotBeRead(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")
	tablePresent(runner)
	delete(files.files, "/etc/regied/secrets/pppoe-password")
	before := len(runner.ran)

	_, err := engine.Reconcile(context.Background())

	requireErrorContaining(t, err, "/etc/regied/secrets/pppoe-password")
	if len(commandsSince(runner, before)) != 0 {
		t.Errorf("a turn that could not read a credential ran %v", commandsSince(runner, before))
	}
	report := mustLastTurn(t, engine)
	if report.State != StateFailing || !strings.Contains(strings.Join(report.Failing, "\n"), "pppoe-password") {
		t.Errorf("the report does not say what failed: %+v", report)
	}
}

// --- helpers -----------------------------------------------------------------------

func mustLastTurn(t *testing.T, engine *Engine) *TurnReport {
	t.Helper()
	report, err := engine.LastTurn()
	if err != nil {
		t.Fatalf("reading the report of the last turn failed: %v", err)
	}
	return report
}

func commandsSince(runner *fakeRunner, n int) []string {
	return runner.commands()[n:]
}

func countOf(paths []string, path string) int {
	var n int
	for _, p := range paths {
		if p == path {
			n++
		}
	}
	return n
}

// --- What the turn did, beside where it left the host -------------------------------

// ADR 0007 asked the report for the outcome and the phases that ran, and ADR 0016 keeps
// both beside the state it added. They answer a different question: the state says where
// the host is, the outcome and the phases say what this turn did to get there.
func TestTheReportSaysWhatTheTurnDidAndWhichPhasesRan(t *testing.T) {
	engine, _, runner, _ := planFixture(t)

	mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")

	report := mustLastTurn(t, engine)
	if report.Outcome != OutcomeApplied {
		t.Errorf("the report says the turn %q, want applied", report.Outcome)
	}
	phases := strings.Join(report.Phases, "\n")
	for _, want := range []string{"firewall", "kernel switches", "networkd", "processes"} {
		if !strings.Contains(phases, want) {
			t.Errorf("the report does not say the %s phase ran: %v", want, report.Phases)
		}
	}

	// A turn that found nothing to do says so, and names no phase: none ran.
	tablePresent(runner)
	setsHold(runner, map[string][]string{"uplink4_pppoe0": {}, "uplink6_pppoe0": {}})
	if _, err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("the reconcile failed: %v", err)
	}
	report = mustLastTurn(t, engine)
	if report.Outcome != OutcomeUnchanged {
		t.Errorf("a turn with nothing to do says it %q, want unchanged", report.Outcome)
	}
	if len(report.Phases) != 0 {
		t.Errorf("a turn that ran no phase names %v", report.Phases)
	}
	if report.State != StateConverged {
		t.Errorf("the state is %q, want converged", report.State)
	}
}

// A turn that stopped part-way says so as its outcome, which is not the same statement as
// the state: the state is about the host against the declaration, the outcome is about
// what this turn managed (ADR 0007, ADR 0016).
func TestTheReportSaysWhenTheTurnWasPutBack(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	runner.fail["systemctl enable --now regied-dnsmasq.service"] = errFake

	plan := mustPlan(t, engine, load(t, hostFixture))
	if _, err := engine.Submit(context.Background(), plan, Declaration{Bytes: declarationOf(hostFixture)}); err == nil {
		t.Fatal("the failing start was not reported")
	}

	report := mustLastTurn(t, engine)
	if report.Outcome != OutcomeStopped {
		t.Errorf("the report says the turn %q, want stopped", report.Outcome)
	}
	if report.State != StateFailing {
		t.Errorf("the state is %q, want failing", report.State)
	}
}

// Validation warnings are about the declaration, so they are in the report of every turn
// that converged toward it — including the ones nobody was watching, which is the only
// place a warning about the record can still be read (ADR 0006, ADR 0016).
func TestTheReportCarriesTheDeclarationsWarnings(t *testing.T) {
	engine, files, _, _ := planFixture(t)
	// Prefix delegation with no DUID file: networkd sends one of its own, and a host
	// replacing one that already holds a delegation is silently given another prefix.
	warns := strings.Replace(hostFixture, "      spec: {ifname: eth0}\n", `      spec:
        ifname: eth0
        dhcpv6:
          prefixDelegation:
            prefixLength: 56
`, 1)
	_ = files

	mustSubmit(t, engine, warns, "/etc/regied/config.yaml")

	report := mustLastTurn(t, engine)
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "duidFile") {
		t.Errorf("the report does not carry what validation warned about: %v", report.Warnings)
	}
}

// A submission that finds nothing to change still records the declaration. That is the
// path a host already running regied takes when it is upgraded to a version that keeps a
// record: its files and its ruleset are all in place, so the turn changes nothing, and if
// that left no record the next boot would find none and do nothing (ADR 0016).
func TestASubmissionRecordsEvenWhenThereIsNothingToChange(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")
	tablePresent(runner)
	setsHold(runner, map[string][]string{"uplink4_pppoe0": {}, "uplink6_pppoe0": {}})

	// The host holds everything, and the record is taken away: this is the host that was
	// applied to by a version of regied that kept none.
	record := "/var/lib/regied/accepted/declaration.yaml"
	delete(files.files, record)
	before := len(runner.ran)

	result := mustSubmit(t, engine, hostFixture, "/etc/regied/config.yaml")

	if result.Changed {
		t.Errorf("the second submission changed something: %s", result.Plan.Summary())
	}
	if got, _ := files.content(record); got != string(declarationOf(hostFixture)) {
		t.Error("a submission with nothing to do did not record the declaration")
	}
	for _, cmd := range commandsSince(runner, before) {
		if cmd != "nft list tables" && cmd != listTableCommand().String() {
			t.Errorf("a submission with nothing to do ran %q", cmd)
		}
	}
	// And the host can now be reconciled, which is the point of recording it.
	if _, err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconciling after the record was written failed: %v", err)
	}
}
