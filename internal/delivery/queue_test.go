package delivery

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func config() Config {
	return Config{MaxRecords: 4, BatchSize: 2, ScheduledDelay: time.Hour, ExportTimeout: time.Second, FlushTimeout: time.Second, ShutdownTimeout: time.Second}
}
func wait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}
func stop[T any](t *testing.T, q *Queue[T]) {
	t.Helper()
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Fails if capacity counts only waiting records and forgets the in-flight batch.
func TestCapacityIncludesInflight(t *testing.T) {
	started := make(chan []int, 2)
	release := make(chan struct{})
	q := New(config(), func(ctx context.Context, batch []int) error {
		started <- batch
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil)
	if !q.Add(1) || !q.Add(2) {
		t.Fatal("initial records rejected")
	}
	got := wait(t, started)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("batch %v", got)
	}
	if !q.Add(3) || !q.Add(4) || q.Add(5) {
		t.Fatal("inflight capacity not enforced")
	}
	if got := q.Diagnostics(); got != (Stats{Accepted: 4, Dropped: 1, Pending: 4}) {
		t.Fatalf("stats %+v", got)
	}
	close(release)
	stop(t, q)
	if got := q.Diagnostics(); got != (Stats{Accepted: 4, Exported: 4, Dropped: 1}) {
		t.Fatalf("stats %+v", got)
	}
}

// Fails if an export failure is retried, left pending, or counted as delivered.
func TestFailureDropsAndReports(t *testing.T) {
	problem := errors.New("offline")
	issues := make(chan Issue, 2)
	q := New(config(), func(context.Context, []int) error { return problem }, func(i Issue) { issues <- i })
	q.Add(1)
	if err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	issue := wait(t, issues)
	if issue.Code != "export_failed" || issue.Count != 1 || !errors.Is(issue.Err, problem) {
		t.Fatalf("issue %+v", issue)
	}
	if got := q.Diagnostics(); got != (Stats{Accepted: 1, Dropped: 1}) {
		t.Fatalf("stats %+v", got)
	}
	stop(t, q)
}

// Fails if scheduled delivery never drains a batch smaller than BatchSize.
func TestScheduledDrain(t *testing.T) {
	cfg := config()
	cfg.ScheduledDelay = 5 * time.Millisecond
	sent := make(chan int, 1)
	q := New(cfg, func(_ context.Context, b []int) error { sent <- b[0]; return nil }, nil)
	q.Add(7)
	if got := wait(t, sent); got != 7 {
		t.Fatal(got)
	}
	stop(t, q)
}

// Fails if flush waits for records added after its accepted snapshot.
func TestFlushSnapshot(t *testing.T) {
	cfg := config()
	cfg.BatchSize = 4
	started := make(chan []int, 2)
	release := make(chan struct{})
	q := New(cfg, func(ctx context.Context, b []int) error {
		started <- b
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil)
	q.Add(1)
	flushed := make(chan error, 1)
	go func() { flushed <- q.Flush(context.Background()) }()
	wait(t, started)
	q.Add(2)
	close(release)
	if err := wait(t, flushed); err != nil {
		t.Fatal(err)
	}
	if got := q.Diagnostics(); got.Pending != 1 {
		t.Fatalf("flush consumed later record: %+v", got)
	}
	stop(t, q)
}

// Fails if a flush deadline leaves accepted records pending or fails to cancel export.
func TestFlushDeadlineCancelsAndDrops(t *testing.T) {
	cfg := config()
	cfg.FlushTimeout = 20 * time.Millisecond
	started := make(chan struct{})
	cancelled := make(chan struct{})
	issues := make(chan Issue, 4)
	q := New(cfg, func(ctx context.Context, _ []int) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}, func(i Issue) { issues <- i })
	q.Add(1)
	q.Add(2)
	wait(t, started)
	q.Add(3)
	if err := q.Flush(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v", err)
	}
	wait(t, cancelled)
	if got := q.Diagnostics(); got.Pending != 0 || got.Dropped != 3 {
		t.Fatalf("stats %+v", got)
	}
	issue := wait(t, issues)
	if issue.Code != "deadline" || issue.Count != 3 {
		t.Fatalf("issue %+v", issue)
	}
	stop(t, q)
}

// Fails if exporter timeout is not applied independently of Flush.
func TestExportTimeout(t *testing.T) {
	cfg := config()
	cfg.ExportTimeout = 10 * time.Millisecond
	issues := make(chan Issue, 1)
	q := New(cfg, func(ctx context.Context, _ []int) error { <-ctx.Done(); return ctx.Err() }, func(i Issue) { issues <- i })
	q.Add(1)
	q.Add(2)
	issue := wait(t, issues)
	if issue.Code != "deadline" || issue.Count != 2 {
		t.Fatalf("issue %+v", issue)
	}
	stop(t, q)
}

// Fails if concurrent shutdowns export accepted records twice or admit late records.
func TestConcurrentShutdownFinalDrain(t *testing.T) {
	cfg := config()
	cfg.MaxRecords = 1000
	cfg.BatchSize = 1000
	var delivered atomic.Int64
	var active atomic.Int64
	q := New(cfg, func(_ context.Context, b []int) error {
		if active.Add(1) != 1 {
			t.Error("concurrent export")
		}
		delivered.Add(int64(len(b)))
		active.Add(-1)
		return nil
	}, nil)
	for i := 0; i < 50; i++ {
		q.Add(i)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := q.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if q.Add(100) {
		t.Fatal("accepted after shutdown")
	}
	if delivered.Load() != 50 {
		t.Fatalf("delivered %d", delivered.Load())
	}
	if got := q.Diagnostics(); got != (Stats{Accepted: 50, Exported: 50, Dropped: 1}) {
		t.Fatalf("stats %+v", got)
	}
}

// Fails if shutdown misses the last accepted record racing its final drain.
func TestAddRacesFinalDrain(t *testing.T) {
	for run := 0; run < 50; run++ {
		cfg := config()
		cfg.MaxRecords = 1000
		var delivered atomic.Uint64
		q := New(cfg, func(_ context.Context, b []int) error { delivered.Add(uint64(len(b))); return nil }, nil)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				q.Add(i)
			}
		}()
		stop(t, q)
		wg.Wait()
		stats := q.Diagnostics()
		if stats.Pending != 0 || stats.Exported != stats.Accepted || delivered.Load() != stats.Accepted {
			t.Fatalf("stats %+v delivered %d", stats, delivered.Load())
		}
	}
}

