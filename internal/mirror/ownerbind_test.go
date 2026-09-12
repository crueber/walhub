// ownerbind_test.go — Forgejo #346: the create-from-URL twin enforces the
// creation owner-admission rule BEFORE CreateRepo (a deny writes nothing).
package mirror

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

func ownerHook(allow map[string]bool, unavailable bool) func(context.Context, string, auth.Principal) *auth.AuthError {
	return func(_ context.Context, owner string, p auth.Principal) *auth.AuthError {
		if unavailable {
			return &auth.AuthError{Kind: auth.ErrUnavailable, Why: "org membership unavailable"}
		}
		if p.Admin || allow[owner] {
			return nil
		}
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "owner " + owner + " not permitted"}
	}
}

func TestCreateFromURLOwnerAdmission(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	up := initUpstream(t)
	fileBody := func(owner, name string) string {
		return `{"source_url":` + quote("file://"+up) + `,"owner":"` + owner + `","name":"` + name + `"}`
	}
	writer := auth.Principal{Name: "dev", Write: true}

	// Admitted owner → 202.
	h := testHandler(t, svc, reg, writer)
	h.CheckCreateOwner = ownerHook(map[string]bool{"dev": true}, false)
	rec := doHandle(h, http.MethodPost, "/api/v1/repos/mirrors", fileBody("dev", "m-ok"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("admitted = %d: %s", rec.Code, rec.Body.String())
	}
	// Join the background sync before returning: it clones into CacheDir
	// (a t.TempDir), and a sync still cloning while TempDir cleanup runs
	// RemoveAll fails the test with "directory not empty" under CI load
	// (Forgejo #397; the TestCreateFromURL precedent).
	waitAsync(t, svc, taskID(t, rec.Body.Bytes()), 30*time.Second)
	// Foreign owner → 403 naming the owner, and nothing is created.
	h2 := testHandler(t, svc, reg, writer)
	h2.CheckCreateOwner = ownerHook(map[string]bool{"dev": true}, false)
	rec = doHandle(h2, http.MethodPost, "/api/v1/repos/mirrors", fileBody("acme", "m-no"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "acme") {
		t.Fatalf("403 must name the owner: %q", rec.Body.String())
	}
	if _, err := reg.Open(context.Background(), "acme/m-no"); err == nil {
		t.Fatal("denied create must not create the repo")
	}
	// Probe failure → 503, never 403.
	h3 := testHandler(t, svc, reg, writer)
	h3.CheckCreateOwner = ownerHook(nil, true)
	if rec := doHandle(h3, http.MethodPost, "/api/v1/repos/mirrors", fileBody("acme", "m-down")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable = %d: %s", rec.Code, rec.Body.String())
	}
	// Nil hook → legacy-open (instances without the identity surface).
	h4 := testHandler(t, svc, reg, writer)
	rec = doHandle(h4, http.MethodPost, "/api/v1/repos/mirrors", fileBody("acme", "m-legacy"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("nil hook = %d: %s", rec.Code, rec.Body.String())
	}
	waitAsync(t, svc, taskID(t, rec.Body.Bytes()), 30*time.Second)
}

// taskID extracts the async task id from a 202 create-from-URL body.
func taskID(t *testing.T, body []byte) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	task, _ := out["task"].(map[string]any)
	id, _ := task["id"].(string)
	if id == "" {
		t.Fatalf("202 body carries no task id: %s", body)
	}
	return id
}
