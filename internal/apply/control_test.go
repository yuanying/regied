package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const trialFixture = "  resources: []\n"

type fakeTimer struct{ elapsed chan time.Time }

func (t *fakeTimer) After(time.Duration) <-chan time.Time {
	t.elapsed = make(chan time.Time, 1)
	return t.elapsed
}

func TestUnixControlCarriesExactlyFourSubmissionMessages(t *testing.T) {
	transport := OSControl{}
	path := filepath.Join(t.TempDir(), "control.sock")
	requests, closeControl, err := transport.Listen(context.Background(), path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()

	for _, verb := range []ControlVerb{ControlSubmit, ControlTrial, ControlConfirm, ControlCancel} {
		done := make(chan error, 1)
		go func() {
			_, err := transport.Do(context.Background(), path, ControlRequest{Verb: verb})
			done <- err
		}()
		request := <-requests
		if request.Verb != verb {
			t.Fatalf("received %q, want %q", request.Verb, verb)
		}
		request.Reply(ControlResponse{Revision: "sha256:test", State: StateWaiting})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	if _, err := transport.Do(context.Background(), path, ControlRequest{Verb: "status"}); err == nil {
		t.Fatal("an unsupported control verb was accepted")
	}
}

func TestClientDisconnectDoesNotCancelTheDaemonsTurn(t *testing.T) {
	host, files, _ := testHost()
	engine := New(host, Options{StateDir: "/state"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(t.TempDir(), "control.sock")
	done := make(chan error, 1)
	go func() { done <- ServeControl(ctx, engine, nil, nil, socket, &bytes.Buffer{}) }()
	waitForSocket(t, socket)

	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	request := ControlRequest{Verb: ControlSubmit, Declaration: declarationOf(trialFixture), Source: "/tmp/config.yaml"}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 100; i++ {
		if got, ok := files.content("/state/accepted/declaration.yaml"); ok && got == string(request.Declaration) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the daemon abandoned the submission when its client disconnected")
}

func TestClientCanStopWaitingForADaemonThatDoesNotAnswer(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() { conn, _ := listener.Accept(); accepted <- conn }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlSubmit, Declaration: declarationOf(trialFixture)})
		done <- err
	}()
	conn := <-accepted
	defer conn.Close()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unanswered request returned %v, want context deadline", err)
	}
}

func TestServeCreatesARootOnlySocket(t *testing.T) {
	host, _, _ := testHost()
	engine := New(host, Options{StateDir: "/state"})
	ctx, cancel := context.WithCancel(context.Background())
	socket := filepath.Join(t.TempDir(), "control.sock")
	done := make(chan error, 1)
	go func() { done <- ServeControl(ctx, engine, nil, nil, socket, &bytes.Buffer{}) }()
	waitForSocket(t, socket)
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %04o, want 0600", got)
	}
	cancel()
	<-done
}

func TestSecondServeIsRefusedWhileDaemonOwnsTheLock(t *testing.T) {
	state := t.TempDir()
	host, _, _ := testHost()
	host.Locker = OSLocker{}
	first := New(host, Options{StateDir: state})
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	firstSocket := filepath.Join(t.TempDir(), "first.sock")
	go func() { firstDone <- ServeControl(ctx, first, nil, nil, firstSocket, &bytes.Buffer{}) }()
	waitForSocket(t, firstSocket)

	second := New(host, Options{StateDir: state})
	err := ServeControl(context.Background(), second, nil, nil, filepath.Join(t.TempDir(), "second.sock"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "another resident process") {
		t.Fatalf("second serve returned %v, want ownership refusal", err)
	}
	cancel()
	<-firstDone
}

func TestTrialWithoutDeadlineIsRejected(t *testing.T) {
	host, _, _ := testHost()
	engine := New(host, Options{StateDir: "/state"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(t.TempDir(), "control.sock")
	done := make(chan error, 1)
	go func() { done <- ServeControl(ctx, engine, nil, nil, socket, &bytes.Buffer{}) }()
	waitForSocket(t, socket)

	_, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlTrial, Declaration: declarationOf(trialFixture)})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("trial without a deadline returned %v, want a deadline error", err)
	}
}

