package apply

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/render/networkd"
	"github.com/yuanying/regied/internal/render/nftables"
	"github.com/yuanying/regied/internal/render/pppd"
)

// Options is where on the host the engine puts what it owns. The defaults are the paths
// the renderers name, and a test overrides them to keep out of the way of a real host.
type Options struct {
	NetworkdDir string
	Root        string
	StateDir    string
	UnitDir     string
	// PPPDir is pppd's own directory, the one holding its hook directories. It is not
	// under Root: it belongs to whoever installed ppp, and regied writes two files in it
	// (ADR 0009, ADR 0015).
	PPPDir string
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
	if o.PPPDir == "" {
		o.PPPDir = pppd.DefaultPPPDir
	}
	// The rules a directory carries are found by comparing paths, and a path with a
	// slash too many is not the same string. Where a caller puts a slash must not decide
	// whether a credential is hidden.
	o.NetworkdDir, o.Root, o.StateDir, o.UnitDir, o.PPPDir =
		path.Clean(o.NetworkdDir), path.Clean(o.Root), path.Clean(o.StateDir),
		path.Clean(o.UnitDir), path.Clean(o.PPPDir)
	return o
}

// rulesetRecord is where the engine remembers the ruleset it installed. It is the only
// state an apply leaves behind, because for a file the previous generation is the file
// itself, while a ruleset is kernel state and has none (ADR 0004).
func (o Options) rulesetRecord() string { return o.StateDir + "/applied/ruleset.nft" }

// Engine puts a rendered configuration on one host.
type Engine struct {
	host          Host
	opts          Options
	retries       map[string]retryState
	retryRevision string
}

func New(host Host, opts Options) *Engine {
	if host.Clock == nil {
		host.Clock = OSClock{}
	}
	if host.Locker == nil {
		host.Locker = OSLocker{}
	}
	if host.Control == nil {
		host.Control = OSControl{}
	}
	if host.Timer == nil {
		host.Timer = OSTimer{}
	}
	return &Engine{host: host, opts: opts.withDefaults(), retries: make(map[string]retryState)}
}

type retryState struct {
	attempts int
	next     time.Time
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
	// Ruleset is the table, as text. It is a function of the configuration alone: it
	// names a set per uplink where an address would be, and holds no address (ADR 0015).
	Ruleset string
	// Elements is the uplink sets this apply would write, and what it would write into
	// each: the addresses the uplink's link is holding now. A set is in here when what
	// the kernel holds is not that — which, for a table about to be replaced, is every
	// set with something to hold, because replacing a table empties them. An apply that
	// finds every set right writes none (ADR 0015).
	//
	// They are not part of Ruleset, which is what is recorded and compared, and they
	// never decide whether the table has to be applied.
	Elements []SetElements
	// Before is the text the last apply recorded having installed.
	Before string
	// Table is what the probe could establish about the kernel, and Note is why it
	// could establish nothing, when that is the answer.
	Table TableState
	Note  string
	// Apply is whether the ruleset has to be installed.
	Apply bool
}

// SetElements is what goes into one named set.
type SetElements struct {
	Set      string
	Elements []string
}

// String is the elements as they are written in a rule or a set declaration. A set with
// nothing in it is written as such, because writing it empties it.
func (s SetElements) String() string {
	return fmt.Sprintf("%s { %s }", s.Set, strings.Join(s.Elements, ", "))
}

// Text is the table and the seeding of its sets as one file, which is what `nft --check`
// is handed: a check of the ruleset that left the elements out would pass on something
// the apply then runs (ADR 0006).
//
// The commit stage runs them as two steps rather than as this one file, so that a
// seeding that nft refuses cannot take the table with it (ADR 0005).
func (c FirewallChange) Text() string {
	return c.Ruleset + seedingText(c.Elements)
}

