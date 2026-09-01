package store

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// testCreds are the AWS SigV4 documentation example credentials.
var testCreds = sigv4Creds{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}

var testScope = sigv4Scope{Date: "20130524", Region: "us-east-1", Service: "s3"}

// The canonical AWS "GET Object" example (S3 SigV4 docs): signature
// f0e8bdb8... over host;range;x-amz-content-sha256;x-amz-date.
func TestSigV4VectorGetObject(t *testing.T) {
	headers := []sigv4Header{
		{name: "host", value: "examplebucket.s3.amazonaws.com"},
		{name: "x-amz-date", value: "20130524T000000Z"},
		{name: "x-amz-content-sha256", value: emptyHash},
		{name: "range", value: "bytes=0-9"},
	}
	auth := signRequestHeaders("GET", "/test.txt", nil, headers, emptyHash, testCreds, testScope, "20130524T000000Z")
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if auth != want {
		t.Fatalf("Authorization =\n%s\nwant\n%s", auth, want)
	}
}

// The canonical AWS presigned-URL example: signature aeeed9bb... with
// UNSIGNED-PAYLOAD and X-Amz-SignedHeaders=host.
func TestSigV4VectorPresigned(t *testing.T) {
	q := presignQuery("GET", "/test.txt", "examplebucket.s3.amazonaws.com", url.Values{}, testCreds, testScope, "20130524T000000Z", 86400*time.Second)
	if got := q.Get("X-Amz-Algorithm"); got != "AWS4-HMAC-SHA256" {
		t.Fatalf("algorithm %q", got)
	}
	if got, want := q.Get("X-Amz-Credential"), "AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request"; got != want {
		t.Fatalf("credential %q, want %q", got, want)
	}
	if got := q.Get("X-Amz-Expires"); got != "86400" {
		t.Fatalf("expires %q", got)
	}
	if got := q.Get("X-Amz-SignedHeaders"); got != "host" {
		t.Fatalf("signed headers %q", got)
	}
	if got := q.Get("X-Amz-Signature"); got != "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404" {
		t.Fatalf("presigned signature %q", got)
	}
	if q.Get("X-Amz-Security-Token") != "" {
		t.Fatal("no session token expected")
	}
	// With a session token the token joins the signed headers.
	sc := testCreds
	sc.Session = "THETOKEN"
	q2 := presignQuery("GET", "/test.txt", "examplebucket.s3.amazonaws.com", url.Values{}, sc, testScope, "20130524T000000Z", time.Hour)
	if q2.Get("X-Amz-SignedHeaders") != "host;x-amz-security-token" || q2.Get("X-Amz-Security-Token") != "THETOKEN" {
		t.Fatalf("session presign: signed=%q token=%q", q2.Get("X-Amz-SignedHeaders"), q2.Get("X-Amz-Security-Token"))
	}
}

func TestEncodeRfc3986(t *testing.T) {
	cases := map[string]string{
		"a b":       "a%20b",
		"a+b":       "a%2Bb",
		"a~b-c_d.e": "a~b-c_d.e",
		"a/b":       "a%2Fb",
		"héllo":     "h%C3%A9llo",
		"":          "",
	}
	for in, want := range cases {
		if got := encodeRfc3986(in); got != want {
			t.Errorf("encodeRfc3986(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodePathS3(t *testing.T) {
	if got, want := encodePathS3("wal/ab cd/pack"), "wal/ab%20cd/pack"; got != want {
		t.Fatalf("encodePathS3 = %q, want %q", got, want)
	}
}

func TestCanonicalQuery(t *testing.T) {
	q := url.Values{}
	q.Set("uploads", "")
	q.Add("b", "2")
	q.Add("a", "1")
	q.Add("a", "0")
	q.Add("key with space", "v/x")
	got := canonicalQuery(q)
	want := "a=0&a=1&b=2&key%20with%20space=v%2Fx&uploads="
	if got != want {
		t.Fatalf("canonicalQuery = %q, want %q", got, want)
	}
}

func TestCanonicalHeadersAndTrim(t *testing.T) {
	headers := []sigv4Header{
		{name: "x-amz-date", value: "t"},
		{name: "host", value: "  example.com   "},
		{name: "content-type", value: "application/octet-stream"},
	}
	block, signed := canonicalHeaders(headers)
	if !strings.Contains(block, "host:example.com\n") {
		t.Fatalf("header block %q", block)
	}
	if signed != "content-type;host;x-amz-date" {
		t.Fatalf("signed list %q", signed)
	}
	if got := trimHeaderValue("a   b\t c"); got != "a b c" {
		t.Fatalf("trimHeaderValue = %q", got)
	}
}

func TestHashesAndScope(t *testing.T) {
	if got, want := sha256Hex([]byte("abc")), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Fatalf("sha256Hex = %s", got)
	}
	if len(hmacSha256([]byte("k"), []byte("d"))) != 32 {
		t.Fatal("hmac length")
	}
	if got, want := testScope.String(), "20130524/us-east-1/s3/aws4_request"; got != want {
		t.Fatalf("scope = %q", got)
	}
	// deriveSigningKey is deterministic; a changed scope must change it.
	if string(deriveSigningKey("s", testScope)) == string(deriveSigningKey("s", sigv4Scope{Date: "20130525", Region: "us-east-1", Service: "s3"})) {
		t.Fatal("signing key ignores scope date")
	}
}
