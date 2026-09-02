// s3.go: the S3 backend (03_store_backends.md §2). Hand-rolled SigV4
// (sigv4.go) over plain net/http — no AWS SDK. Reads are presigned GETs with
// conditional headers attached unsigned; writes are signed single PUTs with
// conditionals; multipart only for Overwrite above the threshold; compose is
// emulated with UploadPartCopy (compose_is_native = false, §2.4); CAS delete
// is HEAD-then-delete (§2.5). Error mapping per §2.6; retry only Retryable,
// and never a non-replayable Stream PUT.
package store

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
)

// s3MinCopyPart is the minimum UploadPartCopy part size except for the last
// part of a source: 5 MiB (§2.4).
const s3MinCopyPart int64 = 5 << 20

// s3CopyChunk is the max bytes one UploadPartCopy moves: 1 GiB (§2.4).
const s3CopyChunk int64 = 1 << 30

// emptyHash is hex(sha256()) — the payload hash of bodyless requests.
const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// S3 is the S3/rustfs/MinIO backend.
type S3 struct {
	bucket         string
	endpoint       *url.URL // scheme://host
	region         string
	forcePathStyle bool
	creds          sigv4Creds

	maxRetries         int
	multipartThreshold int64
	multipartPartSize  int64

	control *http.Client // .pb/.json keys, non-ranged ops (§6.1)
	bulk    *http.Client // wal/, bundles/, lfs/, every Range, every stripe
	sem     *Weighted

	now func() time.Time // test hook
}

// NewS3 builds the S3 backend from the store config. Credentials come from
// env-var NAMES in config (store.s3.access_key_env / secret_key_env, defaults
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY); AWS_SESSION_TOKEN is honored
// when set (§2).
func NewS3(cfg *config.Store) (*S3, error) {
	region := cfg.S3.Region
	if region == "" {
		region = os.Getenv("AWS_REGION")
		if region == "" {
			region = "us-east-1"
		}
	}
	akEnv := cfg.S3.AccessKeyEnv
	if akEnv == "" {
		akEnv = "AWS_ACCESS_KEY_ID"
	}
	skEnv := cfg.S3.SecretKeyEnv
	if skEnv == "" {
		skEnv = "AWS_SECRET_ACCESS_KEY"
	}
	access, secret := os.Getenv(akEnv), os.Getenv(skEnv)
	if access == "" || secret == "" {
		return nil, NewInvalid("s3", fmt.Errorf("missing credentials: set %s / %s", akEnv, skEnv))
	}
	if cfg.Bucket == "" {
		return nil, NewInvalid("s3", fmt.Errorf("store.bucket required"))
	}
	endpoint := cfg.S3.Endpoint
	if endpoint == "" {
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	ep, err := url.Parse(endpoint)
	if err != nil || ep.Host == "" {
		return nil, NewInvalid("s3", fmt.Errorf("bad endpoint %q", endpoint))
	}
	retries := cfg.MaxRetries
	if retries <= 0 {
		retries = 8
	}
	threshold := int64(cfg.MultipartThreshold)
	if threshold <= 0 {
		threshold = 64 << 20
	}
	partSize := int64(cfg.MultipartPartSize)
	if partSize <= 0 {
		partSize = 32 << 20
	}
	return newS3Client(s3Config{
		Bucket: cfg.Bucket, Endpoint: ep, Region: region,
		ForcePathStyle: cfg.S3.ForcePathStyle,
		Creds:          sigv4Creds{AccessKey: access, SecretKey: secret, Session: os.Getenv("AWS_SESSION_TOKEN")},
		MaxRetries:     retries, MultipartThreshold: threshold, MultipartPartSize: partSize,
	}), nil
}

// s3Config is the injected constructor used by tests.
type s3Config struct {
	Bucket             string
	Endpoint           *url.URL
	Region             string
	ForcePathStyle     bool
	Creds              sigv4Creds
	MaxRetries         int
	MultipartThreshold int64
	MultipartPartSize  int64
}

func newS3Client(c s3Config) *S3 {
	// Two clients per backend instance (§6.1): control never queues behind
	// bulk bytes. On S3 one client is *acceptable*, but the classifier still
	// routes and the bulk pool is sized for stripe fan-out.
	control := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second},
	}
	bulk := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 64, MaxConnsPerHost: 64, IdleConnTimeout: 90 * time.Second},
	}
	return &S3{
		bucket: c.Bucket, endpoint: c.Endpoint, region: c.Region,
		forcePathStyle: c.ForcePathStyle, creds: c.Creds,
		maxRetries: c.MaxRetries, multipartThreshold: c.MultipartThreshold,
		multipartPartSize: c.MultipartPartSize,
		control:           control,
		bulk:              bulk,
		sem:               NewWeighted(64), // generous S3 default (§6.2)
		now:               time.Now,
	}
}