func TestSubmitReturnsTheCompleteTurnReport(t *testing.T) {
	host, _, _ := testHost()
	engine := New(host, Options{StateDir: "/state"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(t.TempDir(), "control.sock")
	done := make(chan error, 1)
	go func() { done <- ServeControl(ctx, engine, nil, nil, socket, &bytes.Buffer{}) }()
	waitForSocket(t, socket)

	response, err := (OSControl{}).Do(ctx, socket, ControlRequest{
		Verb: ControlSubmit, Declaration: declarationOf(trialFixture), Source: "/tmp/config.yaml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Report == nil || response.Report.Revision == "" || response.Report.Source != "/tmp/config.yaml" {
		t.Fatalf("submit response has no complete report: %#v", response)
	}
}

func TestServeConfirmsATrialAndMakesItTheRecord(t *testing.T) {
	host, files, _ := testHost()
	host.Control = OSControl{}
	engine := New(host, Options{StateDir: "/state"})
	mustSubmit(t, engine, trialFixture, "/etc/regied/config.yaml")
	replacement := declarationOf("  global:\n    ipForwarding: false\n  resources: []\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(t.TempDir(), "control.sock")
	done := make(chan error, 1)
	go func() { done <- ServeControl(ctx, engine, nil, nil, socket, &bytes.Buffer{}) }()
	waitForSocket(t, socket)
	deadline := time.Now().Add(time.Minute)
	if _, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlTrial, Declaration: replacement, Deadline: deadline}); err != nil {
		t.Fatal(err)
	}
	response, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlConfirm})
	if err != nil {
		t.Fatal(err)
	}
	if response.Revision != Revision(replacement) {
		t.Fatalf("confirmed %q", response.Revision)
	}
	if got, _ := files.content("/state/accepted/declaration.yaml"); got != string(replacement) {
		t.Fatal("confirmation did not make the trial durable")
	}
	report, err := engine.LastTurn()
	if err != nil || report.Trial {
		t.Fatalf("trial diagnosis remained after confirmation: %#v, %v", report, err)
	}
	cancel()
	<-done
}

func TestStoppingDuringATrialLeavesThePreviousRecord(t *testing.T) {
	host, files, _ := testHost()
	host.Control = OSControl{}
	engine := New(host, Options{StateDir: "/state"})
	mustSubmit(t, engine, trialFixture, "/etc/regied/config.yaml")
	accepted, _ := files.content("/state/accepted/declaration.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	socket := filepath.Join(t.TempDir(), "control.sock")
	done := make(chan error, 1)
	go func() { done <- ServeControl(ctx, engine, nil, nil, socket, &bytes.Buffer{}) }()
	waitForSocket(t, socket)
	_, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlTrial, Declaration: declarationOf("  global:\n    ipForwarding: false\n  resources: []\n"), Deadline: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
	if got, _ := files.content("/state/accepted/declaration.yaml"); got != accepted {
		t.Fatal("a trial survived daemon shutdown in the record")
	}
}

func TestCancelAndExpiryReconcileTowardThePreviousRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(context.Context, string, *fakeTimer) error
	}{
		{name: "cancel", end: func(ctx context.Context, socket string, _ *fakeTimer) error {
			_, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlCancel})
			return err
		}},
		{name: "expiry", end: func(ctx context.Context, socket string, timer *fakeTimer) error {
			timer.elapsed <- time.Now()
			for i := 0; i < 100; i++ {
				_, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlConfirm})
				if err != nil && strings.Contains(err.Error(), "no trial") {
					return nil
				}
				time.Sleep(time.Millisecond)
			}
			return errors.New("trial did not expire")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, _, runner := testHost()
			host.Control = OSControl{}
			timer := &fakeTimer{}
			host.Timer = timer
			engine := New(host, Options{StateDir: "/state"})
			mustSubmit(t, engine, trialFixture, "/etc/regied/config.yaml")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			socket := filepath.Join(t.TempDir(), "control.sock")
			done := make(chan error, 1)
			go func() { done <- ServeControl(ctx, engine, nil, nil, socket, &bytes.Buffer{}) }()
			waitForSocket(t, socket)
			trial := declarationOf("  global:\n    ipForwarding: false\n  resources: []\n")
			if _, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlTrial, Declaration: trial, Deadline: time.Now().Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			runner.ran = nil
			if err := tc.end(ctx, socket, timer); err != nil {
				t.Fatal(err)
			}
			if len(runner.ran) == 0 {
				t.Fatal("ending the trial did not run a submitted revert turn")
			}
			if _, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlConfirm}); err == nil || !strings.Contains(err.Error(), "no trial") {
				t.Fatalf("confirm after %s returned %v", tc.name, err)
			}
		})
	}
}

