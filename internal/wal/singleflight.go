// singleflight.go — the hand-rolled per-key single-flight of 13_concurrency.md §3.
// Joiners get the SAME result; DoCtx lets a joiner abandon the wait on its own
// context (the leader's call continues; the abandoned joiner gets ctx.Err()).
package wal

import (
	"context"
	"sync"
)

type sfCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Group deduplicates concurrent calls per key.
type Group struct {
	mu sync.Mutex
	fl map[string]*sfCall
}

// Do runs fn for the key, deduplicating concurrent callers.
func (g *Group) Do(key string, fn func() (any, error)) (any, error) {
	return g.DoCtx(context.Background(), key, fn)
}

// DoCtx is Do with a bounded join: a joiner whose ctx is done stops waiting
// (the leader still completes and other joiners share its result).
func (g *Group) DoCtx(ctx context.Context, key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if g.fl == nil {
		g.fl = map[string]*sfCall{}
	}
	if c, ok := g.fl[key]; ok {
		g.mu.Unlock()
		done := make(chan struct{})
		go func() { c.wg.Wait(); close(done) }()
		select {
		case <-done:
			return c.val, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &sfCall{}
	c.wg.Add(1)
	g.fl[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.fl, key)
	g.mu.Unlock()
	return c.val, c.err
}
