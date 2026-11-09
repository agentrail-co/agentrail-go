// Package delivery provides bounded, best-effort asynchronous batch delivery.
package delivery

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

type Config struct {
	MaxRecords, BatchSize                                        int
	ScheduledDelay, ExportTimeout, FlushTimeout, ShutdownTimeout time.Duration
}

type Stats struct{ Accepted, Exported, Dropped, Pending uint64 }
type Issue struct {
	Code  string
	Count int
	Err   error
}
type record[T any] struct {
	sequence uint64
	value    T
}
type batch[T any] struct {
	records []record[T]
	cancel  context.CancelFunc
	dropped bool
}

// Queue retains at most MaxRecords accepted records, including the active batch.
// Exports run serially. Export must honor its context; retry policy belongs to it.
// Configuration must be validated by the caller before New.
type Queue[T any] struct {
	cfg          Config
	export       func(context.Context, []T) error
	report       func(Issue)
	mu           sync.Mutex
	records      []record[T]
	active       *batch[T]
	stats        Stats
	force        uint64
	closed       bool
	changed      chan struct{}
	wake         chan struct{}
	done         chan struct{}
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

func New[T any](cfg Config, export func(context.Context, []T) error, report func(Issue)) *Queue[T] {
	q := &Queue[T]{cfg: cfg, export: export, report: report, changed: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{}), shutdownDone: make(chan struct{})}
	go q.run()
	return q
}

// Add never waits for an export. False means the record was dropped.
func (q *Queue[T]) Add(value T) bool {
	q.mu.Lock()
	retained := len(q.records)
	if q.active != nil {
		retained += len(q.active.records)
	}
	code := ""
	if q.closed {
		code = "closed"
	} else if retained >= q.cfg.MaxRecords {
		code = "queue_full"
	}
	if code != "" {
		q.stats.Dropped++
		q.mu.Unlock()
		q.issue(Issue{Code: code, Count: 1})
		return false
	}
	q.stats.Accepted++
	q.stats.Pending++
	q.records = append(q.records, record[T]{sequence: q.stats.Accepted, value: value})
	ready := len(q.records) >= q.cfg.BatchSize
	q.mu.Unlock()
	if ready {
		q.signal()
	}
	return true
}

func (q *Queue[T]) Diagnostics() Stats { q.mu.Lock(); defer q.mu.Unlock(); return q.stats }

// Flush attempts every record accepted at entry. Export failures are reported
// and dropped; only a flush deadline or cancellation is returned to the caller.
func (q *Queue[T]) Flush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, q.cfg.FlushTimeout)
	defer cancel()
	q.mu.Lock()
	target := q.stats.Accepted
	if target > q.force {
		q.force = target
	}
	q.mu.Unlock()
	q.signal()
	return q.wait(ctx, target)
}

// Shutdown closes admission immediately and shares one bounded final drain
// across all callers. Each caller can stop waiting through its own context.
func (q *Queue[T]) Shutdown(ctx context.Context) error {
	q.shutdownOnce.Do(func() {
		q.mu.Lock()
		q.closed = true
		target := q.stats.Accepted
		q.force = target
		q.mu.Unlock()
		q.signal()
		drainCtx, cancel := context.WithTimeout(ctx, q.cfg.ShutdownTimeout)
		go func() {
			defer cancel()
			err := q.wait(drainCtx, target)
			// On success the worker has no accepted work left. On a deadline it
			// exits once the cancelled exporter acknowledges cancellation.
			if err == nil {
				select {
				case <-q.done:
				case <-drainCtx.Done():
					err = drainCtx.Err()
				}
			}
			q.mu.Lock()
			q.shutdownErr = err
			q.mu.Unlock()
			close(q.shutdownDone)
		}()
	})
	select {
	case <-q.shutdownDone:
		q.mu.Lock()
		err := q.shutdownErr
		q.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue[T]) wait(ctx context.Context, target uint64) error {
	for {
		q.mu.Lock()
		pending := q.outstanding(target)
		changed := q.changed
		q.mu.Unlock()
		if !pending {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			q.abort(target, ctx.Err())
			return ctx.Err()
		}
	}
}

func (q *Queue[T]) outstanding(target uint64) bool {
	if q.active != nil && !q.active.dropped && q.active.records[0].sequence <= target {
		return true
	}
	return len(q.records) > 0 && q.records[0].sequence <= target
}

func (q *Queue[T]) abort(target uint64, err error) {
	q.mu.Lock()
	count := 0
	if q.active != nil && !q.active.dropped && q.active.records[0].sequence <= target {
		q.active.dropped = true
		q.active.cancel()
		count += len(q.active.records)
	}
	n := 0
	for n < len(q.records) && q.records[n].sequence <= target {
		n++
	}
	count += n
	// Clear references so a drained queue does not retain captured records.
	var zero record[T]
	for i := 0; i < n; i++ {
		q.records[i] = zero
	}
	q.records = q.records[n:]
	q.stats.Dropped += uint64(count)
	q.stats.Pending -= uint64(count)
	q.notify()
	q.mu.Unlock()
	if count > 0 {
		q.issue(Issue{Code: "deadline", Count: count, Err: err})
	}
	q.signal()
}

func (q *Queue[T]) run() {
	defer close(q.done)
	ticker := time.NewTicker(q.cfg.ScheduledDelay)
	defer ticker.Stop()
	for {
		select {
		case <-q.wake:
		case <-ticker.C:
			q.mu.Lock()
			q.force = q.stats.Accepted
			q.mu.Unlock()
		}
		for {
			q.mu.Lock()
			if len(q.records) == 0 {
				closed := q.closed
				q.mu.Unlock()
				if closed {
					return
				}
				break
			}
			if !q.closed && len(q.records) < q.cfg.BatchSize && q.records[0].sequence > q.force {
				q.mu.Unlock()
				break
			}
			n := min(len(q.records), q.cfg.BatchSize)
			// Do not sweep records accepted after a forced-drain snapshot into a
			// partial batch solely because that earlier snapshot requested a flush.
			if !q.closed && q.records[0].sequence <= q.force {
				for n > 1 && q.records[n-1].sequence > q.force {
					n--
				}
			}
			records := append([]record[T](nil), q.records[:n]...)
			var zero record[T]
			for i := 0; i < n; i++ {
				q.records[i] = zero
			}
			q.records = q.records[n:]
			ctx, cancel := context.WithTimeout(context.Background(), q.cfg.ExportTimeout)
			current := &batch[T]{records: records, cancel: cancel}
			q.active = current
			q.mu.Unlock()
			values := make([]T, len(records))
			for i, r := range records {
				values[i] = r.value
			}
			err := q.export(ctx, values)
			ctxErr := ctx.Err()
			cancel()
			if ctxErr != nil {
				err = ctxErr
			}
			q.mu.Lock()
			alreadyDropped := current.dropped
			if !alreadyDropped {
				q.stats.Pending -= uint64(n)
				if err == nil {
					q.stats.Exported += uint64(n)
				} else {
					q.stats.Dropped += uint64(n)
				}
			}
			q.active = nil
			q.notify()
			q.mu.Unlock()
			if !alreadyDropped && err != nil {
				code := "export_failed"
				if ctxErr != nil {
					code = "deadline"
				}
				q.issue(Issue{Code: code, Count: n, Err: err})
			}
		}
	}
}

func (q *Queue[T]) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
func (q *Queue[T]) notify() { close(q.changed); q.changed = make(chan struct{}) }
func (q *Queue[T]) issue(issue Issue) {
	if q.report == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintln(os.Stderr, "agentrail delivery diagnostic callback panicked")
		}
	}()
	q.report(issue)
}
