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

	// FirewallReapplied is whether the ruleset had to be rendered and installed a
	// second time because an uplink's address appeared or changed while the apply was
	// running. That is the ordinary case on a cold start: the table written first was
	// rendered before the line had dialled.
	FirewallReapplied bool
}

// Error is what Apply returns when the commit stage failed. It says what failed, and
// what the rollback then managed to do about it, because on a host with one uplink the
// operator is the recovery path and what they need is the truth about where it stopped
// (ADR 0005).
type Error struct {
	Phase    Phase
	Step     string
	Cause    error
	Rollback []string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the apply failed in the %s phase, at %q: %v", e.Phase, e.Step, e.Cause)
	if len(e.Rollback) == 0 {
		b.WriteString("\n  the host was rolled back to the configuration it was running")
		return b.String()
	}
	b.WriteString("\n  the rollback also failed, so the host is running a mixture:")
	for _, problem := range e.Rollback {
		b.WriteString("\n    " + problem)
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
// — so everything knowable is found out while a rollback still costs nothing
// (ADR 0004).
func (e *Engine) Apply(ctx context.Context, cfg *config.Config) (*Result, error) {
	plan, err := e.Plan(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return e.ApplyPlan(ctx, cfg, plan)
}

// ApplyPlan commits a plan that has already been computed, which is what lets a dry-run
// and an apply be the same code path up to the point where the commands run (ADR 0006).
func (e *Engine) ApplyPlan(ctx context.Context, cfg *config.Config, plan *Plan) (*Result, error) {
	result := &Result{Plan: plan, Changed: !plan.Empty()}
	if plan.Empty() {
		return result, nil
	}

	if err := e.stage(plan); err != nil {
		// Nothing has run, so putting the files back is the whole of the rollback.
		e.restoreFiles(plan)
		return nil, err
	}

	if attempted, failure := e.commit(ctx, plan); failure != nil {
		failure.Rollback = e.undo(ctx, plan, attempted)
		return nil, failure
	}

	if err := e.record(plan); err != nil {
		return nil, err
	}

	reapplied, err := e.settleFirewall(ctx, cfg, plan)
	if err != nil {
		return nil, err
	}
	result.FirewallReapplied = reapplied
	return result, nil
}

// stage writes every file and takes away what was reclaimed. None of it is an effect
// yet: networkd does not act on a file until it is reloaded, pppd reads its options when
// it starts, and nft never reads a file by itself.
func (e *Engine) stage(plan *Plan) error {
	for _, change := range plan.Files {
		switch change.Kind {
		case ChangeCreate, ChangeUpdate:
			if err := e.write(change); err != nil {
				return err
			}
		case ChangeRemove:
			if err := e.host.Files.Remove(change.Path); err != nil {
				return fmt.Errorf("cannot reclaim %s: %w", change.Path, err)
			}
		}
	}
	return nil
}

func (e *Engine) write(change FileChange) error {
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
	if err := e.host.Files.WriteFile(change.Path, []byte(change.Content), change.Mode); err != nil {
		return fmt.Errorf("cannot write %s: %w", change.Path, err)
	}
	return nil
}

// commit runs the steps, in order. The first failure stops it, and how many steps had
// been attempted is what the rollback then has to undo.
func (e *Engine) commit(ctx context.Context, plan *Plan) (int, *Error) {
	for i, step := range plan.Steps {
		if err := e.run(ctx, step); err != nil {
			return i + 1, &Error{Phase: step.Phase, Step: step.describe(), Cause: err}
		}
	}
	return len(plan.Steps), nil
}

func (e *Engine) run(ctx context.Context, step Step) error {
	if step.Kind == StepSysctl {
		if err := e.host.Sysctl.Set(step.Switch.Key, step.Switch.Value); err != nil {
			return fmt.Errorf("cannot set %s: %w", step.Switch.Key, err)
		}
		return nil
	}
	_, err := e.host.Runner.Run(ctx, step.Command)
	return err
}

// restoreFiles puts every file back as it was, including the ones the apply reclaimed.
// For a file the previous generation is the file itself, which is why the engine keeps
// almost no state (ADR 0005).
func (e *Engine) restoreFiles(plan *Plan) []string {
	var problems []string
	for _, change := range plan.Files {
		var err error
		switch {
		case change.Kind == ChangeNone:
			continue
		case change.HadBefore:
			err = e.host.Files.WriteFile(change.Path, []byte(change.Before), change.BeforeMode)
		default:
			err = e.host.Files.Remove(change.Path)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s could not be put back: %v", change.Path, err))
		}
	}
	return problems
}

// undo re-runs, over the restored files, the steps that had already been attempted. A
// step that failed is undone too: it may have taken effect before it failed.
//
// It goes forward rather than backward, because the forward order is the safe one: the
// firewall is restored before forwarding is, and the links before the processes. It does
// not stop at the first failure — abandoning the rest would leave more of a mixture than
// finishing does — but it does not retry either, and every failure is reported
// (ADR 0005).
func (e *Engine) undo(ctx context.Context, plan *Plan, attempted int) []string {
	problems := e.restoreFiles(plan)
	for _, step := range plan.Steps[:attempted] {
		if step.Undo == nil {
			problems = append(problems, fmt.Sprintf("%q has no way back and was left as it is", step.describe()))
			continue
		}
		if err := e.run(ctx, *step.Undo); err != nil {
			problems = append(problems, fmt.Sprintf("%q failed: %v", step.Undo.describe(), err))
		}
	}
	return problems
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
		return fmt.Errorf("cannot write %s: %w", record, err)
	}
	return nil
}

// settleFirewall is the last phase: the ruleset is rendered again, now that the
// processes have been started and a session may have dialled, and installed if it came
// out different.
//
// Only the nftables table depends on the address an uplink is holding, so this is the
// whole of what a changed address calls for. Nothing is reloaded and no process is
// touched (ADR 0004).
func (e *Engine) settleFirewall(ctx context.Context, cfg *config.Config, plan *Plan) (bool, error) {
	// Only the addresses are read again. The credentials were used and dropped in the
	// staging stage, and nothing here needs them (ADR 0003).
	addresses, _ := readUplinkAddresses(cfg, e.host)
	ruleset, err := renderRuleset(cfg, addresses)
	if err != nil {
		return false, err
	}
	if ruleset == plan.Firewall.Ruleset {
		return false, nil
	}

	if _, err := e.host.Runner.Run(ctx, Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: ruleset}); err != nil {
		return false, fmt.Errorf("an uplink address appeared while applying, and the ruleset carrying it was refused: %w", err)
	}
	settled := &Plan{Firewall: FirewallChange{Ruleset: ruleset, Before: plan.Firewall.Ruleset, Present: true, Apply: true}}
	return true, e.record(settled)
}

// ErrCommandNotFound is what a Runner reports for a command that is not installed. It is
// what lets a dry-run away from the host say the ruleset was not checked, rather than
// implying that it was (ADR 0006).
var ErrCommandNotFound = errors.New("command not found")
