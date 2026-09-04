package checks

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// Second edge file: read/index/token/gate failure branches, codec
// corruption, and the remaining handler routes.

// phantomStore lists one extra key that GETs as absent (the raced-delete
// skip in loadStatuses/ListTokens).
type phantomStore struct {
	store.ObjectStore
	phantom string
}

func (p *phantomStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if err := p.ObjectStore.List(ctx, prefix, startAfter, fn); err != nil {
		return err
	}
	if strings.HasPrefix(p.phantom, prefix) {
		return fn(store.ObjectMeta{Key: p.phantom})
	}
	return nil
}

func TestCoverLoadStatusesEdge(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(80)
	e.knowSHA(sha)
	e.mustReport(t, sha, "ci", StateSuccess)
	// Corrupt object ⇒ read fails closed.
	if _, err := store.PutBytes(ctx(), e.store, StatusKey("o", "r", sha, "broken"), []byte("{oops"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.GetStatuses(ctx(), "o", "r", sha, reader()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt: %v", err)
	}
	_ = e.store.Delete(ctx(), StatusKey("o", "r", sha, "broken"), store.Version(""))
	// Phantom key (listed, then absent) is skipped.
	e.svc.Store = &phantomStore{ObjectStore: e.store, phantom: StatusKey("o", "r", sha, "ghost")}
	view, err := e.svc.GetStatuses(ctx(), "o", "r", sha, reader())
	if err != nil || len(view.Statuses) != 1 {
		t.Fatalf("phantom: %+v %v", view, err)
	}
	// Store failures surface (never a partial view).
	e.svc.Store = &errStore{inner: e.store, getErr: errors.New("down")}
	if _, err := e.svc.GetStatuses(ctx(), "o", "r", sha, reader()); err == nil {
		t.Fatal("get failure accepted")
	}
	e.svc.Store = &errStore{inner: e.store, listErr: errors.New("down")}
	if _, err := e.svc.Combined(ctx(), "o", "r", sha, reader()); err == nil {
		t.Fatal("list failure accepted")
	}
	// A failed context surfaces only when the fan-out actually blocks;
	// the memory backend answers inline, so cancellation is not a
	// deterministic unit probe here (the select guards real backends).
	// combinedFor degrades to pending on read failure (the write already
	// committed — never a 500 for the reporter).
	e.svc.Store = &errStore{inner: e.store, listErr: errors.New("down")}
	if got := e.svc.combinedFor(ctx(), "o", "r", sha); got != StatePending {
		t.Fatalf("degraded = %q", got)
	}
	e.svc.Store = e.store
}

func TestCoverIndexEdge(t *testing.T) {
	e := newTestEnv()
	// Corrupt index ⇒ list fails closed.
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), []byte("{oops"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.ListChecks(ctx(), "o", "r", reader(), "", 10); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt list: %v", err)
	}
	if _, err := e.svc.loadIndex(ctx(), "o", "r"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt load: %v", err)
	}
	// updateIndex on a corrupt index drops silently (projection only).
	if n := e.svc.updateIndex(ctx(), "o", "r", &StatusDoc{SHA: hexSHA(81), Context: "ci", State: StateSuccess}); n != 0 {
		t.Fatalf("corrupt update = %d", n)
	}
	_ = e.store.Delete(ctx(), IndexKey("o", "r"), store.Version(""))
	// Store failures drop silently too.
	e.svc.Store = &errStore{inner: e.store, getErr: errors.New("down")}
	if n := e.svc.updateIndex(ctx(), "o", "r", &StatusDoc{SHA: hexSHA(81), Context: "ci", State: StateSuccess}); n != 0 {
		t.Fatalf("failing update = %d", n)
	}
	e.svc.Store = &errStore{inner: e.store, putErr: errors.New("down")}
	if n := e.svc.updateIndex(ctx(), "o", "r", &StatusDoc{SHA: hexSHA(81), Context: "ci", State: StateSuccess}); n != 0 {
		t.Fatalf("failing put = %d", n)
	}
	// Permanent contention exhausts silently (LIST covers reads).
	e.svc.Store = &errStore{inner: e.store, put412: true}
	if n := e.svc.updateIndex(ctx(), "o", "r", &StatusDoc{SHA: hexSHA(81), Context: "ci", State: StateSuccess}); n != 0 {
		t.Fatalf("contended update = %d", n)
	}
	e.svc.Store = e.store
	// Compact: absent ⇒ false; corrupt ⇒ error; window-only trim.
	if compacted, err := e.svc.CompactIndex(ctx(), "o", "r"); err != nil || compacted {
		t.Fatalf("absent: %v %v", compacted, err)
	}
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), []byte("{oops"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.CompactIndex(ctx(), "o", "r"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt compact: %v", err)
	}
	_ = e.store.Delete(ctx(), IndexKey("o", "r"), store.Version(""))
	ix := &IndexDoc{SHAs: []IndexSHA{}}
	for i := 0; i < IndexHotWindow+50; i++ {
		sha := strings.Repeat("c", 38) + sprintf2(i)
		ix.SHAs = append(ix.SHAs, IndexSHA{SHA: sha, State: StateSuccess,
			Contexts:  []IndexContext{{Name: "ci", State: StateSuccess, UpdatedAt: "2026-09-04T12:00:00Z"}},
			UpdatedAt: "2026-09-04T12:00:00Z"})
	}
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), encodeIndex(ix),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	compacted, err := e.svc.CompactIndex(ctx(), "o", "r")
	if err != nil || !compacted {
		t.Fatalf("window trim: %v %v", compacted, err)
	}
	got, _ := e.svc.loadIndex(ctx(), "o", "r")
	if len(got.SHAs) != IndexHotWindow {
		t.Fatalf("window = %d", len(got.SHAs))
	}
	// Compact store failure surfaces; contention drops silently.
	e.svc.Store = &errStore{inner: e.store, getErr: errors.New("down")}
	if _, err := e.svc.CompactIndex(ctx(), "o", "r"); err == nil {
		t.Fatal("compact read failure accepted")
	}
	e.svc.Store = &errStore{inner: e.store, put412: true}
	if compacted, err := e.svc.CompactIndex(ctx(), "o", "r"); err != nil || compacted {
		t.Fatalf("compact contention: %v %v", compacted, err)
	}
	e.svc.Store = &errStore{inner: e.store, putErr: errors.New("down")}
	// Force an oversized index so the PUT path runs.
	big := &IndexDoc{SHAs: got.SHAs}
	big.SHAs[0].Contexts[0].Name = strings.Repeat("q", IndexSizeLimit)
	_, _ = store.PutBytes(ctx(), e.store, IndexKey("o", "r"), encodeIndex(big),
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"})
	if _, err := e.svc.CompactIndex(ctx(), "o", "r"); err == nil {
		t.Fatal("compact put failure accepted")
	}
	e.svc.Store = e.store
}

func sprintf2(i int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[(i>>4)&0xf], digits[i&0xf]})
}

