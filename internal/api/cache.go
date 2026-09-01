package api

import (
	"container/list"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- task surface (07_api.md §12) ------------------------------------------------

// TaskErr is a terminal SSE error packet payload.
type TaskErr struct {
	Status  int
	Message string
}

// TaskDone is the terminal outcome of a task stream.
type TaskDone struct {
	Record TaskRecord
	Value  string
	Err    *TaskErr
}

// TaskStream is one subscription to a task's narration: the record snapshot,
// the buffered replay (bars already deduped by label), the live channel
// (closed by the publisher when the task ends — the subscriber never closes
// it), and the terminal outcome. Other is set for a same-(repo,kind) task
// running on another host: not joinable (§12.2).
type TaskStream struct {
	Record  TaskRecord
	Replay  []Progress
	Updates <-chan Progress
	Done    <-chan TaskDone
	Other   *TaskRecord
}

// Tasks is the Seam-5 task table the handlers depend on (07_api.md §1):
// implemented by internal/wal / internal/maintain.
type Tasks interface {
	// Ops lists the available maintenance ops with their params.
	Ops() []OpSpec
	// List returns this instance's running + recent tasks for a repo
	// (records are instance-memory only).
	List(ctx context.Context, id git.RepoId) (running, recent []TaskRecord, err error)
	// Get returns one task record known on this instance.
	Get(ctx context.Context, id git.RepoId, taskID string) (TaskRecord, bool, error)
	// Begin starts an op, or joins a running same-(repo,kind) task
	// (Begin::AlreadyRunning): the joiner's stream replays then follows live.
	Begin(ctx context.Context, id git.RepoId, op string, params map[string]string) (TaskStream, error)
	// Attach subscribes to a running/recent task's stream.
	Attach(ctx context.Context, id git.RepoId, taskID string) (TaskStream, bool, error)
}

// --- weighted LRU ----------------------------------------------------------------

// lru is a hand-rolled weighted LRU (07_api.md §5 — the dependency policy
// forbids golang-lru/ristretto). Weights are bytes for the render cache and
// 1/entry for the ref→sha cache.
type lru struct {
	mu    sync.Mutex
	max   int64
	cur   int64
	ll    *list.List // front = newest
	items map[string]*list.Element
}

type lruEntry struct {
	key    string
	weight int64
	val    any
}

func newLRU(maxWeight int64) *lru {
	return &lru{max: maxWeight, ll: list.New(), items: map[string]*list.Element{}}
}

// Get returns the value and moves it to the front.
func (c *lru) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*lruEntry).val, true
}

// Put inserts (or refreshes) a weighted entry, evicting from the back while
// over budget.
func (c *lru) Put(key string, weight int64, val any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.cur -= el.Value.(*lruEntry).weight
		c.ll.Remove(el)
		delete(c.items, key)
	}
	if weight > c.max {
		return // never cacheable at this budget
	}
	c.items[key] = c.ll.PushFront(&lruEntry{key: key, weight: weight, val: val})
	c.cur += weight
	for c.cur > c.max {
		back := c.ll.Back()
		if back == nil {
			break
		}
		e := back.Value.(*lruEntry)
		c.ll.Remove(back)
		delete(c.items, e.key)
		c.cur -= e.weight
	}
}

// --- ref→sha cache (§5 #1) ---------------------------------------------------------

// refCache caches resolved ref→sha per (repo, refname), stamped with the
// manifest revision: an entry from an older revision invalidates lazily.
type refCache struct{ l *lru }

func newRefCache(maxEntries int) *refCache { return &refCache{l: newLRU(int64(maxEntries))} }

func refKey(repo, ref string) string { return repo + "\x00" + ref }

// Get returns the cached sha if it was resolved at the current revision.
func (c *refCache) Get(repo, ref string, rev uint64) (string, bool) {
	v, ok := c.l.Get(refKey(repo, ref))
	if !ok {
		return "", false
	}
	e := v.(*revStamped)
	if e.revision != rev {
		return "", false // newer revision → re-resolve
	}
	return e.sha, true
}

// Put stamps an entry with the revision it was resolved at.
func (c *refCache) Put(repo, ref string, rev uint64, sha string) {
	c.l.Put(refKey(repo, ref), 1, &revStamped{revision: rev, sha: sha})
}

type revStamped struct {
	revision uint64
	sha      string
}

// --- render cache (§5 #2 + §5.1) ----------------------------------------------------

// renderCall is one in-flight render (single-flight).
type renderCall struct {
	done chan struct{} // closed exactly once by the renderer
	body []byte
	etag string
	err  error
}

