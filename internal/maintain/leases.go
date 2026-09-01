// leases.go — leases/<name>.pb over the bucket's CAS primitives (§3.3/§4.9):
// the only cross-instance mutex. CAS ladder: absent → Create epoch 0;
// present+expired+skew → Update with epoch+1 (steal); otherwise held.
package maintain

import (
	"context"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// StoreLeaser implements Leaser over store.ObjectStore.
type StoreLeaser struct {
	St store.ObjectStore
}

// leaseLadderAttempts bounds the CAS retries when a create/update races
// another acquirer (412 → re-read; the ladder is cheap, the caller does NOT
// wait while a live lease exists — that surfaces as ErrLeaseHeld).
const leaseLadderAttempts = 16

// Acquire implements Leaser (§3.3). ttl comes from the unit (compaction uses
// compaction.lease_ttl); skew is the §4.9 steal tolerance — bundle leases keep
// the historical absence of the skew tolerance (0) for wire compatibility.
func (l StoreLeaser) Acquire(ctx context.Context, name, holder, purpose string, ttl, skew time.Duration) (func(), error) {
	key := store.LeaseKey(name)
	for range leaseLadderAttempts {
		body, meta, err := store.GetBytes(ctx, l.St, key, store.GetOptions{})
		if err != nil && !store.IsNotFound(err) {
			return nil, err
		}
		now := time.Now().UTC()
		if body == nil {
			lease := &proto.Lease{Holder: holder, Purpose: purpose}
			acq, exp := proto.TimeFromGo(now), proto.TimeFromGo(now.Add(ttl))
			lease.AcquiredAt, lease.ExpiresAt, lease.Epoch = &acq, &exp, 0
			put, err := l.St.Put(ctx, key, store.PutBody{Bytes: lease.Marshal()},
				store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"})
			if err == nil {
				return l.releaseFunc(key, holder, put.Version), nil
			}
			if !store.IsPreconditionFailed(err) {
				return nil, err
			}
			continue // raced another acquirer; re-read
		}
		cur := &proto.Lease{}
		if err := cur.Unmarshal(body); err != nil {
			return nil, err
		}
		var expires time.Time
		if cur.ExpiresAt != nil {
			expires = cur.ExpiresAt.Go()
		}
		if now.Before(expires.Add(skew)) {
			return nil, ErrLeaseHeld // §3.3: never waits, never retries within the pass
		}
		// Steal: expired (+ skew when the lease honors it). The epoch
		// increments on every steal (proto.Lease: "incremented on every
		// heartbeat/steal"), so stale writers holding the pre-steal epoch
		// always lose their CAS.
		next := *cur
		next.Holder = holder
		next.Purpose = purpose
		next.Epoch = cur.Epoch + 1
		acq, exp := proto.TimeFromGo(now), proto.TimeFromGo(now.Add(ttl))
		next.AcquiredAt, next.ExpiresAt = &acq, &exp
		put, err := l.St.Put(ctx, key, store.PutBody{Bytes: next.Marshal()},
			store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"})
		if err == nil {
			return l.releaseFunc(key, holder, put.Version), nil
		}
		if !store.IsPreconditionFailed(err) {
			return nil, err
		}
	}
	return nil, ErrLeaseHeld
}

// releaseFunc deletes the lease only if it still names us (CAS delete; an
// expired-and-stolen lease is left for its new owner).
func (l StoreLeaser) releaseFunc(key, holder string, version store.Version) func() {
	return func() {
		body, meta, err := store.GetBytes(context.Background(), l.St, key, store.GetOptions{})
		if err != nil || body == nil {
			return
		}
		cur := &proto.Lease{}
		if cur.Unmarshal(body) != nil || cur.Holder != holder {
			return
		}
		_ = l.St.Delete(context.Background(), key, meta.Version)
	}
}
