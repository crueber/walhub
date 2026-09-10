package tags

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestValidateTagName(t *testing.T) {
	ok := []string{"v1", "v1.0.0", "release-candidate", "a/b", "a/b/c", "x.y", "X", "0"}
	for _, name := range ok {
		if _, err := validateTagName(name); err != nil {
			t.Errorf("validateTagName(%q) = %v, want nil", name, err)
		}
	}
	bad := map[string]string{
		"":                       "empty",
		"   ":                    "empty",
		"bad name":               "space",
		"a\tb":                   "control",
		"a~b":                    "~",
		"a^b":                    "^",
		"a:b":                    ":",
		"a?b":                    "?",
		"a*b":                    "*",
		"a[b":                    "[",
		`a\b`:                    "backslash",
		"a..b":                   "..",
		"a@{b":                   "@{",
		"a//b":                   "//",
		"/lead":                  "leading /",
		"trail/":                 "trailing /",
		"trail.":                 "trailing .",
		"x.lock":                 ".lock",
		strings.Repeat("x", 501): "length",
	}
	for name, why := range bad {
		if _, err := validateTagName(name); !isErr(err, errInvalid) {
			t.Errorf("validateTagName(%q) [%s] = %v, want ErrInvalid", name, why, err)
		}
	}
}

func TestCreateTagGates(t *testing.T) {
	t.Run("anonymous 401", func(t *testing.T) {
		x := newHarness(t)
		if _, err := x.svc.CreateTag(ctx(), "o", "r", auth.Anonymous(), CreateInput{Name: "v1", SHA: testSHA}); !isErr(err, errUnauthorized) {
			t.Fatalf("err = %v, want ErrUnauthorized", err)
		}
	})
	t.Run("read role 403", func(t *testing.T) {
		x := newHarness(t)
		x.roles.grant("o", "r", "jane", "read")
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA}); !isErr(err, errForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})
	t.Run("host admin passes", func(t *testing.T) {
		x := newHarness(t)
		tag, err := x.svc.CreateTag(ctx(), "o", "r", admin(), CreateInput{Name: "v1", SHA: testSHA})
		if err != nil {
			t.Fatalf("admin: %v", err)
		}
		if tag.Ref != "refs/tags/v1" || tag.SHA != testSHA || tag.Name != "v1" {
			t.Fatalf("tag = %+v", tag)
		}
	})
	t.Run("nil roles falls back to flags", func(t *testing.T) {
		x := newHarness(t)
		x.svc.Roles = nil
		if _, err := x.svc.CreateTag(ctx(), "o", "r", auth.Anonymous(), CreateInput{Name: "v1", SHA: testSHA}); !isErr(err, errUnauthorized) {
			t.Fatalf("anon/nil-roles err = %v", err)
		}
		if _, err := x.svc.CreateTag(ctx(), "o", "r", auth.Principal{Name: "bob"}, CreateInput{Name: "v1", SHA: testSHA}); !isErr(err, errForbidden) {
			t.Fatalf("authed/nil-roles err = %v", err)
		}
		if _, err := x.svc.CreateTag(ctx(), "o", "r", auth.Principal{Name: "w", Write: true}, CreateInput{Name: "v1", SHA: testSHA}); err != nil {
			t.Fatalf("write-flag/nil-roles: %v", err)
		}
	})
}

func TestCreateTagValidation(t *testing.T) {
	t.Run("whitespace message is lightweight", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		tag, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: "   \n "})
		if err != nil {
			t.Fatalf("whitespace message: %v", err)
		}
		if tag.SHA != testSHA {
			t.Fatalf("tag.SHA = %q, want lightweight commit %q", tag.SHA, testSHA)
		}
		if len(x.refs.created) != 1 || len(x.refs.annotated) != 0 {
			t.Fatalf("created=%d annotated=%d, want 1/0", len(x.refs.created), len(x.refs.annotated))
		}
	})
	t.Run("empty sha 400", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1"}); !isErr(err, errInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
	t.Run("bad name 400", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "bad name", SHA: testSHA}); !isErr(err, errInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
}

func TestCreateTagResolution(t *testing.T) {
	t.Run("unknown sha 404", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: strings.Repeat("f", 40)})
		if !isErr(err, errNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("unwired git 503", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.svc.Git = nil
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA}); !isErr(err, errUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("dirs outage 503", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.svc.Dirs = errDirs{}
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA}); !isErr(err, errUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("git backend failure propagates", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.git.err = fmt.Errorf("%w: git down", ErrUnavailable)
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA}); !isErr(err, errUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
}

