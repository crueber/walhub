// memory.go: the in-memory backend (03_store_backends.md §9). Tests and the
// simulation use it as the truth store: a BTreeMap-equivalent (map + sorted
// keys at iteration time), a global monotonic version counter unique across
// keys (mimics GCS generations), full interface incl. compose (concat under
// lock, CAS via Put) and range clamping. Test knobs: per-op latency,
// fake_object_urls (accel returns a GCS-like URL + bearer for edge tests),
// signing_fails (SignedGetURL errors like VPC-SC). The op counter feeds the
// §7 round-trip budget assertions.
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Memory is the in-memory ObjectStore. The zero value is not usable; NewMemory.
type Memory struct {
	mu      sync.Mutex
	objs    map[string]*memObj
	counter uint64 // global monotonic version counter, unique across keys

	// Test knobs (§9).
	Latency       time.Duration // artificial per-op latency
	FakeObjectURL bool          // AccelTarget returns a GCS-like URL + bearer
	SigningFails  bool          // SignedGetURL errors like VPC-SC

	// Ops counts every store operation (get/head/put/delete/list/compose);
	// budget assertions (§7) snapshot it around protocol steps.
	Ops atomic.Int64
}

type memObj struct {
	data        []byte
	version     Version
	contentType string
}

// NewMemory returns an empty memory backend.
func NewMemory() *Memory { return &Memory{objs: map[string]*memObj{}} }

func (m *Memory) Backend() string { return "memory" }

// OpCounter returns the live op counter (§7 budget assertions snapshot it).
func (m *Memory) OpCounter() *atomic.Int64 { return &m.Ops }

