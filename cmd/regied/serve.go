package main

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/yuanying/regied/internal/apply"
)

func serveCommand(args []string, _ io.Writer, stderr io.Writer) int {
	flags := newFlagSet("serve", stderr)
	resync := flags.Duration("resync", time.Minute, "periodic reconciliation interval")
	if err := flags.Parse(args); err != nil {
		return parseExit(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "regied serve: takes no arguments; it reads the accepted declaration and nothing else")
		return 2
	}
	if *resync <= 0 {
		fmt.Fprintln(stderr, "regied serve: -resync must be greater than zero")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	events, err := (apply.OSAddressEvents{}).Subscribe(ctx)
	if err != nil {
		return reportError(stderr, fmt.Errorf("cannot subscribe to address events: %w", err))
	}
	ticker := time.NewTicker(*resync)
	defer ticker.Stop()
	engine := apply.New(apply.OSHost(), apply.Options{})
	if err := apply.Serve(ctx, engine, ticker.C, events, stderr); err != nil {
		return reportError(stderr, err)
	}
	return 0
}
