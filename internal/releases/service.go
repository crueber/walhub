package releases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns mutation policy: P6 gates, the PUT upsert CAS loop, the
// monotonic latest pointer, the asset two-step, and autodraft. Raw shapes
// live in model.go; git argv lives in git.go.

// --- tag resolution ---------------------------------------------------------

// resolveTag resolves refs/tags/<tag> to its commit sha (snapshotted at
// creation; later tag moves/deletes never rewrite the release). Absent ⇒
// 404 unknown revision; unwired git ⇒ 503.
func (s *Service) resolveTag(ctx context.Context, owner, repo, tag string) (string, error) {
	if s.Git == nil || s.Dirs == nil {
		return "", fmt.Errorf("%w: tag resolution unavailable", ErrUnavailable)
	}
	dir, err := s.Dirs.Dir(ctx, repoName(owner, repo))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	sha, err := s.Git.ResolveRef(ctx, dir, "refs/tags/"+tag)
	if err != nil {
		return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, tag)
	}
	return sha, nil
}

// --- PUT: create-or-update (upsert) -----------------------------------------

// PutRelease creates the release when absent (201-class) or CAS-updates the
// mutable fields when present (200-class). tag/tag_sha are immutable after
// creation. A concurrent creator losing the absent-CAS converges into the
// update path on retry (idempotent PUTs are retry-safe; the P8 backfill
// contract needs retry-safe publishes — see Decisions).
//
// Publish transitions (create-published or draft→publish flip) CAS the
// latest pointer (§2) and fan out synchronously per P8 (notifications to
// watchers + webhook enqueue via Emitter, SSE release frame via Streamer).
// Non-publishing mutations stream "edited".
//
// expectedVer carries an If-Match ETag token ("" = no constraint,
// last-writer-wins via the CAS loop).
func (s *Service) PutRelease(ctx context.Context, owner, repo string, p auth.Principal, tag string, in ReleaseInput, expectedVer store.Version) (*Release, bool, error) {
	if err := s.requireRole(ctx, owner, repo, p, string(identity.RoleWrite)); err != nil {
		return nil, false, err
	}
	tag, err := validateTag(tag)
	if err != nil {
		return nil, false, err
	}
	name, body, err := validateReleaseInput(in)
	if err != nil {
		return nil, false, err
	}
	draft := false
	if in.Draft != nil {
		draft = *in.Draft
	}
	prerelease := false
	if in.Prerelease != nil {
		prerelease = *in.Prerelease
	}
	if name == "" {
		name = tag
	}

	key := ReleaseKey(owner, repo, tag)
	var out *Release
	var created bool
	var published bool
	_, err = s.casUpdate(ctx, key, 8, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if expectedVer != "" && expectedVer != "*" && cur != nil && ver != expectedVer {
			return nil, false, fmt.Errorf("%w: release %q changed; reload and retry", ErrConflict, tag)
		}
		now := s.nowUTC().Format(dateTimeFmt)
		if cur == nil {
			if expectedVer == "*" {
				return nil, false, fmt.Errorf("%w: unknown release %q", ErrNotFound, tag)
			}
			sha, rerr := s.resolveTag(ctx, owner, repo, tag)
			if rerr != nil {
				return nil, false, rerr
			}
			var publishedAt *string
			if !draft {
				publishedAt = &now
				published = true
			}
			out = &Release{
				Tag: tag, TagSHA: sha, Name: name, Body: body,
				Draft: draft, Prerelease: prerelease,
				Author: normPrincipal(p.Name), CreatedAt: now,
				PublishedAt: publishedAt, UpdatedAt: now,
				Assets: []AssetEntry{}, Version: 1,
			}
			created = true
			return encodeRelease(out), true, nil
		}
		r, perr := parseRelease(cur)
		if perr != nil {
			return nil, false, perr
		}
		if in.Name != nil {
			r.Name = name
		}
		if in.Body != nil {
			r.Body = body
		}
		if in.Draft != nil {
			if r.Draft && !draft {
				published = true
				r.PublishedAt = &now
			}
			if draft {
				r.PublishedAt = nil
			}
			r.Draft = draft
		}
		if in.Prerelease != nil {
			r.Prerelease = prerelease
		}
		r.UpdatedAt = now
		r.Version++
		out = r
		return encodeRelease(r), true, nil
	})
	if err != nil {
		return nil, false, err
	}
	if published {
		s.updateLatestPointer(ctx, owner, repo, out.Tag, out.CreatedAt)
		s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Tag: out.Tag, Actor: normPrincipal(p.Name)})
		s.stream(ctx, StreamEvent{Name: "release", Repo: repoName(owner, repo), Action: "published", Tag: out.Tag})
	} else {
		s.stream(ctx, StreamEvent{Name: "release", Repo: repoName(owner, repo), Action: "edited", Tag: out.Tag})
	}
	return out, created, nil
}

