// Package sizecatalog implements Forgejo #248 server-side repo size tracking
// (02_storage_protobuf.md §2.1, 07_api.md §8, 10_maintenance.md §4).
//
// Size semantic (canonical): stored-object size = Σ live-pack PackSize +
// Σ live-pack IdxSize over the manifest's denormalized live pack set;
// object_count = Σ ObjectCount. The derivation is pure arithmetic on data
// the publish path already holds (no I/O, no failure mode beyond overflow —
// uint64 saturates); persisting it costs one repo-scoped sidecar PUT issued
// in parallel with the manifest CAS (+1 total op, +0 sequential trips). Explicitly OUT: .rev/.bitmap/.commit-graph
// side files, bundles/ advertisement state, LFS objects (separate key
// family — a future LFS accounting would sum lfs/objects/ in the sweep,
// never on publish). Overview PacksInfo.LiveBytes (Σ PackSize only) is
// frozen and intentionally differs by IdxSize; this package documents both.
//
// Durable shape (shared rails #247 builds on): per-repo sidecar
// repos/<o>/<r>/meta/stats.json (overwrite JSON, version 1) written on every
// pack-changing publish (PUSH/COMPACT) by the publish path itself, in
// parallel with the manifest CAS (+1 total op, +0 sequential trips — R1 B1;
// the bucket-root catalog is never touched on push), with the maintainer
// sweep as backfill/repair (pre-existing repos, crashed pushes, role
// splits; PUT-if-changed converges). #247 activity derivation adds optional
// fields on this same sidecar/row (one PUT, both concerns).
//
// Null-vs-0: absent row/sidecar = unknown/unbackfilled (API emits null, UI
// hides); present row with 0 = verified-empty repo.
package sizecatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// StatsVersion is the meta/stats.json version writers emit; readers refuse
// unknown versions (fail closed on shape drift, same rule as policy.json).
const StatsVersion = 1

// Stats is the per-repo sidecar body (meta/stats.json). Extra optional
// fields from #247 activity derivation are preserved opaquely via Raw.
type Stats struct {
	Version     int    `json:"version"`
	SizeBytes   uint64 `json:"size_bytes"`
	ObjectCount uint64 `json:"object_count"`
	HeadSeq     uint64 `json:"head_seq"`
	UpdatedAt   string `json:"updated_at"` // RFC 3339 UTC
}

// SizeOf derives (size_bytes, object_count) from a live pack set. Pure
// arithmetic: no I/O, no argv, saturating on overflow (never wraps).
// Empty/nil pack set → (0, 0): verified-empty, not unknown.
func SizeOf(packs []*proto.PackRef) (uint64, uint64) {
	var size, objs uint64
	for _, p := range packs {
		if p == nil {
			continue
		}
		size = satAdd(size, satAdd(p.PackSize, p.IdxSize))
		objs = satAdd(objs, p.ObjectCount)
	}
	return size, objs
}

// ManifestSize derives the canonical size from a manifest snapshot.
// Nil manifest → (0, 0, false): unknown, not empty.
func ManifestSize(m *proto.Manifest) (size, objs uint64, ok bool) {
	if m == nil {
		return 0, 0, false
	}
	size, objs = SizeOf(m.Packs)
	return size, objs, true
}

func satAdd(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}

// EncodeStats renders the sidecar body (updated_at = now UTC RFC 3339).
func EncodeStats(size, objs, headSeq uint64, now time.Time) []byte {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s := Stats{Version: StatsVersion, SizeBytes: size, ObjectCount: objs, HeadSeq: headSeq, UpdatedAt: now.UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(s)
	return b
}

// DecodeStats parses and validates the sidecar body. Unknown version →
// error (fail closed); missing body → (nil, false, nil) = unknown.
func DecodeStats(body []byte) (*Stats, bool, error) {
	if len(body) == 0 {
		return nil, false, nil
	}
	var s Stats
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, false, err
	}
	if s.Version != StatsVersion {
		return nil, false, fmt.Errorf("sizecatalog: unknown stats version %d", s.Version)
	}
	return &s, true, nil
}

// Row is one aggregate listing row served by the detailed endpoint.
// SizeBytes nil = unknown/unbackfilled (UI hides); non-nil 0 = empty.
type Row struct {
	Owner       string
	Name        string
	SizeBytes   *uint64
	ObjectCount *uint64
	HeadSeq     *uint64
	UpdatedAt   *string
}

// FullName renders "<owner>/<repo>".
func (r Row) FullName() string { return r.Owner + "/" + r.Name }

