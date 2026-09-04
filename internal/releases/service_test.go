package releases

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

func TestTagEncodingRoundTrip(t *testing.T) {
	tags := []string{"v1.2.0", "a/b", "a%2Fb", "100%", "release candidate", "v1+2"}
	for _, tag := range tags {
		enc := encodeTag(tag)
		if strings.Contains(enc, "/") {
			t.Fatalf("encoded tag %q contains a literal slash: %q", tag, enc)
		}
		if got := decodeTag(enc); got != tag {
			t.Fatalf("round trip %q → %q → %q", tag, enc, got)
		}
		key := ReleaseKey("o", "r", tag)
		if strings.Count(strings.TrimPrefix(key, "repos/o/r/releases/"), "/") != 0 {
			t.Fatalf("key spans segments: %q", key)
		}
	}
}

func TestValidateModel(t *testing.T) {
	if _, err := validateTag(""); err == nil {
		t.Fatal("empty tag accepted")
	}
	if _, err := validateTag(strings.Repeat("x", 501)); err == nil {
		t.Fatal("overlong tag accepted")
	}
	for _, bad := range []string{"", "a/b", ".hidden", strings.Repeat("n", 201), " a", "a\nb"} {
		if _, err := validateAssetName(bad); err == nil {
			t.Fatalf("bad asset name accepted: %q", bad)
		}
	}
	if _, err := validateAssetName("walhub-linux-amd64.tar.gz"); err != nil {
		t.Fatalf("good asset name rejected: %v", err)
	}
	if _, err := normalizeSHA256("xyz"); err == nil {
		t.Fatal("bad sha accepted")
	}
	sha := shaOf([]byte("x"))
	if got, err := normalizeSHA256(strings.ToUpper(sha)); err != nil || got != sha {
		t.Fatalf("sha normalize: %v %q", err, got)
	}
	if ct, _ := normalizeContentType(""); ct != "application/octet-stream" {
		t.Fatalf("default content type: %q", ct)
	}
	if _, err := normalizeContentType(strings.Repeat("x", 201)); err == nil {
		t.Fatal("overlong content type accepted")
	}
	big := strings.Repeat("b", MaxBodyBytes+1)
	if _, _, err := validateReleaseInput(ReleaseInput{Body: &big}); err == nil {
		t.Fatal("overlong body accepted")
	}
}

func TestPutReleaseGates(t *testing.T) {
	x := newHarness(t)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	// Anonymous create → 401.
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", auth.Anonymous(), "v1", ReleaseInput{}, ""); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon create: %v", err)
	}
	// Authenticated without role → 403.
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v1", ReleaseInput{}, ""); !isErr(err, ErrForbidden) {
		t.Fatalf("roleless create: %v", err)
	}
	// Write role but unknown tag → 404 unknown revision.
	grantWrite(x)
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "nope", ReleaseInput{}, ""); !isErr(err, ErrNotFound) {
		t.Fatalf("unknown tag: %v", err)
	}
	// Unknown field values → 400.
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v1", ReleaseInput{Body: strptr(strings.Repeat("x", MaxBodyBytes+1))}, ""); !isErr(err, ErrInvalid) {
		t.Fatalf("overlong body: %v", err)
	}
}

func TestPutReleaseCreatePublish(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1.0.0"] = strings.Repeat("c", 40)
	var notified []NotifyEvent
	var streamed []StreamEvent
	x.svc.Notify = func(_ context.Context, ev NotifyEvent) { notified = append(notified, ev) }
	x.svc.Stream = func(_ context.Context, ev StreamEvent) { streamed = append(streamed, ev) }

	rel, created, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v1.0.0",
		ReleaseInput{Name: strptr("First"), Body: strptr("hello")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected created=true")
	}
	if rel.TagSHA != strings.Repeat("c", 40) || rel.Name != "First" || rel.Draft {
		t.Fatalf("bad header: %+v", rel)
	}
	if rel.PublishedAt == nil {
		t.Fatal("published release needs published_at")
	}
	if len(notified) != 1 || notified[0].Tag != "v1.0.0" {
		t.Fatalf("publish fan-out: %+v", notified)
	}
	if len(streamed) != 1 || streamed[0].Action != "published" {
		t.Fatalf("publish stream: %+v", streamed)
	}
	// Latest pointer tracks the publish.
	got, _, err := x.svc.LatestRelease(ctx(), "o", "r", writer(), false)
	if err != nil || got.Tag != "v1.0.0" {
		t.Fatalf("latest: %+v %v", got, err)
	}
	// Tag moves do NOT rewrite the release (snapshot stands).
	x.git.tags["v1.0.0"] = strings.Repeat("d", 40)
	got2, _, _ := x.svc.GetRelease(ctx(), "o", "r", writer(), "v1.0.0")
	if got2.TagSHA != strings.Repeat("c", 40) {
		t.Fatalf("tag_sha moved: %q", got2.TagSHA)
	}
}

