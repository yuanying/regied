package apply

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// A step that makes a written file take effect — reloading networkd, telling udev and
// systemd to read their files again, applying a .link file to its NIC — is decided by
// comparing the files with the host. Once the files are written, the comparison has
// nothing left to find: the next turn sees them as they should be, and nothing would ever
// ask for the step again. So a turn that wrote the files and did not run the step — the
// backoff held it back, the turn failed before it, or the step itself failed — leaves it
// owed, and every later turn runs it until it has run.
//
// Without this, the backoff would suppress more than the command. A delegation that moves
// twice inside a second has the first turn reload networkd and the second write the files
// and hold its reload back; the third finds the files right, and networkd keeps the
// version of the first turn for as long as the host runs. ADR 0016 has the backoff
// suppress the command, not the comparison, and what the comparison found is part of the
// comparison.
//
// What is owed is written under the state directory, beside the report, so that a process
// that is restarted in between still runs it. After a reboot it is run once more than it
// had to be, which is harmless: every step that can be owed only makes a daemon read files
// that are already there.

// owedStep is one step as it is written down: enough to run it again, and nothing that
// was decided by the turn that owed it.
type owedStep struct {
	Phase  Phase    `yaml:"phase"`
	Reason string   `yaml:"reason"`
	Name   string   `yaml:"command"`
	Args   []string `yaml:"args,omitempty"`
}

func (s owedStep) key() string {
	return owedKey(Step{Phase: s.Phase, Kind: StepCommand, Command: Command{Name: s.Name, Args: s.Args}})
}

func (o Options) owedSteps() string { return o.StateDir + "/turn/owed.yaml" }

// owedKey is what tells two owed steps apart: the same command in the same phase is the
// same step, whichever turn asked for it.
func owedKey(step Step) string { return fmt.Sprintf("%d %s", step.Phase, step.describe()) }

// readOwed is what earlier turns left owed. A file that cannot be read is said so in the
// plan's notes and treated as owing nothing: there is no way to know what it held, and
// a turn that refused to run over it would leave the host with nothing at all.
func (e *Engine) readOwed(plan *Plan) []owedStep {
	location := e.opts.owedSteps()
	data, _, err := e.host.Files.ReadFile(location)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		plan.Notes = append(plan.Notes, fmt.Sprintf("what earlier turns left owed could not be read from %s (%v); this assumes nothing is", location, err))
		return nil
	}
	var owed []owedStep
	if err := yaml.Unmarshal(data, &owed); err != nil {
		plan.Notes = append(plan.Notes, fmt.Sprintf("what earlier turns left owed could not be read from %s (%v); this assumes nothing is", location, err))
		return nil
	}
	return owed
}

// addOwed puts into the plan every step an earlier turn left owed and this plan does not
// already have, in its place in the phase order, and records every owed step of the plan
// for the turn to settle once it has run.
func (e *Engine) addOwed(plan *Plan) {
	present := make(map[string]bool)
	for _, step := range plan.Steps {
		present[owedKey(step)] = true
	}
	for _, step := range plan.Steps {
		if step.Owed {
			plan.owed = append(plan.owed, owedStep{Phase: step.Phase, Reason: step.Reason, Name: step.Command.Name, Args: step.Command.Args})
		}
	}
	for _, owed := range e.readOwed(plan) {
		step := Step{
			Phase:   owed.Phase,
			Kind:    StepCommand,
			Reason:  "an earlier turn wrote the files and did not get to run this: " + owed.Reason,
			Command: Command{Name: owed.Name, Args: owed.Args},
			Owed:    true,
		}
		if present[owedKey(step)] || !owable(step.Command) {
			continue
		}
		// A link that is gone has nothing to apply its file to, and udev applies the file
		// when the link appears. Running the step would fail on every turn for as long as
		// the link is away.
		if link, ok := strings.CutPrefix(lastArg(step.Command), "/sys/class/net/"); ok && step.Command.Name == "udevadm" {
			if _, err := e.host.Links.Addresses(link); errors.Is(err, ErrLinkNotFound) {
				continue
			}
		}
		present[owedKey(step)] = true
		at := slices.IndexFunc(plan.Steps, func(s Step) bool { return s.Phase > step.Phase })
		if at < 0 {
			at = len(plan.Steps)
		}
		plan.Steps = slices.Insert(plan.Steps, at, step)
		plan.owed = append(plan.owed, owed)
	}
}

// owable is whether a command is one a turn can leave owed. The file is regied's own, but
// it is read back as commands to run, so nothing in it runs that regied would not have
// asked for itself.
func owable(command Command) bool {
	switch rendered := command.String(); {
	case rendered == "networkctl reload", rendered == "udevadm control --reload", rendered == "systemctl daemon-reload":
		return true
	case strings.HasPrefix(rendered, "udevadm trigger "):
		link, ok := strings.CutPrefix(lastArg(command), "/sys/class/net/")
		return ok && !strings.Contains(link, "/") && command.String() == udevTriggerCommand(link).String()
	}
	return false
}

func lastArg(command Command) string {
	if len(command.Args) == 0 {
		return ""
	}
	return command.Args[len(command.Args)-1]
}

// settleOwed writes down what this turn leaves owed: every owed step of its plan that did
// not run to success. Like every file regied writes, it is written only when what it says
// changed, and it is taken away when nothing is owed (ADR 0004).
func (e *Engine) settleOwed(plan *Plan) error {
	var owed []owedStep
	for _, step := range plan.owed {
		if !plan.ran[step.key()] {
			owed = append(owed, step)
		}
	}
	slices.SortFunc(owed, func(a, b owedStep) int {
		return cmp.Or(cmp.Compare(a.Phase, b.Phase), cmp.Compare(a.Name+" "+strings.Join(a.Args, " "), b.Name+" "+strings.Join(b.Args, " ")))
	})

	location := e.opts.owedSteps()
	current, _, err := e.host.Files.ReadFile(location)
	exists := err == nil
	if len(owed) == 0 {
		if !exists {
			return nil
		}
		if err := e.host.Files.Remove(location); err != nil {
			return fmt.Errorf("cannot take away %s, which says what is owed: %w", location, err)
		}
		return nil
	}
	data, err := yaml.Marshal(owed)
	if err != nil {
		return fmt.Errorf("cannot write down what this turn leaves owed: %w", err)
	}
	if exists && bytes.Equal(current, data) {
		return nil
	}
	if err := e.host.Files.MkdirAll(path.Dir(location), 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", path.Dir(location), err)
	}
	if err := e.host.Files.WriteFile(location, data, 0o644); err != nil {
		return fmt.Errorf("cannot write down what this turn leaves owed in %s: %w", location, err)
	}
	return nil
}
