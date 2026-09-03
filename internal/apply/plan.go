package apply

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/render/networkd"
	"github.com/yuanying/regied/internal/render/nftables"
)

// Options is where on the host the engine puts what it owns. The defaults are the paths
// the renderers name, and a test overrides them to keep out of the way of a real host.
type Options struct {
	NetworkdDir string
	Root        string
	StateDir    string
	UnitDir     string
}

// DefaultUnitDir is where regied puts the systemd units that supervise the processes it
// configures. They carry regied's name and its ownership marker; nothing else in this
// directory is touched (ADR 0009).
const DefaultUnitDir = "/etc/systemd/system"

func (o Options) withDefaults() Options {
	if o.NetworkdDir == "" {
		o.NetworkdDir = networkd.Dir
	}
	if o.Root == "" {
		o.Root = defaultRoot
	}
	if o.StateDir == "" {
		o.StateDir = defaultStateDir
	}
	if o.UnitDir == "" {
		o.UnitDir = DefaultUnitDir
	}
	return o
}

// rulesetRecord is where the engine remembers the ruleset it installed. It is the only
// state an apply leaves behind, because for a file the previous generation is the file
// itself, while a ruleset is kernel state and has none (ADR 0004).
func (o Options) rulesetRecord() string { return o.StateDir + "/applied/ruleset.nft" }

// Engine puts a rendered configuration on one host.
type Engine struct {
	host Host
	opts Options
}

func New(host Host, opts Options) *Engine {
	return &Engine{host: host, opts: opts.withDefaults()}
}

// Phase is one step of the order an apply goes in (ADR 0004). The firewall is first
// because nothing should be able to move a packet before the rules that filter it exist,
// and the processes are last because restarting a session is the one thing a rollback
// cannot undo.
type Phase int

const (
	PhaseFirewall Phase = iota
	PhaseKernel
	PhaseNetworkd
	PhaseProcessConfig
	PhaseProcesses
)

func (p Phase) String() string {
	switch p {
	case PhaseFirewall:
		return "firewall"
	case PhaseKernel:
		return "kernel switches"
	case PhaseNetworkd:
		return "networkd"
	case PhaseProcessConfig:
		return "process configuration"
	case PhaseProcesses:
		return "processes"
	}
	return "unknown"
}

// ChangeKind is what would happen to one file.
type ChangeKind string

const (
	ChangeNone   ChangeKind = "unchanged"
	ChangeCreate ChangeKind = "create"
	ChangeUpdate ChangeKind = "update"
	ChangeRemove ChangeKind = "reclaim"
)

// FileChange is one file, as it would be and as it is.
//
// Before is what the host holds now, and it is kept because it is what a rollback puts
// back: for a file, the previous generation is the file itself (ADR 0005).
type FileChange struct {
	Path    string
	Kind    ChangeKind
	Mode    fs.FileMode
	DirMode fs.FileMode
	Content string

	// Secret says the content holds a credential. Such a file is written, but never
	// printed and never diffed (ADR 0003).
	Secret bool

	// Withheld says the content is not known, only that the file would be written and
	// with what mode. Nothing writes such a file; only `regied render` produces one.
	Withheld bool

	Before     string
	HadBefore  bool
	BeforeMode fs.FileMode
}

// SwitchChange is one kernel switch.
type SwitchChange struct {
	Key       string
	Value     string
	Before    string
	HadBefore bool
	Changed   bool
}

// FirewallChange is the one table regied owns.
type FirewallChange struct {
	// Ruleset is the text to hand to nft.
	Ruleset string
	// Before is the text the last apply recorded having installed.
	Before string
	// Present is whether the table is in the kernel. After a reboot it is not, and the
	// ruleset has to go in again even though nothing about it changed (ADR 0004).
	Present bool
	// Apply is whether the ruleset has to be installed.
	Apply bool
}

// StepKind tells the two things a step can be apart. Both are effects and both belong to
// the commit stage; they differ in that one runs a command and one writes /proc/sys.
type StepKind string

const (
	StepCommand StepKind = "command"
	StepSysctl  StepKind = "sysctl"
)

// Step is one thing the commit stage does.
type Step struct {
	Phase  Phase
	Kind   StepKind
	Reason string

	Command Command
	Switch  SwitchChange

	// Undo puts back what this step changed. Every step has one, because a step without
	// a way back is a step a later failure could not roll back (ADR 0005).
	Undo *Step
}

func (s Step) describe() string {
	if s.Kind == StepSysctl {
		return fmt.Sprintf("%s = %s", s.Switch.Key, s.Switch.Value)
	}
	return s.Command.String()
}

