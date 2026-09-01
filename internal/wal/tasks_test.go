// tasks_test.go — the task system (05 §5.8): (repo,kind) single-flight join,
// lag-tolerant broadcast with replay, drain hooks, recent ring.
package wal

import (
	"context"
	"testing"
	"time"
)

func TestBroadcast_ReplayAndLagTolerance(t *testing.T) {
	var b Broadcast[int]
	b.Send(1)
	b.Send(2)
	id, ch, replay := b.Subscribe()
	if len(replay) != 2 || replay[0] != 1 || replay[1] != 2 {
		t.Fatalf("replay = %v, want [1 2]", replay)
	}
	// Non-blocking send: a full subscriber drops instead of blocking (§5.8).
	for i := 0; i < bcastSubCap+10; i++ {
		b.Send(100 + i)
	}
	dropped := 0
	deadline := time.After(time.Second)
	got := 0
	for got < bcastSubCap {
		select {
		case <-ch:
			got++
		case <-deadline:
			t.Fatalf("only %d of %d buffered packets arrived", got, bcastSubCap)
		}
	}
	_ = dropped
	select {
	case v := <-ch:
		t.Fatalf("buffer overflowed (got %d)", v)
	default:
	}
	// Unsubscribe closes the channel exactly once (owner: the broadcast).
	b.Unsubscribe(id)
	if _, ok := <-ch; ok {
		t.Fatal("channel open after Unsubscribe")
	}
	b.Unsubscribe(id) // idempotent
}

func TestTaskTable_JoinSemantics(t *testing.T) {
	tt := newTaskTable("test-host", context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	var runs int
	run := func(ctx context.Context, task *Task) error {
		runs++
		close(started)
		<-release
		task.Notice("done working")
		return nil
	}

	leaderDone := make(chan struct{})
	go func() {
		rec, err := tt.Run(context.Background(), "acme/api", "materialize", nil, run)
		if err != nil || rec == nil {
			t.Errorf("leader: %v", err)
		}
		close(leaderDone)
	}()
	<-started

	// A second start of the same (repo,kind) JOINS the running one.
	joinerDone := make(chan struct{})
	go func() {
		rec, err := tt.Run(context.Background(), "acme/api", "materialize", nil, run)
		if err != nil || rec == nil {
			t.Errorf("joiner: %v", err)
		}
		if rec != nil && rec.Summary != "" && rec.OK == nil {
			t.Error("joiner got a non-terminal record")
		}
		close(joinerDone)
	}()
	time.Sleep(20 * time.Millisecond) // the joiner must be waiting, not re-running
	if runs != 1 {
		t.Fatalf("runs = %d, want 1 (joiner must join, not re-run)", runs)
	}

	close(release)
	<-leaderDone
	<-joinerDone
	if runs != 1 {
		t.Fatalf("runs = %d after completion, want 1", runs)
	}
	recs := tt.List("acme/api")
	if len(recs) != 1 {
		t.Fatalf("recent = %d records, want 1", len(recs))
	}
	if recs[0].OK == nil || !*recs[0].OK {
		t.Fatalf("record not OK: %+v", recs[0])
	}
	if len(recs[0].LogTail) != 1 || recs[0].LogTail[0] != "done working" {
		t.Fatalf("log tail = %v", recs[0].LogTail)
	}
}

func TestTaskTable_DifferentKindsRunConcurrently(t *testing.T) {
	tt := newTaskTable("test-host", context.Background())
	blocked := make(chan struct{})
	release := make(chan struct{})
	block := func(ctx context.Context, task *Task) error {
		close(blocked)
		<-release
		return nil
	}
	go tt.Run(context.Background(), "acme/api", "materialize", nil, block)
	<-blocked
	// A different kind on the same repo must NOT join.
	done := make(chan struct{})
	go func() {
		defer close(done)
		tt.Run(context.Background(), "acme/api", "remote-index", nil, func(ctx context.Context, t *Task) error { return nil })
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("(repo, kind) collision: remote-index joined materialize")
	}
	close(release)
}

func TestTaskTable_DrainInterrupts(t *testing.T) {
	tt := newTaskTable("test-host", context.Background())
	blocked := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		rec, _ := tt.Run(context.Background(), "acme/api", "fsck", nil, func(tctx context.Context, t *Task) error {
			close(blocked)
			<-tctx.Done() // interrupted by drain
			return tctx.Err()
		})
		if rec != nil && rec.OK != nil && *rec.OK {
			t.Error("interrupted task recorded success")
		}
		close(drained)
	}()
	<-blocked
	tt.Drain()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("drain did not interrupt the running task")
	}
	// Further starts refuse while draining.
	if _, err := tt.Run(context.Background(), "acme/api", "fsck", nil, func(ctx context.Context, t *Task) error { return nil }); err == nil {
		t.Fatal("start during drain succeeded")
	}
}
