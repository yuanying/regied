package apply

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A turn holds a lock under the state directory for as long as it runs, so that a
// submission and the resident process never run a turn beside each other (ADR 0016).
func TestATurnLocksUnderTheStateDirectory(t *testing.T) {
	engine, _, _, host := planFixture(t)
	locker := host.Locker.(*fakeLocker)

	release, err := engine.LockTurn(context.Background())
	if err != nil {
		t.Fatalf("taking the lock failed: %v", err)
	}
	if len(locker.held) != 1 || locker.held[0] != "/var/lib/regied/turn/lock" {
		t.Errorf("the lock was taken on %v, want the one file under the state directory", locker.held)
	}
	if err := release(); err != nil {
		t.Fatalf("releasing the lock failed: %v", err)
	}
	if locker.released != 1 {
		t.Errorf("the lock was released %d times, want once", locker.released)
	}
}

// The lock is a lock across processes, so the real one is tested with the real kernel:
// the second holder waits until the first lets go, or until its context runs out.
func TestOSLockerMakesTheSecondHolderWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turn", "lock")
	first, err := OSLocker{}.Lock(context.Background(), path)
	if err != nil {
		t.Fatalf("the first lock failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := (OSLocker{}).Lock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the second holder got %v while the first held the lock, want to wait until the deadline", err)
	}

	if err := first(); err != nil {
		t.Fatalf("releasing the first lock failed: %v", err)
	}
	second, err := OSLocker{}.Lock(context.Background(), path)
	if err != nil {
		t.Fatalf("the second lock failed after the first was released: %v", err)
	}
	if err := second(); err != nil {
		t.Fatalf("releasing the second lock failed: %v", err)
	}
}
