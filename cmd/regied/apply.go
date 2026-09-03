package main

import (
	"context"
	"fmt"
	"io"

	"github.com/yuanying/regied/internal/apply"
	"github.com/yuanying/regied/internal/config"
)

// applyCommand puts a configuration on this host, or, with --dry-run, shows what doing
// so would change.
//
// Both are the same code path up to the point where the commands run: --dry-run is the
// staging stage with the files kept in memory and the commit stage printed instead of
// run, so there is no class of failure a dry-run passes and an apply then hits at the
// same place (ADR 0006).
func applyCommand(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("apply", stderr)
	path := flags.String("config", DefaultConfigPath, "the configuration to apply")
	dryRun := flags.Bool("dry-run", false, "show what would change and do none of it")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*path)
	if err != nil {
		return reportError(stderr, err)
	}
	for _, warning := range cfg.Warnings() {
		fmt.Fprintf(stderr, "regied: %s\n", warning)
	}

	ctx := context.Background()
	engine := apply.New(apply.OSHost(), apply.Options{})

	plan, err := engine.Plan(ctx, cfg)
	if err != nil {
		return reportError(stderr, err)
	}
	if *dryRun {
		apply.Report(stdout, plan)
		return 0
	}

	result, err := engine.ApplyPlan(ctx, cfg, plan)
	if err != nil {
		return reportError(stderr, err)
	}
	reportResult(stdout, result)
	return 0
}

func reportResult(stdout io.Writer, result *apply.Result) {
	if !result.Changed {
		fmt.Fprintln(stdout, "Nothing to do: the host already holds this configuration.")
		return
	}
	fmt.Fprintln(stdout, result.Plan.Summary())
	if result.FirewallReapplied {
		// The ordinary case on a cold start: the line dialled while the apply was
		// running, and the ruleset written first had been rendered without its address.
		fmt.Fprintln(stdout, "An uplink address appeared while applying; the ruleset was rendered again with it.")
	}
	fmt.Fprintln(stdout, "Applied.")
}