// --- reads ------------------------------------------------------------------

// GetRelease reads one release header (drafts included — the read gate is
// the only filter; the public LIST hides drafts).
func (s *Service) GetRelease(ctx context.Context, owner, repo string, p auth.Principal, tag string) (*Release, store.Version, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, "", err
	}
	tag, err := validateTag(tag)
	if err != nil {
		return nil, "", err
	}
	raw, ver, err := s.getJSON(ctx, ReleaseKey(owner, repo, tag))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", fmt.Errorf("%w: unknown release %q", ErrNotFound, tag)
	}
	r, perr := parseRelease(raw)
	if perr != nil {
		return nil, "", perr
	}
	return r, ver, nil
}

// releaseWithVer pairs a header with its CAS token (list ETags).
type releaseWithVer struct {
	rel *Release
	ver store.Version
}

// ListReleases lists published releases newest-created_at-first (drafts
// are hidden from the public list). after is the "<created_at>|<tag>"
// cursor; n is clamped to [1, ListMaxPage].
func (s *Service) ListReleases(ctx context.Context, owner, repo string, p auth.Principal, n int, after string) ([]releaseWithVer, bool, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, false, err
	}
	if n <= 0 {
		n = ListDefaultPage
	}
	if n > ListMaxPage {
		n = ListMaxPage
	}
	var afterTime, afterTag string
	if after != "" {
		ts, tg, ok := splitCursor(after)
		if !ok {
			return nil, false, fmt.Errorf("%w: malformed after cursor", ErrInvalid)
		}
		afterTime, afterTag = ts, tg
	}
	headers, err := s.scanHeaders(ctx, owner, repo, ListScanCap)
	if err != nil {
		return nil, false, err
	}
	out := make([]releaseWithVer, 0, len(headers))
	for _, h := range headers {
		if h.rel.Draft {
			continue
		}
		if after != "" && !cursorAfter(h.rel.CreatedAt, h.rel.Tag, afterTime, afterTag) {
			continue
		}
		out = append(out, h)
	}
	sortReleases(out)
	var more bool
	if len(out) > n {
		out = out[:n]
		more = true
	}
	return out, more, nil
}

// splitCursor splits an "<created_at>|<tag>" cursor on the FIRST "|" (the
// timestamp never contains one; tags may).
func splitCursor(after string) (string, string, bool) {
	i := strings.Index(after, "|")
	if i < 0 {
		return "", "", false
	}
	ts, tag := after[:i], after[i+1:]
	if _, err := time.Parse(dateTimeFmt, ts); err != nil {
		return "", "", false
	}
	if tag == "" {
		return "", "", false
	}
	return ts, tag, true
}

// cursorAfter reports whether (ts, tag) sorts strictly after the cursor
// under the list order (created_at desc, tag asc).
func cursorAfter(ts, tag, afterTime, afterTag string) bool {
	if ts != afterTime {
		return ts < afterTime
	}
	return tag > afterTag
}

// sortReleases orders newest-created_at-first, tag ascending on ties.
func sortReleases(rs []releaseWithVer) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].rel.CreatedAt != rs[j].rel.CreatedAt {
			return rs[i].rel.CreatedAt > rs[j].rel.CreatedAt
		}
		return rs[i].rel.Tag < rs[j].rel.Tag
	})
}

// scanHeaders LISTs the releases prefix and GETs up to cap header bodies
// (asset byte keys and latest.json are filtered, never fetched).
func (s *Service) scanHeaders(ctx context.Context, owner, repo string, cap int) ([]releaseWithVer, error) {
	prefix := ReleasesPrefix(owner, repo)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		k := m.Key
		if !strings.HasSuffix(k, ".json") || strings.Contains(k, "/assets/") {
			return nil
		}
		if k == LatestKey(owner, repo) {
			return nil
		}
		keys = append(keys, k)
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]releaseWithVer, 0, len(keys))
	for _, k := range keys {
		if len(out) >= cap {
			break
		}
		raw, ver, err := s.getJSON(ctx, k)
		if err != nil || raw == nil {
			continue // raced delete / unreadable — skip, never fail a list
		}
		r, perr := parseRelease(raw)
		if perr != nil {
			continue
		}
		out = append(out, releaseWithVer{rel: r, ver: ver})
	}
	return out, nil
}