func TestCoverTokensEdge(t *testing.T) {
	e := newTestEnv()
	// Corrupt token record ⇒ load fails closed.
	if _, err := store.PutBytes(ctx(), e.store, TokenKey("o", "r", "deadbeef"), []byte("{oops"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.ListTokens(ctx(), "o", "r", admin()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt list: %v", err)
	}
	if err := e.svc.RevokeToken(ctx(), "o", "r", "deadbeef", admin()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt revoke: %v", err)
	}
	_ = e.store.Delete(ctx(), TokenKey("o", "r", "deadbeef"), store.Version(""))
	// Phantom token (listed, then absent) is skipped.
	e.svc.Store = &phantomStore{ObjectStore: e.store, phantom: TokenKey("o", "r", "ghost123")}
	list, err := e.svc.ListTokens(ctx(), "o", "r", admin())
	if err != nil || len(list) != 0 {
		t.Fatalf("phantom: %+v %v", list, err)
	}
	e.svc.Store = e.store
	// Store failures surface.
	e.svc.Store = &errStore{inner: e.store, listErr: errors.New("down")}
	if _, err := e.svc.ListTokens(ctx(), "o", "r", admin()); err == nil {
		t.Fatal("list failure accepted")
	}
	e.svc.Store = e.store
}

func TestCoverNotifyEdge(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(90)
	other := hexSHA(91)
	e.knowSHA(sha)
	e.knowSHA(other)
	// Corrupt shared index ⇒ best-effort silence (report still 200s).
	if _, err := store.PutBytes(ctx(), e.store, "repos/o/r/issues/index.json", []byte("{oops"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var emitted []NotifyEvent
	e.svc.Notify = func(_ context.Context, ev NotifyEvent) { emitted = append(emitted, ev) }
	e.mustReport(t, sha, "ci", StateFailure)
	if len(emitted) != 0 {
		t.Fatalf("corrupt index emit = %+v", emitted)
	}
	_ = e.store.Delete(ctx(), "repos/o/r/issues/index.json", store.Version(""))
	// Merged PRs and closed threads never notify.
	seedPR(t, e, 11, "open", true, sha)    // merged ⇒ skip
	seedPR(t, e, 12, "closed", false, sha) // closed card ⇒ skip (not in open)
	e.mustReport(t, sha, "ci", StateError)
	if len(emitted) != 0 {
		t.Fatalf("merged/closed emit = %+v", emitted)
	}
	// Cap: openPRHeads stops at maxRows.
	seedPR(t, e, 13, "open", false, other)
	heads, err := e.svc.openPRHeads(ctx(), "o", "r", 1)
	if err != nil || len(heads) != 1 {
		t.Fatalf("cap: %+v %v", heads, err)
	}
	// Missing pr.json sidecar ⇒ skipped.
	heads, err = e.svc.openPRHeads(ctx(), "o", "r", 200)
	if err != nil {
		t.Fatalf("heads: %v", err)
	}
	for _, h := range heads {
		if h.Num == 12 {
			t.Fatalf("closed PR listed: %+v", heads)
		}
	}
}

func TestCoverGateEdge(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(92)
	// Policy read failure surfaces (fail closed).
	e.svc.Store = &errStore{inner: e.store, getErr: errors.New("down")}
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "m"); err == nil {
		t.Fatal("policy failure accepted")
	}
	e.svc.Store = e.store
	// Status read failure surfaces.
	e.putPolicy(t, `{"version":1,"rules":[{"name":"g","match":{},"effect":{"protect":{"require_checks":["ci"]}}}]}`)
	e.svc.Store = &errStore{inner: e.store, listErr: errors.New("statuses down")}
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "m"); err == nil {
		t.Fatal("status failure accepted")
	}
	e.svc.Store = e.store
}

func TestCoverCodec(t *testing.T) {
	if _, err := parseStatus([]byte("{oops")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("status corrupt: %v", err)
	}
	if _, err := parseStatus([]byte(`{"sha":"x"}`)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("status short: %v", err)
	}
	if _, err := parseToken([]byte("{oops")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("token corrupt: %v", err)
	}
	if _, err := parseToken([]byte(`{"id":"x"}`)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("token short: %v", err)
	}
	tok, err := parseToken([]byte(`{"id":"abcd1234","token_hash":"h"}`))
	if err != nil || tok.Scopes == nil || len(tok.Scopes) != 0 {
		t.Fatalf("token scopes: %+v %v", tok, err)
	}
	ix, err := parseIndex([]byte(`{"shas":[{"sha":"x","state":"pending"}]}`))
	if err != nil || ix.SHAs[0].Contexts == nil || len(ix.SHAs[0].Contexts) != 0 {
		t.Fatalf("index norms: %+v %v", ix, err)
	}
	if _, err := parseIndex([]byte("{oops")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("index corrupt: %v", err)
	}
	if nonNilStr(nil) == nil || len(nonNilStr([]string{"a"})) != 1 {
		t.Fatal("nonNilStr")
	}
}
