package checks

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Fourth edge file: the last uncovered branches for margin above the
// 95% gate.

func TestCoverAuthRemainder(t *testing.T) {
	// Basic without a colon: the whole decoded value is the token.
	req := httptest.NewRequest("GET", "/o/r/api/checks", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("justatoken")))
	if got := extractToken(req); got != "justatoken" {
		t.Fatalf("colonless basic: %q", got)
	}
	// Malformed wct_ ⇒ no secret.
	req2 := httptest.NewRequest("GET", "/o/r/api/checks", nil)
	req2.Header.Set("Authorization", "Bearer wct_short")
	if _, _, ok := CISecretOf(req2); ok {
		t.Fatal("malformed secret claimed")
	}
	// api-browser top-level ⇒ not a checks route; non-lane repo path ⇒
	// not a checks route.
	e, h := testHandler()
	for _, path := range []string{"/api-browser/v1/repos", "/o/r/tree/main"} {
		if h.Handle(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil)) {
			t.Fatalf("%s claimed", path)
		}
	}
	_ = e
	// decodeStrict: allowed keys but wrong types ⇒ 400.
	authed := &Handler{Svc: e.svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return admin(), nil
	}}
	rec := doRequest(authed, "POST", "/o/r/api/checks/statuses/"+hexSHA(120), `{"context":123,"state":"success"}`, "")
	if rec.Code != 400 {
		t.Fatalf("typed body: %d", rec.Code)
	}
}

func TestCoverOpenPRHeadsRemainder(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(121)
	e.knowSHA(sha)
	seedPR(t, e, 31, "open", false, sha)
	// Corrupt thread.json ⇒ skipped, no error.
	overwrite(t, e, "repos/o/r/issues/00001f/thread.json", `{oops`)
	heads, err := e.svc.openPRHeads(ctx(), "o", "r", 200)
	if err != nil || len(heads) != 0 {
		t.Fatalf("corrupt thread: %+v %v", heads, err)
	}
	// Index reads fine but sidecar reads fail ⇒ skipped, no error.
	e2 := newTestEnv()
	e2.knowSHA(sha)
	seedPR(t, e2, 32, "open", false, sha)
	e2.svc.Store = &selectiveFail{ObjectStore: e2.store}
	heads, err = e2.svc.openPRHeads(ctx(), "o", "r", 200)
	if err != nil || len(heads) != 0 {
		t.Fatalf("sidecar failure: %+v %v", heads, err)
	}
}

// selectiveFail fails every GET except the shared index (the open-PR
// lookup's best-effort skips).
type selectiveFail struct{ store.ObjectStore }

func (s *selectiveFail) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if strings.HasSuffix(key, "issues/index.json") {
		return s.ObjectStore.Get(ctx, key, opts)
	}
	return nil, errors.New("sidecar down")
}

func TestCoverTokenRemainder(t *testing.T) {
	e := newTestEnv()
	// Oversized name ⇒ 400.
	if _, err := e.svc.CreateToken(ctx(), "o", "r", admin(), strings.Repeat("x", 101), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long name: %v", err)
	}
	// Store failure on create ⇒ surfaces.
	e.svc.Store = &errStore{inner: e.store, putErr: errors.New("down")}
	if _, err := e.svc.CreateToken(ctx(), "o", "r", admin(), "x", nil); err == nil {
		t.Fatal("create failure accepted")
	}
	e.svc.Store = e.store
	// loadToken: store failure and corrupt record.
	e.svc.Store = &errStore{inner: e.store, getErr: errors.New("down")}
	if _, _, err := e.svc.loadToken(ctx(), "o", "r", "abcd1234"); err == nil {
		t.Fatal("token read failure accepted")
	}
	e.svc.Store = e.store
	if _, err := store.PutBytes(ctx(), e.store, TokenKey("o", "r", "abcd1234"), []byte("{oops"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := e.svc.loadToken(ctx(), "o", "r", "abcd1234"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt token: %v", err)
	}
}