func TestASecondTrialReplacesTheFirst(t *testing.T) {
	host, files, _ := testHost()
	host.Control = OSControl{}
	engine := New(host, Options{StateDir: "/state"})
	mustSubmit(t, engine, trialFixture, "/etc/regied/config.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(t.TempDir(), "control.sock")
	done := make(chan error, 1)
	go func() { done <- ServeControl(ctx, engine, nil, nil, socket, &bytes.Buffer{}) }()
	waitForSocket(t, socket)
	first := declarationOf("  global:\n    ipForwarding: false\n  resources: []\n")
	second := declarationOf("  global:\n    ipForwarding: true\n  resources: []\n")
	for _, declaration := range [][]byte{first, second} {
		if _, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlTrial, Declaration: declaration, Deadline: time.Now().Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	response, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlConfirm})
	if err != nil {
		t.Fatal(err)
	}
	if response.Revision != Revision(second) {
		t.Fatalf("confirmed %q, want second trial %q", response.Revision, Revision(second))
	}
	if got, _ := files.content("/state/accepted/declaration.yaml"); got != string(second) {
		t.Fatal("confirmation did not accept the replacement trial")
	}
}

func TestAPlainApplyEndsTheActiveTrial(t *testing.T) {
	host, _, _ := testHost()
	host.Control = OSControl{}
	engine := New(host, Options{StateDir: "/state"})
	mustSubmit(t, engine, trialFixture, "/etc/regied/config.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan struct{}, 1)
	socket := filepath.Join(t.TempDir(), "control.sock")
	done := make(chan error, 1)
	go func() { done <- ServeControl(ctx, engine, nil, events, socket, &bytes.Buffer{}) }()
	waitForSocket(t, socket)
	trial := declarationOf("  global:\n    ipForwarding: false\n  resources: []\n")
	if _, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlTrial, Declaration: trial, Deadline: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	plain := "  global:\n    ipForwarding: true\n  resources: []\n"
	if _, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlSubmit, Declaration: declarationOf(plain), Source: "/etc/regied/config.yaml"}); err != nil {
		t.Fatal(err)
	}
	events <- struct{}{}
	for i := 0; i < 100; i++ {
		_, err := (OSControl{}).Do(ctx, socket, ControlRequest{Verb: ControlConfirm})
		if err != nil && strings.Contains(err.Error(), "no trial") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("plain apply did not end the active trial")
}

func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if _, err := (OSControl{}).Do(context.Background(), socket, ControlRequest{Verb: ControlConfirm}); err != nil && !strings.Contains(err.Error(), "connect") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("control socket did not start")
}

func TestTrialDoesNotReplaceTheAcceptedDeclaration(t *testing.T) {
	host, files, _ := testHost()
	engine := New(host, Options{StateDir: "/state"})
	mustSubmit(t, engine, trialFixture, "/etc/regied/config.yaml")
	accepted, _ := files.content("/state/accepted/declaration.yaml")
	replacement := "  global:\n    ipForwarding: false\n  resources: []\n"

	result, err := engine.SubmitTrial(context.Background(), Declaration{Bytes: declarationOf(replacement), Source: "/tmp/trial.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := files.content("/state/accepted/declaration.yaml"); got != accepted {
		t.Fatal("the trial replaced the accepted declaration")
	}
	if result.Revision != Revision(declarationOf(replacement)) {
		t.Fatalf("trial revision = %q", result.Revision)
	}
}

func TestTrialReportNamesTheDeadline(t *testing.T) {
	host, _, _ := testHost()
	engine := New(host, Options{StateDir: "/state"})
	mustSubmit(t, engine, trialFixture, "/etc/regied/config.yaml")
	deadline := time.Date(2026, 9, 5, 12, 5, 0, 0, time.UTC)
	report, err := engine.ReportTrial(Revision(declarationOf(trialFixture)), deadline)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Trial || !report.TrialDeadline.Equal(deadline) {
		t.Fatalf("trial diagnosis = %#v", report)
	}
}