// seedingText is the nft statements that make each set hold exactly its elements, or
// nothing at all when there is nothing to write.
//
// Each set is flushed and refilled rather than having single elements added or deleted.
// One transaction is what makes the write atomic against the pppd hook, which may be
// adding to the same set at the same moment, and idempotent: a delete of an element the
// hook already took out would fail, and a flush of an empty set does not.
func seedingText(elements []SetElements) string {
	if len(elements) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# What each uplink is holding, read from the kernel by this apply. It is not part of\n")
	b.WriteString("# the ruleset above: what a line holds is runtime state (ADR 0015).\n")
	for _, entry := range elements {
		fmt.Fprintf(&b, "flush set %s %s %s\n", nftables.TableFamily, nftables.TableName, entry.Set)
		if len(entry.Elements) > 0 {
			fmt.Fprintf(&b, "add element %s %s %s\n", nftables.TableFamily, nftables.TableName, entry)
		}
	}
	return b.String()
}

// desiredUplinkSets is what every uplink set should hold: what its uplink's link is
// holding in that family. Every set is here, the ones with nothing to hold included,
// because a set that should be empty and is not is a set to write.
func desiredUplinkSets(cfg *config.Config, addresses map[string][]netip.Addr) []SetElements {
	var out []SetElements
	for _, link := range uplinkResources(cfg) {
		for _, family := range []v1alpha1.Family{v1alpha1.FamilyIPv4, v1alpha1.FamilyIPv6} {
			var values []string
			for _, address := range addresses[link.name] {
				if addressFamily(address) == family {
					values = append(values, address.String())
				}
			}
			out = append(out, SetElements{Set: nftables.UplinkSetName(link.ifname, family), Elements: values})
		}
	}
	slices.SortFunc(out, func(a, b SetElements) int { return cmp.Compare(a.Set, b.Set) })
	return out
}

// setsToWrite is the desired sets that differ from what the kernel holds. A set the
// kernel does not have is left out and named, because writing to it would fail the
// apply, and putting it back is the table's business (ADR 0004).
func setsToWrite(desired []SetElements, held map[string][]string) (write []SetElements, missing []string) {
	for _, want := range desired {
		have, ok := held[want.Set]
		if !ok {
			missing = append(missing, want.Set)
			continue
		}
		if !slices.Equal(want.Elements, have) {
			write = append(write, want)
		}
	}
	return write, missing
}

// emptySets is what a table that is about to be replaced will hold: nothing in any set.
func emptySets(desired []SetElements) map[string][]string {
	held := make(map[string][]string, len(desired))
	for _, set := range desired {
		held[set.Set] = nil
	}
	return held
}

func addressFamily(address netip.Addr) v1alpha1.Family {
	if address.Is4() {
		return v1alpha1.FamilyIPv4
	}
	return v1alpha1.FamilyIPv6
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
	// StepSeed puts into the ruleset's uplink sets what the links are holding. It runs a
	// command like StepCommand and is a kind of its own so that the dry-run can tell the
	// two nft transactions of the firewall phase apart (ADR 0015).
	StepSeed StepKind = "seed"
	// StepKeep does nothing on purpose and carries the reason a turn could not act.
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
}