// --- latest pointer (§2) ----------------------------------------------------

// LatestRelease resolves the latest published release: the pointer when it
// verifies (exists, not draft, prerelease rule), else the bounded
// self-healing scan (which lazily repairs the pointer).
func (s *Service) LatestRelease(ctx context.Context, owner, repo string, p auth.Principal, includePrereleases bool) (*Release, store.Version, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, "", err
	}
	if raw, _, err := s.getJSON(ctx, LatestKey(owner, repo)); err == nil && raw != nil {
		if ptr, perr := parseLatest(raw); perr == nil && ptr.Tag != "" {
			if raw2, ver2, gerr := s.getJSON(ctx, ReleaseKey(owner, repo, ptr.Tag)); gerr == nil && raw2 != nil {
				if r, perr2 := parseRelease(raw2); perr2 == nil && !r.Draft && (includePrereleases || !r.Prerelease) {
					return r, ver2, nil
				}
			}
		}
	} else if err != nil {
		return nil, "", err
	}
	best, err := s.scanBest(ctx, owner, repo, includePrereleases)
	if err != nil {
		return nil, "", err
	}
	if best == nil {
		return nil, "", fmt.Errorf("%w: no releases", ErrNotFound)
	}
	// Lazy repair: point at the scan winner (best-effort; the next read
	// re-scans on any failure, so correctness never depends on it).
	s.repairLatestPointer(ctx, owner, repo, best.Tag, best.CreatedAt)
	// A concurrent delete between the scan and this read surfaces as
	// unknown release — honest, and the next read re-scans.
	return s.GetRelease(ctx, owner, repo, p, best.Tag)
}

// scanBest returns the newest published release (bounded scan, cap
// ScanHeaderCap bodies), or nil when none qualifies.
func (s *Service) scanBest(ctx context.Context, owner, repo string, includePrereleases bool) (*Release, error) {
	headers, err := s.scanHeaders(ctx, owner, repo, ScanHeaderCap)
	if err != nil {
		return nil, err
	}
	var best *Release
	for _, h := range headers {
		r := h.rel
		if r.Draft || (!includePrereleases && r.Prerelease) {
			continue
		}
		if best == nil || r.CreatedAt > best.CreatedAt ||
			(r.CreatedAt == best.CreatedAt && r.Tag < best.Tag) {
			best = r
		}
	}
	return best, nil
}

// updateLatestPointer CAS-updates the pointer ONLY when the new release is
// strictly newer than the pointer's current target (monotonic: concurrent
// publishers converge on the newest regardless of CAS order; the loser's
// CAS is a skip, not a retry).
func (s *Service) updateLatestPointer(ctx context.Context, owner, repo, tag, createdAt string) {
	now := s.nowUTC().Format(dateTimeFmt)
	_, _ = s.casUpdate(ctx, LatestKey(owner, repo), 8, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if cur == nil {
			ptr := &LatestPointer{Tag: tag, CreatedAt: createdAt, UpdatedAt: now}
			raw, _ := json.Marshal(ptr)
			return raw, true, nil
		}
		ptr, perr := parseLatest(cur)
		if perr != nil {
			return nil, false, perr
		}
		curCreated := s.pointerTargetCreated(ctx, owner, repo, ptr.Tag)
		if !newerThan(createdAt, curCreated) {
			return nil, false, nil // stale publisher — skip, not retry
		}
		ptr.Tag = tag
		ptr.CreatedAt = createdAt
		ptr.UpdatedAt = now
		raw, _ := json.Marshal(ptr)
		return raw, true, nil
	})
}

