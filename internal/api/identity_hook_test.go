package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// stubAccess is a scripted ReadAccess gate.
type stubAccess struct{ err *auth.AuthError }

func (s *stubAccess) CheckRead(_ context.Context, _, _ string, _ auth.Principal) *auth.AuthError {
	return s.err
}

func TestDispatchAccessHook(t *testing.T) {
	newSeeded := func(t *testing.T) *fixture {
		f := newFixture(t)
		seedSummary(f)
		return f
	}
	authed := &auth.Principal{Name: "jane"}
	cases := []struct {
		name string
		gate ReadAccess
		code int
	}{
		{"nil gate (legacy)", nil, http.StatusOK},
		{"allow", &stubAccess{}, http.StatusOK},
		{"anonymous denied", &stubAccess{err: &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}}, http.StatusUnauthorized},
		{"forbidden", &stubAccess{err: &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}}, http.StatusForbidden},
		{"unavailable", &stubAccess{err: &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}}, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		f := newSeeded(t)
		f.env.Access = tc.gate
		w := f.do("GET", "/demo/walgit/api", nil, nil, authed)
		if w.Code != tc.code {
			t.Errorf("%s: = %d, want %d (%s)", tc.name, w.Code, tc.code, w.Body.String())
		}
		if tc.code == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: 401 must carry Bearer", tc.name)
		}
	}
	// Non-read routes skip the hook even when set.
	f := newSeeded(t)
	f.env.Access = &stubAccess{err: &auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}}
	w := f.do("PUT", "/demo/walgit/api", strings.NewReader("{}"), nil,
		&auth.Principal{Name: "jane", Write: true})
	if w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "no") {
		t.Error("write routes must not consult the read gate")
	}
}

func TestFindOpSpecExtension(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	// gaps_test.go's opsTasks overrides the frozen table with an extension
	// table; the extension op must start through the union rule.
	f.env.Tasks = &opsTasks{fakeTasks: *f.tasks, ops: []OpSpec{{Op: "access-bootstrap"}}}
	f.tasks.streams["op:access-bootstrap"] = streamWithOutcome(
		TaskRecord{ID: "t9", Kind: "access-bootstrap", Repo: "demo/walgit", Hostname: "host-a"},
		TaskDone{Record: TaskRecord{ID: "t9"}})
	w := f.do("POST", "/demo/walgit/api/ops/access-bootstrap", nil,
		map[string]string{"Accept": "text/event-stream"}, &auth.Principal{Name: "jane", Write: true})
	if w.Code != http.StatusOK {
		t.Fatalf("extension op start = %d (%s)", w.Code, w.Body.String())
	}
	// Unknown with a non-empty extension table → 404.
	if w := f.do("POST", "/demo/walgit/api/ops/nope", nil, nil, &auth.Principal{Name: "jane", Write: true}); w.Code != http.StatusNotFound {
		t.Fatalf("unknown op = %d", w.Code)
	}
}

// stubExpander resolves team: spellings from a fixed roster.
type stubExpander struct{ teams map[string][]string }

func (s *stubExpander) ExpandGroups(_ context.Context, members []string) ([]string, []string) {
	var out, warn []string
	for _, m := range members {
		if team, ok := strings.CutPrefix(m, "team:"); ok {
			if t, ok := s.teams[team]; ok {
				out = append(out, t...)
				continue
			}
			warn = append(warn, "unresolvable team reference "+m)
			continue
		}
		out = append(out, m)
	}
	return out, warn
}

func TestDryRunExpansion(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.env.GroupExpander = &stubExpander{teams: map[string][]string{"acme/bots": {"svc:bot"}}}
	f.view.pushes["demo/walgit"] = []PushRecord{{
		Seq: 7, Principal: "bob@example.com",
		Refs: []PushRef{{Name: "refs/heads/main", Old: fakeSHA, New: fakeSHA2}},
	}}
	body := `{"version":1,"rules":[
		{"name":"bots-only","match":{"refs":["refs/heads/main"]},
		 "effect":{"protect":{"restricts":["update"],"bypass":["team:acme/bots"]}}},
		{"name":"ghost","match":{"refs":["refs/heads/other"]},
		 "effect":{"protect":{"restricts":["update"],"bypass":["team:acme/ghost"]}}},
		{"name":"plain","match":{"refs":["refs/heads/other"]},
		 "effect":{"protect":{"restricts":["update"]}}}]}`

	w := f.do("POST", "/demo/walgit/api/policy/dry-run?last=1", strings.NewReader(body),
		map[string]string{"Content-Type": "application/json"}, &auth.Principal{Name: "jane"})
	if w.Code != http.StatusOK {
		t.Fatalf("dry-run = %d (%s)", w.Code, w.Body.String())
	}
	var out struct {
		Denied   int      `json:"denied"`
		Warnings []string `json:"warnings"`
	}
	decodeJSON(t, w, &out)
	if out.Denied != 1 {
		t.Errorf("bob must be denied by bots-only: %+v", out)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "team:acme/ghost") {
		t.Errorf("warnings = %v", out.Warnings)
	}
}