func TestPutReleaseDraftPublishFlip(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v2"] = strings.Repeat("e", 40)
	var notified int
	x.svc.Notify = func(_ context.Context, _ NotifyEvent) { notified++ }

	rel, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v2",
		ReleaseInput{Draft: boolptr(true), Name: strptr("D")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !rel.Draft || rel.PublishedAt != nil {
		t.Fatalf("draft shape: %+v", rel)
	}
	if notified != 0 {
		t.Fatal("draft create must not fan out")
	}
	// Draft is hidden from the public list and latest.
	if rels, _, err := x.svc.ListReleases(ctx(), "o", "r", writer(), 50, ""); err != nil || len(rels) != 0 {
		t.Fatalf("draft listed: %+v %v", rels, err)
	}
	// Publish flip: same PUT, draft=false → 200-class update + fan-out.
	rel2, created, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v2", ReleaseInput{Draft: boolptr(false)}, "")
	if err != nil || created {
		t.Fatalf("flip: %+v %v", rel2, err)
	}
	if rel2.Draft || rel2.PublishedAt == nil {
		t.Fatalf("flipped shape: %+v", rel2)
	}
	if notified != 1 {
		t.Fatalf("flip fan-out: %d", notified)
	}
	got, _, err := x.svc.LatestRelease(ctx(), "o", "r", writer(), false)
	if err != nil || got.Tag != "v2" {
		t.Fatalf("latest after flip: %+v %v", got, err)
	}
}

func TestPutReleaseIfMatch(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v3"] = strings.Repeat("f", 40)
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v3", ReleaseInput{}, ""); err != nil {
		t.Fatal(err)
	}
	_, ver, _ := x.svc.GetRelease(ctx(), "o", "r", writer(), "v3")
	// Stale token → 409.
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v3",
		ReleaseInput{Name: strptr("x")}, "stale-token"); !isErr(err, ErrConflict) {
		t.Fatalf("stale If-Match: %v", err)
	}
	// Fresh token → update.
	rel, created, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v3",
		ReleaseInput{Name: strptr("Renamed")}, ver)
	if err != nil || created || rel.Name != "Renamed" {
		t.Fatalf("fresh If-Match: %+v %v", rel, err)
	}
}

func TestLatestMonotonicAndSelfHeal(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	for tag, sha := range map[string]string{"v1": strings.Repeat("1", 40), "v2": strings.Repeat("2", 40)} {
		x.git.tags[tag] = sha
	}
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	x.svc.Now = func() (later time.Time) { return x.now.Add(time.Hour) }
	mustPut(t, x, writer(), "v2", ReleaseInput{})
	// Concurrent older publish converges on the newest (skip, not retry).
	x.svc.updateLatestPointer(ctx(), "o", "r", "v1", x.now.Format(dateTimeFmt))
	got, _, err := x.svc.LatestRelease(ctx(), "o", "r", writer(), false)
	if err != nil || got.Tag != "v2" {
		t.Fatalf("monotonic: %+v %v", got, err)
	}
	// Dangling pointer self-heals on read (delete the pointer target).
	if err := x.svc.Store.Delete(ctx(), ReleaseKey("o", "r", "v2"), ""); err != nil {
		t.Fatal(err)
	}
	// Pointer still names v2 → read falls back to the bounded scan (v1).
	got2, _, err := x.svc.LatestRelease(ctx(), "o", "r", writer(), false)
	if err != nil || got2.Tag != "v1" {
		t.Fatalf("self-heal: %+v %v", got2, err)
	}
	// Repair wrote the pointer back at v1.
	raw, _, _ := x.svc.getJSON(ctx(), LatestKey("o", "r"))
	var ptr LatestPointer
	if err := json.Unmarshal(raw, &ptr); err != nil || ptr.Tag != "v1" {
		t.Fatalf("pointer repair: %+v %v", ptr, err)
	}
	// Prerelease filtering: v1 prerelease, include flag off → 404.
	mustPut(t, x, writer(), "v1", ReleaseInput{Prerelease: boolptr(true)})
	if _, _, err := x.svc.LatestRelease(ctx(), "o", "r", writer(), false); !isErr(err, ErrNotFound) {
		t.Fatalf("prerelease latest: %v", err)
	}
}

