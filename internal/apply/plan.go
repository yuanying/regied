package apply

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"path"
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
	// PhaseStaging is not one of the six: it is where the files are written and
	// nothing has run. It exists so that a failure there can say where it was.
	PhaseStaging Phase = iota
	PhaseFirewall
	PhaseKernel
	PhaseNetworkd
	PhaseProcessConfig
	PhaseProcesses
)

func (p Phase) String() string {
	switch p {
	case PhaseStaging:
		return "staging"
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

	// Deferred says this file is not reclaimed while the files are being written, but
	// by a step, once whatever still runs from it has been stopped (ADR 0004).
	Deferred bool

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
	// Table is what the probe could establish about the kernel, and Note is why it
	// could establish nothing, when that is the answer.
	Table TableState
	Note  string
	// Apply is whether the ruleset has to be installed.
	Apply bool
}

// TableState is what asking the kernel about regied's table answered.
//
// "Not there" and "could not be asked" are different answers. Reading the second as the
// first makes a dry-run on a machine without nft report that it would install a ruleset
// the host already has (ADR 0006) — and, worse, "not there" is the one answer that lets
// a rollback delete the table, so a probe that merely failed must never produce it
// (ADR 0005).
type TableState int

const (
	// TableUnknown is what a machine that could not ask says: nft is not installed, or
	// it ran and failed. It is treated as present, which is the safe side in both
	// directions: nothing is deleted over it, and the ruleset still goes in when it
	// differs from the one recorded.
	TableUnknown TableState = iota
	// TablePresent and TableAbsent are answers: nft ran and listed the tables.
	TablePresent
	TableAbsent
)

// StepKind tells the two things a step can be apart. Both are effects and both belong to
// the commit stage; they differ in that one runs a command and one writes /proc/sys.
type StepKind string

const (
	StepCommand StepKind = "command"
	StepSysctl  StepKind = "sysctl"
	// StepRemove and StepWrite are the reclaiming that cannot happen while the files
	// are being written, because something is still running from the file.
	StepRemove StepKind = "remove"
	StepWrite  StepKind = "write"
	// StepKeep does nothing on purpose. It is what an undo is when there is nothing
	// safe to put back, and its Reason is what the rollback reports (ADR 0005).
	StepKeep StepKind = "keep"
)

// Step is one thing the commit stage does.
type Step struct {
	Phase  Phase
	Kind   StepKind
	Reason string

	Command Command
	Switch  SwitchChange
	File    FileChange

	// Undo puts back what this step changed. Every step has one, because a step without
	// a way back is a step a later failure could not roll back (ADR 0005).
	Undo *Step
}

func (s Step) describe() string {
	switch s.Kind {
	case StepSysctl:
		return fmt.Sprintf("%s = %s", s.Switch.Key, s.Switch.Value)
	case StepRemove:
		return "reclaim " + s.File.Path
	case StepWrite:
		return "put back " + s.File.Path
	case StepKeep:
		return s.Reason
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

	// secrets is the content of the files marked Secret, which is why they are not in
	// the FileChanges above. Nothing that prints can reach this field, so printing a
	// plan cannot print a credential — which is what ADR 0003 asks for, and what a rule
	// to be careful would not give (ADR 0004).
	secrets map[string]secretContent

	// uplinks is what each uplink was holding when this plan was computed. It is what
	// the settle step compares against, so that a link read while its session is
	// redialling cannot take the rules that depend on an address away (ADR 0004).
	uplinks map[string][]netip.Addr
}

// secretContent is what a file the plan may not carry holds, and what is there now. Both
// are credentials, so both live here rather than in a FileChange.
type secretContent struct {
	content   string
	before    string
	hadBefore bool
}

// hide moves a secret file's content out of the change and into the plan's own store.
// The change keeps everything an operator may see: the path, the mode, and whether the
// file would be created or replaced.
func (p *Plan) hide(change *FileChange) {
	if !change.Secret || change.Withheld {
		return
	}
	if p.secrets == nil {
		p.secrets = make(map[string]secretContent)
	}
	p.secrets[change.Path] = secretContent{
		content:   change.Content,
		before:    change.Before,
		hadBefore: change.HadBefore,
	}
	change.Content, change.Before = "", ""
}

// contentFor is what to write for one file, whether or not the plan carries its content.
func (p *Plan) contentFor(change FileChange) (string, error) {
	if !change.Secret {
		return change.Content, nil
	}
	secret, ok := p.secrets[change.Path]
	if !ok {
		return "", fmt.Errorf("nothing was rendered for %s", change.Path)
	}
	return secret.content, nil
}

// previousFor is what one file held before this plan, whether or not the plan carries
// it. The second result says whether there was a file at all.
func (p *Plan) previousFor(change FileChange) (string, bool) {
	if !change.Secret {
		return change.Before, change.HadBefore
	}
	secret := p.secrets[change.Path]
	return secret.before, secret.hadBefore
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
	if runtime == nil {
		runtime = &Runtime{}
	}
	rendered, err := e.render(cfg, runtime)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Warnings: rendered.warnings, Notes: runtime.Notes, Rendered: true}
	for _, item := range rendered.artifacts {
		change := FileChange{
			Path:     item.Path,
			Kind:     ChangeCreate,
			Mode:     item.Mode,
			DirMode:  item.DirMode,
			Content:  item.Content,
			Secret:   item.Secret,
			Withheld: item.Withheld,
		}
		plan.hide(&change)
		plan.Files = append(plan.Files, change)
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

	plan := &Plan{
		Warnings: rendered.warnings,
		Notes:    runtime.Notes,
		uplinks:  runtime.NFTables.UplinkAddresses,
	}

	desired := make(map[string]bool, len(rendered.artifacts))
	for _, item := range rendered.artifacts {
		desired[item.Path] = true
		change, err := e.compare(item)
		if err != nil {
			return nil, err
		}
		plan.hide(&change)
		plan.Files = append(plan.Files, change)
	}

	reclaimed, err := e.reclaim(desired)
	if err != nil {
		return nil, err
	}
	for _, change := range reclaimed {
		// A credential being taken away is a credential (ADR 0003).
		plan.hide(&change)
		plan.Files = append(plan.Files, change)
	}
	slices.SortFunc(plan.Files, func(a, b FileChange) int { return cmp.Compare(a.Path, b.Path) })

	plan.Switches, err = e.switches(cfg)
	if err != nil {
		return nil, err
	}

	plan.Firewall, err = e.firewall(ctx, rendered.ruleset)
	if err != nil {
		return nil, err
	}
	if plan.Firewall.Note != "" {
		plan.Notes = append(plan.Notes, plan.Firewall.Note)
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
	e.applyPolicy(&change)

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
			file := dir.path + "/" + name
			if desired[file] {
				continue
			}
			if dir.prefix != "" && !strings.HasPrefix(name, dir.prefix) {
				continue
			}
			before, mode, err := e.host.Files.ReadFile(file)
			if err != nil {
				return nil, fmt.Errorf("cannot read %s: %w", file, err)
			}
			if dir.marked && !hasOwnershipMarker(before) {
				continue
			}
			change := FileChange{
				Path:       file,
				Kind:       ChangeRemove,
				Before:     string(before),
				HadBefore:  true,
				BeforeMode: mode,
			}
			e.applyPolicy(&change)
			out = append(out, change)
		}
	}
	return out, nil
}

// ownedDir is one directory regied owns files in, and everything that is true of every
// file in it.
//
// It is the one place these properties are decided. Both the writing path and the
// reclaiming path ask it, so a directory cannot acquire half its rules: the first round
// of this engine marked credentials secret where the files were written and forgot to
// where they were taken away, and deferred the reclaim of a unit on the way forward and
// not on the way back.
type ownedDir struct {
	path string
	// prefix is on the name of every file regied puts here.
	prefix string
	// marked says the first line of every file regied puts here carries the ownership
	// marker.
	marked bool
	// deferred says a file here is written and taken away out of step with the rest,
	// because something may still be running from it — see the unit ordering in
	// ADR 0004.
	deferred bool
	// secret says every file here holds a credential, whichever direction it is being
	// moved in (ADR 0003).
	secret bool
}

func (e *Engine) ownedDirs() []ownedDir {
	return []ownedDir{
		// networkd's directory is shared with the distribution and with other
		// renderers, and the file name is the mark (ADR 0012).
		{path: e.opts.NetworkdDir, prefix: networkd.FilePrefix},
		{path: e.opts.Root + "/ppp/peers", marked: true},
		{path: e.opts.Root + "/ppp/credentials", marked: true, secret: true},
		{path: e.opts.Root + "/dnsmasq", marked: true},
		// /etc/systemd/system holds everybody's units and the symlinks systemctl
		// enable makes, so both the name and the marker have to say it is ours.
		{path: e.opts.UnitDir, prefix: unitPrefix, marked: true, deferred: true},
	}
}

// applyPolicy puts on a change everything that follows from where the file is. It is
// called on every FileChange this package builds, in either direction.
func (e *Engine) applyPolicy(change *FileChange) {
	for _, dir := range e.ownedDirs() {
		if path.Dir(change.Path) != dir.path {
			continue
		}
		change.Secret = change.Secret || dir.secret
		change.Deferred = dir.deferred
		return
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
	change.Table, change.Note = e.probeTable(ctx)
	change.Apply = change.Table == TableAbsent || change.Ruleset != change.Before
	return change, nil
}

// probeTable asks the kernel which tables it holds and looks for regied's among them.
//
// It lists every table rather than asking for one, so that "not there" is something nft
// said with a successful exit, and every failure — nft not installed, a netlink error, a
// capability this process lacks — is the one remaining answer, "could not be asked". The
// second result says why, for the plan's notes.
func (e *Engine) probeTable(ctx context.Context) (TableState, string) {
	out, err := e.host.Runner.Run(ctx, listTablesCommand())
	switch {
	case errors.Is(err, ErrCommandNotFound):
		return TableUnknown, "nft is not installed here, so whether the table is already in the kernel could not be asked; this assumes it is"
	case err != nil:
		return TableUnknown, fmt.Sprintf("whether the table is already in the kernel could not be asked (%v); this assumes it is", err)
	}
	ours := []string{"table", nftables.TableFamily, nftables.TableName}
	for _, line := range strings.Split(string(out), "\n") {
		if slices.Equal(strings.Fields(line), ours) {
			return TablePresent, ""
		}
	}
	return TableAbsent, ""
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

func listTablesCommand() Command {
	return Command{Name: "nft", Args: []string{"list", "tables"}}
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

	// A unit that was written is one systemd has to be told about before anything is
	// started from it. A unit that goes away is told about afterwards, because it is
	// taken away after the stop (ADR 0004).
	if writtenIn(plan, e.opts.UnitDir+"/") {
		reload := Command{Name: "systemctl", Args: []string{"daemon-reload"}}
		steps = append(steps, Step{
			Phase:   PhaseProcessConfig,
			Kind:    StepCommand,
			Reason:  "a unit was written",
			Command: reload,
			Undo:    &Step{Phase: PhaseProcessConfig, Kind: StepCommand, Command: reload},
		})
	}

	steps = append(steps, e.sessionSteps(plan)...)
	steps = append(steps, e.dnsmasqSteps(plan, rendered)...)
	steps = append(steps, e.reclaimUnitSteps(plan)...)
	return steps
}

// reclaimUnitSteps takes the units away, after everything that ran from them has been
// stopped.
//
// systemctl resolves an instance through its template, so taking the template away first
// would make the stop fail — and on a configuration that removed its last session, that
// failure would roll the whole apply back and put the session's configuration back
// (ADR 0004).
func (e *Engine) reclaimUnitSteps(plan *Plan) []Step {
	var steps []Step
	for _, change := range plan.Files {
		if !change.Deferred || change.Kind != ChangeRemove {
			continue
		}
		restored := change
		restored.Content, restored.Mode = change.Before, change.BeforeMode
		steps = append(steps, Step{
			Phase:  PhaseProcesses,
			Kind:   StepRemove,
			Reason: "nothing runs from this unit any more",
			File:   change,
			Undo:   &Step{Phase: PhaseProcesses, Kind: StepWrite, File: restored},
		})
	}
	if len(steps) == 0 {
		return nil
	}
	reload := Command{Name: "systemctl", Args: []string{"daemon-reload"}}
	return append(steps, Step{
		Phase:   PhaseProcesses,
		Kind:    StepCommand,
		Reason:  "a unit was taken away",
		Command: reload,
		Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: reload},
	})
}

func firewallReason(change FirewallChange) string {
	if change.Table == TableAbsent {
		return "the table is not in the kernel"
	}
	return "the ruleset changed"
}

// firewallUndo puts the table back as it was.
//
// There are three answers, not two. With a recorded ruleset, that text goes back in — one
// transaction, as ADR 0013 makes it. With no record and no table before this apply,
// taking ours off *is* putting the host back.
//
// With no record and a table that was already there, there is nothing to put back and
// deleting is not the same thing: it would take the firewall off a host that was running
// one, to recover from a missing note. The table this apply installed is left, and the
// rollback says so (ADR 0005).
func firewallUndo(change FirewallChange) *Step {
	if change.Before != "" {
		return &Step{
			Phase:   PhaseFirewall,
			Kind:    StepCommand,
			Reason:  "put the ruleset the previous apply installed back",
			Command: Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: change.Before},
		}
	}
	if change.Table == TableAbsent {
		return &Step{
			Phase:  PhaseFirewall,
			Kind:   StepCommand,
			Reason: "take the table back off, because there was none before",
			Command: Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: fmt.Sprintf(
				"table %s %s\ndelete table %s %s\n",
				nftables.TableFamily, nftables.TableName, nftables.TableFamily, nftables.TableName)},
		}
	}
	return &Step{
		Phase: PhaseFirewall,
		Kind:  StepKeep,
		Reason: fmt.Sprintf(
			"the %s %s table this apply installed was left in place: there is no record of what was there before it, and taking it off would leave the host with no firewall",
			nftables.TableFamily, nftables.TableName),
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
func (e *Engine) sessionSteps(plan *Plan) []Step {
	var steps []Step
	// The one unit a session runs from. Any other unit changing has nothing to do with
	// it, and restarting a line over an unrelated change is exactly what ADR 0005 says
	// must not happen. But a template that had to be *written back* — because somebody
	// deleted it — leaves the sessions stopped and not enabled, and that needs starting
	// rather than restarting.
	template := changeFor(plan, e.opts.UnitDir+"/"+pppoeTemplateUnit)

	for _, session := range sessionsIn(plan, e.opts.Root+"/ppp/peers/") {
		peer := changeFor(plan, e.opts.Root+"/ppp/peers/"+session+".conf")
		credentials := changeFor(plan, e.opts.Root+"/ppp/credentials/"+session+".conf")
		unit := pppoeUnit(session)

		switch {
		case peer.Kind == ChangeCreate || template.Kind == ChangeCreate:
			steps = append(steps, Step{
				Phase:   PhaseProcesses,
				Kind:    StepCommand,
				Reason:  "the session, or the unit it runs from, is not on the host yet",
				Command: systemctl("enable", "--now", unit),
				Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: systemctl("disable", "--now", unit)},
			})
		case wasWritten(peer) || wasWritten(credentials) || wasWritten(template):
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

// dnsmasqSteps starts, restarts or stops regied's own dnsmasq. It comes after the links,
// because it binds to the addresses they hold.
func (e *Engine) dnsmasqSteps(plan *Plan, rendered *rendering) []Step {
	conf := e.opts.Root + "/dnsmasq/dnsmasq.conf"
	change := changeFor(plan, conf)
	unit := changeFor(plan, e.opts.UnitDir+"/"+dnsmasqUnit)

	switch {
	case rendered.dnsmasq && (change.Kind == ChangeCreate || unit.Kind == ChangeCreate):
		return []Step{{
			Phase:   PhaseProcesses,
			Kind:    StepCommand,
			Reason:  "dnsmasq, or the unit it runs from, is not on the host yet",
			Command: systemctl("enable", "--now", dnsmasqUnit),
			Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: systemctl("disable", "--now", dnsmasqUnit)},
		}}
	case rendered.dnsmasq && (wasWritten(change) || wasWritten(unit)):
		// Restart, not reload. dnsmasq re-reads /etc/hosts, its lease file and
		// resolv.conf on SIGHUP, and nothing else: a reload would leave the
		// configuration that was just written unapplied.
		restart := systemctl("restart", dnsmasqUnit)
		return []Step{{
			Phase:   PhaseProcesses,
			Kind:    StepCommand,
			Reason:  "the dnsmasq configuration changed",
			Command: restart,
			Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: restart},
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

// wasWritten is whether a change puts a file on the host, whether or not one was there
// before. Several questions in this package are that question, and asking it by name
// keeps a caller from testing for one of the two kinds and forgetting the other.
func wasWritten(change FileChange) bool {
	return change.Kind == ChangeCreate || change.Kind == ChangeUpdate
}

func changedIn(plan *Plan, prefix string) bool {
	for _, change := range plan.Files {
		if strings.HasPrefix(change.Path, prefix) && change.Kind != ChangeNone {
			return true
		}
	}
	return false
}

// writtenIn is whether any file under a prefix would be created or replaced. It is not
// the same question as changedIn: a file that goes away is reclaimed later, once nothing
// runs from it.
func writtenIn(plan *Plan, prefix string) bool {
	for _, change := range plan.Files {
		if !strings.HasPrefix(change.Path, prefix) {
			continue
		}
		if wasWritten(change) {
			return true
		}
	}
	return false
}

// sessionsIn is the sessions this plan writes a peer file for, in path order.
func sessionsIn(plan *Plan, prefix string) []string {
	var out []string
	for _, change := range plan.Files {
		if change.Kind == ChangeRemove || !strings.HasPrefix(change.Path, prefix) {
			continue
		}
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(change.Path, prefix), ".conf"))
	}
	return out
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