func TestCreateAnnotatedHappyPath(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	tag, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v2", SHA: testSHA, Message: "release two"})
	if err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	if tag.Name != "v2" || tag.Ref != "refs/tags/v2" {
		t.Fatalf("tag = %+v", tag)
	}
	if tag.SHA != testTagOid {
		t.Fatalf("tag.SHA = %q, want tag object %q", tag.SHA, testTagOid)
	}
	// The lightweight funnel stays untouched by annotated creates.
	if len(x.refs.created) != 0 {
		t.Fatalf("lightweight creates = %d, want 0", len(x.refs.created))
	}
	if len(x.refs.annotated) != 1 {
		t.Fatalf("annotated creates = %d, want 1", len(x.refs.annotated))
	}
	a := x.refs.annotated[0]
	if a.repo != "o/r" || a.name != "v2" || a.tagOid != testTagOid || a.peeled != testSHA {
		t.Fatalf("annotated = %+v", a)
	}
	if string(a.pack) != "PACK-bytes" {
		t.Fatalf("pack = %q", a.pack)
	}
	if a.meta["principal"] != "jane" {
		t.Fatalf("meta = %v", a.meta)
	}
	// mktag got exactly one object; pack-objects got the tag oid back.
	if len(x.git.tagBodies) != 1 {
		t.Fatalf("mktag bodies = %d, want 1", len(x.git.tagBodies))
	}
	if len(x.git.packOids) != 1 || x.git.packOids[0] != testTagOid {
		t.Fatalf("pack oids = %v", x.git.packOids)
	}
	// The tag body is the canonical server-rendered shape: headers, blank,
	// message; tagger attributes the principal as server-minted.
	body := string(x.git.tagBodies[0])
	for _, want := range []string{
		"object " + testSHA + "\n",
		"type commit\n",
		"tag v2\n",
		"tagger jane <jane@walhub.local>",
		"+0000\n",
		"\nrelease two\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("mktag body %q lacks %q", body, want)
		}
	}
	if !strings.HasSuffix(body, "release two\n") || strings.HasSuffix(body, "\n\n") {
		t.Fatalf("message not normalized to one trailing newline: %q", body)
	}
}

func TestCreateAnnotatedValidation(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    tagError
		status  int
	}{
		{"nul rejected", "hi\x00there", errInvalid, 400},
		{"non-utf8 rejected", "hi\xffthere", errInvalid, 400},
		{"oversize rejected", strings.Repeat("x", MaxTagMessageLen+1), errInvalid, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t)
			grantWrite(x)
			_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: tc.message})
			if !isErr(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if statusFor(err) != tc.status {
				t.Fatalf("status = %d, want %d", statusFor(err), tc.status)
			}
			if len(x.refs.annotated) != 0 || len(x.refs.created) != 0 {
				t.Fatal("invalid message must not publish")
			}
			if len(x.git.tagBodies) != 0 {
				t.Fatal("invalid message must not reach mktag")
			}
		})
	}
	t.Run("max size accepted", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: strings.Repeat("y", MaxTagMessageLen)}); err != nil {
			t.Fatalf("max-size message: %v", err)
		}
	})
	t.Run("empty sha 400 before git", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", Message: "m"})
		if !isErr(err, errInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
		if len(x.git.tagBodies) != 0 {
			t.Fatal("empty sha must not reach mktag")
		}
	})
	t.Run("unknown sha 404 before mktag", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: strings.Repeat("f", 40), Message: "m"})
		if !isErr(err, errNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if len(x.git.tagBodies) != 0 {
			t.Fatal("unknown sha must not reach mktag")
		}
	})
	t.Run("policy deny 403 before mktag", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		seedPolicy(t, x, `{"version":1,"rules":[{"name":"freeze-tags","match":{"refs":["refs/tags/**"]},"effect":{"protect":{"restricts":["create"]}}}]}`)
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: "m"})
		if !isErr(err, errForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
		if len(x.git.tagBodies) != 0 {
			t.Fatal("policy-denied create must not reach mktag")
		}
	})
}

