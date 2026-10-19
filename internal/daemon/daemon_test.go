package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
)

// syncBuf is a log sink safe for concurrent writers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog routes the default logger into a buffer for the test.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

func TestStateDir(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", "")
	if got := StateDir(); got != "/var/lib/pbp" {
		t.Errorf("StateDir = %q", got)
	}
	t.Setenv("PBP_STATE_DIR", "/tmp/x")
	if got := StateDir(); got != "/tmp/x" || LockPath() != "/tmp/x/run.lock" {
		t.Errorf("StateDir = %q, LockPath = %q", got, LockPath())
	}
}

func TestAcquireLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "run.lock")
	release, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("second AcquireLock err = %v, want ErrBusy", err)
	}
	release()
	release2, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()
}

func TestServeSkipsWhileRunActive(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	buf := captureLog(t)
	var active, maxActive, calls atomic.Int32
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	runFn := func(ctx context.Context, trigger string) {
		if trigger != "schedule" {
			t.Errorf("trigger = %q", trigger)
		}
		calls.Add(1)
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(2500 * time.Millisecond)
		active.Add(-1)
		stop() // shut down once the run ends, so no second run starts
	}

	if err := Serve(ctx, config.Config{Schedule: "@every 1s"}, runFn); err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 {
		t.Error("Serve returned while a run was active")
	}
	if maxActive.Load() != 1 || calls.Load() != 1 {
		t.Errorf("max concurrent = %d, calls = %d; want 1, 1", maxActive.Load(), calls.Load())
	}
	if !strings.Contains(buf.String(), "previous run still active, skipping") {
		t.Errorf("skip not logged: %s", buf)
	}
}

func TestServeSkipsWhenLockHeldElsewhere(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	buf := captureLog(t)
	release, err := AcquireLock(LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	if err := Serve(ctx, config.Config{Schedule: "@every 1s"}, func(context.Context, string) { calls.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Errorf("ran %d times while another process held the lock", calls.Load())
	}
	if !strings.Contains(buf.String(), "previous run still active, skipping") {
		t.Errorf("skip not logged: %s", buf)
	}
}

func TestServeHoldsLockDuringRun(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	captureLog(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var lockErr error
	runFn := func(context.Context, string) {
		defer stop()
		release, err := AcquireLock(LockPath())
		if err == nil {
			release()
		}
		lockErr = err
	}
	if err := Serve(ctx, config.Config{Schedule: "@every 1s"}, runFn); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(lockErr, ErrBusy) {
		t.Errorf("lock during run: %v, want ErrBusy", lockErr)
	}
	release, err := AcquireLock(LockPath())
	if err != nil {
		t.Fatalf("lock not released after run: %v", err)
	}
	release()
}

func TestServeShutdownGrace(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	captureLog(t)
	old := shutdownGrace
	shutdownGrace = 300 * time.Millisecond
	t.Cleanup(func() { shutdownGrace = old })

	started := make(chan struct{})
	var runCancelledAfter time.Duration
	var stopAt time.Time
	var mu sync.Mutex
	runFn := func(ctx context.Context, _ string) {
		close(started)
		<-ctx.Done()
		mu.Lock()
		runCancelledAfter = time.Since(stopAt)
		mu.Unlock()
		time.Sleep(100 * time.Millisecond) // teardown
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Serve(ctx, config.Config{Schedule: "@every 1s"}, runFn) }()
	<-started
	mu.Lock()
	stopAt = time.Now()
	mu.Unlock()
	cancel()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	if runCancelledAfter < shutdownGrace {
		t.Errorf("run cancelled %v after shutdown, want >= %v", runCancelledAfter, shutdownGrace)
	}
	if since := time.Since(stopAt); since < shutdownGrace+100*time.Millisecond {
		t.Errorf("Serve returned %v after shutdown, before the run's teardown finished", since)
	}
}

func TestServeBadSchedule(t *testing.T) {
	t.Setenv("PBP_STATE_DIR", t.TempDir())
	if err := Serve(context.Background(), config.Config{Schedule: "nope"}, nil); err == nil {
		t.Error("want error")
	}
}
