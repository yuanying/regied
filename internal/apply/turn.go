package apply

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"

	"github.com/yuanying/regied/internal/config"
)

// A turn is what an apply already does, minus reading a file: collect what only the host
// knows, render the accepted declaration, compare, and run what the difference asks for
// in the order ADR 0004 fixes. This file is what a turn reads and leaves behind under
// regied's state directory — the record it converges toward, the report of where it left
// the host, and the lock it holds while it runs — and the two entry points that run one:
// Submit, for a declaration read from a file, and Reconcile, for the record (ADR 0016).

// State is where a turn left the host. Converged and waiting are the two answers a turn
// that ran to the end can give; a turn that failed returns an Error, and its state is
// failing (ADR 0016).
type State string

const (
	// StateConverged says the host holds the whole declaration: nothing was left out and
	// nothing is being retried.
	StateConverged State = "converged"
	// StateWaiting says the turn did everything it could and left something out for want
	// of a value the host could not answer yet. The report names what it waits for.
	StateWaiting State = "waiting"
	// StateFailing says something was tried and did not work, or differs and this turn was
	// not allowed to fix it. The report names what and why.
	StateFailing State = "failing"
)

// Outcome is what a turn did to the host. It is not the same statement as the state: the
// state is about the host against the declaration, the outcome is about what this one turn
// managed. A host can be failing after a turn that changed nothing, and converged after a
// turn that changed everything (ADR 0007, ADR 0016).
type Outcome string

const (
	// OutcomeApplied says the turn moved something on the host.
	OutcomeApplied Outcome = "applied"
	// OutcomeUnchanged says it found nothing to change and ran no command. On an
	// idempotent engine this is the ordinary answer (ADR 0004).
	OutcomeUnchanged Outcome = "unchanged"
	// OutcomeStopped says a turn failed after it had begun changing the host and left it
	// at that safe prefix of the phase order.
	OutcomeStopped Outcome = "stopped"
	// OutcomeNothingRun says the turn stopped before its first command: the declaration
	// could not be staged, or something it had to read could not be read. The host was
	// never touched.
	OutcomeNothingRun Outcome = "nothing run"
)

// Declaration is a declaration as it is submitted: the bytes that were validated, exactly
// as they were read, and where they were read from. The bytes are what the record holds;
// the source is provenance, and the digest of the bytes is identity (ADR 0007).
type Declaration struct {
	Bytes  []byte
	Source string
}

// Revision is the digest of a declaration's bytes. It is comparable off the host: anyone
// with a copy of the file can compute it, without regied and without ssh (ADR 0007).
func Revision(declaration []byte) string {
	sum := sha256.Sum256(declaration)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Where a turn's state lives, under the state directory. The accepted declaration is the
// spec; the report is diagnosis; the lock is held by whichever turn is running.
func (o Options) acceptedDeclaration() string { return o.StateDir + "/accepted/declaration.yaml" }
func (o Options) turnReport() string          { return o.StateDir + "/turn/report.yaml" }
func (o Options) turnLock() string            { return o.StateDir + "/turn/lock" }

// RecordPath is where the accepted declaration is kept on this host. It is the one thing
// that has to survive a reboot: everything else regied writes is derived from it or is
// diagnosis (ADR 0016).
func (e *Engine) RecordPath() string { return e.opts.acceptedDeclaration() }

// ErrNoRecord is what a turn over the record reports on a host that has never accepted a
// declaration. Such a host is left alone: converging on nothing would take the firewall
// off a running router over a missing file (ADR 0016).
var ErrNoRecord = errors.New("no declaration has been accepted on this host")

// ErrNoReport is what LastTurn reports on a host no turn has reported on yet.
var ErrNoReport = errors.New("no turn has been reported on this host")

// RecordError is what a turn over the record reports when the record no longer parses or
// validates: regied was updated and accepts less than the version that recorded it. The
// host is left as it is, and the operator is told to submit a declaration this version
// accepts (ADR 0016).
type RecordError struct {
	Path     string
	Revision string
	Cause    error
}

func (e *RecordError) Error() string {
	return fmt.Sprintf("the accepted declaration in %s is not accepted by this version of regied, so nothing was changed; submit one it accepts with `regied apply`\n  %v",
		e.Path, strings.ReplaceAll(e.Cause.Error(), "\n", "\n  "))
}

func (e *RecordError) Unwrap() error { return e.Cause }

// Record is the accepted declaration as read back from the state directory.
type Record struct {
	Declaration []byte
	Revision    string
	Config      *config.Config
}

// LoadRecord reads the accepted declaration and validates it, so that a turn can render it.
//
// The files the declaration names — credentials, the DUID — are not checked here. The
// turn reads them: a credential that cannot be read fails the turn before the first
// command, and a DUID that cannot be read is waited for (ADR 0016). Checking them here
// would make a file that vanished look like a record this version of regied refuses.
func (e *Engine) LoadRecord() (*Record, error) {
	location := e.opts.acceptedDeclaration()
	data, _, err := e.host.Files.ReadFile(location)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: there is nothing at %s", ErrNoRecord, location)
	case err != nil:
		return nil, fmt.Errorf("cannot read the accepted declaration %s: %w", location, err)
	}
	record := &Record{Declaration: data, Revision: Revision(data)}

	document, err := config.Parse(data)
	if err != nil {
		return nil, &RecordError{Path: location, Revision: record.Revision, Cause: err}
	}
	record.Config, err = config.Validate(document, config.WithSecretFiles(namedFiles{}))
	if err != nil {
		return nil, &RecordError{Path: location, Revision: record.Revision, Cause: err}
	}
	return record, nil
}

