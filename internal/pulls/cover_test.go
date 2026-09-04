package pulls

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// flakyStore injects store failures: failGets maps key → error for Get;
// failPuts maps key → remaining 412 failures for Put; failPutsErr maps
// key → a non-412 Put error (backend outage, never a CAS signal). All maps
// are mutex-guarded: tests toggle faults while background recompute/merge
// passes may be in flight, so faults NEVER swap the Service's store.
type flakyStore struct {
	mu          sync.Mutex
	inner       store.ObjectStore
	failGets    map[string]error
	failPuts    map[string]int
	failPutsErr map[string]error
}

func (f *flakyStore) Backend() string { return f.inner.Backend() }

func (f *flakyStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failGets[key]; ok {
		return nil, err
	}
	return f.inner.Get(ctx, key, opts)
}

func (f *flakyStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	return f.inner.Head(ctx, key)
}

func (f *flakyStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failPutsErr[key]; ok {
		return store.ObjectMeta{}, err
	}
	if n, ok := f.failPuts[key]; ok && n > 0 {
		f.failPuts[key] = n - 1
		return store.ObjectMeta{}, store.NewPrecondition(key, "v9")
	}
	return f.inner.Put(ctx, key, body, opts)
}

func (f *flakyStore) Delete(ctx context.Context, key string, v store.Version) error {
	return f.inner.Delete(ctx, key, v)
}

func (f *flakyStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	return f.inner.List(ctx, prefix, startAfter, fn)
}

func (f *flakyStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	return f.inner.ListPrefixes(ctx, prefix, fn)
}

func (f *flakyStore) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error) {
	return f.inner.SignedGetURL(ctx, key, ttl)
}

func (f *flakyStore) AccelTarget(ctx context.Context, key string) (*store.AccelTarget, error) {
	return f.inner.AccelTarget(ctx, key)
}

func (f *flakyStore) SupportsCompose() bool { return f.inner.SupportsCompose() }
func (f *flakyStore) ComposeIsNative() bool { return f.inner.ComposeIsNative() }

func (f *flakyStore) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	return f.inner.Compose(ctx, dst, sources, opts)
}

