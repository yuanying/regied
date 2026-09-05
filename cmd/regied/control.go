package main

import (
	"context"
	"fmt"
	"io"

	"github.com/yuanying/regied/internal/apply"
)

func confirmCommand(args []string, stdout, stderr io.Writer) int {
	return controlCommand("confirm", apply.ControlConfirm, args, stdout, stderr)
}

func cancelCommand(args []string, stdout, stderr io.Writer) int {
	return controlCommand("cancel", apply.ControlCancel, args, stdout, stderr)
}

func controlCommand(name string, verb apply.ControlVerb, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet(name, stderr)
	socket := flags.String("control", DefaultControlPath, "resident process control socket")
	if err := flags.Parse(args); err != nil {
		return parseExit(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "regied %s: takes no arguments\n", name)
		return 2
	}
	response, err := (apply.OSControl{}).Do(context.Background(), *socket, apply.ControlRequest{Verb: verb})
	if err != nil {
		return reportError(stderr, err)
	}
	if verb == apply.ControlConfirm {
		fmt.Fprintf(stdout, "Confirmed %s; last turn state: %s.\n", response.Revision, response.State)
	} else {
		fmt.Fprintf(stdout, "Cancelled the trial; converged toward %s with state: %s.\n", response.Revision, response.State)
	}
	return 0
}