// namedFiles answers validation's question about the files a declaration names without
// looking at any. The turn reads them for real.
type namedFiles struct{}

func (namedFiles) CheckSecretFile(string) error { return nil }

// accept writes a declaration down as the one this host converges toward. From this moment
// it is the spec, whatever the commands that follow do (ADR 0016).
//
// Writing it down is safe because ADR 0003 made it so: the declaration names the files
// that hold credentials and has no field that could hold one, so no credential can be in
// these bytes. A declaration already recorded is not rewritten (ADR 0004).
func (e *Engine) accept(declaration []byte) error {
	location := e.opts.acceptedDeclaration()
	current, _, err := e.host.Files.ReadFile(location)
	if err == nil && bytes.Equal(current, declaration) {
		return nil
	}
	if err := e.host.Files.MkdirAll(path.Dir(location), 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", path.Dir(location), err)
	}
	if err := e.host.Files.WriteFile(location, declaration, 0o644); err != nil {
		return fmt.Errorf("cannot record the declaration as accepted in %s: %w", location, err)
	}
	return nil
}

// TurnReport is where the last turn left the host, as written under the state directory.
// It is diagnosis and not a second spec: nothing converges toward it (ADR 0016).
//
// It carries the time the state was entered, not the time it was last confirmed. A report
// carrying the time of the last turn would be rewritten every minute for the life of the
// host to say nothing new; when the last turn ran is what `systemctl status` answers.
type TurnReport struct {
	// Revision is the digest of the declaration the turn converged toward.
	Revision string `yaml:"revision"`
	// Source is the file the declaration was submitted from. A turn over the record does
	// not know it and carries it forward from the previous report of the same revision.
	Source string `yaml:"source,omitempty"`
	State  State  `yaml:"state"`
	// Outcome is what this turn did to the host, which is a different question from the
	// state beside it (ADR 0007).
	Outcome Outcome `yaml:"outcome"`
	// Phases is the phases that ran and what each of them changed. A turn that changed
	// nothing names none (ADR 0004's last consequence, ADR 0007).
	Phases []string `yaml:"phases,omitempty"`
	// Since is when the host entered this state for this revision.
	Since time.Time `yaml:"since"`
	// Waiting names what the turn waits for, when it is waiting.
	Waiting []string `yaml:"waiting,omitempty"`
	// Failing names what failed and why, when it is failing.
	Failing []string `yaml:"failing,omitempty"`
	// Warnings is what validation and the renderers said about the declaration.
	Warnings []string `yaml:"warnings,omitempty"`
	// Notes is what the host could not answer without that being a failure.
	Notes         []string  `yaml:"notes,omitempty"`
	Trial         bool      `yaml:"trial,omitempty"`
	TrialDeadline time.Time `yaml:"trialDeadline,omitempty"`
}