func TestCreateAnnotatedFailures(t *testing.T) {
	t.Run("mktag rejection 400 nothing published", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.git.errTag = fmt.Errorf("%w: mktag rejected tag: bad tagger", ErrInvalid)
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: "m"})
		if !isErr(err, errInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
		if statusFor(err) != 400 {
			t.Fatalf("status = %d, want 400", statusFor(err))
		}
		if len(x.refs.annotated) != 0 || len(x.git.packOids) != 0 {
			t.Fatal("mktag failure must publish nothing and pack nothing")
		}
	})
	t.Run("mktag backend 503 nothing published", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.git.errTag = fmt.Errorf("%w: git down", ErrUnavailable)
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: "m"})
		if !isErr(err, errUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
		if len(x.refs.annotated) != 0 {
			t.Fatal("backend failure must publish nothing")
		}
	})
	t.Run("pack failure 5xx nothing published", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.git.errPack = fmt.Errorf("%w: git pack-objects: boom", ErrUnavailable)
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: "m"})
		if !isErr(err, errUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
		if statusFor(err)/100 != 5 {
			t.Fatalf("status = %d, want 5xx", statusFor(err))
		}
		if len(x.refs.annotated) != 0 {
			t.Fatal("pack failure must publish nothing")
		}
	})
	t.Run("dirs flake between resolve and mktag 503", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.svc.Dirs = &flakyDirs{dir: t.TempDir() + "/repo.git"}
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: "m"})
		if !isErr(err, errUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
		if len(x.refs.annotated) != 0 || len(x.git.tagBodies) != 0 {
			t.Fatal("dirs failure must publish nothing and mint nothing")
		}
	})
	t.Run("unsanitizable principal 400", func(t *testing.T) {
		x := newHarness(t)
		x.roles.grant("o", "r", "<>", "write")
		_, err := x.svc.CreateTag(ctx(), "o", "r", auth.Principal{Name: "<>"}, CreateInput{Name: "v1", SHA: testSHA, Message: "m"})
		if !isErr(err, errInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
		if statusFor(err) != 400 {
			t.Fatalf("status = %d, want 400", statusFor(err))
		}
		if len(x.refs.annotated) != 0 || len(x.git.tagBodies) != 0 {
			t.Fatal("bad principal must publish nothing and mint nothing")
		}
	})
	t.Run("existing tag 409", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.refs.existing["v1"] = testSHA
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: "m"})
		if !isErr(err, errConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if statusFor(err) != 409 {
			t.Fatalf("status = %d, want 409", statusFor(err))
		}
	})
	t.Run("unwired publisher 503", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.svc.Refs = nil
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: "m"})
		if !isErr(err, errUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("publish backend error propagates", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.refs.err = fmt.Errorf("bucket down")
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: "m"})
		if err == nil || isErr(err, errConflict) {
			t.Fatalf("err = %v, want non-conflict failure", err)
		}
	})
}

func TestRenderTagger(t *testing.T) {
	tagger, err := renderTagger(writer(), nowFixed())
	if err != nil {
		t.Fatalf("renderTagger: %v", err)
	}
	want := fmt.Sprintf("jane <jane@walhub.local> %d +0000", nowFixed().Unix())
	if tagger != want {
		t.Fatalf("tagger = %q, want %q", tagger, want)
	}
	// Sanitization strips angle brackets and line breaks.
	tagger, err = renderTagger(auth.Principal{Name: "a<b>\nc\rd>"}, nowFixed())
	if err != nil {
		t.Fatalf("sanitized: %v", err)
	}
	if !strings.HasPrefix(tagger, "abcd <abcd@walhub.local> ") {
		t.Fatalf("sanitized tagger = %q", tagger)
	}
	// Empty-after-sanitize is a 400.
	if _, err := renderTagger(auth.Principal{Name: "<>\n\r  "}, nowFixed()); !isErr(err, errInvalid) {
		t.Fatalf("empty-after-sanitize err = %v, want ErrInvalid", err)
	}
	if _, err := renderTagger(auth.Principal{}, nowFixed()); !isErr(err, errInvalid) {
		t.Fatalf("empty name err = %v, want ErrInvalid", err)
	}
	// Negative offsets render with a minus sign and zero-padded fields.
	neg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("EST", -5*3600-1800))
	tagger, err = renderTagger(writer(), neg)
	if err != nil {
		t.Fatalf("negative tz: %v", err)
	}
	wantNeg := fmt.Sprintf("jane <jane@walhub.local> %d -0530", neg.Unix())
	if tagger != wantNeg {
		t.Fatalf("negative-tz tagger = %q, want %q", tagger, wantNeg)
	}
}

func TestNowDefaultsToWallClock(t *testing.T) {
	svc := New(store.NewMemory(), nil)
	if svc.now().IsZero() {
		t.Fatal("nil Now must fall back to wall clock")
	}
}