// renderCache is the rendered-immutable LRU keyed by canonical request key,
// revision-stamped, with per-key single-flight (§5.1 — the canonical sketch,
// verbatim semantics: never render under the lock, bounded join so a hung
// leader cannot wedge followers, the leader is the only remover of its own
// inflight entry, done closes exactly once).
type renderCache struct {
	mu       sync.Mutex
	l        *lru
	inflight map[string]*renderCall
}

type renderEntry struct {
	revision uint64
	body     []byte
	etag     string
}

func newRenderCache(budget int64) *renderCache {
	return &renderCache{l: newLRU(budget), inflight: map[string]*renderCall{}}
}

// Get answers from cache or renders once per key+revision. The render
// function runs lock-free. etag is the bare sha (quoted at header time).
func (c *renderCache) Get(key string, rev uint64, render func() ([]byte, string, error)) ([]byte, string, error) {
	c.mu.Lock()
	if v, ok := c.l.Get(key); ok {
		e := v.(*renderEntry)
		if e.revision == rev { // revision-stamped
			c.mu.Unlock()
			return e.body, e.etag, nil
		}
	}
	if call := c.inflight[key]; call != nil {
		c.mu.Unlock()
		select { // wait for the OTHER goroutine's render; bounded join:
		case <-call.done:
			return call.body, call.etag, call.err
		case <-time.After(30 * time.Second): // then render ourselves (fall through)
		}
	}
	call := &renderCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock() // NEVER render under the lock

	call.body, call.etag, call.err = render()
	c.mu.Lock()
	if call.err == nil {
		c.l.Put(key, int64(len(call.body)), &renderEntry{revision: rev, body: call.body, etag: call.etag})
	}
	delete(c.inflight, key) // only the leader deletes its own entry
	c.mu.Unlock()
	close(call.done)
	return call.body, call.etag, call.err
}

// --- shared bucket mirror (§5) -------------------------------------------------------

// bucketEnvelope wraps a mirrored render so stale generations are harmless:
// a revision mismatch on read means re-render; a lost create race is fine.
type bucketEnvelope struct {
	Revision uint64          `json:"revision"`
	Body     json.RawMessage `json:"body"`
}

// bucketKey is the mirror location: hex SHA-1 of the canonical request key.
func bucketKey(key string) string {
	sum := sha1.Sum([]byte(key))
	return "cache/api/v1/" + hex.EncodeToString(sum[:]) + ".json"
}

// bucketGet reads the mirrored envelope when the shared render cache is on;
// ok=false on any miss/mismatch (never an error path for the request).
func (e *Env) bucketGet(ctx context.Context, key string, rev uint64) ([]byte, bool) {
	if e.Store == nil {
		return nil, false
	}
	res, err := e.Store.Get(ctx, bucketKey(key), store.GetOptions{})
	if err != nil {
		return nil, false
	}
	obj, ok := res.(store.Object)
	if !ok {
		return nil, false
	}
	defer obj.Body.Close()
	var env bucketEnvelope
	if err := json.NewDecoder(obj.Body).Decode(&env); err != nil {
		return nil, false
	}
	if env.Revision != rev {
		return nil, false // stale generation: discard
	}
	return env.Body, true
}

// bucketPut mirrors a rendered body on a worker goroutine — it NEVER delays
// the response (§5). Create-if-absent semantics; a lost race is fine (same
// key, same body as long as revisions match).
func (e *Env) bucketPut(key string, rev uint64, body []byte) {
	if e.Store == nil {
		return
	}
	raw, err := json.Marshal(bucketEnvelope{Revision: rev, Body: body})
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = e.Store.Put(ctx, bucketKey(key), store.PutBody{Bytes: raw}, store.PutOptions{
			Mode:        store.PutCreate,
			ContentType: "application/json",
			Immutable:   true,
		})
	}()
}

// renderImmutable is the shared path behind every sha-addressed handler:
// LRU (revision-stamped) → shared bucket (when enabled) → render → stash +
// async mirror. etag is the bare resolved sha the caller already knows; the
// caller's render closure captures the view call.
func (e *Env) renderImmutable(ctx context.Context, key string, rev uint64, etag string, render func() ([]byte, error)) ([]byte, error) {
	body, _, err := e.cache.Get(key, rev, func() ([]byte, string, error) {
		// The bucket is checked inside the single-flight render: N concurrent
		// misses produce at most one render and at most one bucket read.
		if b, ok := e.bucketGet(ctx, key, rev); ok {
			return b, etag, nil
		}
		b, err := render()
		if err != nil {
			return nil, "", err
		}
		e.bucketPut(key, rev, b)
		return b, etag, nil
	})
	return body, err
}