// repairLatestPointer points the pointer at (tag, createdAt) under the
// SAME monotonic rule as the publish path (skip when the pointer already
// targets this tag or a strictly newer release — a truncated scan must
// never park the pointer at an older release). Idempotent.
func (s *Service) repairLatestPointer(ctx context.Context, owner, repo, tag, createdAt string) {
	now := s.nowUTC().Format(dateTimeFmt)
	_, _ = s.casUpdate(ctx, LatestKey(owner, repo), 4, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if cur != nil {
			if ptr, perr := parseLatest(cur); perr == nil {
				if ptr.Tag == tag {
					return nil, false, nil
				}
				if !newerThan(createdAt, s.pointerTargetCreated(ctx, owner, repo, ptr.Tag)) {
					return nil, false, nil // scan found an older release — skip, not retry
				}
			}
			// Unparseable pointer: fall through and overwrite (the
			// self-healing write replaces corruption, same as absent).
		}
		ptr := &LatestPointer{Tag: tag, CreatedAt: createdAt, UpdatedAt: now}
		raw, _ := json.Marshal(ptr)
		return raw, true, nil
	})
}

// pointerTargetCreated returns the created_at of the release the pointer
// targets ("" when the target is missing/unreadable — a dangling pointer
// always loses the monotonic compare, so repair proceeds).
func (s *Service) pointerTargetCreated(ctx context.Context, owner, repo, tag string) string {
	if traw, _, gerr := s.getJSON(ctx, ReleaseKey(owner, repo, tag)); gerr == nil && traw != nil {
		if tr, perr := parseRelease(traw); perr == nil {
			return tr.CreatedAt
		}
	}
	return ""
}

// newerThan compares RFC 3339 timestamps (lexical fallback for
// never-empty/corrupt values: corrupt sorts oldest).
func newerThan(a, b string) bool {
	ta, aerr := time.Parse(dateTimeFmt, a)
	tb, berr := time.Parse(dateTimeFmt, b)
	if aerr == nil && berr == nil {
		return ta.After(tb)
	}
	if aerr != nil {
		return false
	}
	return a > b
}

// --- DELETE release (maintain) --------------------------------------------

// DeleteRelease removes the header plus every asset byte, then repairs the
// latest pointer synchronously (same scan). Header-first: a crash leaves
// unreferenced immutable bytes (harmless), never a header pointing at
// missing bytes.
func (s *Service) DeleteRelease(ctx context.Context, owner, repo string, p auth.Principal, tag string) error {
	if err := s.requireRole(ctx, owner, repo, p, string(identity.RoleMaintain)); err != nil {
		return err
	}
	tag, err := validateTag(tag)
	if err != nil {
		return err
	}
	raw, _, err := s.getJSON(ctx, ReleaseKey(owner, repo, tag))
	if err != nil {
		return err
	}
	if raw == nil {
		return fmt.Errorf("%w: unknown release %q", ErrNotFound, tag)
	}
	if derr := s.Store.Delete(ctx, ReleaseKey(owner, repo, tag), ""); derr != nil && !store.IsNotFound(derr) {
		return derr
	}
	// Asset bytes: bounded LIST over the per-release prefix.
	var assets []string
	if lerr := s.Store.List(ctx, AssetPrefix(owner, repo, tag), "", func(m store.ObjectMeta) error {
		assets = append(assets, m.Key)
		return nil
	}); lerr != nil {
		return lerr
	}
	for _, k := range assets {
		if derr := s.Store.Delete(ctx, k, ""); derr != nil && !store.IsNotFound(derr) {
			return derr
		}
	}
	// Synchronous latest repair (same scan the read path uses).
	best, serr := s.scanBest(ctx, owner, repo, true)
	if serr != nil {
		return serr
	}
	if best == nil {
		_ = s.Store.Delete(ctx, LatestKey(owner, repo), "")
	} else {
		s.repairLatestPointer(ctx, owner, repo, best.Tag, best.CreatedAt)
	}
	s.stream(ctx, StreamEvent{Name: "release", Repo: repoName(owner, repo), Action: "deleted", Tag: tag})
	return nil
}

// --- assets (two-step: bytes first, header CAS second) ----------------------

