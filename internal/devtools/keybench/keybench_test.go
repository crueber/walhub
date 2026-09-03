// Package keybench is the performance-evidence harness for the SSH key
// registry (docs/EVIDENCE.md E1). Skipped unless WALHUB_EVIDENCE=1: the 1M-key
// run takes ~40s and ~2 GB of memory-store objects, so it never runs in CI.
//
// Reproduce:
//
//	WALHUB_EVIDENCE=1 go test ./internal/devtools/keybench/ -run TestSSHKeyRegistryScale -v -timeout 30m
//
// The output rows are markdown-ready and are what E1's tables quote.
package keybench

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/store"
)

// TestSSHKeyRegistryScale measures the SSH key registry (17_ssh.md §3) at two
// population sizes: 10k and 1M registered keys, spread over 100 principals.
// Paths measured: SSH-auth lookup (LookupByFingerprint — the hot path), the
// keys-page list (per-principal), and a single add. Memory store: the numbers
// isolate the registry's algorithmic shape from network RTT; over S3/GCS add
// one GET of latency per lookup and per list entry (see E1 in the evidence doc).
func TestSSHKeyRegistryScale(t *testing.T) {
	if os.Getenv("WALHUB_EVIDENCE") != "1" {
		t.Skip("set WALHUB_EVIDENCE=1 to run the evidence benchmark (~40s, ~2GB RAM)")
	}
	if testing.Short() {
		t.Skip("long evidence run")
	}
	for _, n := range []int{10_000, 1_000_000} {
		t.Run(fmt.Sprintf("keys=%d", n), func(t *testing.T) {
			start := time.Now()
			r, ctx, drop := benchRegistry(n)
			defer drop()
			t.Logf("setup: %d keys in %v", n, time.Since(start))

			// SSH-auth lookups against registered fingerprints of one principal.
			keys, err := r.List(ctx, "user0")
			if err != nil || len(keys) == 0 {
				t.Fatalf("lookup fixture: %v (%d keys)", err, len(keys))
			}
			lat := []time.Duration{}
			for i := 0; i < 1000; i++ {
				fp := keys[i%len(keys)].Fingerprint
				s := time.Now()
				e, err := r.LookupByFingerprint(ctx, fp)
				d := time.Since(s)
				if err != nil || e.Principal == "" {
					t.Fatalf("lookup %d: %v", i, err)
				}
				lat = append(lat, d)
			}
			var sum time.Duration
			max := time.Duration(0)
			for _, d := range lat {
				sum += d
				if d > max {
					max = d
				}
			}
			t.Logf("lookup: mean %v max %v (n=1000, memory store)", sum/time.Duration(len(lat)), max)

			// keys page for a principal holding n/100 keys
			s := time.Now()
			page, err := r.List(ctx, "user0")
			d := time.Since(s)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("list: %v for %d keys (one principal)", d, len(page))

			// a single add at this population
			s = time.Now()
			line := newKeyLine(fmt.Sprintf("fresh-%d", n))
			if _, err := r.Add(ctx, "fresh", line, ""); err != nil {
				t.Fatalf("add: %v", err)
			}
			t.Logf("add: %v", time.Since(s))
		})
	}
}

// benchRegistry registers n keys over 100 principals and returns a stop func
// (the memory store holds ~2n objects; drop is a no-op for GC-only backends).
func benchRegistry(n int) (*server.SSHKeyRegistry, context.Context, func()) {
	c := config.Defaults()
	c.Server.Auth.Mode = "none"
	a := server.NewAuthService(&c.Server.Auth, nil)
	r := server.NewSSHKeyRegistry(store.NewMemory(), a, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	for i := 0; i < n; i++ {
		principal := fmt.Sprintf("user%d", i%100)
		line := newKeyLine(fmt.Sprintf("u%d-%d", i%100, i))
		if _, err := r.Add(ctx, principal, line, ""); err != nil {
			panic(err)
		}
	}
	return r, ctx, func() {}
}

// newKeyLine generates a real, distinct authorized_keys line (the fingerprint
// must be unique per key — the registry enforces it).
func newKeyLine(comment string) string {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + comment
}
