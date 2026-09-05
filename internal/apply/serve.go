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
	return serve(ctx, engine, resync, events, nil, log)
}

// ServeControl is Serve with the local control socket used for trial, confirm, and
// cancel. Those are the only messages the socket accepts.
func ServeControl(ctx context.Context, engine *Engine, resync <-chan time.Time, events <-chan struct{}, socket string, log io.Writer) error {
	requests, closeControl, err := engine.host.Control.Listen(ctx, socket, 0o660)
	if err != nil {
		return fmt.Errorf("cannot listen on control socket %s: %w", socket, err)
	}
	defer closeControl()
	return serve(ctx, engine, resync, events, requests, log)
}

func serve(ctx context.Context, engine *Engine, resync <-chan time.Time, events <-chan struct{}, requests <-chan ControlRequest, log io.Writer) error {
	var previousState State
	previousDrift := map[string]bool{}
	var trial *Declaration
	var trialBase string
	var trialDeadline time.Time
	var expiry <-chan time.Time
	revertTrial := func(reason string) (*Result, error) {
		trial, expiry = nil, nil
		fmt.Fprintf(log, "trial %s\n", reason)
		return reconcileLocked(ctx, engine)
	}
	run := func() {
		release, err := engine.LockTurn(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				fmt.Fprintf(log, "turn lock failed: %v\n", err)
			}
			return
		}
		if trial != nil {
			if record, err := engine.LoadRecord(); err == nil && record.Revision != trialBase {
				fmt.Fprintln(log, "trial ended by a plain apply")
				trial, expiry = nil, nil
			}
		}
		var result *Result
		var turnErr error
		if trial != nil {
			result, turnErr = engine.ReconcileTrial(ctx, *trial, true)
			if _, err := engine.ReportTrial(Revision(trial.Bytes), trialDeadline); err != nil && turnErr == nil {
				turnErr = err
			}
		} else {
			result, turnErr = engine.ReconcileUnattended(ctx)
		}
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
		case request, ok := <-requests:
			if !ok {
				requests = nil
				continue
			}
			switch request.Verb {
			case ControlTrial:
				candidate := Declaration{Bytes: request.Declaration, Source: request.Source}
				release, err := engine.LockTurn(ctx)
				if err != nil {
					request.Reply(ControlResponse{Error: err.Error()})
					continue
				}
				if record, err := engine.LoadRecord(); err == nil {
					trialBase = record.Revision
				} else {
					trialBase = ""
				}
				trial = &candidate
				trialDeadline = request.Deadline
				delay := trialDeadline.Sub(engine.host.Clock.Now())
				if delay < 0 {
					delay = 0
				}
				expiry = engine.host.Timer.After(delay)
				result, err := engine.SubmitTrial(ctx, candidate)
				_ = release()
				state := StateFailing
				if result != nil {
					state = result.State
				}
				if _, reportErr := engine.ReportTrial(Revision(candidate.Bytes), trialDeadline); reportErr != nil && err == nil {
					err = reportErr
				}
				fmt.Fprintf(log, "trial started: revision=%s deadline=%s\n", Revision(candidate.Bytes), trialDeadline.UTC().Format(time.RFC3339))
				response := ControlResponse{Revision: Revision(candidate.Bytes), State: state}
				if err != nil {
					response.Error = err.Error()
				}
				request.Reply(response)
			case ControlConfirm:
				if trial == nil {
					request.Reply(ControlResponse{Error: "there is no trial to confirm"})
					continue
				}
				release, lockErr := engine.LockTurn(ctx)
				if lockErr != nil {
					request.Reply(ControlResponse{Error: lockErr.Error()})
					continue
				}
				report, _ := engine.LastTurn()
				if err := engine.AcceptTrial(*trial); err != nil {
					_ = release()
					request.Reply(ControlResponse{Error: err.Error()})
					continue
				}
				revision := Revision(trial.Bytes)
				state := StateFailing
				if report != nil {
					state = report.State
				}
				trial, expiry = nil, nil
				if err := engine.ClearTrialReport(); err != nil {
					fmt.Fprintf(log, "confirmed trial report update failed: %v\n", err)
				}
				_ = release()
				fmt.Fprintf(log, "trial confirmed: revision=%s\n", revision)
				request.Reply(ControlResponse{Revision: revision, State: state})
			case ControlCancel:
				if trial == nil {
					request.Reply(ControlResponse{Error: "there is no trial to cancel"})
					continue
				}
				result, err := revertTrial("cancelled")
				response := ControlResponse{}
				if result != nil {
					response.Revision, response.State = result.Revision, result.State
				}
				if err != nil {
					response.Error = err.Error()
				}
				request.Reply(response)
			}
		case <-expiry:
			_, _ = revertTrial("expired")
		}
	}
}

func reconcileLocked(ctx context.Context, engine *Engine) (*Result, error) {
	release, err := engine.LockTurn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return engine.Reconcile(ctx)
}