// Plan is everything one apply would do: what it would write, what it would reclaim,
// what it would set, and what it would run.
//
// It carries no credential. The content of a secret file is not in it, which is what
// makes printing a plan safe by construction rather than by remembering to be careful
// (ADR 0006).
type Plan struct {
	// Warnings is what the renderers said about declarations they could not render as
	// written. They are shown before anything else.
	Warnings []string

	// Notes is what the host could not answer without that being a failure: a link that
	// is not up, a check that could not be made.
	Notes []string

	Files    []FileChange
	Switches []SwitchChange
	Firewall FirewallChange
	Steps    []Step

	// Rendered says this is a rendering rather than a plan against a host: nothing was
	// read, so every file is shown as it would be written and nothing is compared.
	Rendered bool
}

// Empty is whether applying this plan would change nothing at all. An idempotent engine
// answers this most of the time, and a dry-run has to say so unmistakably (ADR 0006).
func (p *Plan) Empty() bool {
	if p.Firewall.Apply || len(p.Steps) > 0 {
		return false
	}
	for _, file := range p.Files {
		if file.Kind != ChangeNone {
			return false
		}
	}
	for _, change := range p.Switches {
		if change.Changed {
			return false
		}
	}
	return true
}

// Summary is one line per thing that would change, for an error message and a log. The
// full account is what the dry-run printer produces.
func (p *Plan) Summary() string {
	var lines []string
	for _, file := range p.Files {
		if file.Kind != ChangeNone {
			lines = append(lines, fmt.Sprintf("%s %s", file.Kind, file.Path))
		}
	}
	for _, change := range p.Switches {
		if change.Changed {
			lines = append(lines, fmt.Sprintf("set %s = %s", change.Key, change.Value))
		}
	}
	for _, step := range p.Steps {
		if step.Kind == StepCommand {
			lines = append(lines, "run "+step.Command.String())
		}
	}
	if len(lines) == 0 {
		return "nothing to do"
	}
	return strings.Join(lines, "\n")
}

// Render is the whole rendering of a configuration, as it would be written to an empty
// host. It reads nothing and runs nothing, and the values that exist only at apply time
// are whatever the caller supplies — which may be none of them.
//
// It is what answers "what does this configuration mean", for a host other than this one
// and for a host that does not exist yet (ADR 0006).
func (e *Engine) Render(cfg *config.Config, runtime *Runtime) (*Plan, error) {
	rendered, err := e.render(cfg, runtime)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Warnings: rendered.warnings, Notes: runtime.Notes, Rendered: true}
	for _, item := range rendered.artifacts {
		plan.Files = append(plan.Files, FileChange{
			Path:     item.Path,
			Kind:     ChangeCreate,
			Mode:     item.Mode,
			DirMode:  item.DirMode,
			Content:  item.Content,
			Secret:   item.Secret,
			Withheld: item.Withheld,
		})
	}
	slices.SortFunc(plan.Files, func(a, b FileChange) int { return cmp.Compare(a.Path, b.Path) })
	plan.Firewall = FirewallChange{Ruleset: rendered.ruleset, Apply: true}
	return plan, nil
}

// Plan reads the host, renders the configuration, and works out what would have to
// change. It writes nothing and runs nothing that changes anything: the two commands it
// may run — the probe that asks whether the table is in the kernel, and the check that
// asks nft whether it would accept the ruleset — are both read-only (ADR 0004).
func (e *Engine) Plan(ctx context.Context, cfg *config.Config) (*Plan, error) {
	runtime, err := CollectRuntime(ctx, cfg, e.host)
	if err != nil {
		return nil, err
	}
	return e.planWith(ctx, cfg, runtime)
}

func (e *Engine) planWith(ctx context.Context, cfg *config.Config, runtime *Runtime) (*Plan, error) {
	rendered, err := e.render(cfg, runtime)
	if err != nil {
		return nil, err
	}

	plan := &Plan{Warnings: rendered.warnings, Notes: runtime.Notes}

	desired := make(map[string]bool, len(rendered.artifacts))
	for _, item := range rendered.artifacts {
		desired[item.Path] = true
		change, err := e.compare(item)
		if err != nil {
			return nil, err
		}
		plan.Files = append(plan.Files, change)
	}

	reclaimed, err := e.reclaim(desired)
	if err != nil {
		return nil, err
	}
	plan.Files = append(plan.Files, reclaimed...)
	slices.SortFunc(plan.Files, func(a, b FileChange) int { return cmp.Compare(a.Path, b.Path) })

	plan.Switches, err = e.switches(cfg)
	if err != nil {
		return nil, err
	}

	plan.Firewall, err = e.firewall(ctx, rendered.ruleset)
	if err != nil {
		return nil, err
	}
	if plan.Firewall.Apply {
		if err := e.checkRuleset(ctx, plan); err != nil {
			return nil, err
		}
	}

	plan.Steps = e.steps(plan, rendered)
	return plan, nil
}