func TestCoverStatusAndValidation(t *testing.T) {
	for err, want := range map[error]int{
		ErrNotFound: 404, ErrInvalid: 400, ErrUnauthorized: 401, ErrForbidden: 403,
		ErrConflict: 409, ErrUnprocessable: 422, ErrUnavailable: 503, errors.New("x"): 500,
	} {
		if got := statusFor(err); got != want {
			t.Fatalf("statusFor(%v) = %d, want %d", err, got, want)
		}
	}
	if _, err := validateTitle(strings.Repeat("t", MaxTitleLen+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long title: %v", err)
	}
	for _, sha := range []string{"abc", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "ABCDEF1234", ""} {
		if err := validateSHA(sha); !errors.Is(err, ErrInvalid) {
			t.Fatalf("sha %q: %v", sha, err)
		}
	}
	if err := validateSHA(hexSHA(1)); err != nil {
		t.Fatalf("good sha: %v", err)
	}
	if err := validateStrategy(""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty strategy: %v", err)
	}
	if got := nonNilStr(nil); got == nil || len(got) != 0 {
		t.Fatalf("nonNilStr: %v", got)
	}
}

func TestCoverCodecs(t *testing.T) {
	if _, err := parseThread([]byte("{oops")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("thread corrupt: %v", err)
	}
	th, err := parseThread([]byte(`{"num":1,"kind":"pr","title":"t"}`))
	if err != nil || th.Labels == nil || th.Assignees == nil || th.Participants == nil {
		t.Fatalf("thread normalize: %+v %v", th, err)
	}
	raw := encodeThread(&Thread{Num: 1})
	var back Thread
	if jerr := json.Unmarshal(raw, &back); jerr != nil {
		t.Fatalf("encodeThread: %v", jerr)
	}
	if _, err := parseEvent([]byte("{oops")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("event corrupt: %v", err)
	}
	if _, err := parseEvent([]byte(`{"seq":0,"type":"opened"}`)); err != nil {
		t.Fatalf("event ok: %v", err)
	}
	if _, err := parsePR([]byte("{oops")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("pr corrupt: %v", err)
	}
	if _, err := parseMergeable([]byte("{oops")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("mergeable corrupt: %v", err)
	}
	m, err := parseMergeable([]byte(`{"state":"clean"}`))
	if err != nil || m.Conflicts == nil {
		t.Fatalf("mergeable normalize: %+v %v", m, err)
	}
	if out := encodeMergeable(&MergeableDoc{}); !strings.Contains(string(out), `"conflicts":[]`) {
		t.Fatalf("encodeMergeable: %s", out)
	}
	if _, err := parseIndex([]byte("{oops")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("index corrupt: %v", err)
	}
	if _, err := parseForks([]byte("{oops")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("forks corrupt: %v", err)
	}
	fx, err := parseForks([]byte(`{"version":1}`))
	if err != nil || fx.Forks == nil {
		t.Fatalf("forks normalize: %+v %v", fx, err)
	}
	// Key helpers (contract pins).
	for _, k := range []string{EventsPrefix("o", "r", 1), IssuesPrefix("o", "r"), PullsPrefix("o", "r")} {
		if k == "" {
			t.Fatal("empty key")
		}
	}
}

func TestCoverRolesNil(t *testing.T) {
	e := newTestEnv()
	e.svc.Roles = nil
	admin := auth.Principal{Name: "a@x", Admin: true}
	if got := e.svc.roleOf(ctx(), "o", "r", admin); got != "admin" {
		t.Fatalf("admin role = %q", got)
	}
	if got := e.svc.roleOf(ctx(), "o", "r", auth.Principal{Name: "w@x", Write: true}); got != "write" {
		t.Fatalf("write role = %q", got)
	}
	if got := e.svc.roleOf(ctx(), "o", "r", auth.Anonymous()); got != "" {
		t.Fatalf("anon role = %q", got)
	}
	if got := e.svc.roleOf(ctx(), "o", "r", auth.Principal{Name: "u@x"}); got != "read" {
		t.Fatalf("default role = %q", got)
	}
	if err := e.svc.requireRole(ctx(), "o", "r", admin, "admin"); err != nil {
		t.Fatalf("admin pass: %v", err)
	}
	if err := e.svc.requireRead(ctx(), "o", "r", admin); err != nil {
		t.Fatalf("admin read: %v", err)
	}
	if err := e.svc.requireRead(ctx(), "o", "r", auth.Principal{Name: "w@x", Write: true}); err != nil {
		t.Fatalf("write read: %v", err)
	}
	if err := e.svc.requireRead(ctx(), "o", "r", auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon read: %v", err)
	}
	if err := e.svc.requireRead(ctx(), "o", "r", auth.Principal{Name: "u@x"}); err != nil {
		t.Fatalf("authed read: %v", err)
	}
	svc := New(store.NewMemory(), nil)
	svc.Now = nil
	if svc.nowUTC().IsZero() {
		t.Fatal("nowUTC nil clock")
	}
	for _, tc := range []struct {
		id          string
		name, email string
	}{
		{"Boss <boss@x>", "Boss", "boss@x"},
		{"boss@x", "walhub", "boss@x"},
		{"custom", "custom", "walhub@localhost"},
		{"", "walhub", "walhub@localhost"},
		{"<>", "walhub", "walhub@localhost"},
		{"<onlymail@x>", "walhub", "onlymail@x"},
	} {
		svc.ServerID = tc.id
		if n, em := svc.committer(); n != tc.name || em != tc.email {
			t.Fatalf("committer(%q) = %q %q", tc.id, n, em)
		}
	}
	// Nil emitter/streamer are documented no-ops.
	svc.emit(ctx(), NotifyEvent{})
	svc.stream(ctx(), StreamEvent{})
}

type gateRoles struct {
	FakeRoles
	checkErr *auth.AuthError
}

func (g *gateRoles) CheckRead(_ context.Context, _, _ string, _ auth.Principal) *auth.AuthError {
	return g.checkErr
}

func TestCoverRequireReadGates(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *auth.AuthError
		want error
	}{
		{"forbidden", &auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}, ErrForbidden},
		{"unavailable", &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}, nil},
		{"invalid", &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad"}, ErrUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv()
			e.svc.Roles = &gateRoles{checkErr: tc.err}
			err := e.svc.requireRead(ctx(), "o", "r", auth.Principal{Name: "u@x"})
			if tc.want == nil {
				if err == nil || !strings.Contains(err.Error(), "unavailable") {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestCoverCASHelpers(t *testing.T) {
	e := newTestEnv()
	// attempts <= 0 defaults; fn error aborts; non-412 aborts; exhaustion 409s.
	if _, err := e.svc.casUpdate(ctx(), "k", 0, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return nil, false, errors.New("boom")
	}); err == nil || err.Error() != "boom" {
		t.Fatalf("fn err: %v", err)
	}
	e.failGet("k", errors.New("disk down"))
	if _, err := e.svc.casUpdate(ctx(), "k", 3, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return []byte("{}"), true, nil
	}); err == nil {
		t.Fatal("get err must abort")
	}
	if _, _, err := e.svc.getJSON(ctx(), "k"); err == nil {
		t.Fatal("getJSON err must surface")
	}
	e.clearFails()
	// 412 then success converges.
	e.failPut("k", 1)
	m, err := e.svc.casUpdate(ctx(), "k", 3, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return []byte(`{"v":1}`), true, nil
	})
	if err != nil || m.Key != "k" {
		t.Fatalf("retry: %+v %v", m, err)
	}
	// Persistent 412 exhausts to conflict.
	e.failPut("k2", 99)
	if _, err := e.svc.casUpdate(ctx(), "k2", 3, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return []byte(`{}`), true, nil
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("exhaust: %v", err)
	}
	// Non-412 put error aborts.
	e.clearFails()
	e.failPutErr("k3", errors.New("bad put"))
	if _, err := e.svc.casUpdate(ctx(), "k3", 3, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return []byte(`{}`), true, nil
	}); err == nil || !strings.Contains(err.Error(), "bad put") {
		t.Fatalf("bad put: %v", err)
	}
	e.clearFails()
	// No-write returns current meta without a PUT.
	m, err = e.svc.casUpdate(ctx(), "k", 3, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return nil, false, nil
	})
	if err != nil {
		t.Fatalf("no-write: %v", err)
	}
}