func (e *Engine) ReportTrial(revision string, deadline time.Time) (*TurnReport, error) {
	report, _, err := e.readTurnReport()
	if err != nil {
		return nil, err
	}
	report.Trial = true
	report.TrialDeadline = deadline.UTC()
	data, err := yaml.Marshal(report)
	if err != nil {
		return nil, err
	}
	if err := e.host.Files.WriteFile(e.opts.turnReport(), data, 0o644); err != nil {
		return nil, err
	}
	return report, nil
}

func (e *Engine) ClearTrialReport() error {
	report, _, err := e.readTurnReport()
	if err != nil {
		return err
	}
	report.Trial = false
	report.TrialDeadline = time.Time{}
	data, err := yaml.Marshal(report)
	if err != nil {
		return err
	}
	return e.host.Files.WriteFile(e.opts.turnReport(), data, 0o644)
}

// LastTurn reads the report of the last turn.
func (e *Engine) LastTurn() (*TurnReport, error) {
	report, _, err := e.readTurnReport()
	return report, err
}

func (e *Engine) readTurnReport() (*TurnReport, []byte, error) {
	location := e.opts.turnReport()
	data, _, err := e.host.Files.ReadFile(location)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil, fmt.Errorf("%w: there is nothing at %s", ErrNoReport, location)
	case err != nil:
		return nil, nil, fmt.Errorf("cannot read the report of the last turn %s: %w", location, err)
	}
	var report TurnReport
	if err := yaml.Unmarshal(data, &report); err != nil {
		return nil, data, fmt.Errorf("cannot read the report of the last turn %s: %w", location, err)
	}
	return &report, data, nil
}

// ReportTurn writes down where a turn left the host, and returns what it wrote.
//
// The plan is what the turn worked from, and may be nil when the turn failed before it had
// one. The failure is nil for a turn that ran to the end. The report is written only when
// what it says changed, the rule ADR 0004 gives every file regied writes, and the time it
// carries is the time the state was entered (ADR 0016).
func (e *Engine) ReportTurn(revision, source string, plan *Plan, failure error) (*TurnReport, error) {
	report := &TurnReport{
		Revision: revision,
		Source:   source,
		State:    stateOf(plan, failure),
		Outcome:  outcomeOf(plan, failure),
		Phases:   phasesOf(plan, failure),
	}
	if plan != nil {
		report.Waiting = plan.Waiting
		report.Failing = append(report.Failing, plan.Failing...)
		report.Warnings = append(append([]string(nil), plan.validation...), plan.Warnings...)
		report.Notes = plan.Notes
	}
	if failure != nil {
		report.Failing = linesOf(failure)
	}

	previous, previousBytes, _ := e.readTurnReport()
	if previous != nil && previous.Revision == revision {
		if report.Source == "" {
			report.Source = previous.Source
		}
		if previous.State == report.State {
			report.Since = previous.Since.UTC()
		}
	}
	if report.Since.IsZero() {
		report.Since = e.host.Clock.Now().UTC()
	}

	data, err := yaml.Marshal(report)
	if err != nil {
		return report, fmt.Errorf("cannot write the report of this turn: %w", err)
	}
	if bytes.Equal(data, previousBytes) {
		return report, nil
	}
	location := e.opts.turnReport()
	if err := e.host.Files.MkdirAll(path.Dir(location), 0o755); err != nil {
		return report, fmt.Errorf("cannot create %s: %w", path.Dir(location), err)
	}
	if err := e.host.Files.WriteFile(location, data, 0o644); err != nil {
		return report, fmt.Errorf("cannot write the report of this turn to %s: %w", location, err)
	}
	return report, nil
}

// stateOf is the one place the three states are decided: a failure is failing, a plan
// that left something out is waiting, and anything else has converged (ADR 0016).
func stateOf(plan *Plan, failure error) State {
	switch {
	case failure != nil:
		return StateFailing
	case plan != nil && len(plan.Failing) > 0:
		return StateFailing
	case plan != nil && len(plan.Waiting) > 0:
		return StateWaiting
	}
	return StateConverged
}