// compare reads what the host holds for one artifact.
func (e *Engine) compare(item artifact) (FileChange, error) {
	change := FileChange{
		Path:     item.Path,
		Mode:     item.Mode,
		DirMode:  item.DirMode,
		Content:  item.Content,
		Secret:   item.Secret,
		Withheld: item.Withheld,
	}
	before, mode, err := e.host.Files.ReadFile(item.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		change.Kind = ChangeCreate
		return change, nil
	case err != nil:
		return change, fmt.Errorf("cannot read %s: %w", item.Path, err)
	}
	change.Before, change.HadBefore, change.BeforeMode = string(before), true, mode
	// A file already holding what it should is left alone, timestamp included: a
	// timestamp is what several things on a host watch (ADR 0004).
	if change.Before == item.Content && mode == item.Mode {
		change.Kind = ChangeNone
	} else {
		change.Kind = ChangeUpdate
	}
	return change, nil
}

// reclaim finds what an earlier apply left behind and the configuration no longer asks
// for. What is reclaimed is only what carries regied's mark, so a file somebody else put
// in the same directory is left alone (ADR 0009).
func (e *Engine) reclaim(desired map[string]bool) ([]FileChange, error) {
	var out []FileChange
	for _, dir := range e.ownedDirs() {
		names, err := e.host.Files.List(dir.path)
		if err != nil {
			return nil, fmt.Errorf("cannot read %s: %w", dir.path, err)
		}
		for _, name := range names {
			path := dir.path + "/" + name
			if desired[path] {
				continue
			}
			if dir.prefix != "" && !strings.HasPrefix(name, dir.prefix) {
				continue
			}
			before, mode, err := e.host.Files.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("cannot read %s: %w", path, err)
			}
			if dir.marked && !hasOwnershipMarker(before) {
				continue
			}
			out = append(out, FileChange{
				Path:       path,
				Kind:       ChangeRemove,
				Before:     string(before),
				HadBefore:  true,
				BeforeMode: mode,
			})
		}
	}
	return out, nil
}

// ownedDir is one directory regied reclaims from, and how it tells its own files apart
// from everybody else's in it.
type ownedDir struct {
	path string
	// prefix is on the name of every file regied puts here.
	prefix string
	// marked says the first line of every file regied puts here carries the ownership
	// marker.
	marked bool
}

func (e *Engine) ownedDirs() []ownedDir {
	return []ownedDir{
		// networkd's directory is shared with the distribution and with other
		// renderers, and the file name is the mark (ADR 0012).
		{path: e.opts.NetworkdDir, prefix: networkd.FilePrefix},
		{path: e.opts.Root + "/ppp/peers", marked: true},
		{path: e.opts.Root + "/ppp/credentials", marked: true},
		{path: e.opts.Root + "/dnsmasq", marked: true},
		// /etc/systemd/system holds everybody's units and the symlinks systemctl
		// enable makes, so both the name and the marker have to say it is ours.
		{path: e.opts.UnitDir, prefix: unitPrefix, marked: true},
	}
}

func hasOwnershipMarker(content []byte) bool {
	return strings.HasPrefix(string(content), ownershipMarker)
}

// switches works out what spec.global asks of the kernel and what the kernel currently
// says. A switch already holding the value asked for is in the plan as unchanged, so a
// dry-run can show that it was considered, and produces no step.
func (e *Engine) switches(cfg *config.Config) ([]SwitchChange, error) {
	var out []SwitchChange
	for _, want := range kernelSwitches(cfg.Global()) {
		change := SwitchChange{Key: want.key, Value: want.value}
		before, err := e.host.Sysctl.Get(want.key)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// A key this kernel does not have. Writing it would fail, and refusing the
			// whole apply over one absent knob would be worse than saying so.
			continue
		case err != nil:
			return nil, fmt.Errorf("cannot read the kernel switch %s: %w", want.key, err)
		}
		change.Before, change.HadBefore = before, true
		change.Changed = before != want.value
		out = append(out, change)
	}
	return out, nil
}

