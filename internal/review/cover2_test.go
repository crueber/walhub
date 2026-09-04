package review

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// This file drives every HTTP handler's error arms (auth failures,
// unknown PRs, wrong methods) plus the tiny non-nil helpers, so the
// per-statement gate rests on exercised wire behavior.

func TestHTTPErrorArms(t *testing.T) {
	anchor := `{"path":"src/main.go","side":"NEW","new_start":120,"new_lines":3,` +
		`"commit_sha":"` + testHead + `",` +
		`"context_sha":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`
	for _, tc := range []struct {
		name         string
		method, path string
		token, body  string
		want         int
	}{
		{"list unknown PR", "GET", "/o/r/api/pulls/999/reviews", "carol", "", 404},
		{"list anon", "GET", "/o/r/api/pulls/7/reviews", "", "", 401},
		{"submit unknown PR", "POST", "/o/r/api/pulls/999/reviews", "bob", `{"state":"APPROVED","commit_sha":"` + testHead + `"}`, 404},
		{"submit anon", "POST", "/o/r/api/pulls/7/reviews", "", `{"state":"APPROVED","commit_sha":"` + testHead + `"}`, 401},
		{"get unknown PR", "GET", "/o/r/api/pulls/999/reviews/1", "carol", "", 404},
		{"get review wrong method", "POST", "/o/r/api/pulls/7/reviews/1", "carol", "", 405},
		{"dismiss unknown review", "POST", "/o/r/api/pulls/7/reviews/99/dismiss", "bob", `{"reason":"x"}`, 404},
		{"dismiss unknown PR", "POST", "/o/r/api/pulls/999/reviews/1/dismiss", "bob", `{"reason":"x"}`, 404},
		{"dismiss wrong method", "GET", "/o/r/api/pulls/7/reviews/1/dismiss", "bob", "", 405},
		{"dismiss anon", "POST", "/o/r/api/pulls/7/reviews/1/dismiss", "", `{"reason":"x"}`, 401},
		{"open unknown PR", "POST", "/o/r/api/pulls/999/threads", "carol", `{"anchor":` + anchor + `,"body":"x"}`, 404},
		{"open anon", "POST", "/o/r/api/pulls/7/threads", "", `{"anchor":` + anchor + `,"body":"x"}`, 401},
		{"threads unknown PR", "GET", "/o/r/api/pulls/999/threads", "carol", "", 404},
		{"threads anon", "GET", "/o/r/api/pulls/7/threads", "", "", 401},
		{"get thread unknown PR", "GET", "/o/r/api/pulls/999/threads/00000001", "carol", "", 404},
		{"get thread anon", "GET", "/o/r/api/pulls/7/threads/00000001", "", "", 401},
		{"comment unknown tid", "POST", "/o/r/api/pulls/7/threads/00000009/comments", "bob", `{"body":"x"}`, 404},
		{"comment unknown PR", "POST", "/o/r/api/pulls/999/threads/00000001/comments", "bob", `{"body":"x"}`, 404},
		{"comment anon", "POST", "/o/r/api/pulls/7/threads/00000001/comments", "", `{"body":"x"}`, 401},
		{"resolve unknown tid", "POST", "/o/r/api/pulls/7/threads/00000009/resolve", "bob", "", 404},
		{"resolve unknown PR", "POST", "/o/r/api/pulls/999/threads/00000001/resolve", "bob", "", 404},
		{"resolve anon", "POST", "/o/r/api/pulls/7/threads/00000001/resolve", "", "", 401},
		{"unresolve wrong method", "GET", "/o/r/api/pulls/7/threads/00000001/unresolve", "bob", "", 405},
		{"requests unknown PR", "GET", "/o/r/api/pulls/999/review-requests", "carol", "", 404},
		{"requests anon", "GET", "/o/r/api/pulls/7/review-requests", "", "", 401},
		{"requests wrong method", "PUT", "/o/r/api/pulls/7/review-requests", "alice", "", 405},
		{"add requests unknown PR", "POST", "/o/r/api/pulls/999/review-requests", "alice", `{"reviewers":["bob"]}`, 404},
		{"remove requests unknown PR", "DELETE", "/o/r/api/pulls/999/review-requests", "alice", `{"reviewers":["bob"]}`, 404},
		{"suggest unknown PR", "GET", "/o/r/api/pulls/999/review-suggest", "carol", "", 404},
		{"suggest wrong method", "POST", "/o/r/api/pulls/7/review-suggest", "carol", "", 405},
		{"auth down reviews", "GET", "/o/r/api/pulls/7/reviews", "broken", "", 503},
		{"auth down submit", "POST", "/o/r/api/pulls/7/reviews", "broken", `{"state":"COMMENTED","commit_sha":"` + testHead + `"}`, 503},
		{"auth down get", "GET", "/o/r/api/pulls/7/reviews/1", "broken", "", 503},
		{"auth down dismiss", "POST", "/o/r/api/pulls/7/reviews/1/dismiss", "broken", `{"reason":"x"}`, 503},
		{"auth down threads", "GET", "/o/r/api/pulls/7/threads", "broken", "", 503},
		{"auth down open", "POST", "/o/r/api/pulls/7/threads", "broken", `{"anchor":` + anchor + `,"body":"x"}`, 503},
		{"auth down thread", "GET", "/o/r/api/pulls/7/threads/00000001", "broken", "", 503},
		{"auth down comment", "POST", "/o/r/api/pulls/7/threads/00000001/comments", "broken", `{"body":"x"}`, 503},
		{"auth down resolve", "POST", "/o/r/api/pulls/7/threads/00000001/resolve", "broken", "", 503},
		{"auth down requests", "GET", "/o/r/api/pulls/7/review-requests", "broken", "", 503},
		{"auth down add-req", "POST", "/o/r/api/pulls/7/review-requests", "broken", `{"reviewers":["x"]}`, 503},
		{"auth down rm-req", "DELETE", "/o/r/api/pulls/7/review-requests", "broken", `{"reviewers":["x"]}`, 503},
		{"auth down suggest", "GET", "/o/r/api/pulls/7/review-suggest", "broken", "", 503},
		{"null body", "POST", "/o/r/api/pulls/7/reviews", "bob", "null", 400},
		{"suggest extra tail", "GET", "/o/r/api/pulls/7/review-suggest/extra", "carol", "", 404},
		{"threads wrong method", "DELETE", "/o/r/api/pulls/7/threads", "carol", "", 405},
		{"short path", "GET", "/x", "carol", "", 404},
		{"empty repo", "GET", "/o//api/pulls/7/reviews", "carol", "", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := testHandler(t)
			w := doReq(h, tc.method, tc.path, tc.token, tc.body)
			if w.Code != tc.want {
				t.Fatalf("= %d (%s), want %d", w.Code, w.Body.String(), tc.want)
			}
		})
	}
}