// UploadAsset streams the upload to a spool file (never buffered in
// memory), verifies sha256 + size BEFORE the store write, Creates the
// immutable bytes, then CAS-appends the header entry. An existing object
// with the same sha256 is idempotent success; a different sha is 409. A
// crash between the steps leaves orphan bytes (harmless); a header entry
// NEVER points at missing bytes.
func (s *Service) UploadAsset(ctx context.Context, owner, repo string, p auth.Principal, tag, name string, body io.Reader, contentLength int64, shaHex, contentType string) (*AssetEntry, error) {
	if err := s.requireRole(ctx, owner, repo, p, string(identity.RoleWrite)); err != nil {
		return nil, err
	}
	tag, err := validateTag(tag)
	if err != nil {
		return nil, err
	}
	name, err = validateAssetName(name)
	if err != nil {
		return nil, err
	}
	sha, err := normalizeSHA256(shaHex)
	if err != nil {
		return nil, err
	}
	ct, err := normalizeContentType(contentType)
	if err != nil {
		return nil, err
	}
	max := s.maxAssetBytes()
	if contentLength >= 0 {
		if contentLength > max {
			return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrTooLarge, contentLength, max)
		}
	} else {
		return nil, fmt.Errorf("%w: content length required", ErrInvalid)
	}
	hraw, _, err := s.getJSON(ctx, ReleaseKey(owner, repo, tag))
	if err != nil {
		return nil, err
	}
	if hraw == nil {
		return nil, fmt.Errorf("%w: unknown release %q", ErrNotFound, tag)
	}
	if _, perr := parseRelease(hraw); perr != nil {
		return nil, perr
	}

	// Spool-verify: stream to a cache-dir file while hashing (LFS §6.2
	// pattern). The cap is enforced during the stream, so a lying
	// Content-Length cannot over-allocate.
	spoolDir := s.SpoolDir
	if spoolDir == "" {
		spoolDir = os.TempDir()
	}
	if merr := os.MkdirAll(spoolDir, 0o700); merr != nil {
		return nil, merr
	}
	tmp, terr := os.CreateTemp(spoolDir, "asset-*")
	if terr != nil {
		return nil, terr
	}
	spoolName := tmp.Name()
	defer os.Remove(spoolName) //nolint:errcheck — best-effort cleanup
	defer tmp.Close()          //nolint:errcheck — closed before the backend reads
	h := sha256.New()
	n, cerr := io.Copy(tmp, io.TeeReader(io.LimitReader(body, max+1), h))
	if cerr != nil {
		return nil, cerr
	}
	if n > max {
		return nil, fmt.Errorf("%w: upload exceeds %d bytes", ErrTooLarge, max)
	}
	if contentLength >= 0 && n != contentLength {
		return nil, fmt.Errorf("%w: truncated upload: got %d of %d bytes", ErrInvalid, n, contentLength)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return nil, fmt.Errorf("%w: sha256 mismatch", ErrInvalid)
	}
	if serr := tmp.Sync(); serr != nil {
		return nil, serr
	}
	if cerr := tmp.Close(); cerr != nil {
		return nil, cerr
	}

	key := AssetKey(owner, repo, tag, name)
	entry := &AssetEntry{Name: name, Size: n, SHA256: sha, ContentType: ct,
		UploadedAt: s.nowUTC().Format(dateTimeFmt), Uploader: normPrincipal(p.Name)}
	// Bytes Create arbitrates name races; resolve 412s against the header
	// (same-sha = done, clash = 409, orphan-match = adopt, vanished =
	// retry, then 503 rather than appending blind).
	appendEntry := false
	for attempt := 0; attempt < 3; attempt++ {
		putErr := s.putFileCreate(ctx, key, spoolName)
		if putErr == nil {
			appendEntry = true
			break
		}
		if !store.IsPreconditionFailed(putErr) {
			return nil, putErr
		}
		done, adopt, derr := s.resolveAssetClash(ctx, owner, repo, tag, entry)
		if derr != nil {
			return nil, derr
		}
		if done != nil {
			return done, nil
		}
		if adopt {
			appendEntry = true
			break
		}
	}
	if !appendEntry {
		return nil, fmt.Errorf("%w: asset store unsettled; retry the upload", ErrUnavailable)
	}
	return s.appendAssetEntry(ctx, owner, repo, tag, entry)
}

// putFileCreate Creates key from the spool file (streamed by the backend,
// never re-buffered here).
func (s *Service) putFileCreate(ctx context.Context, key, spoolName string) error {
	_, err := s.Store.Put(ctx, key, store.PutBody{File: spoolName}, store.PutOptions{Mode: store.PutCreate})
	return err
}

