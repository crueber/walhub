package bundle

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// ErrRetriesExhausted reports a CAS ladder that exceeded maxRetries
// (§8.11: the pass reports and retries next pass; nothing is lost).
var ErrRetriesExhausted = errors.New("bundle: cas retries exhausted")

// errCASAbort is the sentinel a mutator returns (nil, nil) to bail without
// writing (§8.11).
var errCASAbort = errors.New("bundle: cas abort")

// BundleListKey is the repo-relative advertisement state key (§8.11).
const BundleListKey = store.BundleList

// updateList is the generic CAS helper over bundles/list.pb (doc 02 §2.7,
// §8.11): read-modify-CAS; 412 → re-read and retry immediately, counted to
// maxRetries; Retryable → jittered 5→100 ms backoff (does not consume a
// conflict retry); f returning (nil, nil) aborts without writing.
func updateList(ctx context.Context, st store.ObjectStore, maxRetries int, f func(cur *proto.BundleList) (*proto.BundleList, error)) error {
	key := BundleListKey
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, meta, err := store.GetBytes(ctx, st, key, store.GetOptions{})
		if err != nil && !store.IsNotFound(err) {
			if store.IsRetryable(err) {
				if serr := backoff(ctx, attempt); serr != nil {
					return serr
				}
				attempt-- // retryable backoff does not consume a CAS retry (§8.11)
				continue
			}
			return err
		}
		var cur *proto.BundleList
		if meta.Key != "" {
			cur, err = proto.UnmarshalBundleList(body)
			if err != nil {
				return store.NewCorrupt(key, err)
			}
		}
		existed := meta.Key != ""
		next, err := f(cur)
		if err != nil {
			return err
		}
		if next == nil {
			return nil // aborted without writing
		}
		ts := proto.TimeFromGo(time.Now())
		next.UpdatedAt = &ts
		opts := store.PutOptions{Mode: store.PutCreate}
		if existed {
			opts = store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version}
		}
		if _, err := st.Put(ctx, key, store.PutBody{Bytes: next.Marshal()}, opts); err != nil {
			if store.IsPreconditionFailed(err) {
				continue // 412 → immediate re-read, retry counted (§8.11)
			}
			if store.IsRetryable(err) {
				if serr := backoff(ctx, attempt); serr != nil {
					return serr
				}
				attempt--
				continue
			}
			return err
		}
		return nil
	}
	return ErrRetriesExhausted
}