// CatalogKey is the bucket-root aggregate key (store.Catalog).
const CatalogKey = "meta/repos.pb"

// ReadCatalog fetches and decodes the aggregate. Absent → (empty, nil):
// the catalog is optional/rebuildable; callers fall back to null rows.
func ReadCatalog(ctx context.Context, st store.ObjectStore) (*proto.RepoCatalog, error) {
	body, _, err := store.GetBytes(ctx, st, CatalogKey, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return &proto.RepoCatalog{}, nil
		}
		return nil, err
	}
	// Empty body decodes to the empty catalog (proto3 zero value).
	return proto.UnmarshalRepoCatalog(body)
}

// WriteCatalog CAS-writes the aggregate with an inline read-modify-write
// ladder (the store is the lock; 412 → immediate re-read, no sleep;
// retryable → 5ms doubling backoff capped at 100ms). f must be pure
// (re-derive, never accumulate): it runs again on every retry.
func WriteCatalog(ctx context.Context, st store.ObjectStore, f func(cur *proto.RepoCatalog) (*proto.RepoCatalog, error)) (*proto.RepoCatalog, error) {
	const maxRetries = 8
	var last error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body, meta, err := store.GetBytes(ctx, st, CatalogKey, store.GetOptions{})
		if err != nil && !store.IsNotFound(err) {
			if store.IsRetryable(err) {
				if serr := sleepBackoff(ctx, attempt); serr != nil {
					return nil, serr
				}
				attempt--
				continue
			}
			return nil, err
		}
		var cur *proto.RepoCatalog
		if meta.Key != "" {
			cur, err = proto.UnmarshalRepoCatalog(body)
			if err != nil {
				return nil, err
			}
		}
		next, err := f(cur)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, nil
		}
		opts := store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}
		if meta.Key != "" {
			opts = store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"}
		}
		if _, err := st.Put(ctx, CatalogKey, store.PutBody{Bytes: next.Marshal()}, opts); err != nil {
			last = err
			if store.IsPreconditionFailed(err) {
				continue
			}
			if store.IsRetryable(err) {
				if serr := sleepBackoff(ctx, attempt); serr != nil {
					return nil, serr
				}
				attempt--
				continue
			}
			return nil, err
		}
		return next, nil
	}
	return nil, last
}