func TestCoverAppendEventPaths(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// Unknown thread.
	if _, _, err := e.svc.appendEvent(ctx(), "o", "r", 99, func(t *Thread, seq int) (*Event, error) {
		return &Event{}, nil
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	// Mutate error aborts.
	if _, _, err := e.svc.appendEvent(ctx(), "o", "r", 1, func(t *Thread, seq int) (*Event, error) {
		return nil, errors.New("mut boom")
	}); err == nil {
		t.Fatal("mut err must abort")
	}
	// No-op (nil event) returns the thread untouched.
	th, ev, err := e.svc.appendEvent(ctx(), "o", "r", 1, func(t *Thread, seq int) (*Event, error) {
		return nil, nil
	})
	if err != nil || ev != nil || th == nil {
		t.Fatalf("noop: %+v %v %v", th, ev, err)
	}
	// Corrupt thread.
	raw := []byte("{oops")
	_, _ = store.PutBytes(ctx(), e.store, ThreadKey("o", "r", 2), raw, store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if _, _, err := e.svc.appendEvent(ctx(), "o", "r", 2, func(t *Thread, seq int) (*Event, error) {
		return &Event{}, nil
	}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt: %v", err)
	}
	// 412 on the header CAS retries and converges.
	e.failPut(ThreadKey("o", "r", 1), 1)
	th, ev, err = e.svc.appendEvent(ctx(), "o", "r", 1, func(t *Thread, seq int) (*Event, error) {
		t.NextEventSeq = seq + 1
		t.Version++
		return &Event{Type: "commented"}, nil
	})
	if err != nil || ev == nil || th == nil {
		t.Fatalf("retry: %+v %v %v", th, ev, err)
	}
	e.clearFails()
	// Persistent 412 exhausts.
	e.failPut(ThreadKey("o", "r", 1), 99)
	if _, _, err := e.svc.appendEvent(ctx(), "o", "r", 1, func(t *Thread, seq int) (*Event, error) {
		t.NextEventSeq = seq + 1
		return &Event{}, nil
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("exhaust: %v", err)
	}
	e.clearFails()
}

func TestCoverLoadPaths(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	if _, _, err := e.svc.loadThread(ctx(), "o", "r", 99); err != nil {
		t.Fatalf("absent thread: %v", err)
	}
	if _, _, err := e.svc.loadPR(ctx(), "o", "r", 99); err != nil {
		t.Fatalf("absent pr: %v", err)
	}
	if _, _, err := e.svc.loadMergeable(ctx(), "o", "r", 99); err != nil {
		t.Fatalf("absent mergeable: %v", err)
	}
	if _, _, err := e.svc.loadIndex(ctx(), "o", "r"); err != nil {
		t.Fatalf("index: %v", err)
	}
	// Corrupt objects.
	putCorrupt := func(key string) {
		_, ver, _ := e.svc.getJSON(ctx(), key)
		opts := store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}
		if ver != "" {
			opts.Mode = store.PutUpdate
			opts.IfVersion = ver
		}
		if _, err := store.PutBytes(ctx(), e.store, key, []byte("{oops"), opts); err != nil {
			t.Fatalf("corrupt write %s: %v", key, err)
		}
	}
	putCorrupt(ThreadKey("o", "r", 5))
	putCorrupt(PRKey("o", "r", 5))
	putCorrupt(MergeableKey("o", "r", 5))
	putCorrupt(IndexKey("o", "r"))
	if _, _, err := e.svc.loadThread(ctx(), "o", "r", 5); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("thread: %v", err)
	}
	if _, _, err := e.svc.loadPR(ctx(), "o", "r", 5); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("pr: %v", err)
	}
	if _, _, err := e.svc.loadMergeable(ctx(), "o", "r", 5); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("m: %v", err)
	}
	// Store error surfaces.
	e.failGet(ThreadKey("o", "r", 1), errors.New("down"))
	if _, _, err := e.svc.loadThread(ctx(), "o", "r", 1); err == nil {
		t.Fatal("get err must surface")
	}
	e.clearFails()
	// savePR conflict-exhaustion via always-412 on pr key.
	e.clearFails()
	e.failPut(PRKey("o", "r", 1), 99)
	pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
	_ = pr
	if pr != nil {
		if err := e.svc.savePR(ctx(), "o", "r", pr, "stale-version"); !errors.Is(err, ErrConflict) {
			// savePR retries against the live version; the flaky store 412s
			// every attempt ⇒ conflict.
			t.Fatalf("savePR: %v", err)
		}
	}
	e.clearFails()
	// allocNum corrupt counter.
	_, _ = store.PutBytes(ctx(), e.store, CounterKey("o", "r"), []byte("{oops"), store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	_ = e.store.Delete(ctx(), CounterKey("o", "r"), "")
	_, _ = store.PutBytes(ctx(), e.store, CounterKey("o", "r"), []byte(`{"next":0}`), store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if _, err := e.svc.allocNum(ctx(), "o", "r"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("alloc corrupt: %v", err)
	}
}

func TestCoverRequiredChecksGate(t *testing.T) {
	e := newTestEnv()
	// A rule carrying a require_checks gate with no checks backend fails
	// closed (05 §6) — the strict parse now accepts the key, the gate
	// probe names the rule, and the push half still enforces restricts.
	putPolicy(t, e, `{"version":1,"rules":[{"name":"gated","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["ci"]}}}]}`)
	if err := e.svc.checkRequiredChecksGate(ctx(), "o", "r", strings.Repeat("a", 40), "refs/heads/main", "m"); !errors.Is(err, ErrConflict) {
		t.Fatalf("gate: %v", err)
	} else if !strings.Contains(err.Error(), "gated") || !strings.Contains(err.Error(), "no checks backend") {
		t.Fatalf("gate must name the rule and the missing backend: %v", err)
	}
	if err := e.svc.checkProtectedRef(ctx(), "o", "r", "m", "refs/heads/main", "update"); err == nil || !strings.Contains(err.Error(), "gated") {
		t.Fatalf("protected: %v", err)
	}
	// A bypassed rule contributes nothing — the queue merges fine with no
	// backend (03 §5 step 4: bypass lists apply unchanged).
	putPolicy(t, e, `{"version":1,"rules":[{"name":"gated","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["ci"],"bypass":["m"]}}}]}`)
	if err := e.svc.checkRequiredChecksGate(ctx(), "o", "r", strings.Repeat("a", 40), "refs/heads/main", "m"); err != nil {
		t.Fatalf("bypassed: %v", err)
	}
	// Plain protect rules merge fine with no backend.
	putPolicy(t, e, `{"version":1,"rules":[{"name":"plain","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"]}}}]}`)
	if err := e.svc.checkRequiredChecksGate(ctx(), "o", "r", strings.Repeat("a", 40), "refs/heads/main", "m"); err != nil {
		t.Fatalf("plain: %v", err)
	}
	// Missing policy ⇒ no gate; unparseable ⇒ no gate verdict here
	// (loadPolicy fails closed next).
	_ = e.store.Delete(ctx(), PolicyKey("o", "r"), "")
	if err := e.svc.checkRequiredChecksGate(ctx(), "o", "r", strings.Repeat("a", 40), "refs/heads/main", "m"); err != nil {
		t.Fatalf("absent: %v", err)
	}
	putPolicy(t, e, `{oops`)
	if err := e.svc.checkRequiredChecksGate(ctx(), "o", "r", strings.Repeat("a", 40), "refs/heads/main", "m"); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
}