func (s *S3) Backend() string       { return "s3" }
func (s *S3) SupportsCompose() bool { return true }
func (s *S3) ComposeIsNative() bool { return false }

// clock returns the test-injectable time.
func (s *S3) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// urlFor builds the request URL for key: path-style
// https://<endpoint>/<bucket>/<encoded-key> or virtual-host style (§2).
func (s *S3) urlFor(key string, query url.Values) *url.URL {
	u := *s.endpoint
	var rawPath, plainPath string
	if s.forcePathStyle {
		rawPath = "/" + s.bucket + "/" + encodePathS3(key)
		plainPath = "/" + s.bucket + "/" + key
	} else {
		u.Host = s.bucket + "." + u.Host
		rawPath = "/" + encodePathS3(key)
		plainPath = "/" + key
	}
	// RawPath carries the SigV4-encoded form; Path the decoded one. Go only
	// uses RawPath when it is a valid encoding of Path — this keeps the wire
	// path byte-identical to canonicalURI instead of double-escaping.
	u.Path = plainPath
	u.RawPath = rawPath
	if query != nil {
		u.RawQuery = query.Encode()
	}
	return &u
}

// canonicalURI is the canonical request's URI (§2.1): absolute path, each
// segment URI-encoded, '/' not encoded — identical to the URL path.
func (s *S3) canonicalURI(key string) string {
	if s.forcePathStyle {
		return "/" + s.bucket + "/" + encodePathS3(key)
	}
	return "/" + encodePathS3(key)
}

// scope is the credential scope for time t.
func (s *S3) scope(t time.Time) sigv4Scope {
	return sigv4Scope{Date: t.Format(amzDayLayout), Region: s.region, Service: "s3"}
}

// stripETag removes the surrounding quotes from an ETag header value.
func stripETag(v string) string { return strings.Trim(v, `"`) }

// isETagShape reports whether v looks like an S3 ETag (32 hex chars,
// optionally "-N" multipart suffix). A PutUpdate with any other token shape
// silently skips the precondition (§2.3 quirk).
func isETagShape(v string) bool {
	if len(v) < 32 {
		return false
	}
	hexPart := v
	if i := strings.IndexByte(v, '-'); i >= 0 {
		hexPart = v[:i]
		if _, err := strconv.Atoi(v[i+1:]); err != nil {
			return false
		}
	}
	if len(hexPart) != 32 {
		return false
	}
	for i := range len(hexPart) {
		c := hexPart[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ---- HTTP plumbing ----

// clientFor routes by the §1 classifier: bulk bytes (bulk keys or ranged
// reads) take the bulk client; control-plane never does.
func (s *S3) clientFor(key string, ranged bool) *http.Client {
	if ranged || IsBulkKey(key) {
		return s.bulk
	}
	return s.control
}

// do executes one request, mapping transport errors to Retryable (§2.6).
func (s *S3) do(ctx context.Context, client *http.Client, req *http.Request, key string) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &StoreError{Kind: ErrKindOther, Key: key, Err: ctx.Err()}
		}
		return nil, NewRetryable(key, err)
	}
	return resp, nil
}

// mapStatus applies the §2.6 wire→StoreError table.
func (s *S3) mapStatus(key string, resp *http.Response) error {
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300, code == http.StatusNotModified:
		// 304 is a success for a conditional GET (§2.2): Get maps it to
		// NotModified; no other op accepts a 304.
		return nil
	case code == http.StatusNotFound:
		return NewNotFound(key)
	case code == http.StatusPreconditionFailed:
		return NewPrecondition(key, "")
	case code == http.StatusRequestedRangeNotSatisfiable:
		return NewPrecondition(key, "") // 416: range past EOF (§2.6)
	case code == http.StatusMethodNotAllowed:
		return NewInvalid(key, fmt.Errorf("method not allowed on this endpoint"))
	case code == http.StatusBadRequest:
		// Malformed signed request (signature error codes) → Other.
		return NewOther(key, fmt.Errorf("s3 400: %s", s.errorCode(resp)))
	case code == http.StatusTooManyRequests || code >= 500:
		return NewRetryable(key, fmt.Errorf("s3 %d: %s", code, s.errorCode(resp)))
	default:
		// 403 and friends: carry the S3 error <Code> so operators can tell a
		// signature problem from an ACL/credential one.
		return NewOther(key, fmt.Errorf("s3 %d: %s", code, s.errorCode(resp)))
	}
}

