// locks_test.go — the try-first + measured lock discipline and checkpoint
// trigger math (05 §5.5/§5.9).
package wal

import (
	"context"
	"testing"

	"git.packden.us/crueber/walhub/internal/store/proto"
	"time"
)

func TestSyncMutex_LockMeasuredRecordsWait(t *testing.T) {
	var m syncMutex
	if err := m.LockMeasured(context.Background(), "test_lock", "x/y"); err != nil {
		t.Fatal(err)
	}
	m.Unlock()

	// Contended path: the second acquisition is queued and measured.
	m.mu.Lock()
	acquired := make(chan struct{})
	go func() {
		if err := m.LockMeasured(context.Background(), "test_lock", "x/y"); err != nil {
			t.Errorf("LockMeasured: %v", err)
		} else {
			defer m.Unlock() // balance the helper's acquisition
		}
		close(acquired)
	}()
	// Release from a helper so the waiter can complete; histogram must record.
	go func() {
		time.Sleep(20 * time.Millisecond)
		m.Unlock()
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("LockMeasured did not complete after unlock")
	}
	snaps := LockStatsSnapshot()
	if len(snaps["test_lock"]) == 0 {
		t.Fatal("no histogram recorded for test_lock")
	}

	// ctx abandonment: the waiter returns ctx.Err() and never deadlocks.
	m.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	go func() {
		if err := m.LockMeasured(ctx, "test_lock", "x/y"); err == nil {
			t.Error("abandoned acquisition returned nil")
		}
	}()
	time.Sleep(10 * time.Millisecond)
	m.mu.Unlock() // the handoff goroutine must not wedge the mutex
}

func TestCheckpointTriggerMath(t *testing.T) {
	base := &pbManifest{
		HeadSeq:     300,
		MinSeq:      1,
		LogSegments: []*proto.LogSegmentRef{{LastSeq: 300, Size: 1000}},
		UpdatedAt:   TsPtr(time.Now().UTC().Add(-2 * time.Hour)),
	}
	vals := &configVals{
		snapshotEveryEntries: 256,
		checkpointTailBytes:  8 << 20,
		checkpointInterval:   time.Hour,
	}

	cases := []struct {
		name    string
		mut     func(*pbManifest)
		vals    *configVals
		want    bool
		trigger CheckpointTrigger
	}{
		{"entries threshold", func(m *pbManifest) {}, vals, true, TriggerEntries},
		{"below entries threshold", func(m *pbManifest) {
			m.HeadSeq = 200
			m.UpdatedAt = TsPtr(time.Now()) // keep the age trigger out of it
		}, vals, false, ""},
		{"tail bytes", func(m *pbManifest) {
			m.HeadSeq = 10
			m.LogSegments = []*proto.LogSegmentRef{{LastSeq: 10, Size: 9 << 20}}
			m.UpdatedAt = TsPtr(time.Now())
		}, vals, true, TriggerTailBytes},
		{"tail bytes below", func(m *pbManifest) {
			m.HeadSeq = 10
			m.LogSegments = []*proto.LogSegmentRef{{LastSeq: 10, Size: 7 << 20}}
			m.UpdatedAt = TsPtr(time.Now())
		}, vals, false, ""},
		{"age", func(m *pbManifest) {
			m.HeadSeq = 10
			m.LogSegments = []*proto.LogSegmentRef{{LastSeq: 10, Size: 1000}}
			m.UpdatedAt = TsPtr(time.Now().Add(-2 * time.Hour))
		}, vals, true, TriggerAge},
		{"zero disables", func(m *pbManifest) {}, &configVals{}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := *base
			m2 := &m
			m2.LogSegments = base.LogSegments
			m2.UpdatedAt = base.UpdatedAt
			tc.mut(m2)
			trig, ok := checkpointTrigger(tc.vals, m2, time.Time{}, time.Time{})
			if ok != tc.want || (ok && trig != tc.trigger) {
				t.Fatalf("got (%q, %v), want (%q, %v)", trig, ok, tc.trigger, tc.want)
			}
		})
	}
}