// firewall reads what the last apply recorded and whether the table is in the kernel.
func (e *Engine) firewall(ctx context.Context, ruleset string) (FirewallChange, error) {
	change := FirewallChange{Ruleset: ruleset}

	recorded, _, err := e.host.Files.ReadFile(e.opts.rulesetRecord())
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return change, fmt.Errorf("cannot read %s: %w", e.opts.rulesetRecord(), err)
	default:
		change.Before = string(recorded)
	}

	// The probe changes nothing. Its answer is what makes a reboot, and a flush
	// somebody else did, put the ruleset back (ADR 0004).
	_, err = e.host.Runner.Run(ctx, listTableCommand())
	change.Present = err == nil
	change.Apply = !change.Present || change.Ruleset != change.Before
	return change, nil
}

func (e *Engine) checkRuleset(ctx context.Context, plan *Plan) error {
	_, err := e.host.Runner.Run(ctx, Command{
		Name:  "nft",
		Args:  []string{"--check", "-f", "-"},
		Stdin: plan.Firewall.Ruleset,
	})
	switch {
	case errors.Is(err, ErrCommandNotFound):
		// A dry-run away from the host it is about. Saying the check was skipped is the
		// honest answer; implying the ruleset was validated is not (ADR 0006).
		plan.Notes = append(plan.Notes, "nft is not installed here, so the ruleset was not checked")
		return nil
	case err != nil:
		return fmt.Errorf("nft --check refused the ruleset: %w", err)
	}
	return nil
}

func listTableCommand() Command {
	return Command{Name: "nft", Args: []string{"list", "table", nftables.TableFamily, nftables.TableName}}
}

// steps is what the commit stage would run, in the order ADR 0004 fixes.
func (e *Engine) steps(plan *Plan, rendered *rendering) []Step {
	var steps []Step

	if plan.Firewall.Apply {
		steps = append(steps, Step{
			Phase:   PhaseFirewall,
			Kind:    StepCommand,
			Reason:  firewallReason(plan.Firewall),
			Command: Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: plan.Firewall.Ruleset},
			Undo:    firewallUndo(plan.Firewall),
		})
	}

	for _, change := range plan.Switches {
		if !change.Changed {
			continue
		}
		steps = append(steps, Step{
			Phase:  PhaseKernel,
			Kind:   StepSysctl,
			Reason: "spec.global",
			Switch: change,
			Undo:   sysctlUndo(change),
		})
	}

	if changedIn(plan, e.opts.NetworkdDir+"/") {
		// Reload, never restart: restarting networkd takes the links down, and the
		// PPPoE link's file is only safe to reload because it says KeepConfiguration
		// (ADR 0012).
		reload := Command{Name: "networkctl", Args: []string{"reload"}}
		steps = append(steps, Step{
			Phase:   PhaseNetworkd,
			Kind:    StepCommand,
			Reason:  "the systemd-networkd configuration changed",
			Command: reload,
			Undo:    &Step{Phase: PhaseNetworkd, Kind: StepCommand, Command: reload},
		})
	}

	unitsChanged := changedIn(plan, e.opts.UnitDir+"/")
	if unitsChanged {
		reload := Command{Name: "systemctl", Args: []string{"daemon-reload"}}
		steps = append(steps, Step{
			Phase:   PhaseProcessConfig,
			Kind:    StepCommand,
			Reason:  "a unit changed",
			Command: reload,
			Undo:    &Step{Phase: PhaseProcessConfig, Kind: StepCommand, Command: reload},
		})
	}

	steps = append(steps, e.sessionSteps(plan, rendered, unitsChanged)...)
	steps = append(steps, e.dnsmasqSteps(plan, rendered, unitsChanged)...)
	return steps
}

func firewallReason(change FirewallChange) string {
	if !change.Present {
		return "the table is not in the kernel"
	}
	return "the ruleset changed"
}

// firewallUndo puts the table back as it was: the text the last apply installed, or, if
// there was none, no table at all. Both are one nft transaction (ADR 0005).
func firewallUndo(change FirewallChange) *Step {
	ruleset := change.Before
	reason := "put the ruleset the previous apply installed back"
	if ruleset == "" {
		ruleset = fmt.Sprintf("table %s %s\ndelete table %s %s\n",
			nftables.TableFamily, nftables.TableName, nftables.TableFamily, nftables.TableName)
		reason = "take the table back off, because there was none before"
	}
	return &Step{
		Phase:   PhaseFirewall,
		Kind:    StepCommand,
		Reason:  reason,
		Command: Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: ruleset},
	}
}