// errorCode extracts the S3 error <Code> from the response body (best effort).
func (s *S3) errorCode(resp *http.Response) string {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return ""
	}
	var xe struct {
		Code string `xml:"Code"`
	}
	if err := xml.Unmarshal(body, &xe); err != nil {
		return ""
	}
	return xe.Code
}

// retry runs op up to maxRetries with jittered exponential backoff, only for
// Retryable errors. replayable=false (a Stream PUT that failed mid-body)
// surfaces the error rather than re-reading (§2.6).
func (s *S3) retry(ctx context.Context, replayable bool, op func() error) error {
	attempt := 0
	err := op()
	for err != nil && IsRetryable(err) && replayable && attempt < s.maxRetries {
		d := jitterBackoff(attempt, 20*time.Millisecond, 2*time.Second)
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return err
		}
		attempt++
		err = op()
	}
	return err
}

// jitterBackoff = jittered exponential backoff: base*2^n capped at max, ±25%.
func jitterBackoff(n int, base, max time.Duration) time.Duration {
	d := base << n
	if d > max || d <= 0 {
		d = max
	}
	var b [1]byte
	_, _ = randRead(b[:])
	frac := 0.75 + float64(b[0])/255*0.5 // 0.75..1.25
	return time.Duration(float64(d) * frac)
}

// randRead is crypto/rand.Read (indirected for a narrower import surface).
func randRead(b []byte) (int, error) { return cryptoRandRead(b) }

// sign attaches SigV4 Authorization headers to req (§2.1: sign host,
// x-amz-date, x-amz-content-sha256, range when present; NEVER if-match /
// if-none-match or any conditional header).
func (s *S3) sign(req *http.Request, payloadHash string, t time.Time) {
	amzDate := t.Format(amzDateLayout)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if s.creds.Session != "" {
		req.Header.Set("X-Amz-Security-Token", s.creds.Session)
	}
	headers := signedHeadersFor(req.Header, req.URL.Host)
	auth := signRequestHeaders(req.Method, req.URL.Path, req.URL.Query(), headers, payloadHash, s.creds, s.scope(t), amzDate)
	req.Header.Set("Authorization", auth)
}

// signedHeadersFor lists the headers signed on an ordinary request: host,
// x-amz-date, x-amz-content-sha256, range when present, content-type,
// security token, and the copy-source pair on UploadPartCopy.
func signedHeadersFor(hdr http.Header, host string) []sigv4Header {
	out := []sigv4Header{
		{name: "host", value: host},
		{name: "x-amz-date", value: hdr.Get("X-Amz-Date")},
		{name: "x-amz-content-sha256", value: hdr.Get("X-Amz-Content-Sha256")},
	}
	if ct := hdr.Get("Content-Type"); ct != "" {
		out = append(out, sigv4Header{name: "content-type", value: ct})
	}
	if r := hdr.Get("Range"); r != "" {
		out = append(out, sigv4Header{name: "range", value: r})
	}
	if tok := hdr.Get("X-Amz-Security-Token"); tok != "" {
		out = append(out, sigv4Header{name: "x-amz-security-token", value: tok})
	}
	if cs := hdr.Get("X-Amz-Copy-Source"); cs != "" {
		out = append(out, sigv4Header{name: "x-amz-copy-source", value: cs})
	}
	if cr := hdr.Get("X-Amz-Copy-Source-Range"); cr != "" {
		out = append(out, sigv4Header{name: "x-amz-copy-source-range", value: cr})
	}
	return out
}

// signedReq builds a signed request (Authorization header style).
func (s *S3) signedReq(ctx context.Context, method, key string, query url.Values, payloadHash string, body io.Reader, length int64) (*http.Request, error) {
	t := s.clock()
	u := s.urlFor(key, query)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, NewOther(key, err)
	}
	if length >= 0 {
		req.ContentLength = length
	}
	if ct := req.Header.Get("Content-Type"); ct == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	s.sign(req, payloadHash, t)
	return req, nil
}

// presignGet mints a presigned GET URL for key with the given TTL (§2.2).
func (s *S3) presignGet(key string, ttl time.Duration) string {
	t := s.clock()
	u := s.urlFor(key, nil)
	q := presignQuery(http.MethodGet, s.canonicalURI(key), u.Host, url.Values{}, s.creds, s.scope(t), t.Format(amzDateLayout), ttl)
	u.RawQuery = q.Encode()
	return u.String()
}