// resolveAssetClash decides a byte-Create 412: header entry with the same
// name+sha ⇒ idempotent success (done); same name different sha ⇒ 409
// (derr); no entry (orphan) ⇒ stream-verify the stored bytes
// (failure-path verification): match ⇒ adopt, verify-error ⇒ retry
// (adopt=false, derr=nil → the caller retries the Create).
func (s *Service) resolveAssetClash(ctx context.Context, owner, repo, tag string, entry *AssetEntry) (done *AssetEntry, adopt bool, derr error) {
	hraw, _, err := s.getJSON(ctx, ReleaseKey(owner, repo, tag))
	if err != nil {
		return nil, false, err
	}
	if hraw == nil {
		return nil, false, fmt.Errorf("%w: unknown release %q", ErrNotFound, tag)
	}
	r, perr := parseRelease(hraw)
	if perr != nil {
		return nil, false, perr
	}
	if i := findAsset(r, entry.Name); i >= 0 {
		if r.Assets[i].SHA256 == entry.SHA256 {
			return &r.Assets[i], false, nil
		}
		return nil, false, fmt.Errorf("%w: asset %q already uploaded with a different digest", ErrConflict, entry.Name)
	}
	match, verr := s.assetBytesMatch(ctx, AssetKey(owner, repo, tag, entry.Name), entry.SHA256)
	if verr != nil {
		return nil, false, nil // concurrently removed — let the caller retry Create
	}
	if !match {
		return nil, false, fmt.Errorf("%w: asset %q already uploaded with a different digest", ErrConflict, entry.Name)
	}
	return nil, true, nil
}

// assetBytesMatch streams the stored object and compares its sha256.
func (s *Service) assetBytesMatch(ctx context.Context, key, sha string) (bool, error) {
	res, err := s.Store.Get(ctx, key, store.GetOptions{})
	if err != nil {
		return false, err
	}
	obj, ok := res.(store.Object)
	if !ok {
		return false, fmt.Errorf("%w: asset changed during read", ErrConflict)
	}
	defer obj.Body.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(obj.Body, s.maxAssetBytes()+1)); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == sha, nil
}

// appendAssetEntry CAS-appends the entry (concurrent attaches both
// survive; same-name same-sha converges; same-name different-sha 409s).
func (s *Service) appendAssetEntry(ctx context.Context, owner, repo, tag string, entry *AssetEntry) (*AssetEntry, error) {
	var done *AssetEntry
	_, err := s.casUpdate(ctx, ReleaseKey(owner, repo, tag), 8, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown release %q", ErrNotFound, tag)
		}
		r, perr := parseRelease(cur)
		if perr != nil {
			return nil, false, perr
		}
		if i := findAsset(r, entry.Name); i >= 0 {
			if r.Assets[i].SHA256 == entry.SHA256 {
				done = &r.Assets[i]
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("%w: asset %q already uploaded with a different digest", ErrConflict, entry.Name)
		}
		r.Assets = append(r.Assets, *entry)
		r.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		r.Version++
		done = entry
		return encodeRelease(r), true, nil
	})
	if err != nil {
		return nil, err
	}
	return done, nil
}

// DeleteAsset CAS-removes the header entry and deletes the bytes (write
// gate). Entry-first ordering keeps the header authoritative; a crash
// leaves orphan bytes (harmless, same class as upload orphans).
func (s *Service) DeleteAsset(ctx context.Context, owner, repo string, p auth.Principal, tag, name string) (*AssetEntry, error) {
	if err := s.requireRole(ctx, owner, repo, p, string(identity.RoleWrite)); err != nil {
		return nil, err
	}
	tag, err := validateTag(tag)
	if err != nil {
		return nil, err
	}
	name, err = validateAssetName(name)
	if err != nil {
		return nil, err
	}
	var removed *AssetEntry
	_, err = s.casUpdate(ctx, ReleaseKey(owner, repo, tag), 8, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown release %q", ErrNotFound, tag)
		}
		r, perr := parseRelease(cur)
		if perr != nil {
			return nil, false, perr
		}
		i := findAsset(r, name)
		if i < 0 {
			return nil, false, fmt.Errorf("%w: unknown asset %q", ErrNotFound, name)
		}
		removed = &r.Assets[i]
		r.Assets = append(r.Assets[:i], r.Assets[i+1:]...)
		// append onto a found index never yields nil (i >= 0 implies a
		// non-nil slice), and encodeRelease normalizes nil anyway.
		r.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		r.Version++
		return encodeRelease(r), true, nil
	})
	if err != nil {
		return nil, err
	}
	if derr := s.Store.Delete(ctx, AssetKey(owner, repo, tag, name), ""); derr != nil && !store.IsNotFound(derr) {
		return nil, derr
	}
	return removed, nil
}