func sysctlUndo(change SwitchChange) *Step {
	if !change.HadBefore {
		return nil
	}
	restored := change
	restored.Value, restored.Before = change.Before, change.Value
	return &Step{Phase: PhaseKernel, Kind: StepSysctl, Switch: restored}
}

// sessionSteps starts, restarts and stops the PPPoE sessions.
//
// A session is restarted only when its own configuration changed, because a restart is
// the one thing a rollback cannot undo: what comes back is a new session, possibly on a
// different address (ADR 0005).
func (e *Engine) sessionSteps(plan *Plan, rendered *rendering, unitsChanged bool) []Step {
	var steps []Step
	for _, session := range rendered.sessions {
		peer := changeFor(plan, e.opts.Root+"/ppp/peers/"+session+".conf")
		credentials := changeFor(plan, e.opts.Root+"/ppp/credentials/"+session+".conf")
		unit := pppoeUnit(session)

		switch {
		case peer.Kind == ChangeCreate:
			steps = append(steps, Step{
				Phase:   PhaseProcesses,
				Kind:    StepCommand,
				Reason:  "the session is new",
				Command: systemctl("enable", "--now", unit),
				Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: systemctl("disable", "--now", unit)},
			})
		case peer.Kind == ChangeUpdate || credentials.Kind != ChangeNone || unitsChanged:
			restart := systemctl("restart", unit)
			steps = append(steps, Step{
				Phase:   PhaseProcesses,
				Kind:    StepCommand,
				Reason:  "the session's configuration changed",
				Command: restart,
				Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: restart},
			})
		}
	}
	for _, session := range reclaimedSessions(plan, e.opts.Root+"/ppp/peers/") {
		unit := pppoeUnit(session)
		steps = append(steps, Step{
			Phase:   PhaseProcesses,
			Kind:    StepCommand,
			Reason:  "the session is no longer declared",
			Command: systemctl("disable", "--now", unit),
			Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: systemctl("enable", "--now", unit)},
		})
	}
	return steps
}

// dnsmasqSteps starts, reloads or stops regied's own dnsmasq. It comes after the links,
// because it binds to the addresses they hold.
func (e *Engine) dnsmasqSteps(plan *Plan, rendered *rendering, unitsChanged bool) []Step {
	path := e.opts.Root + "/dnsmasq/dnsmasq.conf"
	change := changeFor(plan, path)

	switch {
	case rendered.dnsmasq && change.Kind == ChangeCreate:
		return []Step{{
			Phase:   PhaseProcesses,
			Kind:    StepCommand,
			Reason:  "a host that had no dnsmasq now declares one",
			Command: systemctl("enable", "--now", dnsmasqUnit),
			Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: systemctl("disable", "--now", dnsmasqUnit)},
		}}
	case rendered.dnsmasq && (change.Kind == ChangeUpdate || unitsChanged):
		reload := systemctl("reload-or-restart", dnsmasqUnit)
		return []Step{{
			Phase:   PhaseProcesses,
			Kind:    StepCommand,
			Reason:  "the dnsmasq configuration changed",
			Command: reload,
			Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: reload},
		}}
	case !rendered.dnsmasq && change.Kind == ChangeRemove:
		return []Step{{
			Phase:   PhaseProcesses,
			Kind:    StepCommand,
			Reason:  "the host no longer declares any address handout or DNS",
			Command: systemctl("disable", "--now", dnsmasqUnit),
			Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: systemctl("enable", "--now", dnsmasqUnit)},
		}}
	}
	return nil
}

func systemctl(args ...string) Command {
	return Command{Name: "systemctl", Args: args}
}

// changeFor is the plan's entry for one path, or an unchanged one when the plan has
// nothing to say about it.
func changeFor(plan *Plan, path string) FileChange {
	for _, change := range plan.Files {
		if change.Path == path {
			return change
		}
	}
	return FileChange{Path: path, Kind: ChangeNone}
}

func changedIn(plan *Plan, prefix string) bool {
	for _, change := range plan.Files {
		if strings.HasPrefix(change.Path, prefix) && change.Kind != ChangeNone {
			return true
		}
	}
	return false
}

// reclaimedSessions is the sessions whose peer file this plan takes away.
func reclaimedSessions(plan *Plan, prefix string) []string {
	var out []string
	for _, change := range plan.Files {
		if change.Kind != ChangeRemove || !strings.HasPrefix(change.Path, prefix) {
			continue
		}
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(change.Path, prefix), ".conf"))
	}
	slices.Sort(out)
	return out
}