func TestDeleteReleaseRepairsLatest(t *testing.T) {
	x := newHarness(t)
	grantMaintain(x)
	x.git.tags["v1"] = strings.Repeat("1", 40)
	x.git.tags["v2"] = strings.Repeat("2", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	x.svc.Now = func() time.Time { return x.now.Add(time.Hour) }
	mustPut(t, x, writer(), "v2", ReleaseInput{})
	// Delete needs maintain: a write-role grant is refused.
	x.roles.grant("o", "r", "mallory", "write")
	if err := x.svc.DeleteRelease(ctx(), "o", "r", auth.Principal{Name: "mallory"}, "v2"); err == nil {
		t.Fatal("write-only delete allowed")
	}
	// Maintain delete repairs the pointer synchronously.
	if err := x.svc.DeleteRelease(ctx(), "o", "r", writer(), "v2"); err != nil {
		t.Fatal(err)
	}
	got, _, err := x.svc.LatestRelease(ctx(), "o", "r", writer(), false)
	if err != nil || got.Tag != "v1" {
		t.Fatalf("post-delete latest: %+v %v", got, err)
	}
	// Deleting the last release empties latest.
	if err := x.svc.DeleteRelease(ctx(), "o", "r", writer(), "v1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.svc.LatestRelease(ctx(), "o", "r", writer(), false); !isErr(err, ErrNotFound) {
		t.Fatalf("empty latest: %v", err)
	}
	if _, _, err := x.svc.GetRelease(ctx(), "o", "r", writer(), "v1"); !isErr(err, ErrNotFound) {
		t.Fatalf("deleted get: %v", err)
	}
}

func TestListReleasesPaging(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	shas := map[string]string{"v1": strings.Repeat("a", 40), "v2": strings.Repeat("b", 40), "v3": strings.Repeat("c", 40)}
	for i, tag := range []string{"v1", "v2", "v3"} {
		x.git.tags[tag] = shas[tag]
		x.svc.Now = func() time.Time { return x.now.Add(time.Duration(i) * time.Minute) }
		mustPut(t, x, writer(), tag, ReleaseInput{})
	}
	rels, more, err := x.svc.ListReleases(ctx(), "o", "r", writer(), 2, "")
	if err != nil || len(rels) != 2 || !more {
		t.Fatalf("page1: %+v %v %v", rels, more, err)
	}
	if rels[0].rel.Tag != "v3" || rels[1].rel.Tag != "v2" {
		t.Fatalf("order: %q %q", rels[0].rel.Tag, rels[1].rel.Tag)
	}
	after := rels[1].rel.CreatedAt + "|" + rels[1].rel.Tag
	rels2, more2, err := x.svc.ListReleases(ctx(), "o", "r", writer(), 2, after)
	if err != nil || len(rels2) != 1 || more2 || rels2[0].rel.Tag != "v1" {
		t.Fatalf("page2: %+v %v %v", rels2, more2, err)
	}
	if _, _, err := x.svc.ListReleases(ctx(), "o", "r", writer(), 2, "bogus"); !isErr(err, ErrInvalid) {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestNewerThan(t *testing.T) {
	if !newerThan("2026-09-04T13:00:00Z", "2026-09-04T12:00:00Z") {
		t.Fatal("newer not newer")
	}
	if newerThan("2026-09-04T12:00:00Z", "2026-09-04T13:00:00Z") {
		t.Fatal("older is newer")
	}
	if newerThan("2026-09-04T12:00:00Z", "2026-09-04T12:00:00Z") {
		t.Fatal("equal is newer")
	}
	if newerThan("bogus", "2026-09-04T12:00:00Z") {
		t.Fatal("corrupt is newer")
	}
}

func TestGitUnavailable(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.svc.Git = nil
	x.svc.Dirs = nil
	if _, err := x.svc.resolveTag(ctx(), "o", "r", "v1"); !isErr(err, ErrUnavailable) {
		t.Fatalf("nil git: %v", err)
	}
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v1", ReleaseInput{}, ""); !isErr(err, ErrUnavailable) {
		t.Fatalf("nil git put: %v", err)
	}
}