// ---- reads (§2.2) ----

func (s *S3) Get(ctx context.Context, key string, opts GetOptions) (GetResult, error) {
	_, release, err := AcquireBulk(ctx, s.sem, key)
	if err != nil {
		return nil, &StoreError{Kind: ErrKindRetryable, Key: key, Err: err}
	}
	defer release()

	ranged := opts.Range != nil
	// The presigned URL covers auth only; conditional headers ride the
	// outgoing request UNSIGNED (§2.1) — the whole reason reads go through
	// an HTTP client instead of a bare redirect.
	presigned := s.presignGet(key, 60*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, presigned, nil)
	if err != nil {
		return nil, NewOther(key, err)
	}
	if opts.IfNoneMatch != "" {
		req.Header.Set("If-None-Match", `"`+string(opts.IfNoneMatch)+`"`) // re-add quotes on the wire
	}
	if opts.IfMatch != "" {
		req.Header.Set("If-Match", `"`+string(opts.IfMatch)+`"`)
	}
	if ranged {
		// Range is inclusive on the wire (§2.2).
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", opts.Range[0], opts.Range[1]-1))
	}

	client := s.clientFor(key, ranged)
	var resp *http.Response
	err = s.retry(ctx, true, func() error {
		r, err := s.do(ctx, client, req.Clone(ctx), key)
		if err != nil {
			return err
		}
		if e := s.mapStatus(key, r); e != nil {
			r.Body.Close()
			return e
		}
		resp = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusNotModified {
		ver := opts.IfNoneMatch
		resp.Body.Close()
		return NotModified{Version: ver}, nil
	}

	etag := stripETag(resp.Header.Get("ETag"))
	meta := ObjectMeta{Key: key, Version: Version(etag)}
	if ranged {
		// 206: Meta.Size = whole object size from
		// Content-Range: bytes s-e/total (§2.2).
		total, perr := parseContentRangeTotal(resp.Header.Get("Content-Range"))
		if perr != nil {
			resp.Body.Close()
			return nil, NewOther(key, fmt.Errorf("bad Content-Range %q", resp.Header.Get("Content-Range")))
		}
		meta.Size = total
	} else {
		meta.Size = resp.ContentLength
	}
	return Object{Meta: meta, Body: resp.Body}, nil
}

// parseContentRangeTotal extracts <total> from "bytes s-e/total" (or
// "bytes */total").
func parseContentRangeTotal(v string) (int64, error) {
	i := strings.LastIndexByte(v, '/')
	if i < 0 {
		return 0, fmt.Errorf("missing /")
	}
	return strconv.ParseInt(v[i+1:], 10, 64)
}

func (s *S3) Head(ctx context.Context, key string) (*ObjectMeta, error) {
	req, err := s.signedReq(ctx, http.MethodHead, key, nil, emptyHash, nil, 0)
	if err != nil {
		return nil, err
	}
	client := s.clientFor(key, false)
	var meta *ObjectMeta
	err = s.retry(ctx, true, func() error {
		resp, err := s.do(ctx, client, req.Clone(ctx), key)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			meta = nil
			return nil
		}
		if e := s.mapStatus(key, resp); e != nil {
			return e
		}
		meta = &ObjectMeta{
			Key:     key,
			Size:    resp.ContentLength,
			Version: Version(stripETag(resp.Header.Get("ETag"))),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return meta, nil
}

// ---- writes (§2.3) ----

// putBodyLen returns the byte length of a PutBody.
func putBodyLen(body PutBody) (int64, error) {
	switch {
	case body.Bytes != nil:
		return int64(len(body.Bytes)), nil
	case body.Stream != nil:
		if body.StreamLen < 0 {
			return 0, fmt.Errorf("negative stream length")
		}
		return body.StreamLen, nil
	case body.File != "":
		st, err := os.Stat(body.File)
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	default:
		return 0, fmt.Errorf("empty put body")
	}
}

func (s *S3) Put(ctx context.Context, key string, body PutBody, opts PutOptions) (ObjectMeta, error) {
	length, err := putBodyLen(body)
	if err != nil {
		return ObjectMeta{}, NewOther(key, err)
	}
	// Multipart ONLY when above the threshold AND mode == Overwrite: S3
	// multipart has no conditional headers, so it can never implement
	// Create/Update (§2.3).
	if opts.Mode == PutOverwrite && length > s.multipartThreshold {
		return s.putMultipart(ctx, key, body, length)
	}
	return s.putSingle(ctx, key, body, length, opts)
}

// putSingle is the single-shot signed PUT with conditionals (§2.3).
func (s *S3) putSingle(ctx context.Context, key string, body PutBody, length int64, opts PutOptions) (ObjectMeta, error) {
	// Payload hash: body bytes are hashed; streams/files are UNSIGNED-PAYLOAD
	// (TLS already protects the wire; rustfs/MinIO accept it).
	payloadHash := unsignedPayload
	var rd io.Reader
	if body.Bytes != nil {
		payloadHash = sha256Hex(body.Bytes)
		rd = bytes.NewReader(body.Bytes)
	} else if body.Stream != nil {
		rd = io.LimitReader(body.Stream, length)
	} else {
		f, ferr := os.Open(body.File)
		if ferr != nil {
			return ObjectMeta{}, NewOther(key, ferr)
		}
		defer f.Close()
		rd = io.LimitReader(f, length)
	}

	req, err := s.signedReq(ctx, http.MethodPut, key, nil, payloadHash, rd, length)
	if err != nil {
		return ObjectMeta{}, err
	}
	// Conditionals ride unsigned (§2.1/§2.3). PutCreate → If-None-Match: *;
	// PutUpdate → If-Match: "<version>". An unparseable Update version
	// silently skips the precondition (§2.3 quirk).
	switch opts.Mode {
	case PutCreate:
		req.Header.Set("If-None-Match", "*")
	case PutUpdate:
		if isETagShape(string(opts.IfVersion)) {
			req.Header.Set("If-Match", `"`+string(opts.IfVersion)+`"`)
		}
	}

	// Retry policy (§2.6): only Retryable, and never a Stream PUT that
	// failed mid-body (non-replayable).
	replayable := body.Bytes != nil || body.File != ""
	client := s.clientFor(key, false)
	var resp *http.Response
	err = s.retry(ctx, replayable, func() error {
		r, err := s.do(ctx, client, req.Clone(ctx), key)
		if err != nil {
			return err
		}
		if e := s.mapStatus(key, r); e != nil {
			r.Body.Close()
			// Verification goes on the failure path (§2.3): fill Current via
			// a follow-up HEAD only now that the write failed.
			if r.StatusCode == http.StatusPreconditionFailed {
				if cur, herr := s.Head(ctx, key); herr == nil && cur != nil {
					return NewPrecondition(key, cur.Version)
				}
			}
			return e
		}
		resp = r
		return nil
	})
	if err != nil {
		return ObjectMeta{}, err
	}
	defer resp.Body.Close()
	return ObjectMeta{Key: key, Size: length, Version: Version(stripETag(resp.Header.Get("ETag")))}, nil
}

// ---- multipart (§2.3) ----

type xmlInitiateResult struct {
	UploadId string `xml:"UploadId"`
}

type xmlCompleteResult struct {
	ETag string `xml:"ETag"`
}

type completedPart struct {
	PartNumber int
	ETag       string
}

// createMultipartUpload mints an upload id.
func (s *S3) createMultipartUpload(ctx context.Context, key string) (string, error) {
	req, err := s.signedReq(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, emptyHash, nil, 0)
	if err != nil {
		return "", err
	}
	resp, err := s.do(ctx, s.clientFor(key, false), req, key)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if e := s.mapStatus(key, resp); e != nil {
		return "", e
	}
	var xr xmlInitiateResult
	if err := xml.NewDecoder(resp.Body).Decode(&xr); err != nil {
		return "", NewOther(key, fmt.Errorf("bad CreateMultipartUpload response: %w", err))
	}
	if xr.UploadId == "" {
		return "", NewOther(key, fmt.Errorf("CreateMultipartUpload: empty upload id"))
	}
	return xr.UploadId, nil
}

// uploadPartData uploads one part's bytes, returning its ETag.
func (s *S3) uploadPartData(ctx context.Context, key, uploadID string, partNumber int, data []byte) (string, error) {
	req, err := s.signedReq(ctx, http.MethodPut, key,
		url.Values{"partNumber": {strconv.Itoa(partNumber)}, "uploadId": {uploadID}},
		sha256Hex(data), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	resp, err := s.do(ctx, s.bulk, req, key)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if e := s.mapStatus(key, resp); e != nil {
		return "", e
	}
	etag := stripETag(resp.Header.Get("ETag"))
	if etag == "" {
		return "", NewOther(key, fmt.Errorf("part %d: missing ETag", partNumber))
	}
	return etag, nil
}

// completeMultipartUpload finalizes the upload with the ordered part list.
func (s *S3) completeMultipartUpload(ctx context.Context, key, uploadID string, parts []completedPart) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		fmt.Fprintf(&buf, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", p.PartNumber, p.ETag)
	}
	buf.WriteString("</CompleteMultipartUpload>")
	body := bytes.NewReader(buf.Bytes())
	req, err := s.signedReq(ctx, http.MethodPost, key,
		url.Values{"uploadId": {uploadID}},
		sha256Hex(buf.Bytes()), body, int64(buf.Len()))
	if err != nil {
		return "", err
	}
	resp, err := s.do(ctx, s.clientFor(key, false), req, key)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if e := s.mapStatus(key, resp); e != nil {
		return "", e
	}
	var cr struct {
		ETag string `xml:"ETag"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", NewOther(key, fmt.Errorf("bad CompleteMultipartUpload response: %w", err))
	}
	return stripETag(cr.ETag), nil
}

// abortMultipartUpload cancels an upload (idempotent on S3).
func (s *S3) abortMultipartUpload(ctx context.Context, key, uploadID string) {
	req, err := s.signedReq(ctx, http.MethodDelete, key,
		url.Values{"uploadId": {uploadID}}, emptyHash, nil, 0)
	if err != nil {
		return
	}
	if resp, err := s.do(ctx, s.clientFor(key, false), req, key); err == nil {
		resp.Body.Close()
	}
}

// putMultipart runs the multipart state machine (§2.3): one writer owns it;
// only it may Complete/Abort (no double-abort). Parts upload concurrently
// from replayable bodies (Bytes/File); a Stream body uploads sequentially
// (buffered per part) so a failed part is retryable.
func (s *S3) putMultipart(ctx context.Context, key string, body PutBody, length int64) (ObjectMeta, error) {
	uploadID, err := s.createMultipartUpload(ctx, key)
	if err != nil {
		return ObjectMeta{}, err
	}
	parts := []completedPart{}
	abort := func() { s.abortMultipartUpload(ctx, key, uploadID) }

	fail := func(err error) (ObjectMeta, error) {
		abort()
		return ObjectMeta{}, err
	}

	switch {
	case body.Bytes != nil || body.File != "":
		// Bounded errgroup over part slices (§2 Concurrency: never 1024
		// goroutines); the owning goroutine owns cancellation via gctx.
		var src io.ReaderAt
		if body.Bytes != nil {
			src = bytes.NewReader(body.Bytes)
		} else {
			f, ferr := os.Open(body.File)
			if ferr != nil {
				return fail(NewOther(key, ferr))
			}
			defer f.Close()
			src = f
		}
		n := (length + s.multipartPartSize - 1) / s.multipartPartSize
		if n > 10000 {
			n = 10000 // S3 hard cap
		}
		g, gctx := WithContext(ctx)
		g.SetLimit(8)
		etags := make([]string, n)
		for i := range n {
			i := i
			start := int64(i) * s.multipartPartSize
			end := min(start+s.multipartPartSize, length)
			g.Go(func() error {
				buf := make([]byte, end-start)
				if _, err := src.ReadAt(buf, start); err != nil && err != io.EOF {
					return NewOther(key, err)
				}
				etag, err := s.uploadPartData(gctx, key, uploadID, int(i+1), buf)
				if err != nil {
					return err
				}
				etags[i] = etag
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return fail(err)
		}
		for i, et := range etags {
			parts = append(parts, completedPart{PartNumber: i + 1, ETag: et})
		}

	default: // Stream: sequential buffered parts (retryable per part).
		rd := body.Stream
		partNumber := 1
		for remaining := length; remaining > 0; partNumber++ {
			thisPart := s.multipartPartSize
			if thisPart > remaining {
				thisPart = remaining
			}
			buf := make([]byte, thisPart)
			if _, err := io.ReadFull(rd, buf); err != nil {
				return fail(NewRetryable(key, fmt.Errorf("stream part read: %w", err)))
			}
			etag, err := s.uploadPartData(ctx, key, uploadID, partNumber, buf)
			if err != nil {
				return fail(err)
			}
			parts = append(parts, completedPart{PartNumber: partNumber, ETag: etag})
			remaining -= thisPart
		}
	}

	etag, err := s.completeMultipartUpload(ctx, key, uploadID, parts)
	if err != nil {
		return fail(err)
	}
	return ObjectMeta{Key: key, Size: length, Version: Version(etag)}, nil
}

// ---- compose via UploadPartCopy (§2.4, compose_is_native = false) ----

func (s *S3) Compose(ctx context.Context, dst string, sources []string, opts PutOptions) (ObjectMeta, error) {
	if len(sources) < 1 || len(sources) > 32 {
		return ObjectMeta{}, NewInvalid(dst, fmt.Errorf("compose needs 1..=32 sources, got %d", len(sources)))
	}

	// Step 1: non-atomic pre-check for the destination CAS (accepted race;
	// all mutation of the same key is lease-guarded by protocol).
	switch opts.Mode {
	case PutCreate, PutUpdate:
		head, err := s.Head(ctx, dst)
		if err != nil {
			return ObjectMeta{}, err
		}
		if opts.Mode == PutCreate && head != nil {
			return ObjectMeta{}, NewPrecondition(dst, head.Version)
		}
		if opts.Mode == PutUpdate && (head == nil || head.Version != opts.IfVersion) {
			cur := Version("")
			if head != nil {
				cur = head.Version
			}
			return ObjectMeta{}, NewPrecondition(dst, cur)
		}
	}

	// Step 2: multipart upload on dest.
	uploadID, err := s.createMultipartUpload(ctx, dst)
	if err != nil {
		return ObjectMeta{}, err
	}
	abort := func() { s.abortMultipartUpload(ctx, dst, uploadID) }
	var plainParts []string // ranged-read fallback parts (real objects to clean)
	fail := func(err error) (ObjectMeta, error) {
		abort()
		for _, k := range plainParts {
			_ = s.Delete(ctx, k, "")
		}
		return ObjectMeta{}, err
	}

	// Step 3: copy each source's bytes as parts, in order. Sources ≥ 5 MiB
	// are sliced into 1 GiB copy chunks; sources (or final tails) < 5 MiB
	// are ranged-read and re-uploaded as ordinary PUT parts (§2.4).
	var parts []completedPart
	partNumber := 1
	var total int64
	for _, src := range sources {
		head, err := s.Head(ctx, src)
		if err != nil {
			return fail(err)
		}
		if head == nil {
			return fail(NewNotFound(src))
		}
		size := head.Size
		total += size
		for start := int64(0); start < size; {
			chunk := min(s3CopyChunk, size-start)
			if chunk >= s3MinCopyPart {
				etag, err := s.uploadPartCopy(ctx, dst, uploadID, partNumber, src, start, start+chunk-1)
				if err != nil {
					return fail(err)
				}
				parts = append(parts, completedPart{PartNumber: partNumber, ETag: etag})
				start += chunk
			} else {
				// Tail < 5 MiB: ranged GET + ordinary PUT part.
				data, err := s.readRange(ctx, src, start, size)
				if err != nil {
					return fail(err)
				}
				etag, err := s.uploadPartData(ctx, dst, uploadID, partNumber, data)
				if err != nil {
					return fail(err)
				}
				parts = append(parts, completedPart{PartNumber: partNumber, ETag: etag})
				start += chunk
			}
			partNumber++
		}
	}

	// Step 4: Complete; sources are left in place.
	etag, err := s.completeMultipartUpload(ctx, dst, uploadID, parts)
	if err != nil {
		return fail(err)
	}
	return ObjectMeta{Key: dst, Size: total, Version: Version(etag)}, nil
}

// uploadPartCopy performs one server-side copy part (inclusive byte range).
func (s *S3) uploadPartCopy(ctx context.Context, dst, uploadID string, partNumber int, srcKey string, start, end int64) (string, error) {
	req, err := s.signedReq(ctx, http.MethodPut, dst,
		url.Values{"partNumber": {strconv.Itoa(partNumber)}, "uploadId": {uploadID}},
		emptyHash, nil, 0)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Amz-Copy-Source", s.bucket+"/"+encodePathS3(srcKey))
	req.Header.Set("X-Amz-Copy-Source-Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := s.do(ctx, s.bulk, req, dst)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if e := s.mapStatus(dst, resp); e != nil {
		return "", e
	}
	var cr struct {
		ETag string `xml:"ETag"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", NewOther(dst, fmt.Errorf("bad CopyPart response: %w", err))
	}
	return stripETag(cr.ETag), nil
}

// readRange reads [start,end) of src (used by the < 5 MiB copy fallback).
func (s *S3) readRange(ctx context.Context, key string, start, end int64) ([]byte, error) {
	res, err := s.Get(ctx, key, GetOptions{Range: &[2]int64{start, end}})
	if err != nil {
		return nil, err
	}
	o, ok := res.(Object)
	if !ok {
		return nil, NewOther(key, fmt.Errorf("unexpected GetResult %T", res))
	}
	defer o.Body.Close()
	data, err := io.ReadAll(o.Body)
	if err != nil {
		return nil, NewRetryable(key, err)
	}
	return data, nil
}

// ---- delete: HEAD-then-delete emulation (§2.5) ----

func (s *S3) Delete(ctx context.Context, key string, ifVersion Version) error {
	if ifVersion != "" {
		// S3 has no conditional delete: HEAD first, then unconditional
		// DELETE. The check-then-act race is accepted (§2.5) — all mutation
		// of the same key is lease-guarded by protocol.
		head, err := s.Head(ctx, key)
		if err != nil {
			return err
		}
		if head == nil {
			return NewNotFound(key)
		}
		if head.Version != ifVersion {
			return NewPrecondition(key, head.Version)
		}
	}
	req, err := s.signedReq(ctx, http.MethodDelete, key, nil, emptyHash, nil, 0)
	if err != nil {
		return err
	}
	return s.retry(ctx, true, func() error {
		resp, err := s.do(ctx, s.clientFor(key, false), req.Clone(ctx), key)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		// S3 DELETE of an absent key returns 204 → unconditional delete of
		// an absent key is Ok; a raced-away CAS target likewise.
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		return s.mapStatus(key, resp)
	})
}

// ---- listing ----

type xmlListResult struct {
	IsTruncated           bool              `xml:"IsTruncated"`
	NextContinuationToken string            `xml:"NextContinuationToken"`
	Contents              []xmlListContents `xml:"Contents"`
	CommonPrefixes        []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

type xmlListContents struct {
	Key  string `xml:"Key"`
	Size int64  `xml:"Size"`
	ETag string `xml:"ETag"`
}

// listPage fetches one ListObjectsV2 page.
func (s *S3) listPage(ctx context.Context, prefix, startAfter, delimiter, token string) (*xmlListResult, error) {
	q := url.Values{
		"list-type": {"2"},
		"prefix":    {prefix},
	}
	if startAfter != "" {
		q.Set("start-after", startAfter)
	}
	if delimiter != "" {
		q.Set("delimiter", delimiter)
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	// Bucket-level request: canonical URI is "/" (no object key).
	t := s.clock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.urlFor("", q).String(), nil)
	if err != nil {
		return nil, NewOther(prefix, err)
	}
	s.sign(req, emptyHash, t)
	resp, err := s.do(ctx, s.clientFor(prefix, false), req, prefix)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if e := s.mapStatus(prefix, resp); e != nil {
		return nil, e
	}
	var lr xmlListResult
	if err := xml.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, NewOther(prefix, fmt.Errorf("bad ListObjectsV2 response: %w", err))
	}
	return &lr, nil
}

func (s *S3) List(ctx context.Context, prefix, startAfter string, fn func(ObjectMeta) error) error {
	token := ""
	for {
		lr, err := s.listPage(ctx, prefix, startAfter, "", token)
		if err != nil {
			return err
		}
		for _, c := range lr.Contents {
			key := c.Key
			if strings.HasSuffix(key, ".lock") || strings.Contains(key, ".tmp-") {
				continue // reserved namespace (filesystem sidecars never exist here)
			}
			if startAfter != "" && key <= startAfter {
				continue // start-after is inclusive on some gateways
			}
			if err := fn(ObjectMeta{
				Key:     key,
				Size:    c.Size,
				Version: Version(stripETag(c.ETag)),
			}); err != nil {
				return err
			}
		}
		if !lr.IsTruncated || lr.NextContinuationToken == "" {
			return nil
		}
		token = lr.NextContinuationToken
	}
}

func (s *S3) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	token := ""
	for {
		lr, err := s.listPage(ctx, prefix, "", "/", token)
		if err != nil {
			return err
		}
		for _, cp := range lr.CommonPrefixes {
			if err := fn(cp.Prefix); err != nil {
				return err
			}
		}
		if !lr.IsTruncated || lr.NextContinuationToken == "" {
			return nil
		}
		token = lr.NextContinuationToken
	}
}

// ---- URLs (§2.2) ----

// SignedGetURL is the presigned GET URL itself (TTL as configured).
func (s *S3) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error) {
	u := s.presignGet(key, ttl)
	return &u, nil
}

// AccelTarget is a presigned GET with TTL 1 h and NO authorization header —
// Range is not a signed header, so an edge may slice the object freely (§2.2).
func (s *S3) AccelTarget(ctx context.Context, key string) (*AccelTarget, error) {
	u := s.presignGet(key, time.Hour)
	return &AccelTarget{URL: u}, nil
}

// cryptoRandRead is crypto/rand.Read.
func cryptoRandRead(b []byte) (int, error) { return crand.Read(b) }