func TestRenderTagBody(t *testing.T) {
	got := string(renderTagBody("v1", testSHA, "jane <jane@walhub.local> 1 +0000", "hello\n"))
	want := "object " + testSHA + "\ntype commit\ntag v1\ntagger jane <jane@walhub.local> 1 +0000\n\nhello\n"
	if got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestNormalizeTagMessage(t *testing.T) {
	if got, err := normalizeTagMessage("hi"); err != nil || got != "hi\n" {
		t.Fatalf("got %q %v", got, err)
	}
	if got, err := normalizeTagMessage("hi\n\n\n"); err != nil || got != "hi\n" {
		t.Fatalf("trailing collapse: %q %v", got, err)
	}
	if got, err := normalizeTagMessage("a\nb\n"); err != nil || got != "a\nb\n" {
		t.Fatalf("interior kept: %q %v", got, err)
	}
}

func TestCreateTagPublish(t *testing.T) {
	t.Run("happy path records CAS create", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		tag, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA})
		if err != nil {
			t.Fatalf("CreateTag: %v", err)
		}
		if tag.Name != "v1" || tag.SHA != testSHA || tag.Ref != "refs/tags/v1" {
			t.Fatalf("tag = %+v", tag)
		}
		if len(x.refs.created) != 1 {
			t.Fatalf("creates = %d, want 1", len(x.refs.created))
		}
		c := x.refs.created[0]
		if c.repo != "o/r" || c.name != "v1" || c.sha != testSHA {
			t.Fatalf("create = %+v", c)
		}
		if c.meta["principal"] != "jane" {
			t.Fatalf("meta = %v", c.meta)
		}
	})
	t.Run("existing tag 409", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.refs.existing["v1"] = testSHA
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA})
		if !isErr(err, errConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if statusFor(err) != 409 {
			t.Fatalf("status = %d, want 409", statusFor(err))
		}
	})
	t.Run("unwired publisher 503", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.svc.Refs = nil
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA}); !isErr(err, errUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("publish backend error propagates", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.refs.err = fmt.Errorf("bucket down")
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA}); err == nil || isErr(err, errConflict) {
			t.Fatalf("err = %v, want non-conflict failure", err)
		}
	})
}

func seedPolicy(t *testing.T, x *harness, doc string) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), x.svc.Store, PolicyKey("o", "r"), []byte(doc),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateTagPolicy(t *testing.T) {
	denyTags := `{"version":1,"rules":[{"name":"freeze-tags","match":{"refs":["refs/tags/**"]},"effect":{"protect":{"restricts":["create"]}}}]}`
	t.Run("deny on tags create 403", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		seedPolicy(t, x, denyTags)
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA})
		if !isErr(err, errForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
		if !strings.Contains(err.Error(), "freeze-tags") {
			t.Fatalf("err = %v, want rule name", err)
		}
		if len(x.refs.created) != 0 {
			t.Fatal("policy-denied create must not publish")
		}
	})
	t.Run("unrelated rule allows", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		seedPolicy(t, x, `{"version":1,"rules":[{"name":"lock-main","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create","update","delete"]}}}]}`)
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA}); err != nil {
			t.Fatalf("allowed: %v", err)
		}
	})
	t.Run("corrupt policy fails closed 500", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		seedPolicy(t, x, `{"version":1,"rules":[`)
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA})
		if !isErr(err, errCorrupt) {
			t.Fatalf("err = %v, want ErrCorrupt", err)
		}
		if statusFor(err) != 500 {
			t.Fatalf("status = %d, want 500", statusFor(err))
		}
	})
	t.Run("store outage on policy load propagates", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		x.svc.Store = &errStore{ObjectStore: x.svc.Store, err: fmt.Errorf("bucket down")}
		if _, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA}); err == nil {
			t.Fatal("want store error")
		}
	})
}

func TestStatusFor(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 200},
		{ErrNotFound, 404},
		{ErrInvalid, 400},
		{ErrUnauthorized, 401},
		{ErrForbidden, 403},
		{ErrConflict, 409},
		{ErrUnavailable, 503},
		{ErrUnsupported, 422},
		{ErrCorrupt, 500},
		{fmt.Errorf("wrapped: %w", ErrNotFound), 404},
		{fmt.Errorf("boom"), 500},
		{&auth.AuthError{Kind: auth.ErrInvalid, Why: "bad"}, 500},
	}
	for _, c := range cases {
		if got := statusFor(c.err); got != c.want {
			t.Errorf("statusFor(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestRoleRank(t *testing.T) {
	for _, r := range []string{"read", "triage", "write", "maintain", "admin"} {
		if roleRank(r) <= 0 {
			t.Errorf("roleRank(%q) <= 0", r)
		}
	}
	if roleRank("bogus") != 0 {
		t.Error("bogus role must rank 0")
	}
	if roleRank("WRITE") != roleRank("write") {
		t.Error("rank must be case-insensitive")
	}
}

func TestRequireRoleMatrix(t *testing.T) {
	x := newHarness(t)
	anon := auth.Anonymous()
	if err := x.svc.requireRole(context.Background(), "o", "r", anon, "write"); !isErr(err, errUnauthorized) {
		t.Fatalf("anon = %v", err)
	}
	x.roles.grant("o", "r", "bob", "read")
	bob := auth.Principal{Name: "bob"}
	if err := x.svc.requireRole(context.Background(), "o", "r", bob, "write"); !isErr(err, errForbidden) {
		t.Fatalf("reader = %v", err)
	}
	if err := x.svc.requireRole(context.Background(), "o", "r", admin(), "write"); err != nil {
		t.Fatalf("admin = %v", err)
	}
}