func sleepBackoff(ctx context.Context, attempt int) error {
	d := 5 * time.Millisecond
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= 100*time.Millisecond {
			d = 100 * time.Millisecond
			break
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// SweepOptions bounds one maintainer fold pass.
type SweepOptions struct {
	// Now overrides the clock (tests).
	Now func() time.Time
	// MaxRepos caps repos folded per call (0 = no cap). Resumability: when
	// the repo list exceeds the cap, Cursor returns the next "owner/name"
	// to resume from (sorted order); callers persist it in memory (cache —
	// the catalog itself is the durable cursor, so loss only repeats work).
	MaxRepos int
	// Cursor resumes after "owner/name" (exclusive, sorted order); "" = start.
	Cursor string
	// Parallel bounds manifest GETs (0 → 8, mirroring liveRepos).
	Parallel int
	// ListRepos enumerates manifest-backed "owner/name" ids in sorted order
	// (the caller's registry listing; never called on hot paths — sweep
	// runs on the maintainer only). When nil, Sweep lists via the store
	// (ListPrefixes over "repos/", manifest-gated Heads at Parallel).
	ListRepos func(ctx context.Context) ([]string, error)
}

// SweepResult reports one fold.
type SweepResult struct {
	Folded     int    // repos whose sidecar+catalog row were refreshed
	Unchanged  int    // sidecar already current (no write)
	Failed     int    // per-repo errors (isolated; sweep continues)
	NextCursor string // "" = complete; else resume here
}

// Sweep folds per-repo sizes into sidecars + the aggregate catalog.
// Bounded, resumable, per-repo error isolation; never holds a lock across
// store I/O. Cost: 1 manifest GET per repo + 1 sidecar PUT per changed repo
// + 1 catalog CAS per pass (off-hot-path maintainer only).
//
// ### Concurrency
// Hazard: parallel manifest GETs racing shared result state.
// Avoidance: one mutex guards the collect only (never held across I/O);
// catalog CAS is lock-free (the store is the lock); parallelism is bounded.
func Sweep(ctx context.Context, st store.ObjectStore, opt SweepOptions) (SweepResult, error) {
	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}
	par := opt.Parallel
	if par <= 0 {
		par = 8
	}
	ids, err := listIDs(ctx, st, opt)
	if err != nil {
		return SweepResult{}, err
	}
	// Resume from cursor (sorted exclusive).
	if opt.Cursor != "" {
		kept := ids[:0]
		for _, id := range ids {
			if id > opt.Cursor {
				kept = append(kept, id)
			}
		}
		ids = kept
	}
	bounded := ids
	nextCursor := ""
	if opt.MaxRepos > 0 && len(ids) > opt.MaxRepos {
		bounded = ids[:opt.MaxRepos]
		nextCursor = bounded[len(bounded)-1]
	}

	type got struct {
		id       string
		size     uint64
		objs     uint64
		head     uint64
		changed  bool
		failed   bool
		manifest bool
	}
	results := make([]got, len(bounded))
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i, id := range bounded {
		if ctx.Err() != nil {
			break
		}
		i, id := i, id
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = got{id: id, failed: true}
				return
			}
			size, objs, head, changed, ok := foldOne(ctx, st, id, now())
			results[i] = got{id: id, size: size, objs: objs, head: head, changed: changed, failed: !ok, manifest: ok}
		}()
	}
	wg.Wait()

	var res SweepResult
	res.NextCursor = nextCursor
	rows := make([]got, 0, len(results))
	for _, g := range results {
		if g.failed {
			res.Failed++
			continue
		}
		if !g.manifest {
			res.Failed++
			continue
		}
		rows = append(rows, g)
		if g.changed {
			res.Folded++
		} else {
			res.Unchanged++
		}
	}
	if len(rows) == 0 {
		return res, nil
	}
	ts := now().UTC()
	_, err = WriteCatalog(ctx, st, func(cur *proto.RepoCatalog) (*proto.RepoCatalog, error) {
		if cur == nil {
			cur = &proto.RepoCatalog{}
		}
		byRepo := map[string]*proto.RepoCatalogEntry{}
		for _, e := range cur.Entries {
			byRepo[e.Repo] = e
		}
		names := map[string]bool{}
		for _, r := range cur.Repos {
			names[r] = true
		}
		for _, g := range rows {
			names[g.id] = true
			byRepo[g.id] = &proto.RepoCatalogEntry{
				Repo: g.id, SizeBytes: g.size, ObjectCount: g.objs,
				HeadSeq: g.head, UpdatedAt: &proto.Timestamp{Seconds: ts.Unix(), Nanos: int32(ts.Nanosecond())},
			}
		}
		cur.Repos = cur.Repos[:0]
		for n := range names {
			cur.Repos = append(cur.Repos, n)
		}
		sort.Strings(cur.Repos)
		cur.Entries = cur.Entries[:0]
		for _, n := range cur.Repos {
			if e, ok := byRepo[n]; ok {
				cur.Entries = append(cur.Entries, e)
			}
		}
		cur.UpdatedAt = &proto.Timestamp{Seconds: ts.Unix(), Nanos: int32(ts.Nanosecond())}
		return cur, nil
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// foldOne refreshes one per-repo sidecar: GET manifest (probe, never LIST),
// derive size arithmetically, PUT sidecar only when changed. The publish
// path owns primary writes (parallel sidecar PUT on PUSH/COMPACT); the sweep
// is backfill/repair for pre-existing repos, crashed pushes, and role
// splits. Returns (size, objs, head, changed, ok).
func foldOne(ctx context.Context, st store.ObjectStore, id string, now time.Time) (uint64, uint64, uint64, bool, bool) {
	owner, name, ok := splitID(id)
	if !ok {
		return 0, 0, 0, false, false
	}
	body, _, err := store.GetBytes(ctx, st, "repos/"+id+"/"+store.Manifest, store.GetOptions{})
	if err != nil || body == nil {
		return 0, 0, 0, false, false
	}
	m, err := proto.UnmarshalManifest(body)
	if err != nil {
		return 0, 0, 0, false, false
	}
	size, objs, _ := ManifestSize(m)
	// Skip write when the sidecar already matches (saves the PUT).
	if cur, _, err := store.GetBytes(ctx, st, store.StatsKey(owner, name), store.GetOptions{}); err == nil && cur != nil {
		if s, ok, derr := DecodeStats(cur); derr == nil && ok && s.SizeBytes == size && s.ObjectCount == objs && s.HeadSeq == m.HeadSeq {
			return size, objs, m.HeadSeq, false, true
		}
	} else if err != nil && !store.IsNotFound(err) {
		return 0, 0, 0, false, false
	}
	next := EncodeStats(size, objs, m.HeadSeq, now)
	_, perr := st.Put(ctx, store.StatsKey(owner, name), store.PutBody{Bytes: next}, store.PutOptions{ContentType: "application/json"})
	if perr != nil {
		// Best-effort overwrite (sidecar is rebuildable): CAS conflicts or
		// retryables fail this repo's fold only, never the pass.
		if store.IsPreconditionFailed(perr) || store.IsRetryable(perr) {
			return 0, 0, 0, false, false
		}
		return 0, 0, 0, false, false
	}
	return size, objs, m.HeadSeq, true, true
}

func splitID(id string) (owner, name string, ok bool) {
	slash := strings.Index(id, "/")
	if slash <= 0 || slash == len(id)-1 || strings.Contains(id[slash+1:], "/") {
		return "", "", false
	}
	return id[:slash], id[slash+1:], true
}

func listIDs(ctx context.Context, st store.ObjectStore, opt SweepOptions) ([]string, error) {
	if opt.ListRepos != nil {
		ids, err := opt.ListRepos(ctx)
		if err != nil {
			return nil, err
		}
		out := append([]string{}, ids...)
		sort.Strings(out)
		return out, nil
	}
	// Store enumeration (maintainer only, never a hot path): owner prefixes
	// via ListPrefixes, repo names via ListPrefixes, manifest-gated.
	var owners []string
	if err := st.ListPrefixes(ctx, "repos/", func(p string) error {
		owners = append(owners, strings.TrimSuffix(strings.TrimPrefix(p, "repos/"), "/"))
		return nil
	}); err != nil {
		return nil, err
	}
	var ids []string
	for _, o := range owners {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var repos []string
		if err := st.ListPrefixes(ctx, "repos/"+o+"/", func(p string) error {
			seg := strings.TrimSuffix(strings.TrimPrefix(p, "repos/"+o+"/"), "/")
			if seg != "" && !strings.Contains(seg, "/") {
				repos = append(repos, seg)
			}
			return nil
		}); err != nil {
			continue
		}
		for _, r := range repos {
			meta, herr := st.Head(ctx, "repos/"+o+"/"+r+"/"+store.Manifest)
			if herr != nil || meta == nil {
				continue
			}
			ids = append(ids, o+"/"+r)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// FilterSort applies the listing query over catalog rows + owner scope:
// min/max byte filters (nil = no bound), sort key ("size"|"name",
// default name), order ("asc"|"desc", default asc for name, desc for size
// when unspecified? No — default asc always; callers pass explicitly).
// Ties break deterministically on (owner, name) — shared with #247
// ordering. Unknown sizes (nil) sort last in asc, first in desc? No:
// unknowns always sort LAST regardless of direction (they are not "small").
func FilterSort(rows []Row, sortKey, order string, minBytes, maxBytes *uint64) []Row {
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		if minBytes != nil && (r.SizeBytes == nil || *r.SizeBytes < *minBytes) {
			continue
		}
		if maxBytes != nil && (r.SizeBytes == nil || *r.SizeBytes > *maxBytes) {
			continue
		}
		out = append(out, r)
	}
	asc := order != "desc"
	bySize := sortKey == "size"
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if bySize {
			if (a.SizeBytes == nil) != (b.SizeBytes == nil) {
				return b.SizeBytes == nil // known before unknown, always
			}
			if a.SizeBytes != nil && b.SizeBytes != nil && *a.SizeBytes != *b.SizeBytes {
				if asc {
					return *a.SizeBytes < *b.SizeBytes
				}
				return *a.SizeBytes > *b.SizeBytes
			}
		}
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		return a.Name < b.Name
	})
	return out
}

// RowsForOwner projects the catalog onto one owner's rows: every known repo
// name gets a row; catalog entries supply sizes; missing entries → nil
// (unknown, never zero). names is the manifest-gated owner listing (the
// source of truth for membership); the catalog never invents repos.
func RowsForOwner(owner string, names []string, cat *proto.RepoCatalog) []Row {
	byRepo := map[string]*proto.RepoCatalogEntry{}
	if cat != nil {
		for _, e := range cat.Entries {
			byRepo[e.Repo] = e
		}
	}
	rows := make([]Row, 0, len(names))
	for _, n := range names {
		r := Row{Owner: owner, Name: n}
		if e, ok := byRepo[owner+"/"+n]; ok {
			sz, oc, hs := e.SizeBytes, e.ObjectCount, e.HeadSeq
			r.SizeBytes, r.ObjectCount, r.HeadSeq = &sz, &oc, &hs
			if e.UpdatedAt != nil {
				ts := e.UpdatedAt.Go().UTC().Format(time.RFC3339)
				r.UpdatedAt = &ts
			}
		}
		rows = append(rows, r)
	}
	return rows
}
