// heartbeat.go — the MaintainerHeartbeat writer (§4.2): bucket-root
// maintain/<host>.pb, Overwrite put, never WAL state; the 24 h purge; the
// 600 s alive rule. Writes happen on the pass goroutine only (§7).
package maintain

import (
	"context"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

const (
	// hbInterval is the mid-pass rewrite cadence (§4.2): a pass lasting hours
	// still looks alive.
	hbInterval = 120 * time.Second
	// hbPurgeAfter is the §4.2 purge horizon.
	hbPurgeAfter = 24 * time.Hour
	// hbAliveWithin is the alive rule (§4.2): last_pass_at within 600 s; a
	// host whose loop goroutine panicked stops being "alive" 10 minutes later
	// without any explicit tombstone.
	hbAliveWithin = 600 * time.Second

	hbPrefix = "maintain/"
)

// writeHeartbeat puts maintain/<host>.pb (Overwrite semantics, no CAS).
func writeHeartbeat(ctx context.Context, st store.ObjectStore, hb *proto.MaintainerHeartbeat) error {
	_, err := st.Put(ctx, store.MaintainerKey(hb.Host), store.PutBody{Bytes: hb.Marshal()},
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/x-protobuf"})
	return err
}

// purgeHeartbeats deletes heartbeats older than horizon (§4.2 purge: a cheap
// list at the start of each pass, prefix maintain/, by a maintain-role host).
// Keys that fail to decode are skipped — the store contract carries no mtime,
// so last_pass_at is the only age signal (deviation: the Rust mtime fallback
// has no wire equivalent here).
func purgeHeartbeats(ctx context.Context, st store.ObjectStore, now time.Time, horizon time.Duration) ([]string, error) {
	var purged []string
	err := st.List(ctx, hbPrefix, "", func(meta store.ObjectMeta) error {
		body, _, err := store.GetBytes(ctx, st, meta.Key, store.GetOptions{})
		if err != nil || body == nil {
			return nil // transient; try again next pass
		}
		hb := &proto.MaintainerHeartbeat{}
		if hb.Unmarshal(body) != nil || hb.LastPassAt == nil {
			return nil
		}
		if now.Sub(hb.LastPassAt.Go()) >= horizon {
			if err := st.Delete(ctx, meta.Key, ""); err != nil {
				return err
			}
			purged = append(purged, meta.Key)
		}
		return nil
	})
	return purged, err
}

// HeartbeatAlive reports last_pass_at within hbAliveWithin (§4.2).
func HeartbeatAlive(hb *proto.MaintainerHeartbeat, now time.Time) bool {
	if hb == nil || hb.LastPassAt == nil {
		return false
	}
	return now.Sub(hb.LastPassAt.Go()) < hbAliveWithin
}

// newHeartbeat builds the writer's own heartbeat object.
func newHeartbeat(host string, repos, exclude []string, eff *config.Config, startedAt time.Time) *proto.MaintainerHeartbeat {
	return &proto.MaintainerHeartbeat{
		Host:        host,
		Repos:       repos,
		Exclude:     exclude,
		MaxPackByte: uint64(eff.Maintenance.MaxPackBytes),
		Disk:        eff.Maintenance.Disk,
		StartedAt:   ptrTs(startedAt),
	}
}

func ptrTs(t time.Time) *proto.Timestamp {
	ts := proto.TimeFromGo(t)
	return &ts
}