func TestNonNilHelpers(t *testing.T) {
	if nonNilReviews(nil) == nil || nonNilThreads(nil) == nil || nonNilComments(nil) == nil || nonNilReviewers(nil) == nil {
		t.Fatalf("nil normalization broken")
	}
	if _, err := parseSeq(""); err == nil {
		t.Fatalf("empty seq accepted")
	}
	if n, err := parseSeq("0"); err != nil || n != 0 {
		t.Fatalf("seq 0: %d %v", n, err)
	}
	if n, err := parseNum("16777215"); err != nil || n != 16777215 {
		t.Fatalf("max num: %d %v", n, err)
	}
	w := httptest.NewRecorder()
	writeJSON(w, 200, func() {})
	if w.Code != 500 {
		t.Fatalf("unserializable: %d", w.Code)
	}
	// Codec nil-arms: absent slices normalize to [] (never null on the wire).
	hdr, err := parsePRHeader([]byte(`{"num":7,"kind":"pr","review_summary":{"decision":"APPROVED"}}`))
	if err != nil || hdr.Labels == nil || hdr.Assignees == nil || hdr.Participants == nil {
		t.Fatalf("%+v %v", hdr, err)
	}
	if hdr.ReviewSummary.Latest == nil || hdr.ReviewSummary.Requested == nil {
		t.Fatalf("%+v", hdr.ReviewSummary)
	}
	enc := encodePRHeader(&PRHeader{Num: 1})
	if !strings.Contains(string(enc), `"labels":[]`) {
		t.Fatalf("%s", enc)
	}
	reqs, err := parseReviewRequests([]byte(`{"version":1}`))
	if err != nil || reqs.Reviewers == nil {
		t.Fatalf("%+v %v", reqs, err)
	}
	if string(encodeReviewRequests(&ReviewRequests{})) == "" {
		t.Fatalf("empty encode")
	}
	if uniqSorted(nil) == nil {
		t.Fatalf("uniqSorted(nil) must be []")
	}
}