// --- autodraft (§3, synchronous, read-level) ---------------------------------

// sharedIndexKey renders the 03/02-owned shared P4 index key (02 owns the
// shape; this package reads its kind:"pr" cards as autodraft candidates).
func sharedIndexKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/issues/index.json"
}

// sharedThreadKey renders one shared P3 header key (02 owns the shape).
func sharedThreadKey(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%06x/thread.json", owner, repo, num)
}

// prSidecarKey renders one 03 pr.json sidecar key (03 owns the shape;
// autodraft reads merged/merge_commit_sha/merged_at only).
func prSidecarKey(owner, repo string, num int) string {
	return fmt.Sprintf("repos/%s/%s/pulls/%06x/pr.json", owner, repo, num)
}

// indexDoc is the shared P4 index projection this package reads (02 owns
// the shape; unknown fields ignored).
type indexDoc struct {
	Open         []indexCard `json:"open"`
	ClosedRecent []indexCard `json:"closed_recent"`
}

// indexCard is one shared card projection (title/author for autodraft).
type indexCard struct {
	Num    int    `json:"num"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Author string `json:"author"`
}

// threadHead is the minimal thread.json projection (title/author).
type threadHead struct {
	Title  string `json:"title"`
	Author string `json:"author"`
}

// prSidecar is the minimal pr.json projection (03 owns the shape).
type prSidecar struct {
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
}

// autodraftCandidate is one merged PR under test.
type autodraftCandidate struct {
	num      int
	title    string
	author   string
	mergeSHA string
	mergedAt string
}

// Autodraft produces the suggested body for the release form: merged PRs
// (from the shared index's kind:"pr" cards + pr.json sidecars) whose merge
// commit is an ancestor of tag but NOT of since. since defaults to the
// current latest release's tag, else the tag preceding `tag` in creation
// order, else "" (all reachable history — the "first commit" lower bound
// is vacuous).
func (s *Service) Autodraft(ctx context.Context, owner, repo string, p auth.Principal, tag, since string) (*Autodraft, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	tag, verr := validateTag(tag)
	if verr != nil {
		return nil, verr
	}
	tagSHA, err := s.resolveTag(ctx, owner, repo, tag)
	if err != nil {
		return nil, err
	}
	sinceLabel := since
	var sinceSHA string
	if since == "" {
		sinceLabel, sinceSHA = s.defaultSince(ctx, owner, repo, tag)
	} else {
		sinceSHA, err = s.resolveAnyRef(ctx, owner, repo, since)
		if err != nil {
			return nil, err
		}
	}
	cands, more, err := s.mergedCandidates(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	// Newest merge first BEFORE the probe cap, so the bounded git budget
	// always covers the most relevant candidates.
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].mergedAt != cands[j].mergedAt {
			return cands[i].mergedAt > cands[j].mergedAt
		}
		return cands[i].num > cands[j].num
	})
	kept := cands
	if len(cands) > 0 {
		// Git availability is gated once, up front, by resolveTag: a nil
		// Git/Dirs fails the request before candidates load, so the probe
		// loop below never sees an unwired runner.
		dir, derr := s.Dirs.Dir(ctx, repoName(owner, repo))
		if derr != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, derr)
		}
		kept = kept[:0]
		for i, c := range cands {
			if i >= MaxAutodraftPRs {
				more = true
				break
			}
			inTag, err := s.Git.IsAncestor(ctx, dir, c.mergeSHA, tagSHA)
			if err != nil || !inTag {
				continue
			}
			if sinceSHA != "" {
				inSince, err := s.Git.IsAncestor(ctx, dir, c.mergeSHA, sinceSHA)
				if err != nil {
					return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
				}
				if inSince {
					continue
				}
			}
			kept = append(kept, c)
		}
		// Newest merge first (unparseable merged_at sorts oldest). The
		// probe loop above already caps kept at MaxAutodraftPRs, so no
		// second truncation is needed here.
		sort.SliceStable(kept, func(i, j int) bool {
			if kept[i].mergedAt != kept[j].mergedAt {
				return kept[i].mergedAt > kept[j].mergedAt
			}
			return kept[i].num > kept[j].num
		})
	}
	prs := make([]AutodraftPR, 0, len(kept))
	for _, c := range kept {
		prs = append(prs, AutodraftPR{Num: c.num, Title: c.title, Author: c.author})
	}
	return &Autodraft{Tag: tag, Since: sinceLabel, Body: draftBody(prs), PRs: prs, More: more}, nil
}

// defaultSince resolves the default since: latest release tag when one
// exists, else the tag preceding `tag` in creation order, else "".
// Git availability is gated up front by resolveTag (the single gate for
// the request); callers never reach here unwired.
func (s *Service) defaultSince(ctx context.Context, owner, repo, tag string) (string, string) {
	if raw, _, err := s.getJSON(ctx, LatestKey(owner, repo)); err == nil && raw != nil {
		if ptr, perr := parseLatest(raw); perr == nil && ptr.Tag != "" && ptr.Tag != tag {
			if sha, rerr := s.resolveAnyRef(ctx, owner, repo, ptr.Tag); rerr == nil {
				return ptr.Tag, sha
			}
		}
	}
	dir, err := s.Dirs.Dir(ctx, repoName(owner, repo))
	if err != nil {
		return "", ""
	}
	tags, err := s.Git.ListTags(ctx, dir)
	if err != nil {
		return "", ""
	}
	for i, t := range tags {
		if t == tag && i+1 < len(tags) {
			if sha, rerr := s.resolveAnyRef(ctx, owner, repo, tags[i+1]); rerr == nil {
				return tags[i+1], sha
			}
			return tags[i+1], ""
		}
	}
	return "", ""
}

// resolveAnyRef resolves an arbitrary ref (since may name a branch or a
// tag; the runner peels to the commit). Availability rides resolveTag's
// gate — callers never reach here unwired.
func (s *Service) resolveAnyRef(ctx context.Context, owner, repo, ref string) (string, error) {
	dir, err := s.Dirs.Dir(ctx, repoName(owner, repo))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	sha, err := s.Git.ResolveRef(ctx, dir, ref)
	if err != nil {
		return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, ref)
	}
	return sha, nil
}

// mergedCandidates reads merged PRs from the shared index window
// (open + closed-recent kind:"pr" cards), enriching each from its thread
// header (title/author) and pr.json sidecar (merge sha/at). The index is
// the hot window (P4); older merges fall out of autodraft scope —
// documented in Decisions (LIST backfill deferred, same class as P4).
func (s *Service) mergedCandidates(ctx context.Context, owner, repo string) ([]autodraftCandidate, bool, error) {
	raw, _, err := s.getJSON(ctx, sharedIndexKey(owner, repo))
	if err != nil {
		return nil, false, err
	}
	if raw == nil {
		return nil, false, nil
	}
	var idx indexDoc
	if jerr := json.Unmarshal(raw, &idx); jerr != nil {
		return nil, false, fmt.Errorf("%w: issues/index.json: %v", ErrCorrupt, jerr)
	}
	cards := append(append([]indexCard{}, idx.Open...), idx.ClosedRecent...)
	var out []autodraftCandidate
	for _, c := range cards {
		if c.Kind != "pr" || c.Num <= 0 {
			continue
		}
		title, author := c.Title, c.Author
		if traw, _, terr := s.getJSON(ctx, sharedThreadKey(owner, repo, c.Num)); terr == nil && traw != nil {
			var th threadHead
			if jerr := json.Unmarshal(traw, &th); jerr == nil {
				if th.Title != "" {
					title = th.Title
				}
				if th.Author != "" {
					author = th.Author
				}
			}
		}
		praw, _, perr := s.getJSON(ctx, prSidecarKey(owner, repo, c.Num))
		if perr != nil || praw == nil {
			continue
		}
		var side prSidecar
		if jerr := json.Unmarshal(praw, &side); jerr != nil {
			continue
		}
		if !side.Merged || side.MergeCommitSHA == nil || *side.MergeCommitSHA == "" {
			continue
		}
		at := ""
		if side.MergedAt != nil {
			at = *side.MergedAt
		}
		out = append(out, autodraftCandidate{num: c.Num, title: title, author: author, mergeSHA: *side.MergeCommitSHA, mergedAt: at})
	}
	if out == nil {
		out = []autodraftCandidate{}
	}
	return out, false, nil
}
