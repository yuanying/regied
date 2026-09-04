package apply

import (
	"slices"
	"strings"
)

// service is one process regied supervises, seen the only way the engine decides about
// it: what the configuration says, and what this plan does to the files it reads.
//
// Every start, restart and stop comes out of this one type. Earlier rounds of this
// engine decided each of them from the kind of change one particular file had — the
// peer file for a start, the unit for a restart — and every file that could go missing
// on its own became a case that was not there: a unit somebody deleted started nothing,
// a configuration somebody deleted stopped nothing. The state of any single file is not
// the question. Whether the configuration declares the process, and whether anything it
// reads was written, are (ADR 0004).
type service struct {
	// what the process is called in a reason: "the session pppoe0", "dnsmasq".
	what string
	// unit is the systemd unit, instance included.
	unit string
	// declared is whether the configuration asks for the process at all.
	declared bool
	// unitFile is the unit it runs from. Every session runs from the one template.
	unitFile FileChange
	// inputs is every other file it reads.
	inputs []FileChange
}

// steps is what the commit stage does to this one process.
//
// A process is restarted only when something it reads was written, because a restart
// is the one thing a rollback cannot undo for a session: what comes back is a new
// session, possibly on a different address (ADR 0005).
func (s service) steps() []Step {
	if !s.declared {
		// Anything of it still on the host is the reason to stop it. Which file is left
		// does not matter: the process is running, or may be, and the configuration
		// no longer asks for it.
		for _, file := range s.files() {
			if file.Kind == ChangeRemove {
				return []Step{s.command(s.what+" is no longer declared",
					systemctl("disable", "--now", s.unit), systemctl("enable", "--now", s.unit))}
			}
		}
		return nil
	}

	fresh := true
	written := wasWritten(s.unitFile)
	for _, file := range s.inputs {
		if file.HadBefore {
			fresh = false
		}
		if wasWritten(file) {
			written = true
		}
	}

	switch {
	case fresh:
		return []Step{s.command(s.what+" is not on the host yet",
			systemctl("enable", "--now", s.unit), systemctl("disable", "--now", s.unit))}
	case s.unitFile.Kind == ChangeCreate:
		// The unit was put back, so the process may be running from systemd's copy of
		// the old one, or not running at all. Starting what is running does nothing,
		// and a start followed by a restart would dial a line twice. Enabling and
		// restarting covers both, once.
		restart := systemctl("restart", s.unit)
		return []Step{
			s.command("the unit "+s.what+" runs from was put back",
				systemctl("enable", s.unit), systemctl("disable", s.unit)),
			s.command("something "+s.what+" reads changed", restart, restart),
		}
	case written:
		restart := systemctl("restart", s.unit)
		return []Step{s.command("something "+s.what+" reads changed", restart, restart)}
	}
	return nil
}

// files is everything the process reads, the unit included.
func (s service) files() []FileChange {
	return append([]FileChange{s.unitFile}, s.inputs...)
}

func (s service) command(reason string, run, undo Command) Step {
	return Step{
		Phase:   PhaseProcesses,
		Kind:    StepCommand,
		Reason:  reason,
		Command: run,
		Undo:    &Step{Phase: PhaseProcesses, Kind: StepCommand, Command: undo},
	}
}

// services is every process this plan has to decide about: the sessions the
// configuration declares, the sessions anything on the host still belongs to, and
// dnsmasq — which comes last, because it binds to the addresses the links hold.
func (e *Engine) services(plan *Plan, rendered *rendering) []service {
	peers := e.opts.Root + "/ppp/peers/"
	credentials := e.opts.Root + "/ppp/credentials/"
	template := changeFor(plan, e.opts.UnitDir+"/"+pppoeTemplateUnit)

	declared := make(map[string]bool, len(rendered.sessions))
	names := slices.Clone(rendered.sessions)
	for _, name := range rendered.sessions {
		declared[name] = true
	}
	names = append(names, namesIn(plan, peers)...)
	names = append(names, namesIn(plan, credentials)...)
	slices.Sort(names)
	names = slices.Compact(names)

	var out []service
	for _, name := range names {
		out = append(out, service{
			what:     "the session " + name,
			unit:     pppoeUnit(name),
			declared: declared[name],
			unitFile: template,
			inputs: []FileChange{
				changeFor(plan, peers+name+".conf"),
				changeFor(plan, credentials+name+".conf"),
			},
		})
	}
	return append(out, service{
		what:     "dnsmasq",
		unit:     dnsmasqUnit,
		declared: rendered.dnsmasq,
		unitFile: changeFor(plan, e.opts.UnitDir+"/"+dnsmasqUnit),
		inputs:   []FileChange{changeFor(plan, e.opts.Root+"/dnsmasq/dnsmasq.conf")},
	})
}

// namesIn is the name of every session with a .conf file under a directory in this
// plan, whatever is happening to the file.
func namesIn(plan *Plan, prefix string) []string {
	var out []string
	for _, change := range plan.Files {
		if !strings.HasPrefix(change.Path, prefix) {
			continue
		}
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(change.Path, prefix), ".conf"))
	}
	return out
}

// deferredReclaim takes deferred files away once nothing runs from them any more, and
// then tells systemd: one removal each, then a daemon-reload.
//
// systemctl resolves an instance through its template, so taking the template away
// first would make the stop fail — and on a configuration that removed its last session,
// that failure would roll the whole apply back and put the session's configuration back.
// Forward, this reclaims the units the configuration no longer needs, after the stops.
// In a rollback, it takes back the units this apply created, after the undo of the
// starts. Both directions are the same steps for the same reason (ADR 0004).
func deferredReclaim(files []FileChange, reason string) []Step {
	var steps []Step
	for _, change := range files {
		restored := change
		restored.Content, restored.Mode = change.Before, change.BeforeMode
		steps = append(steps, Step{
			Phase:  PhaseProcesses,
			Kind:   StepRemove,
			Reason: reason,
			File:   change,
			Undo:   &Step{Phase: PhaseProcesses, Kind: StepWrite, File: restored},
		})
	}
	if len(steps) == 0 {
		return nil
	}
	return append(steps, daemonReload(PhaseProcesses, "a unit was taken away"))
}

// deferredFiles is the deferred files in a plan that a given change is happening to.
func deferredFiles(plan *Plan, kind ChangeKind) []FileChange {
	var out []FileChange
	for _, change := range plan.Files {
		if change.Deferred && change.Kind == kind {
			out = append(out, change)
		}
	}
	return out
}

func daemonReload(phase Phase, reason string) Step {
	reload := systemctl("daemon-reload")
	return Step{
		Phase:   phase,
		Kind:    StepCommand,
		Reason:  reason,
		Command: reload,
		Undo:    &Step{Phase: phase, Kind: StepCommand, Command: reload},
	}
}

func systemctl(args ...string) Command {
	return Command{Name: "systemctl", Args: args}
}