// outcomeOf is what the turn did to the host.
//
// A failure before the first command ran no command; a later failure leaves the host at
// the safe prefix of the phase order it reached (ADR 0016).
func outcomeOf(plan *Plan, failure error) Outcome {
	switch {
	case failure != nil && stoppedInStaging(failure):
		return OutcomeNothingRun
	case failure != nil:
		return OutcomeStopped
	case plan == nil || plan.Empty():
		return OutcomeUnchanged
	}
	return OutcomeApplied
}

// phasesOf is the phases this turn ran and what each of them changed.
//
// It never names a phase the turn did not reach, or a step it did not attempt: a report
// that did would be a claim with nothing behind it, and the whole point of the report is
// to be read by somebody who was not watching. So the steps are cut at the one that
// failed, and a turn that was put back reports no files — they went back with it.
func phasesOf(plan *Plan, failure error) []string {
	if plan == nil {
		return nil
	}
	var out []string
	if failure == nil {
		if line := stagedLine(plan); line != "" {
			out = append(out, line)
		}
	}

	var applyErr *Error
	stopped := errors.As(failure, &applyErr)
	if stopped && applyErr.Phase == PhaseStaging {
		return out
	}

	byPhase := make(map[Phase][]string)
	var order []Phase
	for _, step := range plan.Steps {
		if _, seen := byPhase[step.Phase]; !seen {
			order = append(order, step.Phase)
		}
		byPhase[step.Phase] = append(byPhase[step.Phase], step.describe())
		if stopped && step.Phase == applyErr.Phase && step.describe() == applyErr.Step {
			break
		}
	}
	for _, phase := range order {
		out = append(out, fmt.Sprintf("%s: %s", phase, strings.Join(byPhase[phase], "; ")))
	}
	return out
}

// stagedLine is what the staging stage put on the host, as one line.
func stagedLine(plan *Plan) string {
	var written, reclaimed int
	for _, change := range plan.Files {
		switch {
		case wasWritten(change):
			written++
		case change.Kind == ChangeRemove:
			reclaimed++
		}
	}
	switch {
	case written == 0 && reclaimed == 0:
		return ""
	case reclaimed == 0:
		return fmt.Sprintf("%s: wrote %d files", PhaseStaging, written)
	case written == 0:
		return fmt.Sprintf("%s: reclaimed %d files", PhaseStaging, reclaimed)
	}
	return fmt.Sprintf("%s: wrote %d files, reclaimed %d", PhaseStaging, written, reclaimed)
}

// stoppedInStaging is whether a failure happened before the first command ran, which is
// the one class of failure that leaves the host untouched (ADR 0004).
func stoppedInStaging(failure error) bool {
	var applyErr *Error
	if errors.As(failure, &applyErr) {
		return applyErr.Phase == PhaseStaging
	}
	// Anything that is not an *Error came from before the plan existed: a declaration
	// that would not validate, a value the host could not answer. Nothing has run.
	return true
}

