package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDrainPhases(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if s.drain.Draining() {
		t.Fatal("not draining initially")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.RunPhase1(ctx, ctx)
	if s.drain.Draining() != true || s.drain.Phase2() {
		t.Fatalf("phase 1: draining=%v phase2=%v", s.drain.Draining(), s.drain.Phase2())
	}
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	s.RunPhase2(nil)
	// After phase 2 the gate refuses new git traffic with the drain shape.
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	gitish := true
	s.drainGate(rec, gitish, func(msg string) {
		rec.WriteHeader(http.StatusOK)
		_, _ = rec.Write(pktErrBody(msg))
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("git-ish drain refusal must be pkt-ERR 200, got %d", rec.Code)
	}
	if _, ok := pktErrOf(rec.Body.String()); !ok {
		t.Fatalf("body = %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.drainGate(rec, false, nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "15" {
		t.Fatalf("non-git drain refusal = %d", rec.Code)
	}
	if s.drain.Phase2() != true {
		t.Fatal("phase 2 must be latched")
	}
}