// backoff sleeps 5 ms doubling to a 100 ms cap, jittered, context-cancelled.
func backoff(ctx context.Context, attempt int) error {
	d := 5 * time.Millisecond
	for range attempt {
		d *= 2
		if d >= 100*time.Millisecond {
			d = 100 * time.Millisecond
			break
		}
	}
	d += time.Duration(rand.Int63n(int64(d/4 + 1)))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// UpsertEntry publishes one built bundle into the list (§8.9.6 step 3, 8
// retries): upsert {id, key, strategy, kind, creation_token, seq, size,
// base_id, created_at, version, tips, slot, filter}, stamp mode/heuristic and
// updated_at. Existing entries with the same id are replaced.
func UpsertEntry(ctx context.Context, st store.ObjectStore, e *proto.BundleEntry) error {
	return updateList(ctx, st, 8, func(cur *proto.BundleList) (*proto.BundleList, error) {
		if cur == nil {
			cur = &proto.BundleList{Mode: "all", Heuristic: "creationToken"}
		}
		if cur.Mode == "" {
			cur.Mode = "all"
		}
		if cur.Heuristic == "" {
			cur.Heuristic = "creationToken"
		}
		replaced := false
		for i, old := range cur.Bundles {
			if old.ID == e.ID {
				cur.Bundles[i] = e
				replaced = true
				break
			}
		}
		if !replaced {
			cur.Bundles = append(cur.Bundles, e)
		}
		ts := proto.TimeFromGo(time.Now())
		cur.UpdatedAt = &ts
		return cur, nil
	})
}

// RemoveEntries removes entries by id and returns the object keys that were
// present in the committed old list and absent from the new list (§8.11:
// deletes happen only after the committing CAS).
func RemoveEntries(ctx context.Context, st store.ObjectStore, ids []string) (removedKeys []string, err error) {
	var old, next *proto.BundleList
	err = updateList(ctx, st, 8, func(cur *proto.BundleList) (*proto.BundleList, error) {
		if cur == nil {
			return nil, nil // nothing to remove; abort without writing
		}
		old = &proto.BundleList{Bundles: cur.Bundles}
		drop := make(map[string]bool, len(ids))
		for _, id := range ids {
			drop[id] = true
		}
		kept := make([]*proto.BundleEntry, 0, len(cur.Bundles))
		for _, e := range cur.Bundles {
			if !drop[e.ID] {
				kept = append(kept, e)
			}
		}
		if len(kept) == len(cur.Bundles) {
			return nil, nil // no change; abort without writing
		}
		next = &proto.BundleList{Bundles: kept}
		next.Mode, next.Heuristic, next.Skipped = cur.Mode, cur.Heuristic, cur.Skipped
		ts := proto.TimeFromGo(time.Now())
		next.UpdatedAt = &ts
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	if old == nil || next == nil {
		return nil, nil
	}
	newKeys := make(map[string]bool, len(next.Bundles))
	for _, e := range next.Bundles {
		newKeys[e.Key] = true
	}
	for _, e := range old.Bundles {
		if e.Key != "" && !newKeys[e.Key] {
			removedKeys = append(removedKeys, e.Key)
		}
	}
	return removedKeys, nil
}

// RecordVerdicts batch-commits closed-slot verdicts (§8.7): one CAS per pass.
// Keyed final per (strategy, slot, base_id) — an existing record with the same
// key is replaced (a new base bundle re-opens the slot; settle drops stale
// records).
func RecordVerdicts(ctx context.Context, st store.ObjectStore, verdicts []*proto.SkippedSlot) error {
	if len(verdicts) == 0 {
		return nil
	}
	return updateList(ctx, st, 8, func(cur *proto.BundleList) (*proto.BundleList, error) {
		if cur == nil {
			cur = &proto.BundleList{Mode: "all", Heuristic: "creationToken"}
		}
		for _, v := range verdicts {
			replaced := false
			for i, old := range cur.Skipped {
				if old.Strategy == v.Strategy && old.Slot == v.Slot && old.BaseID == v.BaseID {
					cur.Skipped[i] = v
					replaced = true
					break
				}
			}
			if !replaced {
				cur.Skipped = append(cur.Skipped, v)
			}
		}
		ts := proto.TimeFromGo(time.Now())
		cur.UpdatedAt = &ts
		return cur, nil
	})
}

// settleSkipped drops stale verdict records (§8.7): records whose slot now has
// an entry, whose base_id no longer matches the slot's current base resolution,
// or whose slot left the plan window.
func settleSkipped(cur *proto.BundleList, strategies []Strategy, byName map[string]*Strategy, planWindows map[string][]uint64) *proto.BundleList {
	if cur == nil || len(cur.Skipped) == 0 {
		return cur
	}
	keep := cur.Skipped[:0]
	for _, v := range cur.Skipped {
		s := byName[v.Strategy]
		if s == nil {
			continue // strategy gone from config: leave the record alone
		}
		// Slot now has an entry → re-opened and settled.
		if entryAt(cur, v.Strategy, v.Slot) != nil {
			continue
		}
		// Slot left the plan window.
		window, ok := planWindows[v.Strategy]
		if !ok || !inWindow(window, v.Slot) {
			continue
		}
		// base_id no longer matches the slot's current base resolution.
		wantBase, err := BaseIDFor(s, v.Slot, cur, byName)
		if err == nil && wantBase != v.BaseID {
			continue
		}
		keep = append(keep, v)
	}
	cur.Skipped = keep
	return cur
}

// SettleAndPrune runs the settle step + retention prune in ONE CAS (§8.7
// pass budget: list GET → ≤ 1 retention CAS → ≤ 1 verdict-batch CAS) and
// returns the object keys removed from the list (delete after the CAS).
func SettleAndPrune(ctx context.Context, st store.ObjectStore, strategies []Strategy, now time.Time) (removedKeys []string, err error) {
	byName := ByName(strategies)
	var old, next *proto.BundleList
	err = updateList(ctx, st, 8, func(cur *proto.BundleList) (*proto.BundleList, error) {
		if cur == nil {
			return nil, nil
		}
		old = snapshotOf(cur)
		windows := PlanWindows(strategies, byName, cur, now)
		cur = settleSkipped(cur, strategies, byName, windows)
		cur = pruneRetention(cur, strategies, byName)
		next = cur
		if next.Mode == "" {
			next.Mode = "all"
		}
		if next.Heuristic == "" {
			next.Heuristic = "creationToken"
		}
		ts := proto.TimeFromGo(time.Now())
		next.UpdatedAt = &ts
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	if old == nil || next == nil {
		return nil, nil
	}
	newKeys := make(map[string]bool, len(next.Bundles))
	for _, e := range next.Bundles {
		newKeys[e.Key] = true
	}
	oldKeys := make(map[string]bool, len(old.Bundles))
	for _, e := range old.Bundles {
		oldKeys[e.Key] = true
	}
	for _, e := range old.Bundles {
		if e.Key != "" && !newKeys[e.Key] {
			removedKeys = append(removedKeys, e.Key)
		}
	}
	return removedKeys, nil
}

func entryAt(list *proto.BundleList, strategy string, slot uint64) *proto.BundleEntry {
	if list == nil {
		return nil
	}
	for _, e := range list.Bundles {
		if e.Strategy == strategy && e.Slot == slot {
			return e
		}
	}
	return nil
}

func inWindow(window []uint64, slot uint64) bool {
	for _, s := range window {
		if s == slot {
			return true
		}
	}
	return false
}

func snapshotOf(l *proto.BundleList) *proto.BundleList {
	c := *l
	c.Bundles = l.Bundles
	return &c
}
