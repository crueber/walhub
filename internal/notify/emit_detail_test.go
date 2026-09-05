package notify

// issue #98 regression tests: Emission.Detail is composition-supplied
// (map[string]any) and may hold values encoding/json rejects (chan,
// func, ...). Such an emission must fail with a logged drop — never
// panic the synchronous mutating handler — while valid Details fan out
// untouched.

import (
	"encoding/json"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// panicJSON is a value whose marshaler panics (issue #98: the last
// panic path — encoding/json propagates MarshalJSON panics, so both
// encode and marshalable must convert them into errors).
type panicJSON struct{}

func (panicJSON) MarshalJSON() ([]byte, error) { panic("boom") }

// TestEncodeRejectsUnmarshalable pins the defensive encode contract:
// values encoding/json rejects return an error (no panic), fixed shapes
// and JSON-shaped Details encode cleanly. marshalable must agree with
// encode on every row.
func TestEncodeRejectsUnmarshalable(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		wantErr bool
	}{
		{"chan", map[string]any{"ch": make(chan int)}, true},
		{"func", map[string]any{"fn": func() {}}, true},
		{"nested_chan", map[string]any{"outer": map[string]any{"ch": make(chan string, 1)}}, true},
		{"nested_func_slice", map[string]any{"fns": []any{func() int { return 1 }}}, true},
		{"complex", map[string]any{"c": complex(1, 2)}, true},
		{"panicking_marshaler", map[string]any{"p": panicJSON{}}, true},
		{"nil_detail", map[string]any(nil), false},
		{"empty_detail", map[string]any{}, false},
		{"check_shaped", map[string]any{"sha": "abc123", "state": "failure", "pr": 7}, false},
		{"fixed_shape", Notification{ID: "id", Repo: "acme/repo"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("encode panicked: %v", r)
				}
			}()
			_, err := encode(tc.v)
			if (err != nil) != tc.wantErr {
				t.Fatalf("encode err = %v, wantErr = %v", err, tc.wantErr)
			}
			if marshalable(tc.v) == tc.wantErr {
				t.Fatalf("marshalable = %v, wantErr = %v", marshalable(tc.v), tc.wantErr)
			}
		})
	}
}

// TestEmitPoisonDetailDropsLogged feeds chan/func-carrying Details
// through the emit entry: no panic, a logged drop, zero store writes
// (no seq consumed, no tray entries, no activity event) — and the
// service still fans out a valid emission afterwards (handler intact).
func TestEmitPoisonDetailDropsLogged(t *testing.T) {
	poisons := []struct {
		name   string
		detail map[string]any
	}{
		{"chan", map[string]any{"ch": make(chan int)}},
		{"func", map[string]any{"fn": func() {}}},
		{"nested_chan", map[string]any{"outer": map[string]any{"ch": make(chan string, 1)}}},
		{"nested_func_slice", map[string]any{"fns": []any{func() int { return 1 }}}},
		{"panicking_marshaler", map[string]any{"p": panicJSON{}}},
	}
	for _, tc := range poisons {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("emit panicked on poison detail: %v", r)
				}
			}()
			x := newHarness(t)
			x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
			x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
			buf := captureLog(x.svc)

			x.svc.emit(ctx(), Emission{Repo: "acme/repo", Num: 7, Kind: "issue",
				Class: "subscribed", Actor: "bob@example.com",
				Recipients: []string{"carol@example.com"}, Detail: tc.detail})

			if got := buf.String(); !strings.Contains(got, "unmarshalable detail") {
				t.Fatalf("poison drop not logged: %q", got)
			}
			if got := countNotifs(t, x, "carol@example.com"); got != 0 {
				t.Fatalf("carol notifications = %d, want 0", got)
			}
			if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev != nil {
				t.Fatalf("phantom activity event: %+v", ev)
			}
			if _, _, err := store.GetBytes(ctx(), x.svc.Store, CollabStateKey("acme", "repo"), store.GetOptions{}); !store.IsNotFound(err) {
				t.Fatalf("poisoned emission consumed a seq (err=%v)", err)
			}
			// Handler intact: a valid emission on the same service
			// lands at seq 1 (no gap was burned above).
			x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"carol@example.com"})
			if got := countNotifs(t, x, "carol@example.com"); got != 1 {
				t.Fatalf("post-poison carol notifications = %d, want 1", got)
			}
			if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev == nil {
				t.Fatal("post-poison valid emission left no activity event")
			}
		})
	}
}

// TestEmitValidDetailFansOut proves the screen does not disturb valid
// Details: the emission lands and the Detail round-trips through the
// activity payload byte-identically.
func TestEmitValidDetailFansOut(t *testing.T) {
	valids := []struct {
		name   string
		detail map[string]any
	}{
		{"nil", nil},
		{"empty", map[string]any{}},
		{"check_shaped", map[string]any{"sha": "abc123", "context": "ci", "state": "failure", "description": "d", "target_url": "https://example.com/hook", "pr": 7}},
		{"nested", map[string]any{"a": "x", "n": float64(3), "b": true, "m": map[string]any{"k": "v"}, "s": []any{"a", float64(1)}}},
	}
	for _, tc := range valids {
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t)
			x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
			x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
			buf := captureLog(x.svc)

			x.svc.emit(ctx(), Emission{Repo: "acme/repo", Num: 7, Kind: "issue",
				Class: "subscribed", Actor: "bob@example.com",
				Recipients: []string{"carol@example.com"}, Detail: tc.detail})

			if got := buf.String(); strings.Contains(got, "unmarshalable detail") {
				t.Fatalf("valid detail dropped: %q", got)
			}
			if got := countNotifs(t, x, "carol@example.com"); got != 1 {
				t.Fatalf("carol notifications = %d, want 1", got)
			}
			ev := x.svc.readActivity(ctx(), "acme", "repo", 1)
			if ev == nil {
				t.Fatal("no activity event")
			}
			var payload activityPayload
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if len(tc.detail) == 0 {
				if len(payload.Detail) != 0 {
					t.Fatalf("payload detail = %v, want empty", payload.Detail)
				}
				return
			}
			if string(mustEncode(t, payload.Detail)) != string(mustEncode(t, tc.detail)) {
				t.Fatalf("payload detail = %v, want %v", payload.Detail, tc.detail)
			}
		})
	}
}

// TestAppendActivityPoisonReturnsError covers the second layer: even a
// direct appendActivity caller (bypassing the emit screen) gets an
// error — never a panic — while valid Details still append.
func TestAppendActivityPoisonReturnsError(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("appendActivity panicked: %v", r)
		}
	}()
	x := newHarness(t)

	err := x.svc.appendActivity(ctx(), "acme", "repo", 1,
		Emission{Repo: "acme/repo", Num: 7, Kind: "issue", Class: "subscribed",
			Detail: map[string]any{"ch": make(chan int)}},
		"commented", "Bug title", "bob@example.com", "", nil, false)
	if err == nil {
		t.Fatal("appendActivity with poison detail = nil error, want non-nil")
	}
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev != nil {
		t.Fatalf("phantom activity event: %+v", ev)
	}

	err = x.svc.appendActivity(ctx(), "acme", "repo", 1,
		Emission{Repo: "acme/repo", Num: 7, Kind: "issue", Class: "subscribed",
			Detail: map[string]any{"sha": "abc123"}},
		"commented", "Bug title", "bob@example.com", "", nil, false)
	if err != nil {
		t.Fatalf("appendActivity with valid detail: %v", err)
	}
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev == nil {
		t.Fatal("valid appendActivity left no activity event")
	}
}
