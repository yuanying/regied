package apply

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"
)

// AddressEvents wakes reconciliation when either address family changes. Events are
// latency hints only; every turn reads the kernel again.
type AddressEvents interface {
	Subscribe(context.Context) (<-chan struct{}, error)
}

// OSAddressEvents subscribes to rtnetlink address multicast groups.
type OSAddressEvents struct{}

const (
	rtmgrpIPv4Ifaddr = 0x10
	rtmgrpIPv6Ifaddr = 0x100
)

func (OSAddressEvents) Subscribe(ctx context.Context) (<-chan struct{}, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	groups := uint32(rtmgrpIPv4Ifaddr | rtmgrpIPv6Ifaddr)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK, Groups: groups}); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		defer syscall.Close(fd)
		go func() {
			<-ctx.Done()
			syscall.Close(fd)
		}()
		buffer := make([]byte, 8192)
		for {
			if _, _, err := syscall.Recvfrom(fd, buffer, 0); err != nil {
				return
			}
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}()
	return out, nil
}

// Serve keeps running unattended turns until ctx is cancelled. It performs one turn at
// startup, then reacts to the periodic resync and netlink address events. Cancellation
// only stops convergence; it changes no host state.
func Serve(ctx context.Context, engine *Engine, resync <-chan time.Time, events <-chan struct{}, log io.Writer) error {
	var previousState State
	previousDrift := map[string]bool{}
	run := func() {
		release, err := engine.LockTurn(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				fmt.Fprintf(log, "turn lock failed: %v\n", err)
			}
			return
		}
		result, turnErr := engine.ReconcileUnattended(ctx)
		if err := release(); err != nil {
			fmt.Fprintf(log, "turn lock release failed: %v\n", err)
		}

		state := StateFailing
		var drift []string
		if result != nil {
			state = result.State
			drift = append(drift, result.Plan.Waiting...)
			drift = append(drift, result.Plan.Failing...)
		}
		if turnErr != nil {
			drift = append(drift, turnErr.Error())
		}
		current := make(map[string]bool, len(drift))
		for _, item := range drift {
			current[item] = true
			if !previousDrift[item] {
				fmt.Fprintf(log, "drift appeared: %s\n", item)
			}
		}
		for item := range previousDrift {
			if !current[item] {
				fmt.Fprintf(log, "drift cleared: %s\n", item)
			}
		}
		if state != previousState {
			fmt.Fprintf(log, "state changed: %s", state)
			for _, item := range drift {
				fmt.Fprintf(log, "; %s", item)
			}
			fmt.Fprintln(log)
		}
		previousState, previousDrift = state, current
	}

	run()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-resync:
			run()
		case _, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			run()
		}
	}
}
