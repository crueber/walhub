// util.go — small shared helpers for the engine (config adapter, fs glue).
package wal

import (
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"

	"git.packden.us/crueber/walhub/internal/config"
)

// configVals is the engine's flattened view of the wal/cache config sections
// (05 §5.10). Built once per Registry; tests override fields directly.
type configVals struct {
	freshnessTTL         time.Duration
	batchWindow          time.Duration
	maxBatch             int
	casMaxRetries        int
	snapshotEveryEntries uint64
	checkpointTailBytes  uint64
	checkpointInterval   time.Duration
	prefetchPacks        bool
	prefetchMaxBytes     int64
	remoteObjects        bool
	cacheMode            string
	cacheMaxBytes        int64
	evictIdleAfter       time.Duration
	diskHighWatermark    float64
	storeMount           string
	cacheDir             string
	remoteBlockBytes     int64
	remoteObjectBytes    int64
	// serveSyncTimeout bounds the serve-level Sync WAIT (packMu +
	// materialize join); serveMaterializeTimeout caps the detached
	// materialize body (issue #320, 05 §5.2). Non-positive config
	// falls back to the defaults — there is no unbounded serve wait.
	serveSyncTimeout        time.Duration
	serveMaterializeTimeout time.Duration
}

// DefaultServeSyncTimeout is the serve-wait bound when the config is
// non-positive (45s: under typical proxy 60s timeouts, so the server
// answers 503 itself instead of letting the proxy 504).
const DefaultServeSyncTimeout = 45 * time.Second

// DefaultServeMaterializeTimeout caps one materialize body when the
// config is non-positive (10m: comfortably above the wait bound —
// the wait is per request, this is per body of work).
const DefaultServeMaterializeTimeout = 10 * time.Minute

func newConfigVals(c *config.Config) *configVals {
	return &configVals{
		freshnessTTL:            time.Duration(c.WAL.FreshnessTTL),
		batchWindow:             time.Duration(c.WAL.BatchWindow),
		maxBatch:                c.WAL.MaxBatch,
		casMaxRetries:           int(c.WAL.CASMaxRetries),
		snapshotEveryEntries:    c.WAL.SnapshotEveryEntries,
		checkpointTailBytes:     uint64(c.WAL.CheckpointTailBytes),
		checkpointInterval:      time.Duration(c.WAL.CheckpointInterval),
		prefetchPacks:           c.WAL.PrefetchPacks,
		prefetchMaxBytes:        int64(c.WAL.PrefetchMaxBytes),
		remoteObjects:           c.WAL.RemoteObjects,
		cacheMode:               c.Cache.Mode,
		cacheMaxBytes:           int64(c.Cache.MaxBytes),
		evictIdleAfter:          time.Duration(c.Cache.EvictIdleAfter),
		diskHighWatermark:       c.Cache.DiskHighWatermark,
		storeMount:              c.Cache.StoreMount,
		cacheDir:                c.Cache.Dir,
		remoteBlockBytes:        int64(c.Cache.RemoteBlockBytes),
		remoteObjectBytes:       int64(c.Cache.RemoteObjectBytes),
		serveSyncTimeout:        serveSyncTimeoutOf(c),
		serveMaterializeTimeout: serveMaterializeTimeoutOf(c),
	}
}

// serveSyncTimeoutOf resolves the serve-wait bound: the configured
// server.serve_sync_timeout, or the default when non-positive.
func serveSyncTimeoutOf(c *config.Config) time.Duration {
	if c != nil {
		if d := time.Duration(c.Server.ServeSyncTimeout); d > 0 {
			return d
		}
	}
	return DefaultServeSyncTimeout
}

// serveMaterializeTimeoutOf resolves the materialize body cap: the
// configured server.serve_materialize_timeout, or the default when
// non-positive.
func serveMaterializeTimeoutOf(c *config.Config) time.Duration {
	if c != nil {
		if d := time.Duration(c.Server.ServeMaterializeTimeout); d > 0 {
			return d
		}
	}
	return DefaultServeMaterializeTimeout
}

// gitUpdates converts proto update pointers into the value slice the git
// layer's offline apply takes (git.RefUpdate is an alias of proto.RefUpdate).
func gitUpdates(us []*proto.RefUpdate) []git.RefUpdate {
	out := make([]git.RefUpdate, 0, len(us))
	for _, u := range us {
		if u == nil {
			continue
		}
		out = append(out, *u)
	}
	return out
}