// Fails if a user diagnostic callback panic terminates queue delivery.
func TestReportPanicContained(t *testing.T) {
	q := New(config(), func(context.Context, []int) error { return errors.New("offline") }, func(Issue) { panic("callback") })
	q.Add(1)
	if err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop(t, q)
	if q.Diagnostics().Dropped != 1 {
		t.Fatal("missing drop")
	}
}

// Fails if shutdown lacks its own deadline or leaves its final batch pending.
func TestShutdownDeadline(t *testing.T) {
	cfg := config()
	cfg.ShutdownTimeout = 15 * time.Millisecond
	cancelled := make(chan struct{})
	issues := make(chan Issue, 1)
	q := New(cfg, func(ctx context.Context, _ []int) error { <-ctx.Done(); close(cancelled); return ctx.Err() }, func(i Issue) { issues <- i })
	q.Add(1)
	if err := q.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error %v", err)
	}
	wait(t, cancelled)
	if got := q.Diagnostics(); got != (Stats{Accepted: 1, Dropped: 1}) {
		t.Fatalf("stats %+v", got)
	}
	if err := q.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second shutdown error %v", err)
	}
	issue := wait(t, issues)
	if issue.Code != "deadline" || issue.Count != 1 {
		t.Fatalf("issue %+v", issue)
	}
}

// Fails if rejected records go unreported or reporting holds the queue lock.
func TestRejectionsReportOutsideLock(t *testing.T) {
	cfg := config()
	cfg.MaxRecords = 1
	cfg.BatchSize = 2
	issues := make(chan Issue, 2)
	var q *Queue[int]
	q = New(cfg, func(context.Context, []int) error { return nil }, func(i Issue) { q.Diagnostics(); issues <- i })
	q.Add(1)
	if q.Add(2) {
		t.Fatal("full queue accepted record")
	}
	if issue := wait(t, issues); issue.Code != "queue_full" || issue.Count != 1 {
		t.Fatalf("issue %+v", issue)
	}
	stop(t, q)
	if q.Add(3) {
		t.Fatal("closed queue accepted record")
	}
	if issue := wait(t, issues); issue.Code != "closed" || issue.Count != 1 {
		t.Fatalf("issue %+v", issue)
	}
}

// Fails if deadline accounting frees capacity before the cancelled export exits.
func TestCancelledInflightRetainsCapacity(t *testing.T) {
	cfg := config()
	cfg.MaxRecords = 2
	cfg.FlushTimeout = 10 * time.Millisecond
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	q := New(cfg, func(ctx context.Context, _ []int) error {
		if calls.Add(1) > 1 {
			return nil
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	}, nil)
	q.Add(1)
	q.Add(2)
	wait(t, started)
	if err := q.Flush(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	wait(t, cancelled)
	accepted := q.Add(3)
	close(release)
	stop(t, q)
	if accepted {
		t.Fatal("accepted beyond retained in-flight capacity")
	}
}

// Fails if a batch is closed by record count alone, so records that together
// exceed the byte limit share one export, or if a record over the limit is
// never sent, or if a record leaves the queue without its Done callback.
func TestBatchesCloseBeforeTheirByteLimit(t *testing.T) {
	cfg := Config{MaxRecords: 10, BatchSize: 10, MaxBatchBytes: 100, ScheduledDelay: time.Hour, ExportTimeout: time.Second, FlushTimeout: time.Second, ShutdownTimeout: time.Second}
	var mu sync.Mutex
	batches := [][]int{}
	done := 0
	q := NewWithHooks(cfg, func(_ context.Context, b []int) error {
		mu.Lock()
		defer mu.Unlock()
		batches = append(batches, append([]int(nil), b...))
		return nil
	}, nil, Hooks[int]{Size: func(n int) int { return n }, Done: func(int) { mu.Lock(); done++; mu.Unlock() }})
	for _, size := range []int{40, 40, 40, 150, 10} {
		if !q.Add(size) {
			t.Fatal("record rejected")
		}
	}
	if err := q.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop(t, q)
	mu.Lock()
	defer mu.Unlock()
	want := [][]int{{40, 40}, {40}, {150}, {10}}
	if len(batches) != len(want) {
		t.Fatalf("batches %v", batches)
	}
	for i := range want {
		if len(batches[i]) != len(want[i]) || batches[i][0] != want[i][0] {
			t.Fatalf("batches %v, want %v", batches, want)
		}
	}
	if done != 5 {
		t.Fatalf("done called %d times", done)
	}
}
