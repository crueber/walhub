package api

// gaps4_test.go: final cheap arms — task-subsystem failures, policy PUT
// guards, dispatch short-circuits.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// failingTasks fails one Tasks method on demand.
type failingTasks struct {
	fakeTasks
	fail map[string]error
}

func (f *failingTasks) Begin(ctx context0, id gitRepoId2, op string, params map[string]string) (TaskStream, error) {
	if err := f.fail["Begin"]; err != nil {
		return TaskStream{}, err
	}
	return f.fakeTasks.Begin(ctx, id, op, params)
}

func (f *failingTasks) Attach(ctx context0, id gitRepoId2, taskID string) (TaskStream, bool, error) {
	if err := f.fail["Attach"]; err != nil {
		return TaskStream{}, false, err
	}
	return f.fakeTasks.Attach(ctx, id, taskID)
}

func (f *failingTasks) Get(ctx context0, id gitRepoId2, taskID string) (TaskRecord, bool, error) {
	if err := f.fail["Get"]; err != nil {
		return TaskRecord{}, false, err
	}
	return f.fakeTasks.Get(ctx, id, taskID)
}

// aliases keep override signatures compact.
type context0 = context.Context
type gitRepoId2 = git.RepoId

func TestTaskFailureArms(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	ft := &failingTasks{fail: map[string]error{}}
	ft.running = f.tasks.running
	ft.recent = f.tasks.recent
	ft.records = f.tasks.records
	ft.streams = f.tasks.streams
	f.env.Tasks = ft

	ft.fail["Begin"] = errBoom
	if w := f.req("POST", "/demo/walgit/api/ops/sync"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Begin fail = %d", w.Code)
	}
	ft.fail["Begin"] = nil

	ft.fail["Attach"] = errBoom
	hdr := map[string]string{"Accept": "text/event-stream"}
	p := &auth.Principal{Name: "jane"}
	if w := f.do("GET", "/demo/walgit/api/tasks/t1", nil, hdr, p); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Attach fail = %d", w.Code)
	}
	ft.fail["Attach"] = nil

	ft.fail["Get"] = errBoom
	if w := f.req("GET", "/demo/walgit/api/tasks/t1"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Get fail = %d", w.Code)
	}
}

func TestPolicyPutGuards(t *testing.T) {
	f := newFixture(t)
	// oversize body → 413
	big := strings.Repeat("x", 1<<20+2)
	if w := f.req("PUT", "/demo/walgit/api/policy", strings.NewReader(big)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize policy = %d", w.Code)
	}
	// corrupt stored doc → PUT fails closed with 503
	putPolicyRaw(t, f, `{"version":1,"rules":[{"name":"x","effect":{"protect":{"restricts":["bogus"]}}}]}`)
	if w := f.req("PUT", "/demo/walgit/api/policy", strings.NewReader(allowAllPolicy)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt stored PUT = %d", w.Code)
	}
}

func TestDispatchShortCircuits(t *testing.T) {
	f := newFixture(t)
	// single raw segment → not found
	if w := f.req("GET", "/justjunk"); w.Code != http.StatusNotFound {
		t.Fatalf("short path = %d", w.Code)
	}
	// HEAD on a GET route → 405 (method enforced by the dispatcher)
	if w := f.req("HEAD", "/demo/walgit/api"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD = %d", w.Code)
	}
}
