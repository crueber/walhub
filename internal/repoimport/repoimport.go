// Package repoimport implements Feature 10 (docs/features/10_git_import.md):
// import Git repositories from GitHub or any git source, with UI.
//
// A user posts a source URL + target owner/name (+ optional token for
// private sources, ref filters, format pin); the package clones --mirror
// into task scratch, enumerates + filters refs (the S4 refmap), ingests
// through the existing publish path (source packs as tier-0, a full
// bitmap'd repack as the tier-2 base — the classic runImport shape), and
// commits with manifest.pb PutCreate (the CAS decides ownership). Then it
// writes the importer-admin access.json via the identity service, and the
// meta/import.json provenance (Create-once-then-CAS'd, the frozen
// overwritable family — 14_extensibility.md §14.11 rule 2).
//
// Seams (14_extensibility.md): Seam 1 via Handler (server.ExtraRoutes,
// both lanes, top-level twins); Seam 5 via KindRepoImport run on the core
// wal.TaskTable through the cmd/walhub composition (opsTasks-style wiring
// — Begin subscribes the table-level replay ring, then Tasks().Run on a
// drain-scoped ctx under `<target>/repo-import`: the leader never sees
// the request ctx, so client disconnect cannot cancel it, while phase-1
// Drain cancels it promptly); Seam 7 via the `walhub import --url` CLI. Core
// (internal/store, internal/wal, internal/git) is never imported upward:
// this package depends only on seam interfaces + frozen types. Git is
// always the subprocess binary with pinned argv (docs/go/04_git.md);
// credentials ride per-spawn child env and are never stored (S2).
package repoimport

import (
	"context"
	"fmt"
	"sync"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// KindRepoImport is the Seam 5 task kind (single constant, picked once —
// the maintain.RegisterKind panic-on-duplicate contract is preserved by
// RegisterKind below, called from composition, not from New).
const KindRepoImport = "repo-import"

// ImportKey is the bucket key of the provenance sidecar.
const ImportKey = "meta/import.json"

// importKey returns repos/<o>/<r>/meta/import.json.
func importKey(owner, repo string) string {
	return store.RepoPrefix(owner, repo) + ImportKey
}

// --- kind registration (Seam 5) -----------------------------------------------

var (
	kindsMu sync.Mutex
	kinds   = map[string]bool{}
)

// RegisterKind records a task-kind name; a duplicate panics (the
// maintain.RegisterKind contract, in code terms per R1 B6: there is one
// kind constant and composition registers it exactly once).
func RegisterKind(name string) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if kinds[name] {
		panic(fmt.Sprintf("repoimport: duplicate task kind %q", name))
	}
	kinds[name] = true
}

// --- roles seam -----------------------------------------------------------------

// RoleService is the identity surface import needs (P6 resolution for the
// namespace gates, importer-admin writes at commit). *identity.Service
// satisfies it in production; tests substitute fakes.
type RoleService interface {
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
	CheckRole(ctx context.Context, owner, repo string, p auth.Principal, want identity.Role) *auth.AuthError
	BootstrapRepo(ctx context.Context, owner, repo string) (bool, error)
	GetAccess(ctx context.Context, owner, repo string) (*identity.AccessDoc, store.Version, error)
	PutAccess(ctx context.Context, owner, repo string, base store.Version, vis identity.Visibility, bindings []identity.AccessBinding) (*identity.AccessDoc, error)
}

// --- errors -----------------------------------------------------------------------

// StatusError carries an HTTP status with a plain-text message (wire
// conventions: plain-text errors, never a JSON envelope).
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string { return e.Message }

// --- service ------------------------------------------------------------------------

