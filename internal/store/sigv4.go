// sigv4.go: hand-rolled AWS Signature Version 4 (03_store_backends.md §2.1).
// Only crypto/hmac + crypto/sha256; validated against the AWS SigV4 test
// suite vectors and the S3 signature examples in s3_test.go. No AWS SDK.
package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// sigv4Creds is one credential set (access key + secret + optional session).
type sigv4Creds struct {
	AccessKey string
	SecretKey string
	Session   string // x-amz-security-token; empty = none
}

// sigv4Scope is the credential scope suffix: <date>/<region>/<service>/aws4_request.
type sigv4Scope struct {
	Date    string // YYYYMMDD
	Region  string
	Service string // "s3"
}

func (s sigv4Scope) String() string {
	return s.Date + "/" + s.Region + "/" + s.Service + "/aws4_request"
}

// amzDateLayout is ISO8601 basic: YYYYMMDDTHHMMSSZ.
const amzDateLayout = "20060102T150405Z"
const amzDayLayout = "20060102"

// encodeRfc3986 percent-encodes every byte outside the unreserved set
// (A-Z a-z 0-9 - _ . ~). '/' is NOT special here — callers split paths.
func encodeRfc3986(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// encodePathS3 encodes an object-key path for a canonical request / URL:
// each segment encoded, '/' kept as the key's own separator (§2.1, and the
// Rust encode_path this stays byte-compatible with).
func encodePathS3(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = encodeRfc3986(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery builds the sorted, URI-encoded query string. Keys sort
// byte-wise; k=v both encoded; empty values render as "k="
// (post-vanilla-empty-query-value).
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		ek := encodeRfc3986(k)
		vals := q[k]
		vs := make([]string, 0, len(vals))
		for _, v := range vals {
			vs = append(vs, ek+"="+encodeRfc3986(v))
		}
		sort.Strings(vs)
		parts = append(parts, vs...)
	}
	return strings.Join(parts, "&")
}

// sigv4Header is one canonical header (name already lowercase).
type sigv4Header struct {
	name  string
	value string
}

// canonicalHeaders renders the canonical header block and the
// semicolon-joined signed-headers list. Names are lowercased+sorted; values
// are trimmed of inner/outer runs of spaces.
func canonicalHeaders(headers []sigv4Header) (string, string) {
	sort.Slice(headers, func(i, j int) bool { return headers[i].name < headers[j].name })
	var b strings.Builder
	names := make([]string, 0, len(headers))
	for _, h := range headers {
		names = append(names, h.name)
		b.WriteString(h.name)
		b.WriteByte(':')
		b.WriteString(trimHeaderValue(h.value))
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

// trimHeaderValue trims outer spaces and collapses inner space runs to one
// (the "get-header-value-trim" rule).
func trimHeaderValue(v string) string {
	fields := strings.Fields(v)
	return strings.Join(fields, " ")
}

// sha256Hex is hex(sha256(data)).
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hmacSha256 is HMAC-SHA256(key, data).
func hmacSha256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func buildCanonicalRequest(method, canonicalURI, query string, headers []sigv4Header, payloadHash string) string {
	creq, signed := canonicalHeaders(headers)
	// creq ends with '\n' after the last header; the canonical request needs
	// one more '\n' (the empty line AWS puts between the header block and
	// the signed-headers list) or the signature never verifies.
	return method + "\n" + canonicalURI + "\n" + query + "\n" + creq + "\n" + signed + "\n" + payloadHash
}

// deriveSigningKey computes k = HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date),
// region), service), "aws4_request").
func deriveSigningKey(secret string, scope sigv4Scope) []byte {
	kDate := hmacSha256([]byte("AWS4"+secret), []byte(scope.Date))
	kRegion := hmacSha256(kDate, []byte(scope.Region))
	kService := hmacSha256(kRegion, []byte(scope.Service))
	return hmacSha256(kService, []byte("aws4_request"))
}

// signature computes hex(HMAC(k, stringToSign)) for the string to sign
// "AWS4-HMAC-SHA256\n<amzdate>\n<scope>\n<hex(sha256(creq))>".
func signature(signingKey []byte, amzDate string, scope sigv4Scope, creq string) string {
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope.String() + "\n" + sha256Hex([]byte(creq))
	return hex.EncodeToString(hmacSha256(signingKey, []byte(sts)))
}

// unsignedPayload is the payload hash for presigned/streamed bodies.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// signRequestHeaders returns the Authorization header value for a header-
// signed request. headers must include host, x-amz-date,
// x-amz-content-sha256 (and any other signed header). Conditional headers
// (if-match / if-none-match) are NEVER signed (§2.1).
func signRequestHeaders(method, canonicalURI string, query url.Values, headers []sigv4Header, payloadHash string, creds sigv4Creds, scope sigv4Scope, amzDate string) string {
	creq := buildCanonicalRequest(method, canonicalURI, canonicalQuery(query), headers, payloadHash)
	_, signedHeaders := canonicalHeaders(headers)
	sig := signature(deriveSigningKey(creds.SecretKey, scope), amzDate, scope, creq)
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKey, scope.String(), signedHeaders, sig)
}

// presignQuery returns the query-string SigV4 variant (§2.1 presigned GETs):
// X-Amz-Algorithm, X-Amz-Credential, X-Amz-Date, X-Amz-Expires,
// X-Amz-SignedHeaders=host (plus x-amz-security-token when a session token
// is present), X-Amz-Signature. Payload hash is UNSIGNED-PAYLOAD. host is the
// request's Host value (bucket-prefixed for virtual-host style); it IS part
// of the canonical request and must not be empty.
func presignQuery(method, canonicalURI, host string, extra url.Values, creds sigv4Creds, scope sigv4Scope, amzDate string, ttl time.Duration) url.Values {
	q := url.Values{}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", creds.AccessKey+"/"+scope.String())
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprintf("%d", int(ttl.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	if creds.Session != "" {
		// host;x-amz-security-token
		q.Set("X-Amz-SignedHeaders", "host;x-amz-security-token")
		q.Set("X-Amz-Security-Token", creds.Session)
	}
	signed := []sigv4Header{{name: "host", value: host}}
	if creds.Session != "" {
		signed = append(signed, sigv4Header{name: "x-amz-security-token", value: creds.Session})
	}
	creq := buildCanonicalRequest(method, canonicalURI, canonicalQuery(q), signed, unsignedPayload)
	sig := signature(deriveSigningKey(creds.SecretKey, scope), amzDate, scope, creq)
	q.Set("X-Amz-Signature", sig)
	return q
}
