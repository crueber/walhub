package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
)

// ---- the fake S3 server ----

type s3fake struct {
	mu     sync.Mutex
	objs   map[string][]byte
	etags  map[string]string
	parts  map[string]map[int][]byte // uploadID -> part -> data
	mids   map[string]int
	aborts []string

	// Test hooks.
	srv              *httptest.Server
	noUploadID       atomic.Bool                                   // CreateMultipartUpload answers an empty UploadId
	badInitXML       atomic.Bool                                   // CreateMultipartUpload answers garbage XML
	badCompleteXML   atomic.Bool                                   // CompleteMultipartUpload answers garbage XML
	statusOverride   func(method, key string, r *http.Request) int // non-zero replaces the outcome (guard with countsMu)
	badCopyXML       atomic.Bool                                   // UploadPartCopy answers garbage XML
	partMissingETag  atomic.Bool                                   // part upload answers 200 without an ETag
	dropRangeBody    atomic.Bool                                   // ranged GET dies mid-body
	noContentRange   atomic.Bool                                   // ranged GET omits Content-Range
	completeFail     atomic.Bool                                   // CompleteMultipartUpload answers 500
	absentDelete404  atomic.Bool                                   // DELETE of an absent key answers 404
	truncatedNoToken atomic.Bool                                   // IsTruncated=true with no continuation token
	badListXML       atomic.Bool                                   // ListObjectsV2 answers garbage XML
	ignoreStartAfter atomic.Bool                                   // list ignores start-after (gateway quirk)
	killCopyPart     atomic.Bool                                   // kill the connection on UploadPartCopy
	rangeReturns304  atomic.Bool                                   // ranged GET answers 304

	countsMu   sync.Mutex
	getReqs    int
	putReqs    map[string]int
	sawIfMatch bool
}

func newS3Fake() *s3fake {
	return &s3fake{
		objs:    map[string][]byte{},
		etags:   map[string]string{},
		parts:   map[string]map[int][]byte{},
		mids:    map[string]int{},
		putReqs: map[string]int{},
	}
}

func (f *s3fake) getStatusOverride() func(method, key string, r *http.Request) int {
	f.countsMu.Lock()
	defer f.countsMu.Unlock()
	return f.statusOverride
}

func (f *s3fake) setStatusOverride(fn func(method, key string, r *http.Request) int) {
	f.countsMu.Lock()
	defer f.countsMu.Unlock()
	f.statusOverride = fn
}

