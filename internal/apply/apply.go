package apply

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"slices"
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
	// running. It is not the ordinary case: a session started in the process phase has
	// not dialled by the time the firewall is settled, so on a cold start the rules that
	// depend on its address wait for the next apply, or for the daemon (ADR 0004).
	FirewallReapplied bool

	// Notes is what went wrong after the commit stage had already succeeded. The
	// configuration is on the host; something regied does around it is not, and saying
	// so is not the same as saying the apply failed (ADR 0005).
	Notes []string
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
		// Nothing has run, so putting the files back is the whole of the rollback. What
		// could not be put back is the first thing an operator needs, so it travels
		// with the failure rather than being dropped (ADR 0005).
		failure := &Error{Phase: PhaseStaging, Step: "writing what the configuration asks for", Cause: err}
		failure.Rollback = e.restoreFiles(plan)
		// systemd was never told about the units this apply created, so they are
		// taken away without telling it they are gone.
		for _, change := range deferredFiles(plan, ChangeCreate) {
			if err := e.host.Files.Remove(change.Path); err != nil {
				failure.Rollback = append(failure.Rollback, fmt.Sprintf("%s could not be taken back off: %v", change.Path, err))
			}
		}
		return nil, failure
	}

	if attempted, failure := e.commit(ctx, plan); failure != nil {
		failure.Rollback = e.undo(ctx, plan, attempted)
		return nil, failure
	}

	// Everything the configuration asked for is on the host. What follows is regied's
	// own bookkeeping, and a failure in it is reported rather than rolled back: telling
	// an operator that an apply failed when the host is running the new configuration
	// is worse than telling them what could not be written down (ADR 0005).
	if err := e.record(plan); err != nil {
		result.Notes = append(result.Notes, fmt.Sprintf("%v; the next apply will install the ruleset again", err))
	}
	e.settleFirewall(ctx, cfg, plan, result)
	return result, nil
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
	_, err := e.host.Runner.Run(ctx, step.Command)
	return err
}

// restoreFiles puts every file back as it was, including the ones the apply reclaimed.
// For a file the previous generation is the file itself, which is why the engine keeps
// almost no state (ADR 0005).
//
// Putting content back is always safe: the file goes on existing, so nothing that
// resolves through it breaks. Taking a file away is not, and for a deferred path — a
// unit — it has to wait until the undo steps that resolve through it have run. That is
// the same rule the forward direction follows, read from the same flag (ADR 0004).
func (e *Engine) restoreFiles(plan *Plan) []string {
	var problems []string
	for _, change := range plan.Files {
		var err error
		before, hadBefore := plan.previousFor(change)
		switch {
		case change.Kind == ChangeNone:
			continue
		case hadBefore:
			err = e.host.Files.WriteFile(change.Path, []byte(before), change.BeforeMode)
		case change.Deferred:
			// This apply created it. It goes once the stops are done.
			continue
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
		if step.Undo.Kind == StepKeep {
			// Deliberately nothing. The operator is told what was left and why.
			problems = append(problems, step.Undo.Reason)
			continue
		}
		if err := e.run(ctx, *step.Undo); err != nil {
			problems = append(problems, fmt.Sprintf("%q failed: %v", step.Undo.describe(), err))
		}
	}
	// The units this apply created go last, once the undo of every start has run
	// through them, and systemd is told — the same steps the forward direction takes
	// for a unit the configuration dropped.
	for _, step := range deferredReclaim(deferredFiles(plan, ChangeCreate), "this apply created it") {
		if err := e.run(ctx, step); err != nil {
			problems = append(problems, fmt.Sprintf("%q failed: %v", step.describe(), err))
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
		return fmt.Errorf("cannot record the installed ruleset in %s: %w", record, err)
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
//
// **It only ever adds.** A link read a few milliseconds after its session was restarted
// answers that it is not there, and rendering that would produce a ruleset without the
// hairpin rules — which installing would take a working port forward away and record the
// result as what is in effect. So an uplink that had an address and no longer answers
// stops the settle, and the ruleset already installed stays. Noticing the redial that
// follows belongs to the daemon.
//
// **It does not wait.** A session started a moment ago has not dialled yet either, and
// the settle reads it the same way: nothing to add. The hairpin rules of a cold start
// wait for the daemon, or for the next apply. Blocking here on a provider's dial time
// was considered and rejected (ADR 0004).
func (e *Engine) settleFirewall(ctx context.Context, cfg *config.Config, plan *Plan, result *Result) {
	addresses, _ := readUplinkAddresses(cfg, e.host)

	if lost := uplinksThatWentQuiet(plan.uplinks, addresses.UplinkAddresses); len(lost) > 0 {
		result.Notes = append(result.Notes, fmt.Sprintf(
			"%s held an address when this apply started and answers none now, so the ruleset was left as it was installed; it carries the address they had",
			strings.Join(lost, ", ")))
		return
	}

	ruleset, err := renderRuleset(cfg, addresses)
	if err != nil {
		result.Notes = append(result.Notes, fmt.Sprintf("the ruleset could not be rendered again after the processes started: %v", err))
		return
	}
	if ruleset == plan.Firewall.Ruleset {
		return
	}

	if _, err := e.host.Runner.Run(ctx, Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: ruleset}); err != nil {
		result.Notes = append(result.Notes, fmt.Sprintf("an uplink address appeared while applying, and the ruleset carrying it was refused: %v", err))
		return
	}
	result.FirewallReapplied = true

	settled := &Plan{Firewall: FirewallChange{Ruleset: ruleset, Before: plan.Firewall.Ruleset, Table: TablePresent, Apply: true}}
	if err := e.record(settled); err != nil {
		result.Notes = append(result.Notes, err.Error())
	}
}

// uplinksThatWentQuiet names the uplinks that held an address when the plan was computed
// and hold none now. Which addresses they hold does not matter: a session that came back
// on a different one is exactly what the settle is for.
func uplinksThatWentQuiet(before, after map[string][]netip.Addr) []string {
	var lost []string
	for name, addresses := range before {
		if len(addresses) == 0 {
			continue
		}
		if len(after[name]) == 0 {
			lost = append(lost, name)
		}
	}
	slices.Sort(lost)
	return lost
}

// ErrCommandNotFound is what a Runner reports for a command that is not installed. It is
// what lets a dry-run away from the host say the ruleset was not checked, rather than
// implying that it was (ADR 0006).
var ErrCommandNotFound = errors.New("command not found")
