package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
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
// resident process reads that record, never the file (ADR 0017).
func applyCommand(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("apply", stderr)
	path := flags.String("config", DefaultConfigPath, "the configuration to apply")
	dryRun := flags.Bool("dry-run", false, "show what would change and do none of it")
	confirm := flags.Duration("confirm", 0, "revert unless confirmed within this duration")
	control := flags.String("control", DefaultControlPath, "resident process control socket")
	if err := flags.Parse(args); err != nil {
		return parseExit(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	engine := apply.New(apply.OSHost(), apply.Options{})

	if *dryRun {
		if *confirm != 0 {
			fmt.Fprintln(stderr, "regied apply: -confirm cannot be used with -dry-run")
			return 2
		}
		_, cfg, err := loadDeclaration(*path)
		if err != nil {
			return reportError(stderr, err)
		}
		reportConfigWarnings(stderr, cfg)
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
	data, err := os.ReadFile(*path)
	if err != nil {
		return reportError(stderr, err)
	}
	declaration := apply.Declaration{Bytes: data, Source: *path}
	if *confirm > 0 {
		deadline := time.Now().Add(*confirm)
		response, err := (apply.OSControl{}).Do(ctx, *control, apply.ControlRequest{
			Verb: apply.ControlTrial, Declaration: declaration.Bytes, Source: declaration.Source, Deadline: deadline,
		})
		if err != nil {
			if response.Report != nil {
				reportTurn(stdout, response.Report)
			}
			return reportSubmissionError(stderr, err)
		}
		if response.Report != nil {
			reportTurn(stdout, response.Report)
		}
		fmt.Fprintf(stdout, "Trial %s started; confirm it before %s.\n", response.Revision, deadline.UTC().Format(time.RFC3339))
		if response.State == apply.StateFailing {
			return 1
		}
		return 0
	}
	response, err := (apply.OSControl{}).Do(ctx, *control, apply.ControlRequest{Verb: apply.ControlSubmit, Declaration: declaration.Bytes, Source: declaration.Source})
	if err != nil {
		if response.Report != nil {
			reportTurn(stdout, response.Report)
		}
		return reportSubmissionError(stderr, err)
	}
	if response.Report != nil {
		reportTurn(stdout, response.Report)
	}
	if response.State == apply.StateFailing {
		return 1
	}
	return 0
}

func reportSubmissionError(stderr io.Writer, err error) int {
	if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "connection refused") {
		fmt.Fprintf(stderr, "regied: the resident process is not running: %v\n  start regied.service and try again\n", err)
	} else if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || strings.Contains(err.Error(), "connection reset") {
		fmt.Fprintf(stderr, "regied: the connection to the resident process was lost: %v\n  the turn belongs to the daemon; its outcome is in the turn report and journal\n", err)
	} else {
		fmt.Fprintf(stderr, "regied: submission was refused: %v\n", err)
	}
	return 1
}

func reportTurn(stdout io.Writer, report *apply.TurnReport) {
	fmt.Fprintf(stdout, "Revision: %s\nOutcome: %s\n", report.Revision, report.Outcome)
	for _, phase := range report.Phases {
		fmt.Fprintf(stdout, "  changed: %s\n", phase)
	}
	for _, item := range report.Waiting {
		fmt.Fprintf(stdout, "  waiting: %s\n", item)
	}
	for _, item := range report.Failing {
		fmt.Fprintf(stdout, "  failing: %s\n", item)
	}
	for _, item := range report.Warnings {
		fmt.Fprintf(stdout, "  warning: %s\n", item)
	}
	for _, item := range report.Notes {
		fmt.Fprintf(stdout, "  note: %s\n", item)
	}
	fmt.Fprintf(stdout, "State: %s\n", report.State)
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