// linesOf is a failure as the lines of a report: what every error in this package already
// says, one item per line, without the indentation the console gets.
func linesOf(failure error) []string {
	var lines []string
	for _, line := range strings.Split(failure.Error(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// Submit is a submission: it puts a plan on the host the way ApplyPlan does, and between
// staging and the first command it writes the declaration down as accepted. From then on
// the declaration is the spec, whether or not the commands succeed, and the report says
// where the turn left the host (ADR 0016).
//
// The declaration is the bytes the plan was computed from; the caller has them because
// it read the file, and this is the only place in regied that a file's declaration
// reaches the record.
func (e *Engine) Submit(ctx context.Context, plan *Plan, declaration Declaration) (*Result, error) {
	e.retries = make(map[string]retryState)
	e.retryRevision = Revision(declaration.Bytes)
	revision := Revision(declaration.Bytes)
	var accepted bool
	result, err := e.turn(ctx, plan, func() error {
		if err := e.accept(declaration.Bytes); err != nil {
			return err
		}
		accepted = true
		return nil
	})
	if !accepted {
		// Staging failed, or the record could not be written. Nothing was accepted, the
		// record is what it was, and the report is about the record, not about this.
		return nil, err
	}
	_, reportErr := e.ReportTurn(revision, declaration.Source, plan, err)
	if err != nil {
		return nil, withNote(err, reportErr)
	}
	result.Revision = revision
	if reportErr != nil {
		result.Notes = append(result.Notes, reportErr.Error())
	}
	return result, nil
}

// SubmitTrial runs a submitted turn without replacing the accepted declaration.
func (e *Engine) SubmitTrial(ctx context.Context, declaration Declaration) (*Result, error) {
	return e.submitDeclaration(ctx, declaration, false)
}

// SubmitDeclaration validates and stages bytes received by the resident process before
// recording them and running the submitted turn.
func (e *Engine) SubmitDeclaration(ctx context.Context, declaration Declaration) (*Result, error) {
	return e.submitDeclaration(ctx, declaration, true)
}

// submitDeclaration is the daemon's one admission and turn path. The wire verb decides
// explicitly whether admission records the declaration; a missing trial deadline can
// therefore never turn a trial into a permanent submission.
func (e *Engine) submitDeclaration(ctx context.Context, declaration Declaration, durable bool) (*Result, error) {
	cfg, err := validateDeclaration(declaration)
	if err != nil {
		return nil, err
	}
	plan, err := e.Plan(ctx, cfg)
	if err != nil {
		if durable {
			return nil, err
		}
		_, reportErr := e.ReportTurn(Revision(declaration.Bytes), declaration.Source, nil, err)
		return nil, withNote(err, reportErr)
	}
	if durable {
		return e.Submit(ctx, plan, declaration)
	}
	revision := Revision(declaration.Bytes)
	result, err := e.ApplyPlan(ctx, plan)
	_, reportErr := e.ReportTurn(revision, declaration.Source, plan, err)
	if err != nil {
		return nil, withNote(err, reportErr)
	}
	result.Revision = revision
	if reportErr != nil {
		result.Notes = append(result.Notes, reportErr.Error())
	}
	return result, nil
}

func validateDeclaration(declaration Declaration) (*config.Config, error) {
	document, err := config.Parse(declaration.Bytes)
	if err != nil {
		var parseErr *config.ParseError
		if errors.As(err, &parseErr) {
			parseErr.Path = declaration.Source
		}
		return nil, err
	}
	cfg, err := config.Validate(document, config.WithSecretFiles(namedFiles{}))
	if err != nil {
		var invalid *config.ValidationError
		if errors.As(err, &invalid) {
			invalid.Path = declaration.Source
		}
		return nil, err
	}
	return cfg, nil
}

// ReconcileTrial runs a turn toward an in-memory trial instead of the durable record.
func (e *Engine) ReconcileTrial(ctx context.Context, declaration Declaration, unattended bool) (*Result, error) {
	document, err := config.Parse(declaration.Bytes)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Validate(document, config.WithSecretFiles(namedFiles{}))
	if err != nil {
		return nil, err
	}
	plan, err := e.Plan(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if unattended {
		plan = e.unattendedPlan(plan)
	}
	result, err := e.ApplyPlan(ctx, plan)
	_, reportErr := e.ReportTurn(Revision(declaration.Bytes), declaration.Source, plan, err)
	if err != nil {
		return nil, withNote(err, reportErr)
	}
	result.Revision = Revision(declaration.Bytes)
	return result, reportErr
}

// AcceptTrial makes an already validated in-memory trial durable without requiring it
// to have converged. Confirmation tests reachability, not convergence (ADR 0016).
func (e *Engine) AcceptTrial(declaration Declaration) error { return e.accept(declaration.Bytes) }

// Reconcile runs one turn toward the record and stops. It reads no file but the record:
// this is what a boot unit runs, and what an operator types to put a host back where it
// should be without submitting anything (ADR 0016).
//
// A host with no record is left alone and told so (ErrNoRecord). A host whose record this
// version of regied no longer accepts is left alone too, and the report says why
// (RecordError). Either way nothing is changed: a running router keeps running what it
// runs.
func (e *Engine) Reconcile(ctx context.Context) (*Result, error) {
	return e.reconcile(ctx, false)
}

// ReconcileUnattended runs a resync- or netlink-triggered turn. It performs all cheap
// observation and comparison, but never restarts or stops a running process and never
// reclaims a unit. Differences across that line remain visible as failing drift.
func (e *Engine) ReconcileUnattended(ctx context.Context) (*Result, error) {
	return e.reconcile(ctx, true)
}

func (e *Engine) reconcile(ctx context.Context, unattended bool) (*Result, error) {
	record, err := e.LoadRecord()
	if err != nil {
		var invalid *RecordError
		if errors.As(err, &invalid) {
			_, reportErr := e.ReportTurn(invalid.Revision, "", nil, err)
			err = withNote(err, reportErr)
		}
		return nil, err
	}

	plan, err := e.Plan(ctx, record.Config)
	if err != nil {
		_, reportErr := e.ReportTurn(record.Revision, "", nil, err)
		return nil, withNote(err, reportErr)
	}
	if unattended {
		if e.retryRevision != record.Revision {
			e.retries = make(map[string]retryState)
			e.retryRevision = record.Revision
		}
		plan = e.unattendedPlan(plan)
	}
	result, err := e.ApplyPlan(ctx, plan)
	_, reportErr := e.ReportTurn(record.Revision, "", plan, err)
	if err != nil {
		return nil, withNote(err, reportErr)
	}
	result.Revision = record.Revision
	if reportErr != nil {
		result.Notes = append(result.Notes, reportErr.Error())
	}
	return result, nil
}

func (e *Engine) unattendedPlan(original *Plan) *Plan {
	plan := *original
	plan.Files = append([]FileChange(nil), original.Files...)
	plan.Steps = nil

	for i := range plan.Files {
		if plan.Files[i].Kind == ChangeRemove {
			plan.Files[i].Deferred = true
		}
	}
	present := make(map[string]bool)
	for _, step := range original.Steps {
		if unattendedForbidden(step) {
			plan.Failing = append(plan.Failing, step.Reason+": an unattended turn does not take down something that is up")
			continue
		}
		key := step.describe()
		present[key] = true
		if retry, ok := e.retries[key]; ok && e.host.Clock.Now().Before(retry.next) {
			plan.Failing = append(plan.Failing, fmt.Sprintf("%s: retry backoff level %d until %s", key, retry.attempts, retry.next.UTC().Format(time.RFC3339)))
			continue
		}
		retry := e.retries[key]
		retry.attempts++
		delay := time.Second << min(retry.attempts-1, 8)
		if delay > 5*time.Minute {
			delay = 5 * time.Minute
		}
		retry.next = e.host.Clock.Now().Add(delay)
		e.retries[key] = retry
		plan.Steps = append(plan.Steps, step)
	}
	for key := range e.retries {
		if !present[key] {
			delete(e.retries, key)
		}
	}
	return &plan
}

func unattendedForbidden(step Step) bool {
	if step.Kind == StepRemove {
		return true
	}
	if step.Command.Name != "systemctl" {
		return false
	}
	for _, arg := range step.Command.Args {
		if arg == "restart" || arg == "disable" || arg == "--now" && len(step.Command.Args) > 0 && step.Command.Args[0] == "disable" {
			return true
		}
	}
	return false
}

// withNote attaches what went wrong around a failure — the report that could not be
// written — without losing the failure itself.
func withNote(failure, note error) error {
	if note == nil {
		return failure
	}
	var applyErr *Error
	if errors.As(failure, &applyErr) {
		applyErr.Notes = append(applyErr.Notes, note.Error())
		return failure
	}
	return fmt.Errorf("%w\n  also: %v", failure, note)
}

// LockTurn takes the lock a turn holds for as long as it runs, and returns what releases
// it. A submission runs a turn in its own process while the resident process may be about
// to run one of its own; whichever comes second waits, and a tick that arrives while a
// turn is running is delayed rather than run beside it (ADR 0016).
func (e *Engine) LockTurn(ctx context.Context) (release func() error, err error) {
	release, err = e.host.Locker.Lock(ctx, e.opts.turnLock())
	if err != nil {
		return nil, fmt.Errorf("cannot take the turn lock %s: %w", e.opts.turnLock(), err)
	}
	return release, nil
}
