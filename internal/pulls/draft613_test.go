package pulls

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// Forgejo #613 (option a): draft PRs — open-as-draft plus mark-ready /
// convert-to-draft flips. Draft rides the additive pr.json field (law 5:
// no schema change); flips are author-or-triage like state transitions and
// append draft_changed thread events mirroring the state_changed convention.

func boolPtr(b bool) *bool { return &b }

func TestDraft613Open(t *testing.T) {
	e := newTestEnv()
	_, pr := openBasic(t, e, "o", "r")
	if pr.Draft {
		t.Fatal("default open must not be a draft")
	}
	// Open-as-draft on a fresh head pair.
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/other": hexSHA(3)})
	_, dpr, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{
		Title: "WIP work", BaseRef: "refs/heads/main", HeadRef: "refs/heads/other", Draft: true,
	}, "")
	if err != nil {
		t.Fatalf("OpenPR draft: %v", err)
	}
	if !dpr.Draft {
		t.Fatal("open with draft:true must record Draft")
	}
}

func TestDraft613Flip(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["tri@example.com"] = "triage"
	_, pr := openBasic(t, e, "o", "r")
	if pr.Draft {
		t.Fatal("precondition: default open is ready")
	}
	// Author converts to draft.
	_, dpr, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Draft: boolPtr(true)})
	if err != nil {
		t.Fatalf("convert to draft: %v", err)
	}
	if !dpr.Draft {
		t.Fatal("pr must be draft after flip")
	}
	// The flip appends a draft_changed event (ready → draft narration).
	events, err := e.svc.scanEvents(ctx(), "o", "r", 1)
	if err != nil {
		t.Fatalf("scanEvents: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != EventDraftChanged || last.From == nil || last.To == nil ||
		*last.From != "ready" || *last.To != "draft" {
		t.Fatalf("event = %+v", last)
	}
	// Stream + notify fan-out mirror the state flips.
	foundStream := false
	for _, s := range e.streams() {
		if s.Name == "pull" && s.Action == "converted_to_draft" && s.Num == 1 {
			foundStream = true
		}
	}
	if !foundStream {
		t.Fatalf("no converted_to_draft stream: %+v", e.streams())
	}
	foundNotify := false
	for _, n := range e.notifies() {
		if n.Class == "converted_to_draft" && n.PullNum == 1 {
			foundNotify = true
		}
	}
	if !foundNotify {
		t.Fatalf("no converted_to_draft notify: %+v", e.notifies())
	}
	// Triage (non-author) marks ready.
	_, rpr, err := e.svc.UpdatePR(ctx(), "o", "r", 1, auth.Principal{Name: "tri@example.com"}, PRPatch{Draft: boolPtr(false)})
	if err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if rpr.Draft {
		t.Fatal("pr must be ready after flip back")
	}
	events, _ = e.svc.scanEvents(ctx(), "o", "r", 1)
	last = events[len(events)-1]
	if last.Type != EventDraftChanged || *last.From != "draft" || *last.To != "ready" {
		t.Fatalf("event = %+v", last)
	}
	foundStream = false
	for _, s := range e.streams() {
		if s.Action == "ready_for_review" && s.Num == 1 {
			foundStream = true
		}
	}
	if !foundStream {
		t.Fatalf("no ready_for_review stream: %+v", e.streams())
	}
	// A no-op flip (same value) writes nothing new.
	before, _ := e.svc.scanEvents(ctx(), "o", "r", 1)
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Draft: boolPtr(false)}); err != nil {
		t.Fatalf("no-op flip: %v", err)
	}
	after, _ := e.svc.scanEvents(ctx(), "o", "r", 1)
	if len(after) != len(before) {
		t.Fatalf("no-op flip appended an event: %d → %d", len(before), len(after))
	}
}

func TestDraft613FlipRoles(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// Below-triage non-author cannot flip.
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, auth.Principal{Name: "mallory@example.com"}, PRPatch{Draft: boolPtr(true)}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	// Anonymous cannot flip.
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, auth.Anonymous(), PRPatch{Draft: boolPtr(true)}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestDraft613FlipMergedRefused(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	markMerged594(t, e, "o", "r", 1)
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Draft: boolPtr(true)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestDraft613MergeRefused(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	e.seedRefs("o/r", map[string]string{
		"refs/heads/main":  hexSHA(1),
		"refs/heads/topic": hexSHA(2),
	})
	_, pr, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{
		Title: "WIP", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic", Draft: true,
	}, "")
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if !pr.Draft {
		t.Fatal("precondition: PR is a draft")
	}
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
	if err != nil {
		t.Fatalf("StartMerge: %v", err)
	}
	_ = rec
	done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	if done == nil || done.State != TaskError || !strings.Contains(done.Error, "draft") {
		t.Fatalf("task = %+v", done)
	}
	if len(e.refs.updatesFor("refs/heads/main")) != 0 {
		t.Fatal("draft merge must not publish")
	}
}

func TestDraft613MarkReadyThenMerge(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	e.seedRefs("o/r", map[string]string{
		"refs/heads/main":  hexSHA(1),
		"refs/heads/topic": hexSHA(2),
	})
	if _, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{
		Title: "WIP", BaseRef: "refs/heads/main", HeadRef: "refs/heads/topic", Draft: true,
	}, ""); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Draft: boolPtr(false)}); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
	if err != nil {
		t.Fatalf("StartMerge: %v", err)
	}
	_ = rec
	done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	if done == nil || done.State != TaskOK {
		t.Fatalf("task = %+v", done)
	}
}

func TestDraft613HTTP(t *testing.T) {
	for _, lane := range []string{"api", "browser"} {
		t.Run("lane="+lane, func(t *testing.T) {
			e := newTestEnv()
			e.roles.Roles["jane@example.com"] = "write"
			e.roles.Public = true
			e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1), "refs/heads/topic": hexSHA(2)})
			// Open-as-draft over the wire.
			w := doReq(t, e.h, "POST", lane, lanePath(lane, "/pulls"),
				`{"title":"WIP","base_ref":"refs/heads/main","head_ref":"refs/heads/topic","draft":true}`, writer())
			if w.Code != 201 {
				t.Fatalf("open draft = %d (%s)", w.Code, w.Body.String())
			}
			var opened map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &opened); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if pr, _ := opened["pr"].(map[string]any); pr["draft"] != true {
				t.Fatalf("wire pr.draft = %v", pr["draft"])
			}
			// Flip back over the wire.
			w = doReq(t, e.h, "PUT", lane, lanePath(lane, "/pulls/1"), `{"draft":false}`, writer())
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"draft":false`) {
				t.Fatalf("put draft = %d (%s)", w.Code, w.Body.String())
			}
			// Unknown keys still fail closed; bad draft types 400.
			if w := doReq(t, e.h, "PUT", lane, lanePath(lane, "/pulls/1"), `{"draft":false,"zzz":1}`, writer()); w.Code != 400 {
				t.Fatalf("unknown key = %d", w.Code)
			}
			if w := doReq(t, e.h, "POST", lane, lanePath(lane, "/pulls"), `{"title":"x","base_ref":"refs/heads/main","head_ref":"refs/heads/topic","draft":"yes"}`, writer()); w.Code != 400 {
				t.Fatalf("bad draft type = %d", w.Code)
			}
		})
	}
}
