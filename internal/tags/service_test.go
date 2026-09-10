package tags

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
	t.Run("annotated 422", func(t *testing.T) {
		x := newHarness(t)
		grantWrite(x)
		_, err := x.svc.CreateTag(ctx(), "o", "r", writer(), CreateInput{Name: "v1", SHA: testSHA, Message: "release one"})
		if !isErr(err, errUnsupported) {
			t.Fatalf("err = %v, want ErrUnsupported", err)
		}
		if statusFor(err) != 422 {
			t.Fatalf("status = %d, want 422", statusFor(err))
		}
		if len(x.refs.created) != 0 {
			t.Fatal("annotated request must not publish")
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
