package apply

import (
	"fmt"
	"io"
	"strings"
)

// Report writes what a dry-run shows: the warnings, then what would change, then the
// commands that would run, then whether anything would change at all.
//
// It is written for a person at a console during an outage, which is also why it is not
// the machine-readable interface: anything that wants the plan structured asks the state
// API for it (ADR 0006).
func Report(w io.Writer, plan *Plan) {
	// The warnings come first. A declaration that could not be rendered as written
	// matters more than any line of the diff below it, and it is the reason a dry-run
	// exists at all.
	ReportWarnings(w, plan)

	reportFiles(w, plan)
	reportFirewall(w, plan)
	reportSwitches(w, plan)
	reportSteps(w, plan)

	switch {
	case plan.Rendered:
		fmt.Fprintln(w, "This is a rendering. Nothing on this host was read, and nothing was changed.")
	case plan.Empty() && len(plan.Waiting) > 0:
		// The same "nothing changed" from a host that has converged and from one that
		// has been waiting for a name for an hour mean different things, and the
		// difference has to be visible without anybody inferring it (ADR 0016).
		fmt.Fprintln(w, "Nothing to do now: the host holds everything that could be rendered, and is waiting for the rest.")
	case plan.Empty():
		fmt.Fprintln(w, "Nothing to do: the host already holds this configuration.")
	default:
		fmt.Fprintln(w, "This is a dry run. Nothing above has been done.")
	}
}

// ReportWarnings writes the renderers' warnings and what the host could not answer, and
// nothing else.
//
// Report writes them too. This exists so that an apply that is not a dry run can put
// them in front of what it is about to do: a declaration that could not be rendered as
// written matters more when it is being applied, not less (ADR 0006).
func ReportWarnings(w io.Writer, plan *Plan) {
	section(w, "Warnings", plan.Warnings)
	section(w, "What this host could not answer", plan.Notes)
	section(w, "Left out for want of a value", plan.Waiting)
}

func section(w io.Writer, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "%s\n", title)
	for _, line := range lines {
		fmt.Fprintf(w, "  %s\n", line)
	}
	fmt.Fprintln(w)
}

func reportFiles(w io.Writer, plan *Plan) {
	var unchanged int
	var shown bool
	for _, change := range plan.Files {
		if change.Kind == ChangeNone {
			unchanged++
			continue
		}
		if !shown {
			fmt.Fprintln(w, "Files")
			shown = true
		}
		reportFile(w, change)
	}
	if unchanged > 0 {
		if !shown {
			fmt.Fprintln(w, "Files")
		}
		fmt.Fprintf(w, "  %d unchanged\n", unchanged)
		shown = true
	}
	if shown {
		fmt.Fprintln(w)
	}
}

func reportFile(w io.Writer, change FileChange) {
	switch change.Kind {
	case ChangeRemove:
		fmt.Fprintf(w, "  reclaim %s\n", change.Path)
		return
	case ChangeCreate:
		fmt.Fprintf(w, "  create %s (mode %04o)\n", change.Path, change.Mode)
	case ChangeUpdate:
		fmt.Fprintf(w, "  update %s (mode %04o)\n", change.Path, change.Mode)
		// A file may be replaced because its mode moved and not its content. Saying
		// "update" and then showing nothing is a claim with nothing behind it.
		if change.HadBefore && change.BeforeMode != change.Mode {
			fmt.Fprintf(w, "    mode %04o -> %04o\n", change.BeforeMode, change.Mode)
		}
	}

	// A credentials file is reported by path, mode, and whether its content would
	// change. That reveals nothing and is what decides whether a session is restarted
	// (ADR 0003, ADR 0006).
	if change.Secret {
		fmt.Fprintf(w, "    holds a credential, content not shown; %s\n", secretVerdict(change))
		return
	}
	if change.Withheld {
		fmt.Fprintln(w, "    content not shown: nothing was read from this host")
		return
	}
	// A file that is not replacing anything is shown as it would be written. Decorating
	// every line of it as an addition says nothing, and a rendering is all such files.
	if change.Kind == ChangeCreate {
		indent(w, change.Content)
		return
	}
	diff := unifiedDiff(change.Before, change.Content)
	switch {
	case diff != "":
		indent(w, diff)
	case change.Before != change.Content:
		// A trailing newline is a difference no line diff can show, and it is still a
		// rewrite: whatever reads the file is restarted for it.
		fmt.Fprintln(w, "    the content differs only in a trailing newline; the file is rewritten anyway")
	default:
		fmt.Fprintln(w, "    the content would not change")
	}
}

// secretVerdict says what would happen to a credentials file without saying anything
// about what is in it. The verdict comes from the change kind, because the plan does not
// carry the content to compare (ADR 0003).
func secretVerdict(change FileChange) string {
	switch {
	case change.Withheld:
		return "nothing was read from this host, so whether it would change is not known"
	case change.Kind == ChangeCreate:
		return "there was no such file before"
	default:
		return "its content or mode would change, so the session will be restarted"
	}
}

func reportFirewall(w io.Writer, plan *Plan) {
	if !plan.Firewall.Apply && len(plan.Firewall.Elements) == 0 {
		return
	}
	fmt.Fprintln(w, "Firewall")
	if plan.Firewall.Apply {
		if plan.Firewall.Table == TableAbsent {
			fmt.Fprintln(w, "  the table is not in the kernel, so the whole ruleset goes in")
		}
		if plan.Firewall.Before == "" {
			indent(w, plan.Firewall.Ruleset)
		} else {
			indent(w, unifiedDiff(plan.Firewall.Before, plan.Firewall.Ruleset))
		}
	}
	// The ruleset says which set the hairpin rules match on; what goes into it is what
	// the links are holding, and it is shown beside the ruleset rather than in it
	// (ADR 0015). Which sets are written was decided against the kernel, not against the
	// text, so this is shown whether or not the ruleset changed.
	if len(plan.Firewall.Elements) > 0 {
		if plan.Firewall.Apply {
			fmt.Fprintln(w, "  and into the uplink sets")
		} else {
			fmt.Fprintln(w, "  the ruleset is unchanged, but these uplink sets do not hold what the links do")
		}
		for _, entry := range plan.Firewall.Elements {
			fmt.Fprintf(w, "    %s\n", entry)
		}
	}
	fmt.Fprintln(w)
}

func reportSwitches(w io.Writer, plan *Plan) {
	var lines []string
	for _, change := range plan.Switches {
		if !change.Changed {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s -> %s", change.Key, change.Before, change.Value))
	}
	section(w, "Kernel switches", lines)
}

func reportSteps(w io.Writer, plan *Plan) {
	if len(plan.Steps) == 0 {
		return
	}
	fmt.Fprintln(w, "Then, in this order")
	phase := Phase(-1)
	for _, step := range plan.Steps {
		if step.Phase != phase {
			phase = step.Phase
			fmt.Fprintf(w, "  %s\n", phase)
		}
		fmt.Fprintf(w, "    %s\n", step.describe())
	}
	fmt.Fprintln(w)
}

// indent puts a block of text under the line that introduced it, so that a diff and the
// path it belongs to read as one thing.
func indent(w io.Writer, text string) {
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
}