// Service owns the import surface: the running-task registry (B2
// params-aware join lives here, before TaskTable.Run), the id-keyed replay
// rings (B4 pre-create task home), the import.max_concurrent clone
// semaphore (S9), and the headless runner shared by HTTP + CLI.
type Service struct {
	store    store.ObjectStore
	reg      *wal.Registry
	roles    RoleService
	cfg      *config.Config
	git      *Runner
	hostname string

	mu      sync.Mutex
	running map[string]*running // key: "<owner>/<repo>" target
	streams map[string]*stream  // key: task id (B4 id-keyed attach, incl. finished)
	clones  chan struct{}       // import.max_concurrent semaphore (S9, sender-owns-close n/a: channel is a pool)

	// Phase-1 drain state (13 §8): drainCtx is cancelled by Drain;
	// drive leaders derive their Run-wait ctx from it, so drain
	// cancels an in-flight import promptly. draining flips at the
	// same point so new Begins refuse fast and post-drain manifest
	// commits refuse at the commit-point guard (task.go). drainCtx
	// is immutable after New (no lock to read); draining is
	// mu-guarded; drainCancel is non-blocking, never called under mu.
	drainCtx    context.Context
	drainCancel context.CancelFunc
	draining    bool
}

// Deps wires a Service. Store/Reg/Roles/Cfg are required; GitBinary falls
// back to "git"; Hostname falls back to "unknown".
type Deps struct {
	Store     store.ObjectStore
	Reg       *wal.Registry
	Roles     RoleService
	Cfg       *config.Config
	GitBinary string
	Hostname  string
}

// New builds a Service over the shared store/registry/roles/config. Nil
// Roles means host-flag-only gating (tests without the identity surface).
func New(d Deps) *Service {
	cfg := d.Cfg
	if cfg == nil {
		cfg = config.Defaults()
	}
	maxConc := cfg.Import.MaxConcurrent
	if maxConc < 1 {
		maxConc = 1
	}
	bin := d.GitBinary
	if bin == "" {
		bin = "git"
	}
	host := d.Hostname
	if host == "" {
		host = "unknown"
	}
	drainCtx, drainCancel := context.WithCancel(context.Background())
	return &Service{
		store:    d.Store,
		reg:      d.Reg,
		roles:    d.Roles,
		cfg:      cfg,
		git:      newRunner(bin, cfg),
		hostname: host,
		running:  map[string]*running{},
		streams:  map[string]*stream{},
		clones:   make(chan struct{}, maxConc),

		drainCtx:    drainCtx,
		drainCancel: drainCancel,
	}
}

// Drain enters phase-1 drain for the import surface (13 §8, law 7):
// in-flight leaders see cancellation promptly (their Run wait derives
// from drainCtx), new Begins refuse fast with the 503 below, and the
// commit-point guard refuses post-drain manifest CAS commits. The task
// BODIES die via reg.Tasks().Drain(), which composition calls alongside
// this (serve.go phase 1) — this method scopes itself to the service so
// the registry owner keeps the table lifecycle. Idempotent; safe to call
// with no imports running.
func (s *Service) Drain() {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
	s.drainCancel() // non-blocking; leaders + clone-gate selects observe it
}

// Draining reports whether Drain has begun (phase ≥ 1).
func (s *Service) Draining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// interruptedErr is the phase-1 drain terminal (13 §8: 503 "interrupted",
// safe to retry — law 7 narrates the drain, never a silent spinner or a
// bare context.Canceled).
func interruptedErr() *StatusError {
	return &StatusError{Status: 503, Message: "import interrupted: instance is draining; safe to retry"}
}

// running is one in-flight import (service-level single-flight; the
// TaskTable's own (repo,kind) join is the backstop, never the decision).
type running struct {
	id     string
	params map[string]string // scrubbed canonical params (B2 comparison input)
	rec    *wal.TaskRecord   // live record mirror (outcome on done)
	done   chan struct{}
	err    error // terminal body error (scrubbed)
}

// finish closes the running entry; the stream ring retains replay.
func (s *Service) finishLocked(target string, r *running, rec *wal.TaskRecord, err error) {
	r.rec = rec
	r.err = err
	close(r.done)
	delete(s.running, target)
}

// taskRecord fetches the core table record by id (running or <1h finished).
func (s *Service) taskRecord(id string) *wal.TaskRecord {
	if s.reg == nil {
		return nil
	}
	return s.reg.Tasks().Get(id)
}

// targetKey is the single-flight key "<owner>/<repo>" (13 §3 "task:" row;
// the table key appends "/repo-import" — 04_git.md §2 shape in code terms).
func targetKey(owner, repo string) string { return owner + "/" + repo }