func (s Step) describe() string {
	switch s.Kind {
	case StepSeed:
		return s.Reason
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

	// Waiting is what this turn left out for want of a value the host could not answer
	// yet, naming the value. A turn that finds nothing to do and something in here has
	// not converged: it has done what it can, and a later turn does the rest. A turn
	// with nothing in here that finds nothing to do has (ADR 0016).
	Waiting []string

	// Failing names drift this turn observed but was not allowed to repair. An
	// unattended turn puts restart, stop, and unit reclamation here (ADR 0016).
	Failing []string

	Files    []FileChange
	Switches []SwitchChange
	Firewall FirewallChange
	Steps    []Step

	// Rendered says this is a rendering rather than a plan against a host: nothing was
	// read, so every file is shown as it would be written and nothing is compared.
	Rendered bool

	// validation is what validation warned about, for the report of the turn. The console
	// prints it from the configuration itself, before the plan is made, so it is not in
	// Warnings (ADR 0006).
	validation []string

	// secrets is the content of the files marked Secret, which is why they are not in
	// the FileChanges above. Nothing that prints can reach this field, so printing a
	// plan cannot print a credential — which is what ADR 0003 asks for, and what a rule
	// to be careful would not give (ADR 0004).
	secrets map[string]secretContent
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
		switch step.Kind {
		case StepCommand:
			lines = append(lines, "run "+step.Command.String())
		case StepSeed:
			lines = append(lines, step.describe())
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
	plan := &Plan{Warnings: rendered.warnings, Notes: runtime.Notes, Waiting: rendered.waiting, Rendered: true}
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

	plan := &Plan{Warnings: rendered.warnings, Notes: runtime.Notes, Waiting: rendered.waiting}
	for _, warning := range cfg.Warnings() {
		plan.validation = append(plan.validation, warning.String())
	}

	desired := make(map[string]bool, len(rendered.artifacts))
	// A file that waits for a value is left exactly as it is. Reclaiming it would spell
	// "incomplete" with a missing file, and the host would lose a tunnel that was working
	// because a resolver blinked (ADR 0016).
	for _, path := range rendered.withheld {
		desired[path] = true
	}
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

	plan.Firewall, err = e.firewall(ctx, rendered.ruleset, desiredUplinkSets(cfg, runtime.UplinkAddresses))
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

	plan.Steps = e.steps(ctx, plan, rendered)
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
		// pppd's hook directories hold the distribution's own scripts, so a hook is
		// ours only if both its name and its marker say so (ADR 0015).
		{path: pppd.UpHookDir(e.opts.PPPDir), prefix: pppd.HookPrefix, marked: true},
		{path: pppd.DownHookDir(e.opts.PPPDir), prefix: pppd.HookPrefix, marked: true},
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

// hasOwnershipMarker is whether regied wrote a file it found in a shared directory.
//
// The marker is the first line of everything regied writes, with one exception: a script
// has to begin with its interpreter line, so the marker is the line after it. Reading
// only the first line would leave the pppd hooks unreclaimable for ever (ADR 0015).
func hasOwnershipMarker(content []byte) bool {
	lines := strings.SplitN(string(content), "\n", 3)
	if lines[0] == ownershipMarker {
		return true
	}
	return len(lines) > 1 && strings.HasPrefix(lines[0], "#!") && lines[1] == ownershipMarker
}

// switches works out what spec.global asks of the kernel and what the kernel currently
// says. A switch already holding the value asked for is in the plan as unchanged, so a
// dry-run can show that it was considered, and produces no step.
func (e *Engine) switches(cfg *config.Config) ([]SwitchChange, error) {
	var out []SwitchChange
	for _, want := range append(kernelSwitches(cfg.Global()), linkSwitches(cfg)...) {
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

// firewall reads what the last apply recorded, whether the table is in the kernel, and
// what its uplink sets hold.
//
// Whether the table is replaced and whether a set is written are two questions, answered
// the same way: ask the kernel, compare, act only on a difference. What the links hold
// never decides the first — a line that came up since the last apply is not a reason to
// replace the table — and the ruleset's text never decides the second: a set that is
// wrong is written whether or not the text changed, because an apply is the one general
// way a host is put right (ADR 0015, ADR 0004).
func (e *Engine) firewall(ctx context.Context, ruleset string, desired []SetElements) (FirewallChange, error) {
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

	// A table about to be replaced will hold nothing in any set, so there is nothing to
	// ask: every set with something to hold is written. Otherwise the sets are read, and
	// only the ones that differ are.
	var held map[string][]string
	if change.Apply {
		held = emptySets(desired)
	} else {
		var note string
		held, note = e.probeUplinkSets(ctx)
		if note != "" {
			change.Note = strings.TrimSpace(change.Note + "\n" + note)
			return change, nil
		}
	}
	var missing []string
	change.Elements, missing = setsToWrite(desired, held)
	if len(missing) > 0 {
		change.Note = strings.TrimSpace(change.Note + "\n" + fmt.Sprintf(
			"the table does not hold %s; the set was removed outside regied and is put back when the ruleset next changes, so it was not written",
			strings.Join(missing, ", ")))
	}
	return change, nil
}

// probeUplinkSets asks the kernel what regied's table holds in each set. The second
// result says why it could not, when that is the answer.
//
// As with the table, "could not be asked" is an answer of its own and is never read as
// "empty": a probe that merely failed must not have every set written on every apply
// (ADR 0006). The sets are then left as they are, and the plan says so.
func (e *Engine) probeUplinkSets(ctx context.Context) (map[string][]string, string) {
	out, err := e.host.Runner.Run(ctx, listTableCommand())
	switch {
	case errors.Is(err, ErrCommandNotFound):
		return nil, "nft is not installed here, so what the uplink sets hold could not be read; they were left as they are"
	case err != nil:
		return nil, fmt.Sprintf("what the uplink sets hold could not be read (%v); they were left as they are", err)
	}
	held, err := parseSets(out)
	if err != nil {
		return nil, fmt.Sprintf("what the uplink sets hold could not be read (%v); they were left as they are", err)
	}
	return held, ""
}

// parseSets is the sets of regied's table out of what `nft -j list table` printed, each
// with its elements sorted. An element that is not a plain address — a prefix or a
// range somebody put there — is kept as the text nft gave it, so that it compares as
// different from anything a link holds and the set is written.
func parseSets(output []byte) (map[string][]string, error) {
	var listing struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(output, &listing); err != nil {
		return nil, fmt.Errorf("nft printed something that is not its JSON: %w", err)
	}
	held := make(map[string][]string)
	for _, entry := range listing.Nftables {
		raw, ok := entry["set"]
		if !ok {
			continue
		}
		var set struct {
			Family string            `json:"family"`
			Table  string            `json:"table"`
			Name   string            `json:"name"`
			Elem   []json.RawMessage `json:"elem"`
		}
		if err := json.Unmarshal(raw, &set); err != nil {
			return nil, fmt.Errorf("nft printed a set regied cannot read: %w", err)
		}
		if set.Family != nftables.TableFamily || set.Table != nftables.TableName {
			continue
		}
		elements := make([]string, 0, len(set.Elem))
		for _, element := range set.Elem {
			var address string
			if err := json.Unmarshal(element, &address); err != nil {
				address = string(element)
			}
			elements = append(elements, address)
		}
		slices.Sort(elements)
		held[set.Name] = elements
	}
	return held, nil
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
		Stdin: plan.Firewall.Text(),
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

// listTableCommand asks for regied's table as JSON, which is how what its sets hold is
// read back without depending on the text nft prints for a person.
func listTableCommand() Command {
	return Command{Name: "nft", Args: []string{"-j", "list", "table", nftables.TableFamily, nftables.TableName}}
}

// steps is what the commit stage would run, in the order ADR 0004 fixes.
func (e *Engine) steps(ctx context.Context, plan *Plan, rendered *rendering) []Step {
	var steps []Step

	if plan.Firewall.Apply {
		steps = append(steps, Step{
			Phase:   PhaseFirewall,
			Kind:    StepCommand,
			Reason:  firewallReason(plan.Firewall),
			Command: Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: plan.Firewall.Ruleset},
		})
	}
	// Right after the table when the table went in, and on its own when it did not: the
	// sets that do not hold what the links hold are written, and whether the ruleset
	// changed has nothing to do with it (ADR 0015).
	if len(plan.Firewall.Elements) > 0 {
		steps = append(steps, Step{
			Phase:   PhaseFirewall,
			Kind:    StepSeed,
			Reason:  "write the uplink sets: " + describeElements(plan.Firewall.Elements),
			Command: seedCommand(plan.Firewall.Elements),
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
		})
	}

	// A unit that was written is one systemd has to be told about before anything is
	// started from it. A unit that goes away is told about afterwards, because it is
	// taken away after the stop (ADR 0004).
	if writtenIn(plan, e.opts.UnitDir+"/") {
		steps = append(steps, daemonReload(PhaseProcessConfig, "a unit was written"))
	}

	for _, service := range e.services(ctx, plan, rendered) {
		steps = append(steps, service.steps()...)
	}
	steps = append(steps, deferredReclaim(deferredFiles(plan, ChangeRemove), "nothing runs from this unit any more")...)
	return steps
}

func firewallReason(change FirewallChange) string {
	if change.Table == TableAbsent {
		return "the table is not in the kernel"
	}
	return "the ruleset changed"
}

func seedCommand(elements []SetElements) Command {
	return Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: seedingText(elements)}
}

// describeElements is the seeding as one line, for the dry-run and for the summary.
func describeElements(elements []SetElements) string {
	out := make([]string, 0, len(elements))
	for _, entry := range elements {
		out = append(out, entry.String())
	}
	return strings.Join(out, ", ")
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
