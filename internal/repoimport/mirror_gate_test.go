// mirror_gate_test.go — Forgejo #240 R1 (e): Begin on a live
// mirror.json target is a 409 (the sync writer owns the refs).
package repoimport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func mirrorGateParams(t *testing.T, svc *Service, owner, name string) Params {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"source_url": "file://" + t.TempDir(), "owner": owner, "name": name,
	})
	params, _, err := ParseRequest(raw, svc.cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return params
}

func TestBeginOnMirrorIs409(t *testing.T) {
	ctx := context.Background()
	svc, st := testService(t, nil, nil)
	p := auth.Principal{Name: "op", Write: true}
	params := mirrorGateParams(t, svc, "acme", "m")
	// Live mirror sidecar → 409 naming the mirror.
	if _, err := store.PutBytes(ctx, st, store.MirrorKey("acme", "m"),
		[]byte(`{"version":1,"upstream_url":"file:///x","schedule":"daily"}`),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := svc.Begin(ctx, p, params, "")
	if err == nil || !strings.Contains(err.Error(), "read-only mirror") {
		t.Fatalf("err = %v, want mirror 409", err)
	}
	if se, ok := err.(*StatusError); !ok || se.Status != 409 {
		t.Fatalf("err = %#v, want 409 StatusError", err)
	}
	// Sidecar gone → the gate passes (Begin proceeds to the claim path).
	if err := st.Delete(ctx, store.MirrorKey("acme", "m"), ""); err != nil {
		t.Fatal(err)
	}
	res, _, err := svc.Begin(ctx, p, params, "")
	if err != nil {
		t.Fatalf("unmirrored begin: %v", err)
	}
	if res == nil || res.TaskID == "" {
		t.Fatalf("res = %+v", res)
	}
	// Join the background drive before returning: it clones into
	// cfg.Cache.Dir (a t.TempDir), and an import still cloning while
	// TempDir cleanup runs RemoveAll fails the test with "directory not
	// empty" under CI load (Forgejo #397). The source is an empty dir,
	// never a git repo, so the terminal outcome is deterministically an
	// error — the clone cannot succeed on any git version.
	o := awaitDone(t, svc, res.TaskID, 60*time.Second)
	if o.Err == nil {
		t.Fatalf("empty-dir import should fail, got %+v", o)
	}
}

func TestMirroredTargetFaults(t *testing.T) {
	ctx := context.Background()
	svc, _ := testService(t, nil, nil)
	// Nil store → 503.
	bare := New(Deps{})
	if _, err := bare.mirroredTarget(ctx, "a", "b"); err == nil {
		t.Fatal("nil-store probe succeeded")
	}
	// Absent → false, no error.
	mirrored, err := svc.mirroredTarget(ctx, "acme", "none")
	if err != nil || mirrored {
		t.Fatalf("absent = %v %v", mirrored, err)
	}
}
