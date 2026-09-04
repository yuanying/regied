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
	// Since is when the host entered this state for this revision.
	Since time.Time `yaml:"since"`
	// Waiting names what the turn waits for, when it is waiting.
	Waiting []string `yaml:"waiting,omitempty"`
	// Failing names what failed and why, when it is failing.
	Failing []string `yaml:"failing,omitempty"`
	// Warnings is what validation and the renderers said about the declaration.
	Warnings []string `yaml:"warnings,omitempty"`
	// Notes is what the host could not answer without that being a failure.
	Notes []string `yaml:"notes,omitempty"`
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
	report := &TurnReport{Revision: revision, Source: source, State: stateOf(plan, failure)}
	if plan != nil {
		report.Waiting = plan.Waiting
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
	case plan != nil && len(plan.Waiting) > 0:
		return StateWaiting
	}
	return StateConverged
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

// Reconcile runs one turn toward the record and stops. It reads no file but the record:
// this is what a boot unit runs, and what an operator types to put a host back where it
// should be without submitting anything (ADR 0016).
//
// A host with no record is left alone and told so (ErrNoRecord). A host whose record this
// version of regied no longer accepts is left alone too, and the report says why
// (RecordError). Either way nothing is changed: a running router keeps running what it
// runs.
func (e *Engine) Reconcile(ctx context.Context) (*Result, error) {
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
