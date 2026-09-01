package bundle

import (
	"context"
	"errors"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Bundle lease semantics (§8.9.1 + doc decisions): cross-instance exclusion on
// leases/bundle-<strategy>.pb; TTL fixed at 30 min with 5 min heartbeats (no
// config key exists); 2 s skew tolerance and incrementing epoch on
// heartbeat/steal; the build goroutine owns the lease and spawns the heartbeat,
// both die by the build's context; nobody else ever writes the lease object.

const (
	leaseTTL       = 30 * time.Minute
	leaseHeartbeat = 5 * time.Minute
	leaseSkew      = 2 * time.Second
)

// ErrLeaseHeld reports a live lease held by another host.
var ErrLeaseHeld = errors.New("bundle: lease held by another host")

// ErrLeaseLost is the heartbeat-412 failure the task records ("lease lost;
// another host took over") before the slot is retried next pass (§8.9.1).
var ErrLeaseLost = errors.New("bundle: lease lost; another host took over")

// acquireLease acquires leases/<name>.pb: create-if-absent, or steal when the
// recorded expiry has passed (with the 2 s skew tolerance), incrementing epoch.
func acquireLease(ctx context.Context, st store.ObjectStore, name, holder string) (func(), error) {
	key := store.LeaseKey(name)
	for attempt := 0; attempt < 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := time.Now()
		meta, err := st.Head(ctx, key)
		if err != nil {
			return nil, err
		}
		if meta == nil {
			l := newLeasePtr(holder, 1, now)
			if _, perr := st.Put(ctx, key, leaseBody(l), store.PutOptions{Mode: store.PutCreate}); perr != nil {
				if store.IsPreconditionFailed(perr) {
					continue // someone else claimed it; re-read
				}
				return nil, perr
			}
			return leaseKeeper(ctx, st, key, l), nil
		}
		body, _, gerr := store.GetBytes(ctx, st, key, store.GetOptions{})
		if gerr != nil {
			return nil, gerr
		}
		var cur proto.Lease
		if err := unmarshalLease(body, &cur); err != nil {
			return nil, store.NewCorrupt(key, err)
		}
		expired := cur.ExpiresAt == nil || now.After(cur.ExpiresAt.Go().Add(leaseSkew))
		if !expired && cur.Holder != holder {
			return nil, ErrLeaseHeld
		}
		next := newLease(holder, cur.Epoch+1, now)
		if _, perr := st.Put(ctx, key, leaseBody(&next), store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version}); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return nil, perr
		}
		return leaseKeeper(ctx, st, key, &next), nil
	}
	return nil, ErrLeaseHeld
}

// leaseKeeper starts the heartbeat loop and returns the release func. The
// heartbeat goroutine dies when ctx is cancelled; a heartbeat 412 cancels ctx
// (killing the git subprocess via exec.CommandContext, §8.9.1).
func leaseKeeper(ctx context.Context, st store.ObjectStore, key string, l *proto.Lease) func() {
	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(leaseHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				meta, err := st.Head(hbCtx, key)
				if err != nil {
					continue
				}
				if meta == nil {
					cancel() // lease vanished: lost
					return
				}
				l.Epoch++
				exp := proto.TimeFromGo(time.Now().Add(leaseTTL))
				l.ExpiresAt = &exp
				if _, err := st.Put(hbCtx, key, leaseBody(l), store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version}); err != nil {
					if store.IsPreconditionFailed(err) {
						cancel() // 412 → lease lost (§8.9.1)
						return
					}
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
		// Best-effort release: delete only if still ours.
		meta, err := st.Head(context.WithoutCancel(ctx), key)
		if err != nil || meta == nil {
			return
		}
		body, _, err := store.GetBytes(context.WithoutCancel(ctx), st, key, store.GetOptions{})
		if err != nil {
			return
		}
		var cur proto.Lease
		if unmarshalLease(body, &cur) != nil || cur.Holder != l.Holder {
			return
		}
		_ = st.Delete(context.WithoutCancel(ctx), key, meta.Version)
	}
}

func leaseBody(l *proto.Lease) store.PutBody {
	return store.PutBody{Bytes: marshalLease(l)}
}

func newLease(holder string, epoch uint64, now time.Time) proto.Lease {
	l := newLeasePtr(holder, epoch, now)
	return *l
}

func newLeasePtr(holder string, epoch uint64, now time.Time) *proto.Lease {
	acq := proto.TimeFromGo(now)
	exp := proto.TimeFromGo(now.Add(leaseTTL))
	return &proto.Lease{Holder: holder, Purpose: "bundle build", AcquiredAt: &acq, ExpiresAt: &exp, Epoch: epoch}
}

func marshalLease(l *proto.Lease) []byte {
	return l.Marshal()
}

func unmarshalLease(body []byte, l *proto.Lease) error {
	return l.Unmarshal(body)
}