func (m *Memory) tick(ctx context.Context) error {
	m.Ops.Add(1)
	if m.Latency > 0 {
		select {
		case <-time.After(m.Latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// getLocked returns the object for key; caller holds m.mu.
func (m *Memory) getLocked(key string) *memObj { return m.objs[key] }

func (m *Memory) Get(ctx context.Context, key string, opts GetOptions) (GetResult, error) {
	if err := m.tick(ctx); err != nil {
		return nil, &StoreError{Kind: ErrKindRetryable, Key: key, Err: err}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.getLocked(key)
	if o == nil {
		return nil, NewNotFound(key)
	}
	if opts.IfNoneMatch != "" && opts.IfNoneMatch == o.version {
		return NotModified{Version: o.version}, nil
	}
	if opts.IfMatch != "" && opts.IfMatch != o.version {
		return nil, NewPrecondition(key, o.version)
	}
	size := int64(len(o.data))
	start, end := int64(0), size
	if opts.Range != nil {
		start, end = opts.Range[0], opts.Range[1]
		if start < 0 || end < start {
			return nil, NewInvalid(key, fmt.Errorf("bad range [%d,%d)", start, end))
		}
		if end > size {
			end = size
		}
		if start > size {
			// 416 analog: range entirely past EOF. start == size is the
			// empty-suffix read the contract suite allows.
			return nil, NewPrecondition(key, o.version)
		}
	}
	body := o.data[start:end]
	return Object{
		Meta: ObjectMeta{Key: key, Size: size, Version: o.version},
		Body: io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func (m *Memory) Head(ctx context.Context, key string) (*ObjectMeta, error) {
	if err := m.tick(ctx); err != nil {
		return nil, &StoreError{Kind: ErrKindRetryable, Key: key, Err: err}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.getLocked(key)
	if o == nil {
		return nil, nil
	}
	return &ObjectMeta{Key: key, Size: int64(len(o.data)), Version: o.version}, nil
}

// putLocked stores bytes under a fresh global-counter version. Caller holds m.mu.
func (m *Memory) putLocked(key string, data []byte, contentType string) (ObjectMeta, error) {
	m.counter++
	v := Version(strconv.FormatUint(m.counter, 10))
	cp := make([]byte, len(data))
	copy(cp, data)
	m.objs[key] = &memObj{data: cp, version: v, contentType: contentType}
	return ObjectMeta{Key: key, Size: int64(len(data)), Version: v}, nil
}

func (m *Memory) Put(ctx context.Context, key string, body PutBody, opts PutOptions) (ObjectMeta, error) {
	if key == "" {
		return ObjectMeta{}, NewInvalid(key, fmt.Errorf("empty key"))
	}
	if err := m.tick(ctx); err != nil {
		return ObjectMeta{}, &StoreError{Kind: ErrKindRetryable, Key: key, Err: err}
	}
	data, err := readPutBody(body)
	if err != nil {
		return ObjectMeta{}, NewOther(key, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.getLocked(key)
	switch opts.Mode {
	case PutCreate:
		if cur != nil {
			return ObjectMeta{}, NewPrecondition(key, cur.version)
		}
	case PutUpdate:
		if cur == nil {
			return ObjectMeta{}, NewPrecondition(key, "")
		}
		if opts.IfVersion == "" || opts.IfVersion != cur.version {
			return ObjectMeta{}, NewPrecondition(key, cur.version)
		}
	}
	return m.putLocked(key, data, opts.ContentType)
}

func (m *Memory) Delete(ctx context.Context, key string, ifVersion Version) error {
	if err := m.tick(ctx); err != nil {
		return &StoreError{Kind: ErrKindRetryable, Key: key, Err: err}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.getLocked(key)
	if o == nil {
		if ifVersion != "" {
			return NewNotFound(key)
		}
		return nil // unconditional delete of an absent key is Ok
	}
	if ifVersion != "" && ifVersion != o.version {
		return NewPrecondition(key, o.version)
	}
	delete(m.objs, key)
	return nil
}

func (m *Memory) List(ctx context.Context, prefix, startAfter string, fn func(ObjectMeta) error) error {
	if err := m.tick(ctx); err != nil {
		return &StoreError{Kind: ErrKindRetryable, Key: prefix, Err: err}
	}
	m.mu.Lock()
	keys := slices.Sorted(maps.Keys(m.objs))
	snap := make([]ObjectMeta, 0, len(keys))
	for _, k := range keys {
		if prefix != "" && !bytes.HasPrefix([]byte(k), []byte(prefix)) {
			continue
		}
		if startAfter != "" && k <= startAfter {
			continue
		}
		o := m.objs[k]
		snap = append(snap, ObjectMeta{Key: k, Size: int64(len(o.data)), Version: o.version})
	}
	m.mu.Unlock()
	for _, meta := range snap {
		if err := fn(meta); err != nil {
			return err
		}
	}
	return nil
}

func (m *Memory) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	if err := m.tick(ctx); err != nil {
		return &StoreError{Kind: ErrKindRetryable, Key: prefix, Err: err}
	}
	m.mu.Lock()
	keys := slices.Sorted(maps.Keys(m.objs))
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		if prefix != "" && !bytes.HasPrefix([]byte(k), []byte(prefix)) {
			continue
		}
		rest := k[len(prefix):]
		i := bytes.IndexByte([]byte(rest), '/')
		if i < 0 {
			continue
		}
		p := prefix + rest[:i+1]
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	m.mu.Unlock()
	for _, p := range out {
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

func (m *Memory) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error) {
	if err := m.tick(ctx); err != nil {
		return nil, &StoreError{Kind: ErrKindRetryable, Key: key, Err: err}
	}
	if m.SigningFails {
		// VPC-SC-style: the bucket refuses to mint direct URLs.
		return nil, NewOther(key, fmt.Errorf("signing unavailable (VPC Service Controls)"))
	}
	u := fmt.Sprintf("https://memory.walhub.invalid/%s?sig=mem&expires=%d", key, time.Now().Add(ttl).Unix())
	return &u, nil
}

func (m *Memory) AccelTarget(ctx context.Context, key string) (*AccelTarget, error) {
	if err := m.tick(ctx); err != nil {
		return nil, &StoreError{Kind: ErrKindRetryable, Key: key, Err: err}
	}
	if !m.FakeObjectURL {
		return nil, nil
	}
	return &AccelTarget{
		URL:           "https://storage.googleapis.com/fake-bucket/" + key,
		Authorization: "Bearer fake-edge-token",
	}, nil
}

func (m *Memory) SupportsCompose() bool { return true }
func (m *Memory) ComposeIsNative() bool { return true }

func (m *Memory) Compose(ctx context.Context, dst string, sources []string, opts PutOptions) (ObjectMeta, error) {
	if len(sources) < 1 || len(sources) > 32 {
		return ObjectMeta{}, NewInvalid(dst, fmt.Errorf("compose needs 1..=32 sources, got %d", len(sources)))
	}
	if err := m.tick(ctx); err != nil {
		return ObjectMeta{}, &StoreError{Kind: ErrKindRetryable, Key: dst, Err: err}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var buf bytes.Buffer
	for _, src := range sources {
		o := m.getLocked(src)
		if o == nil {
			return ObjectMeta{}, NewNotFound(src)
		}
		buf.Write(o.data)
	}
	cur := m.getLocked(dst)
	switch opts.Mode {
	case PutCreate:
		if cur != nil {
			return ObjectMeta{}, NewPrecondition(dst, cur.version)
		}
	case PutUpdate:
		if cur == nil {
			return ObjectMeta{}, NewPrecondition(dst, "")
		}
		if opts.IfVersion == "" || opts.IfVersion != cur.version {
			return ObjectMeta{}, NewPrecondition(dst, cur.version)
		}
	}
	return m.putLocked(dst, buf.Bytes(), opts.ContentType)
}

// ---- shared PutBody materialization (memory + filesystem backends) ----

// readPutBody fully materializes a PutBody: Bytes, a Stream of exactly
// StreamLen bytes, or the contents of the local file at File.
func readPutBody(body PutBody) ([]byte, error) {
	switch {
	case body.Bytes != nil:
		return body.Bytes, nil
	case body.Stream != nil:
		if body.StreamLen < 0 {
			return nil, fmt.Errorf("negative stream length")
		}
		data := make([]byte, body.StreamLen)
		if _, err := io.ReadFull(body.Stream, data); err != nil {
			return nil, fmt.Errorf("stream body: %w", err)
		}
		return data, nil
	case body.File != "":
		b, err := os.ReadFile(body.File)
		if err != nil {
			return nil, err
		}
		return b, nil
	default:
		return nil, fmt.Errorf("empty put body")
	}
}

// randHex returns n random bytes hex-encoded (temp-file suffixes).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the box is broken; fall back to time.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}
