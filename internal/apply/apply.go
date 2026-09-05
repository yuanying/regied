package apply

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/yuanying/regied/internal/config"
)

// Result is what one apply did.
type Result struct {
	Plan *Plan

	// Changed is whether anything on the host moved. An idempotent engine answers no
	// most of the time, and that answer is worth having (ADR 0004).
	Changed bool

	// Notes is what went wrong after the commit stage had already succeeded. The
	// configuration is on the host; something regied does around it is not, and saying
	// so is not the same as saying the apply failed (ADR 0005).
	Notes []string

	// State is where the turn left the host: converged, or waiting for something the
	// plan names. A turn that failed does not produce a Result (ADR 0016).
	State State

	// Revision is the digest of the declaration the turn converged toward, when the turn
	// was a Submit or a Reconcile; ApplyPlan knows no declaration and leaves it empty.
	Revision string
}

// Error is what Apply returns when a turn failed. It says exactly where the host was
// left; a later turn retries from there, and going back is a person submitting an older
// declaration (ADR 0016).
type Error struct {
	Phase Phase
	Step  string
	Cause error

	// Notes is what else went wrong around the failure: the report of the turn that could
	// not be written. It is said after the failure, never instead of it.
	Notes []string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the apply failed in the %s phase, at %q: %v", e.Phase, e.Step, e.Cause)
	b.WriteString("\n  the host remains at the point where the turn stopped")
	for _, note := range e.Notes {
		b.WriteString("\n  also: " + note)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

// Apply puts a configuration on the host.
//
// It is two stages. The staging stage collects what only the host knows, renders,
// computes the plan, writes every file and reclaims what an earlier apply left, and
// makes every check that can be made without changing anything. The commit stage runs
// the commands. Writing a file is not an effect — nothing reads one until it is told to
// — so everything knowable is found out before commands can change kernel state
// (ADR 0004).
func (e *Engine) Apply(ctx context.Context, cfg *config.Config) (*Result, error) {
	plan, err := e.Plan(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return e.ApplyPlan(ctx, plan)
}

// ApplyPlan commits a plan that has already been computed, which is what lets a dry-run
// and an apply be the same code path up to the point where the commands run (ADR 0006).
//
// It is one turn over a plan and nothing more: it records no declaration and writes no
// report. Submit and Reconcile do both around it (ADR 0016).
func (e *Engine) ApplyPlan(ctx context.Context, plan *Plan) (*Result, error) {
	return e.turn(ctx, plan, nil)
}

// acceptingStep is what the staging failure is called when it is the record that could
// not be written.
const acceptingStep = "recording the declaration as accepted"

// turn stages a plan, runs accept if there is one, and commits. accept is the moment a
// submission is accepted: after the staging stage has passed and before the first command
// runs, so that from then on the declaration is the spec whatever the commands do
// (ADR 0016). A declaration that cannot be recorded is not applied either — a host running
// a declaration its record does not hold is exactly the drift the record rules out — and
// nothing has run yet, so refusing costs nothing.
func (e *Engine) turn(ctx context.Context, plan *Plan, accept func() error) (*Result, error) {
	result := &Result{Plan: plan, Changed: !plan.Empty(), State: stateOf(plan, nil)}
	if plan.Empty() {
		if accept != nil {
			if err := accept(); err != nil {
				return nil, &Error{Phase: PhaseStaging, Step: acceptingStep, Cause: err}
			}
		}
		return result, nil
	}

	if err := e.stage(plan); err != nil {
		return nil, e.stagingFailure(plan, "writing what the configuration asks for", err)
	}
	if accept != nil {
		if err := accept(); err != nil {
			return nil, e.stagingFailure(plan, acceptingStep, err)
		}
	}

	if failure := e.commit(ctx, plan); failure != nil {
		return nil, failure
	}

	// Everything the configuration asked for is on the host. What follows is regied's
	// own bookkeeping, and a failure in it is reported rather than rolled back: telling
	// an operator that an apply failed when the host is running the new configuration
	// is worse than telling them what could not be written down (ADR 0005).
	if err := e.record(plan); err != nil {
		result.Notes = append(result.Notes, fmt.Sprintf("%v; the next apply will install the ruleset again", err))
	}
	return result, nil
}

// stagingFailure is a failure before the first command. Files already staged remain;
// nothing has acted on them yet, and a later turn compares them normally (ADR 0016).
func (e *Engine) stagingFailure(_ *Plan, step string, cause error) *Error {
	return &Error{Phase: PhaseStaging, Step: step, Cause: cause}
}

// stage writes every file and takes away what was reclaimed. None of it is an effect
// yet: networkd does not act on a file until it is reloaded, pppd reads its options when
// it starts, and nft never reads a file by itself.
func (e *Engine) stage(plan *Plan) error {
	for _, change := range plan.Files {
		if change.Withheld {
			// Only a rendering produces one of these, and a rendering is never applied.
			continue
		}
		switch {
		case change.Kind == ChangeCreate || change.Kind == ChangeUpdate:
			content, err := plan.contentFor(change)
			if err != nil {
				return err
			}
			if err := e.write(change, content); err != nil {
				return err
			}
		case change.Kind == ChangeRemove && change.Deferred:
			// Taken away by a step, once whatever runs from it has been stopped.
		case change.Kind == ChangeRemove:
			if err := e.host.Files.Remove(change.Path); err != nil {
				return fmt.Errorf("cannot reclaim %s: %w", change.Path, err)
			}
		}
	}
	return nil
}

func (e *Engine) write(change FileChange, content string) error {
	dirMode := change.DirMode
	if dirMode == 0 {
		dirMode = 0o755
	}
	dir := path.Dir(change.Path)
	// The mode belongs to the directory the file is in, not to the ones above it. The
	// credentials directory is 0700 (ADR 0014); making /etc/regied/ppp 0700 as a side
	// effect of creating it would take the peer files out of reach of everything that
	// is not root, which is not what was asked for.
	if err := e.host.Files.MkdirAll(path.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", path.Dir(dir), err)
	}
	if err := e.host.Files.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}
	if err := e.host.Files.WriteFile(change.Path, []byte(content), change.Mode); err != nil {
		return fmt.Errorf("cannot write %s: %w", change.Path, err)
	}
	return nil
}

// commit runs the steps in their safety order and stops at the first failure.
func (e *Engine) commit(ctx context.Context, plan *Plan) *Error {
	for _, step := range plan.Steps {
		if err := e.run(ctx, step); err != nil {
			return &Error{Phase: step.Phase, Step: step.describe(), Cause: err}
		}
	}
	return nil
}

func (e *Engine) run(ctx context.Context, step Step) error {
	switch step.Kind {
	case StepSysctl:
		if err := e.host.Sysctl.Set(step.Switch.Key, step.Switch.Value); err != nil {
			return fmt.Errorf("cannot set %s: %w", step.Switch.Key, err)
		}
		return nil
	case StepRemove:
		if err := e.host.Files.Remove(step.File.Path); err != nil {
			return fmt.Errorf("cannot reclaim %s: %w", step.File.Path, err)
		}
		return nil
	case StepWrite:
		return e.write(step.File, step.File.Content)
	case StepKeep:
		return nil
	}
	// StepCommand and StepSeed, both of which are one command.
	_, err := e.host.Runner.Run(ctx, step.Command)
	return err
}

// record remembers the ruleset that was installed. It is the one thing an apply leaves
// behind, and it is written only on success, so a rolled-back apply does not claim its
// ruleset as the one in effect (ADR 0004).
func (e *Engine) record(plan *Plan) error {
	if !plan.Firewall.Apply {
		return nil
	}
	record := e.opts.rulesetRecord()
	if err := e.host.Files.MkdirAll(path.Dir(record), 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", path.Dir(record), err)
	}
	if err := e.host.Files.WriteFile(record, []byte(plan.Firewall.Ruleset), 0o644); err != nil {
		return fmt.Errorf("cannot record the installed ruleset in %s: %w", record, err)
	}
	return nil
}

// ErrCommandNotFound is what a Runner reports for a command that is not installed. It is
// what lets a dry-run away from the host say the ruleset was not checked, rather than
// implying that it was (ADR 0006).
var ErrCommandNotFound = errors.New("command not found")