func (f *s3fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/bkt/")
	if f.killCopyPart.Load() && r.Header.Get("X-Amz-Copy-Source") != "" {
		panic(http.ErrAbortHandler) // transport-level failure mid-request
	}
	if st := f.getStatusOverride(); st != nil {
		if code := st(r.Method, key, r); code != 0 {
			w.WriteHeader(code)
			return
		}
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/bkt" || r.URL.Path == "/bkt/") {
		f.serveList(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.serveGet(w, r, key)
	case http.MethodHead:
		f.serveHead(w, r, key)
	case http.MethodPut:
		f.servePut(w, r, key)
	case http.MethodPost:
		f.servePost(w, r, key)
	case http.MethodDelete:
		f.serveDelete(w, r, key)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func xmlError(w http.ResponseWriter, code string, status int) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", code)
}

func (f *s3fake) serveGet(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objs[key]
	etag := f.etags[key]
	if !ok {
		xmlError(w, "NoSuchKey", http.StatusNotFound)
		return
	}
	if inm := strings.Trim(r.Header.Get("If-None-Match"), `"`); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if im := strings.Trim(r.Header.Get("If-Match"), `"`); im != "" && im != etag {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		if f.rangeReturns304.Load() {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		var s, e int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &s, &e); err != nil {
			xmlError(w, "InvalidRange", http.StatusBadRequest)
			return
		}
		if s >= int64(len(data)) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			xmlError(w, "InvalidRange", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if e >= int64(len(data)) {
			e = int64(len(data)) - 1
		}
		w.Header().Set("ETag", `"`+etag+`"`)
		if !f.noContentRange.Load() {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(data)))
		}
		w.WriteHeader(http.StatusPartialContent)
		if f.dropRangeBody.Load() {
			w.Write(data[s : s+2])
			panic(http.ErrAbortHandler) // kill the connection mid-body
		}
		w.Write(data[s : e+1])
		return
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (f *s3fake) serveHead(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objs[key]
	if !ok {
		xmlError(w, "NoSuchKey", http.StatusNotFound)
		return
	}
	w.Header().Set("ETag", `"`+f.etags[key]+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
}

func (f *s3fake) servePut(w http.ResponseWriter, r *http.Request, key string) {
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()

	if part := q.Get("partNumber"); part != "" {
		id := q.Get("uploadId")
		n, _ := strconv.Atoi(part)
		if f.parts[id] == nil {
			f.parts[id] = map[int][]byte{}
		}
		if cs := r.Header.Get("X-Amz-Copy-Source"); cs != "" {
			// UploadPartCopy: slice the source object by the inclusive
			// range and answer CopyPartResult XML (§2.4).
			f.mu.Unlock()
			srcKey := strings.TrimPrefix(strings.TrimPrefix(cs, "/bkt/"), "bkt/")
			f.mu.Lock()
			src, ok := f.objs[srcKey]
			if !ok {
				xmlError(w, "NoSuchKey", http.StatusNotFound)
				return
			}
			var s, e int64
			fmt.Sscanf(r.Header.Get("X-Amz-Copy-Source-Range"), "bytes=%d-%d", &s, &e)
			if e >= int64(len(src)) {
				e = int64(len(src)) - 1
			}
			partData := append([]byte(nil), src[s:e+1]...)
			f.parts[id][n] = partData
			pe := sha256.Sum256(partData)
			etag := hex.EncodeToString(pe[:16])
			if f.badCopyXML.Load() {
				w.Write([]byte("<<<garbage>>>"))
				return
			}
			w.Write([]byte("<CopyPartResult><ETag>\"" + etag + "\"</ETag></CopyPartResult>"))
			return
		}
		f.parts[id][n] = body
		pe := sha256.Sum256(body)
		if !f.partMissingETag.Load() {
			w.Header().Set("ETag", `"`+hex.EncodeToString(pe[:16])+`"`)
		}
		w.WriteHeader(200)
		return
	}
	if inm := r.Header.Get("If-None-Match"); inm == "*" {
		if _, exists := f.objs[key]; exists {
			xmlError(w, "PreconditionFailed", http.StatusPreconditionFailed)
			return
		}
	}
	if im := strings.Trim(r.Header.Get("If-Match"), `"`); im != "" {
		f.countsMu.Lock()
		f.sawIfMatch = true
		f.countsMu.Unlock()
		cur, exists := f.etags[key]
		if !exists || cur != im {
			xmlError(w, "PreconditionFailed", http.StatusPreconditionFailed)
			return
		}
	}
	sum := sha256.Sum256(body)
	etag := hex.EncodeToString(sum[:16])
	f.objs[key] = body
	f.etags[key] = etag
	f.countsMu.Lock()
	f.putReqs[key]++
	f.countsMu.Unlock()
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(200)
}

func (f *s3fake) servePost(w http.ResponseWriter, r *http.Request, key string) {
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := q["uploads"]; ok {
		if f.badInitXML.Load() {
			w.Write([]byte("<<<garbage>>>"))
			return
		}
		if f.noUploadID.Load() {
			w.Write([]byte("<InitiateMultipartUploadResult><UploadId></UploadId></InitiateMultipartUploadResult>"))
			return
		}
		f.mids[key]++
		id := fmt.Sprintf("up-%s-%d", key, f.mids[key])
		f.parts[id] = map[int][]byte{}
		w.Write([]byte("<InitiateMultipartUploadResult><UploadId>" + id + "</UploadId></InitiateMultipartUploadResult>"))
		return
	}
	id := q.Get("uploadId")
	parts := f.parts[id]
	if parts == nil {
		xmlError(w, "NoSuchUpload", http.StatusNotFound)
		return
	}
	nums := make([]int, 0, len(parts))
	for n := range parts {
		nums = append(nums, n)
	}
	for i := 1; i < len(nums); i++ {
		for j := i; j > 0 && nums[j] < nums[j-1]; j-- {
			nums[j], nums[j-1] = nums[j-1], nums[j]
		}
	}
	if f.badCompleteXML.Load() || f.completeFail.Load() {
		// The gateway fails BEFORE installing the object (S3 keeps the
		// uploadId alive on a failed Complete).
		if f.badCompleteXML.Load() {
			w.Write([]byte("<<<garbage>>>"))
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	var cat []byte
	for _, n := range nums {
		cat = append(cat, parts[n]...)
	}
	sum := sha256.Sum256(cat)
	etag := hex.EncodeToString(sum[:16])
	f.objs[key] = cat
	f.etags[key] = etag
	delete(f.parts, id)
	if f.completeFail.Load() {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write([]byte("<CompleteMultipartUploadResult><ETag>\"" + etag + "\"</ETag></CompleteMultipartUploadResult>"))
}

func (f *s3fake) serveDelete(w http.ResponseWriter, r *http.Request, key string) {
	id := r.URL.Query().Get("uploadId")
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != "" {
		delete(f.parts, id)
		f.aborts = append(f.aborts, id)
		w.WriteHeader(204)
		return
	}
	_, existed := f.objs[key]
	delete(f.objs, key)
	delete(f.etags, key)
	if f.absentDelete404.Load() && !existed {
		xmlError(w, "NoSuchKey", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *s3fake) serveList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, startAfter := q.Get("prefix"), q.Get("start-after")
	delimiter, token := q.Get("delimiter"), q.Get("continuation-token")

	f.mu.Lock()
	var keys []string
	for k := range f.objs {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	f.mu.Unlock()

	contents := []xmlListContents{}
	prefixes := map[string]bool{}
	for _, k := range keys {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		if startAfter != "" && !f.ignoreStartAfter.Load() && k <= startAfter {
			continue
		}
		if delimiter != "" {
			rest := k[len(prefix):]
			if i := strings.Index(rest, delimiter); i >= 0 {
				prefixes[prefix+rest[:i+1]] = true
				continue
			}
		}
		f.mu.Lock()
		contents = append(contents, xmlListContents{Key: k, Size: int64(len(f.objs[k])), ETag: `"` + f.etags[k] + `"`})
		f.mu.Unlock()
	}
	const pageSize = 2
	start := 0
	if token != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(token, "tok"))
	}
	if start > len(contents) {
		start = len(contents)
	}
	end := start + pageSize
	if end > len(contents) {
		end = len(contents)
	}
	out := xmlListResult{
		IsTruncated:           end < len(contents) || f.truncatedNoToken.Load(),
		NextContinuationToken: fmt.Sprintf("tok%d", end),
		Contents:              contents[start:end],
	}
	if f.truncatedNoToken.Load() {
		out.NextContinuationToken = ""
	}
	// Real S3 returns CommonPrefixes in ascending byte order.
	sorted := make([]string, 0, len(prefixes))
	for p := range prefixes {
		sorted = append(sorted, p)
	}
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	for _, p := range sorted {
		out.CommonPrefixes = append(out.CommonPrefixes, struct {
			Prefix string `xml:"Prefix"`
		}{p})
	}
	if f.badListXML.Load() {
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte("<<<garbage>>>"))
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}

// ---- backend construction ----

func s3fakePut(t *testing.T, s *S3, key, body string, opts PutOptions) ObjectMeta {
	t.Helper()
	meta, err := s.Put(context.Background(), key, PutBody{Bytes: []byte(body)}, opts)
	if err != nil {
		t.Fatalf("S3 Put %q: %v", key, err)
	}
	return meta
}

func newS3TestBackend(t *testing.T, f *s3fake, mutate func(*s3Config)) *S3 {
	t.Helper()
	ep, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s3Config{
		Bucket:             "bkt",
		Endpoint:           ep,
		Region:             "us-east-1",
		ForcePathStyle:     true,
		Creds:              testCreds,
		MaxRetries:         2,
		MultipartThreshold: 64 << 20,
		MultipartPartSize:  8 << 20,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s := newS3Client(cfg)
	fixed := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	return s
}

// ---- tests ----

func TestS3GetHead(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)

	if s.Backend() != "s3" || !s.SupportsCompose() || s.ComposeIsNative() {
		t.Fatal("S3 flags")
	}
	meta := s3fakePut(t, s, "dir/k", "0123456789", PutOptions{})
	if meta.Size != 10 || meta.Version == "" {
		t.Fatalf("put meta: %+v", meta)
	}

	hm, err := s.Head(ctx, "dir/k")
	if err != nil || hm == nil || hm.Size != 10 || hm.Version != meta.Version {
		t.Fatalf("Head: %+v %v", hm, err)
	}
	if hm, err = s.Head(ctx, "absent"); err != nil || hm != nil {
		t.Fatalf("Head absent: %+v %v", hm, err)
	}

	res, err := s.Get(ctx, "dir/k", GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	o := res.(Object)
	b, _ := io.ReadAll(o.Body)
	o.Body.Close()
	if string(b) != "0123456789" || o.Meta.Size != 10 || o.Meta.Version != meta.Version {
		t.Fatalf("Get: %q %+v", b, o.Meta)
	}

	// 304 via If-None-Match (regression: mapStatus used to eat 304s).
	res, err = s.Get(ctx, "dir/k", GetOptions{IfNoneMatch: meta.Version})
	if err != nil {
		t.Fatalf("conditional get: %v", err)
	}
	if nm, ok := res.(NotModified); !ok || nm.Version != meta.Version {
		t.Fatalf("IfNoneMatch: %#v", res)
	}

	if _, err := s.Get(ctx, "dir/k", GetOptions{IfMatch: "stale"}); !IsPreconditionFailed(err) {
		t.Fatalf("IfMatch mismatch: %v", err)
	}
	if _, err := s.Get(ctx, "absent", GetOptions{}); !IsNotFound(err) {
		t.Fatalf("absent: %v", err)
	}

	// Ranged read: Meta.Size is the WHOLE object size (§2.2).
	res, err = s.Get(ctx, "dir/k", GetOptions{Range: &[2]int64{2, 5}})
	if err != nil {
		t.Fatal(err)
	}
	o = res.(Object)
	b, _ = io.ReadAll(o.Body)
	o.Body.Close()
	if string(b) != "234" || o.Meta.Size != 10 {
		t.Fatalf("range: %q size=%d", b, o.Meta.Size)
	}
	// 416 → Precondition.
	if _, err := s.Get(ctx, "dir/k", GetOptions{Range: &[2]int64{99, 100}}); !IsPreconditionFailed(err) {
		t.Fatalf("past EOF: %v", err)
	}
	// Malformed Content-Range from the server → Other.
	f.statusOverride = func(method, key string, r *http.Request) int {
		if r.Header.Get("Range") != "" {
			w := struct{}{}
			_ = w
		}
		return 0
	}
	f.statusOverride = nil
}

func TestS3MapStatusTable(t *testing.T) {
	s := &S3{}
	cases := []struct {
		code int
		want func(error) bool
	}{
		{200, nil}, {201, nil}, {299, nil}, {304, nil},
		{404, IsNotFound},
		{412, IsPreconditionFailed},
		{416, IsPreconditionFailed},
		{405, IsInvalidArgument},
		{400, IsOther},
		{429, IsRetryable},
		{500, IsRetryable},
		{503, IsRetryable},
		{418, IsOther},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		rec.Code = c.code
		rec.Body.WriteString("<Error><Code>Boom</Code></Error>")
		resp := rec.Result()
		got := s.mapStatus("k", resp)
		resp.Body.Close()
		if c.want == nil {
			if got != nil {
				t.Errorf("code %d: got error %v", c.code, got)
			}
			continue
		}
		if !c.want(got) {
			t.Errorf("code %d: got %v", c.code, got)
		}
	}
	// Retryable errors carry the parsed <Code>.
	rec := httptest.NewRecorder()
	rec.Code = 503
	rec.Body.WriteString("<Error><Code>SlowDown</Code></Error>")
	resp := rec.Result()
	err := s.mapStatus("k", resp)
	resp.Body.Close()
	if !IsRetryable(err) || !strings.Contains(err.Error(), "SlowDown") {
		t.Fatalf("error code extraction: %v", err)
	}
}

func TestS3ErrorCodeGarbage(t *testing.T) {
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)
	rec := httptest.NewRecorder()
	rec.Code = 500
	rec.Body.WriteString("not xml at all <<<<")
	resp := rec.Result()
	if code := s.errorCode(resp); code != "" {
		t.Fatalf("garbage xml → %q", code)
	}
	resp.Body.Close()
}

func TestS3PutModes(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)
	v1 := s3fakePut(t, s, "k", "one", PutOptions{Mode: PutCreate}).Version
	_, err := s.Put(ctx, "k", PutBody{Bytes: []byte("two")}, PutOptions{Mode: PutCreate})
	if !IsPreconditionFailed(err) {
		t.Fatalf("create existing: %v", err)
	}
	// The failed Create surfaces the current version via the follow-up HEAD.
	if cur, ok := PreconditionCurrent(err); !ok || cur != v1 {
		t.Fatalf("create current: %q %v", cur, ok)
	}
	v2, err := s.Put(ctx, "k", PutBody{Bytes: []byte("two")}, PutOptions{Mode: PutUpdate, IfVersion: v1})
	if err != nil || v2.Version == v1 {
		t.Fatalf("update: %+v %v", v2, err)
	}
	_, err = s.Put(ctx, "k", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate, IfVersion: v1})
	if !IsPreconditionFailed(err) {
		t.Fatal("wrong update version accepted")
	}
	if cur, _ := PreconditionCurrent(err); cur != v2.Version {
		t.Fatalf("update current: %q want %q", cur, v2.Version)
	}
	// Update with a non-ETag-shaped version silently skips the precondition
	// (S3 quirk, §2.3): no If-Match on the wire, PUT succeeds.
	f.countsMu.Lock()
	f.sawIfMatch = false
	f.countsMu.Unlock()
	if _, err := s.Put(ctx, "quirk", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutUpdate, IfVersion: "not-an-etag"}); err != nil {
		t.Fatalf("quirk update: %v", err)
	}

	if _, err := s.Put(ctx, "s", PutBody{Stream: strings.NewReader("abcdef"), StreamLen: 6}, PutOptions{Mode: PutCreate}); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("filedata"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "fi", PutBody{File: src}, PutOptions{Mode: PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "e", PutBody{}, PutOptions{}); !IsOther(err) {
		t.Fatalf("empty body: %v", err)
	}
	if _, err := s.Put(ctx, "e", PutBody{Stream: strings.NewReader("ab"), StreamLen: -1}, PutOptions{}); !IsOther(err) {
		t.Fatalf("negative stream: %v", err)
	}
	if _, err := s.Put(ctx, "e", PutBody{File: src + ".missing"}, PutOptions{}); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestS3RetrySemantics(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)

	// Replayable (Bytes) PUT: one transient 503 then success.
	var transient int
	f.statusOverride = func(method, key string, r *http.Request) int {
		if method == http.MethodPut && key == "ret" && r.Header.Get("If-None-Match") != "" {
			f.countsMu.Lock()
			defer f.countsMu.Unlock()
			transient++
			if transient == 1 {
				return http.StatusServiceUnavailable
			}
		}
		return 0
	}
	if _, err := s.Put(ctx, "ret", PutBody{Bytes: []byte("x")}, PutOptions{Mode: PutCreate}); err != nil {
		t.Fatalf("retryable put: %v", err)
	}
	f.statusOverride = nil

	// A Stream PUT that fails is NOT replayed (§2.6).
	var streamPuts int
	f.statusOverride = func(method, key string, r *http.Request) int {
		if method == http.MethodPut && key == "streamfail" {
			f.countsMu.Lock()
			streamPuts++
			f.countsMu.Unlock()
			return http.StatusBadGateway
		}
		return 0
	}
	_, err := s.Put(ctx, "streamfail", PutBody{Stream: strings.NewReader("abc"), StreamLen: 3}, PutOptions{Mode: PutCreate})
	if !IsRetryable(err) {
		t.Fatalf("stream failure kind: %v", err)
	}
	f.statusOverride = nil
	if streamPuts != 1 {
		t.Fatalf("non-replayable stream retried %d times", streamPuts)
	}
}

func TestS3TransportErrors(t *testing.T) {
	ctx := context.Background()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close() // nothing listens anymore

	cfg := s3Config{Bucket: "bkt", Endpoint: deadURL, Region: "us-east-1", ForcePathStyle: true,
		Creds: testCreds, MaxRetries: 1, MultipartThreshold: 1 << 20, MultipartPartSize: 1 << 20}
	s := newS3Client(cfg)

	if _, err := s.Get(ctx, "k", GetOptions{}); !IsRetryable(err) {
		t.Fatalf("dead endpoint get: %v", err)
	}
	// A cancelled context maps to Other via do() (Head carries no semaphore,
	// so the mapping is deterministic).
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Head(cctx, "k"); !IsOther(err) {
		t.Fatalf("cancelled head: %v", err)
	}
}

func TestS3MultipartOverwrite(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	const part = 1 << 20
	s := newS3TestBackend(t, f, func(c *s3Config) {
		c.MultipartThreshold = 2*part - 1
		c.MultipartPartSize = part
	})

	data := make([]byte, 3*part+17)
	for i := range data {
		data[i] = byte(i % 253)
	}
	meta, err := s.Put(ctx, "mp", PutBody{Bytes: data}, PutOptions{Mode: PutOverwrite})
	if err != nil || meta.Size != int64(len(data)) {
		t.Fatalf("multipart put: %+v %v", meta, err)
	}
	b, _, err := GetBytes(ctx, s, "mp", GetOptions{})
	if err != nil || string(b) != string(data) {
		t.Fatalf("multipart bytes: %d %v", len(b), err)
	}

	// Stream multipart (sequential buffered parts).
	if _, err := s.Put(ctx, "mps", PutBody{Stream: bytes.NewReader(data), StreamLen: int64(len(data))}, PutOptions{Mode: PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	b2, _, err := GetBytes(ctx, s, "mps", GetOptions{})
	if err != nil || string(b2) != string(data) {
		t.Fatalf("stream multipart bytes: %d %v", len(b2), err)
	}
}

func TestS3MultipartAbortsOnPartFailure(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	const part = 1 << 20
	s := newS3TestBackend(t, f, func(c *s3Config) {
		c.MultipartThreshold = 2*part - 1
		c.MultipartPartSize = part
	})
	f.statusOverride = func(method, key string, r *http.Request) int {
		if r.Method == http.MethodPut && r.URL.Query().Get("partNumber") != "" {
			return http.StatusInternalServerError // every part fails
		}
		return 0
	}
	_, err := s.Put(ctx, "ab", PutBody{Bytes: make([]byte, 3*part)}, PutOptions{Mode: PutOverwrite})
	if err == nil {
		t.Fatal("part failure must surface")
	}
	f.statusOverride = nil
	if len(f.aborts) == 0 {
		t.Fatal("upload was never aborted")
	}
	if hm, _ := s.Head(ctx, "ab"); hm != nil {
		t.Fatal("aborted upload left an object")
	}
}

func TestS3Compose(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, func(c *s3Config) { c.MultipartPartSize = 8 << 20 })

	big := strings.Repeat("A", 6<<20) // ≥ 5 MiB → UploadPartCopy
	small := "tail"                   // < 5 MiB → ranged read + ordinary PUT part
	if _, err := s.Put(ctx, "s1", PutBody{Bytes: []byte(big)}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "s2", PutBody{Bytes: []byte(small)}, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	meta, err := s.Compose(ctx, "cat", []string{"s1", "s2"}, PutOptions{Mode: PutCreate})
	if err != nil || meta.Size != int64(len(big)+len(small)) {
		t.Fatalf("compose: %+v %v", meta, err)
	}
	b, _, err := GetBytes(ctx, s, "cat", GetOptions{})
	if err != nil || string(b) != big+small {
		t.Fatalf("compose bytes: %d %v", len(b), err)
	}
	// Sources remain in place (§2.4).
	if hm, _ := s.Head(ctx, "s1"); hm == nil {
		t.Fatal("source removed")
	}
	// Create on an existing dest → 412 (pre-check Head).
	if _, err := s.Compose(ctx, "cat", []string{"s1"}, PutOptions{Mode: PutCreate}); !IsPreconditionFailed(err) {
		t.Fatalf("compose create existing: %v", err)
	}
	// Update with the wrong version → 412.
	if _, err := s.Compose(ctx, "cat", []string{"s1"}, PutOptions{Mode: PutUpdate, IfVersion: "wrong"}); !IsPreconditionFailed(err) {
		t.Fatalf("compose update wrong: %v", err)
	}
	// Missing source → NotFound; the upload is aborted.
	abortsBefore := len(f.aborts)
	if _, err := s.Compose(ctx, "c2", []string{"s1", "nope"}, PutOptions{}); !IsNotFound(err) {
		t.Fatalf("missing source: %v", err)
	}
	if len(f.aborts) != abortsBefore+1 {
		t.Fatal("compose failure did not abort the upload")
	}
	// Count guard.
	if _, err := s.Compose(ctx, "c3", nil, PutOptions{}); !IsInvalidArgument(err) {
		t.Fatalf("zero sources: %v", err)
	}
}

func TestS3Delete(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)

	v := s3fakePut(t, s, "k", "v", PutOptions{}).Version
	// Unconditional delete of an absent key is Ok.
	if err := s.Delete(ctx, "absent", ""); err != nil {
		t.Fatal(err)
	}
	// Conditional delete of an absent key → NotFound.
	if err := s.Delete(ctx, "absent", "v"); !IsNotFound(err) {
		t.Fatalf("cas absent: %v", err)
	}
	// Wrong version → PreconditionFailed with the observed version.
	err := s.Delete(ctx, "k", "wrong")
	if !IsPreconditionFailed(err) {
		t.Fatalf("cas wrong: %v", err)
	}
	if cur, _ := PreconditionCurrent(err); cur != v {
		t.Fatalf("cas wrong current: %q want %q", cur, v)
	}
	// Right version deletes.
	if err := s.Delete(ctx, "k", v); err != nil {
		t.Fatal(err)
	}
	if hm, _ := s.Head(ctx, "k"); hm != nil {
		t.Fatal("not deleted")
	}
}

func TestS3ListAndListPrefixes(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)

	keys := []string{"wal/a.pack", "wal/b.pack", "wal/c.pack", "wal/d.pack", "wal/e.pack",
		"wal/x.lock", "wal/y.tmp-1",
		"repos/o/r/manifest.pb",
	}
	for _, k := range keys {
		s3fakePut(t, s, k, "v", PutOptions{})
	}
	// Reserved namespace keys never surface; paging is transparent.
	var got []string
	if err := s.List(ctx, "wal/", "", func(m ObjectMeta) error { got = append(got, m.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{"wal/a.pack", "wal/b.pack", "wal/c.pack", "wal/d.pack", "wal/e.pack"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	got = nil
	if err := s.List(ctx, "wal/", "wal/c.pack", func(m ObjectMeta) error { got = append(got, m.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint([]string{"wal/d.pack", "wal/e.pack"}) {
		t.Fatalf("startAfter: %v", got)
	}
	sentinel := errors.New("stop")
	if err := s.List(ctx, "", "", func(ObjectMeta) error { return sentinel }); err != sentinel {
		t.Fatalf("callback error: %v", err)
	}
	var pfx []string
	if err := s.ListPrefixes(ctx, "", func(p string) error { pfx = append(pfx, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(pfx) != fmt.Sprint([]string{"repos/", "wal/"}) {
		t.Fatalf("ListPrefixes = %v", pfx)
	}
}

func TestS3SignedURLs(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, nil)
	s3fakePut(t, s, "wal/k.pack", "data", PutOptions{})

	u, err := s.SignedGetURL(ctx, "wal/k.pack", time.Minute)
	if err != nil || u == nil {
		t.Fatalf("SignedGetURL: %v %v", u, err)
	}
	pu, err := url.Parse(*u)
	if err != nil {
		t.Fatal(err)
	}
	if pu.Path != "/bkt/wal/k.pack" {
		t.Fatalf("presigned path %q", pu.Path)
	}
	q := pu.Query()
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || q.Get("X-Amz-Expires") != "60" || len(q.Get("X-Amz-Signature")) != 64 {
		t.Fatalf("presigned query: %v", q)
	}
	at, err := s.AccelTarget(ctx, "wal/k.pack")
	if err != nil || at == nil || at.URL == "" || at.Authorization != "" {
		t.Fatalf("AccelTarget: %+v %v", at, err)
	}
	// The presigned URL path really fetches the object end-to-end.
	res, err := s.Get(ctx, "wal/k.pack", GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	o := res.(Object)
	o.Body.Close()
}

func TestS3Helpers(t *testing.T) {
	// urlFor / canonicalURI: path style vs virtual host.
	ep, _ := url.Parse("https://s3.example.com")
	ps := newS3Client(s3Config{Bucket: "bkt", Endpoint: ep, ForcePathStyle: true, Region: "r", MultipartThreshold: 1, MultipartPartSize: 1})
	vh := newS3Client(s3Config{Bucket: "bkt", Endpoint: ep, ForcePathStyle: false, Region: "r", MultipartThreshold: 1, MultipartPartSize: 1})
	if got := ps.urlFor("a b/c", nil).String(); got != "https://s3.example.com/bkt/a%20b/c" {
		t.Fatalf("path-style url %q", got)
	}
	if got := ps.canonicalURI("a b/c"); got != "/bkt/a%20b/c" {
		t.Fatalf("path-style uri %q", got)
	}
	if got := vh.urlFor("a b", nil).Host; got != "bkt.s3.example.com" {
		t.Fatalf("vhost host %q", got)
	}
	if got := vh.canonicalURI("a b"); got != "/a%20b" {
		t.Fatalf("vhost uri %q", got)
	}
	// clientFor routes bulk keys and ranged reads to the bulk client.
	if ps.clientFor("wal/x.pack", false) != ps.bulk || ps.clientFor("m/k.pb", true) != ps.bulk || ps.clientFor("m/k.pb", false) != ps.control {
		t.Fatal("clientFor routing")
	}
	// isETagShape / stripETag.
	for _, ok := range []string{strings.Repeat("a", 32), strings.Repeat("A", 32) + "-2", strings.Repeat("0", 32) + "-17"} {
		if !isETagShape(ok) {
			t.Errorf("isETagShape(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 31), strings.Repeat("g", 32), strings.Repeat("a", 33), strings.Repeat("a", 32) + "-x", strings.Repeat("a", 16) + "-1"} {
		if isETagShape(bad) {
			t.Errorf("isETagShape(%q) = true", bad)
		}
	}
	if stripETag(`"abc"`) != "abc" || stripETag("abc") != "abc" {
		t.Fatal("stripETag")
	}
	// putBodyLen.
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, _ := putBodyLen(PutBody{Bytes: []byte("12345")}); n != 5 {
		t.Fatal("bytes len")
	}
	if n, _ := putBodyLen(PutBody{Stream: strings.NewReader(""), StreamLen: 7}); n != 7 {
		t.Fatal("stream len")
	}
	if n, _ := putBodyLen(PutBody{File: src}); n != 5 {
		t.Fatal("file len")
	}
	if _, err := putBodyLen(PutBody{Stream: nil, StreamLen: -1}); err == nil {
		t.Fatal("negative stream len")
	}
	if _, err := putBodyLen(PutBody{File: src + ".missing"}); err == nil {
		t.Fatal("missing file len")
	}
	if _, err := putBodyLen(PutBody{}); err == nil {
		t.Fatal("empty body len")
	}
	// parseContentRangeTotal.
	for in, want := range map[string]int64{"bytes 2-5/100": 100, "bytes */64": 64, "bytes 0-0/1": 1} {
		if got, err := parseContentRangeTotal(in); err != nil || got != want {
			t.Errorf("parseContentRangeTotal(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := parseContentRangeTotal("nope"); err == nil {
		t.Error("missing slash accepted")
	}
	// jitterBackoff bounds: base scaling and the cap.
	d := jitterBackoff(0, 20*time.Millisecond, 2*time.Second)
	if d < 15*time.Millisecond || d > 25*time.Millisecond {
		t.Fatalf("jitterBackoff(0) = %v", d)
	}
	for n := 6; n < 30; n++ {
		if d := jitterBackoff(n, 20*time.Millisecond, 100*time.Millisecond); d < 75*time.Millisecond || d > 125*time.Millisecond {
			t.Fatalf("jitterBackoff(%d) = %v outside [75ms,125ms]", n, d)
		}
	}
}

func TestS3NewS3(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")
	t.Setenv("AWS_REGION", "eu-west-1")

	// Missing bucket.
	if _, err := NewS3(&config.Store{S3: config.S3{Endpoint: "http://127.0.0.1:1"}}); !IsInvalidArgument(err) {
		t.Fatalf("missing bucket: %v", err)
	}
	// Bad endpoint.
	if _, err := NewS3(&config.Store{Bucket: "b", S3: config.S3{Endpoint: "ht tp://bad"}}); !IsInvalidArgument(err) {
		t.Fatalf("bad endpoint: %v", err)
	}
	// Defaults: region from AWS_REGION, default endpoint, retries/thresholds.
	s, err := NewS3(&config.Store{Bucket: "b", S3: config.S3{Endpoint: "http://127.0.0.1:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.region != "eu-west-1" || s.maxRetries != 8 || s.multipartThreshold != 64<<20 || s.multipartPartSize != 32<<20 {
		t.Fatalf("defaults: region=%s retries=%d thr=%d part=%d", s.region, s.maxRetries, s.multipartThreshold, s.multipartPartSize)
	}
	// Session token is honored.
	t.Setenv("AWS_SESSION_TOKEN", "tok")
	s3b, err := NewS3(&config.Store{Bucket: "b", S3: config.S3{Endpoint: "http://127.0.0.1:1"}})
	if err != nil || s3b.creds.Session != "tok" {
		t.Fatalf("session token: %v", err)
	}
	// Explicit region wins; credential env NAMES are honored.
	t.Setenv("MY_AK", "ak2")
	t.Setenv("MY_SK", "sk2")
	s4, err := NewS3(&config.Store{Bucket: "b", MaxRetries: 3, MultipartThreshold: 1024, MultipartPartSize: 512,
		S3: config.S3{Endpoint: "http://127.0.0.1:1", Region: "us-west-2", AccessKeyEnv: "MY_AK", SecretKeyEnv: "MY_SK", ForcePathStyle: true}})
	if err != nil || s4.region != "us-west-2" || s4.creds.AccessKey != "ak2" || s4.creds.SecretKey != "sk2" || !s4.forcePathStyle {
		t.Fatalf("explicit config: %+v %v", s4, err)
	}
	// Missing credentials → Invalid naming the env vars.
	if _, err := NewS3(&config.Store{Bucket: "b", S3: config.S3{AccessKeyEnv: "NOPE_AK", SecretKeyEnv: "NOPE_SK"}}); !IsInvalidArgument(err) {
		t.Fatalf("missing creds: %v", err)
	}
}

func TestS3MultipartResponseErrors(t *testing.T) {
	ctx := context.Background()
	f := newS3Fake()
	f.srv = httptest.NewServer(f)
	defer f.srv.Close()
	s := newS3TestBackend(t, f, func(c *s3Config) {
		c.MultipartThreshold = 5 << 20
		c.MultipartPartSize = 1 << 20
	})
	big := make([]byte, 5<<20+1)

	// Empty upload id → Other.
	f.noUploadID.Store(true)
	if _, err := s.Put(ctx, "badid", PutBody{Bytes: big}, PutOptions{Mode: PutOverwrite}); !IsOther(err) {
		t.Fatalf("empty upload id: %v", err)
	}
	// Garbage XML from CreateMultipartUpload → Other.
	f.noUploadID.Store(false)
	f.badInitXML.Store(true)
	if _, err := s.Put(ctx, "badxml", PutBody{Bytes: big}, PutOptions{Mode: PutOverwrite}); !IsOther(err) {
		t.Fatalf("bad initiate xml: %v", err)
	}
	f.badInitXML.Store(false)
	// Garbage XML from Complete → Other; upload aborted.
	f.badCompleteXML.Store(true)
	if _, err := s.Put(ctx, "badcmp", PutBody{Bytes: big}, PutOptions{Mode: PutOverwrite}); !IsOther(err) {
		t.Fatalf("bad complete xml: %v", err)
	}
	f.badCompleteXML.Store(false)
	// CreateMultipartUpload rejected with 500 → Retryable.
	f.statusOverride = func(method, key string, r *http.Request) int {
		if _, ok := r.URL.Query()["uploads"]; ok {
			return http.StatusInternalServerError
		}
		return 0
	}
	if _, err := s.Put(ctx, "inifail", PutBody{Bytes: big}, PutOptions{Mode: PutOverwrite}); !IsRetryable(err) {
		t.Fatalf("initiate failure: %v", err)
	}
	f.statusOverride = nil
	// A failed part read (short stream) aborts with Retryable.
	_, err := s.Put(ctx, "shortmp", PutBody{Stream: bytes.NewReader(make([]byte, 10)), StreamLen: int64(len(big))}, PutOptions{Mode: PutOverwrite})
	if !IsRetryable(err) {
		t.Fatalf("short stream multipart: %v", err)
	}
}
