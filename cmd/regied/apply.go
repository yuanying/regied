package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/yuanying/regied/internal/apply"
	"github.com/yuanying/regied/internal/config"
)

// applyCommand submits a configuration to this host, or, with --dry-run, shows what doing
// so would change.
//
// Both are the same code path up to the point where the commands run: --dry-run is the
// staging stage with the files kept in memory and the commit stage printed instead of
// run, so there is no class of failure a dry-run passes and an apply then hits at the
// same place (ADR 0006).
//
// An apply is a submission, and it is the only thing in regied that reads the
// configuration file. Once the declaration has validated and staged it is written down as
// the one this host converges toward, and the turn that follows runs toward it; the
// resident process and `regied reconcile` read that record, never the file (ADR 0016).
func applyCommand(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("apply", stderr)
	path := flags.String("config", DefaultConfigPath, "the configuration to apply")
	dryRun := flags.Bool("dry-run", false, "show what would change and do none of it")
	confirm := flags.Duration("confirm", 0, "revert unless confirmed within this duration")
	control := flags.String("control", DefaultControlPath, "resident process control socket")
	if err := flags.Parse(args); err != nil {
		return parseExit(err)
	}

	declaration, cfg, err := loadDeclaration(*path)
	if err != nil {
		return reportError(stderr, err)
	}
	reportConfigWarnings(stderr, cfg)

	ctx := context.Background()
	engine := apply.New(apply.OSHost(), apply.Options{})

	if *dryRun {
		if *confirm != 0 {
			fmt.Fprintln(stderr, "regied apply: -confirm cannot be used with -dry-run")
			return 2
		}
		// A dry run is not a turn: it writes nothing and runs nothing that changes
		// anything, so it takes no lock and never waits for one.
		plan, err := engine.Plan(ctx, cfg)
		if err != nil {
			return reportError(stderr, err)
		}
		apply.Report(stdout, plan)
		return 0
	}
	if *confirm < 0 {
		fmt.Fprintln(stderr, "regied apply: -confirm must be greater than zero")
		return 2
	}
	if *confirm > 0 {
		plan, err := engine.Plan(ctx, cfg)
		if err != nil {
			return reportError(stderr, err)
		}
		apply.ReportWarnings(stderr, plan)
		deadline := time.Now().Add(*confirm)
		response, err := (apply.OSControl{}).Do(ctx, *control, apply.ControlRequest{
			Verb: apply.ControlTrial, Declaration: declaration.Bytes, Source: declaration.Source, Deadline: deadline,
		})
		if err != nil {
			fmt.Fprintf(stderr, "regied: cannot start a confirmation trial because the resident process is unavailable or refused it: %v\n  start regied, or apply without -confirm and accept that nothing will undo it\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Trial %s started; state: %s. Confirm it before %s.\n", response.Revision, response.State, deadline.UTC().Format(time.RFC3339))
		return 0
	}

	// A turn holds the lock from the moment it reads the host to the moment it stops
	// changing it, so that the plan is computed against a host nothing else is moving.
	release, err := engine.LockTurn(ctx)
	if err != nil {
		return reportError(stderr, err)
	}
	defer release()
	previousReport, _ := engine.LastTurn()

	plan, err := engine.Plan(ctx, cfg)
	if err != nil {
		return reportError(stderr, err)
	}
	// A declaration that could not be rendered as written matters more when it is being
	// applied than when it is being previewed, so it goes in front of the apply rather
	// than only in front of a dry run (ADR 0006).
	apply.ReportWarnings(stderr, plan)

	result, err := engine.Submit(ctx, plan, declaration)
	if err != nil {
		return reportError(stderr, err)
	}
	reportResult(stdout, result)
	if previousReport != nil && previousReport.Trial {
		fmt.Fprintln(stdout, "The plain apply ended the active confirmation trial; no automatic revert remains.")
	}
	return 0
}

// reconcileCommand runs one turn toward the declaration this host accepted, and stops.
//
// It takes no configuration file, on purpose: it is the one way to ask for a turn that
// reads nothing but the record. It is what a boot unit runs, and what an operator types
// to put a host back where it should be without submitting anything (ADR 0016).
func reconcileCommand(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("reconcile", stderr)
	if err := flags.Parse(args); err != nil {
		return parseExit(err)
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "regied reconcile: takes no arguments; it reads the accepted declaration and nothing else\n")
		return 2
	}

	ctx := context.Background()
	engine := apply.New(apply.OSHost(), apply.Options{})

	release, err := engine.LockTurn(ctx)
	if err != nil {
		return reportError(stderr, err)
	}
	defer release()

	result, err := engine.Reconcile(ctx)
	switch {
	case errors.Is(err, apply.ErrNoRecord):
		// Nothing was done, and this says so. It is still a failure of the turn to
		// converge, which is what a boot unit has to be told (ADR 0016).
		fmt.Fprintf(stderr, "regied: %v\n  nothing was changed; submit a declaration with `regied apply`\n", err)
		return 1
	case err != nil:
		return reportError(stderr, err)
	}
	reportResult(stdout, result)
	return 0
}

// loadDeclaration reads a configuration file and validates it, and keeps the bytes it
// read: they are what the record holds, exactly as they were validated (ADR 0016).
//
// It is config.Load with the bytes kept. The path reaches the error the same way, so
// that an operator is told which file did not parse.
func loadDeclaration(path string) (apply.Declaration, *config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return apply.Declaration{}, nil, err
	}
	document, err := config.Parse(data)
	if err != nil {
		var parseErr *config.ParseError
		if errors.As(err, &parseErr) {
			parseErr.Path = path
		}
		return apply.Declaration{}, nil, err
	}
	cfg, err := config.Validate(document)
	if err != nil {
		var invalid *config.ValidationError
		if errors.As(err, &invalid) {
			invalid.Path = path
		}
		return apply.Declaration{}, nil, err
	}
	return apply.Declaration{Bytes: data, Source: path}, cfg, nil
}

// reportResult says what the turn did and where it left the host. The state comes last,
// because it is the one line an operator reads when nothing else is: the same "nothing
// to do" from a host that has converged and from one waiting for a name mean different
// things, and the exit status cannot tell them apart (ADR 0016).
func reportResult(stdout io.Writer, result *apply.Result) {
	switch {
	case result.Changed:
		fmt.Fprintln(stdout, result.Plan.Summary())
		fmt.Fprintln(stdout, "Applied.")
	case result.State == apply.StateWaiting:
		fmt.Fprintln(stdout, "Nothing to do now: the host holds everything that could be rendered, and is waiting for the rest.")
	default:
		fmt.Fprintln(stdout, "Nothing to do: the host already holds this configuration.")
	}
	// What follows happened after the configuration was on the host. It is not a failed
	// apply, and saying so would tell the operator the opposite of what happened
	// (ADR 0005) — but it still has to be read, so it goes before the state.
	for _, note := range result.Notes {
		fmt.Fprintf(stdout, "  note: %s\n", note)
	}
	fmt.Fprintf(stdout, "State: %s", result.State)
	if result.Revision != "" {
		fmt.Fprintf(stdout, " (%s)", result.Revision)
	}
	fmt.Fprintln(stdout)
	if result.State == apply.StateWaiting {
		fmt.Fprintln(stdout, "  "+strings.Join(result.Plan.Waiting, "\n  "))
	}
}
