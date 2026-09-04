// service.go — Begin (authenticate → authorize → join-or-start), the
// id-keyed GET surface, and the headless runner shared by HTTP + CLI.
package repoimport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// Params is the validated import request (scrubbed: no token anywhere).
type Params struct {
	Owner             string
	Name              string
	Source            Normalized
	Refs              []string
	DefaultBranchOnly bool
	IncludePullHeads  bool
	IncludeNotes      bool
	Format            string // "" (follow source) | "sha1" | "sha256"
	Dangerous         bool
	TokenSet          bool
	// importer is the resolved principal name (set by Begin AFTER auth —
	// never from the request; excluded from scrubbedMap/B2 comparison so
	// two authorized users share one import of the same source).
	importer string
}

// target returns "owner/name".
func (p Params) target() string { return targetKey(p.Owner, p.Name) }

// scrubbedMap is the TaskRecord.Params content (S2: canonical URL +
// options only — never the token, never the raw URL).
func (p Params) scrubbedMap() map[string]string {
	m := map[string]string{
		"source_url":  p.Source.URL,
		"source_kind": string(p.Source.Kind),
		"target":      p.target(),
		"format":      p.Format,
	}
	if len(p.Refs) > 0 {
		m["refs"] = strings.Join(p.Refs, ",")
	}
	if p.DefaultBranchOnly {
		m["default_branch_only"] = "true"
	}
	if p.IncludePullHeads {
		m["include_pull_heads"] = "true"
	}
	if p.IncludeNotes {
		m["include_notes"] = "true"
	}
	if p.TokenSet {
		m["secret_set"] = "true"
	}
	return m
}

// paramsEqual is the B2 comparison: join iff canonical source + refs +
// options + format match; anything else on the same target is a 409.
func paramsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// importRequest is the strict POST body shape.
type importRequest struct {
	SourceURL         string   `json:"source_url"`
	Owner             string   `json:"owner"`
	Name              string   `json:"name"`
	Token             string   `json:"token"`
	Refs              []string `json:"refs"`
	DefaultBranchOnly bool     `json:"default_branch_only"`
	IncludePullHeads  bool     `json:"include_pull_heads"`
	IncludeNotes      bool     `json:"include_notes"`
	Format            string   `json:"format"`
	Dangerous         bool     `json:"dangerous"`
}

// ParseRequest decodes + validates the POST/CLI input (unknown keys 400,
// fail closed). SSRF DNS is checked here (check-time; the clone-time
// TOCTOU residual is documented in url.go).
func ParseRequest(body []byte, cfg *config.Config) (Params, string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return Params{}, "", &StatusError{Status: 400, Message: "invalid JSON body"}
	}
	for k := range raw {
		switch k {
		case "source_url", "owner", "name", "token", "refs",
			"default_branch_only", "include_pull_heads", "include_notes",
			"format", "dangerous":
		default:
			return Params{}, "", &StatusError{Status: 400, Message: fmt.Sprintf("unknown field %q", k)}
		}
	}
	var in importRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return Params{}, "", &StatusError{Status: 400, Message: "invalid JSON body"}
	}
	if strings.TrimSpace(in.Owner) == "" || strings.TrimSpace(in.Name) == "" {
		return Params{}, "", &StatusError{Status: 400, Message: "owner and name are required"}
	}
	if _, err := git.ParseRepoId(in.Owner + "/" + in.Name); err != nil {
		return Params{}, "", &StatusError{Status: 400, Message: fmt.Sprintf("bad target: %v", err)}
	}
	n, err := NormalizeSource(in.SourceURL)
	if err != nil {
		return Params{}, "", err
	}
	token := in.Token
	if err := ValidateTransport(n, token != ""); err != nil {
		return Params{}, "", err
	}
	if cfg == nil {
		cfg = config.Defaults()
	}
	if err := CheckSSRF(n, SSRFConfig{
		AllowPrivate: cfg.Import.AllowPrivateNetworks,
		Allowlist:    cfg.Import.URLAllowlist,
		AllowFile:    cfg.Import.AllowFileURLs,
		Dangerous:    in.Dangerous,
	}, resolveHost); err != nil {
		return Params{}, "", err
	}
	format := strings.ToLower(strings.TrimSpace(in.Format))
	if format != "" && format != "sha1" && format != "sha256" {
		return Params{}, "", &StatusError{Status: 400, Message: fmt.Sprintf("bad format %q: want sha1|sha256", in.Format)}
	}
	refs := in.Refs
	if refs == nil {
		refs = []string{}
	}
	for _, r := range refs {
		if !strings.HasPrefix(r, "refs/") || strings.ContainsAny(r, " \t\n\r~^:?*[") {
			return Params{}, "", &StatusError{Status: 400, Message: fmt.Sprintf("bad refs entry %q: want full refnames", r)}
		}
	}
	return Params{
		Owner:             in.Owner,
		Name:              in.Name,
		Source:            n,
		Refs:              refs,
		DefaultBranchOnly: in.DefaultBranchOnly,
		IncludePullHeads:  in.IncludePullHeads,
		IncludeNotes:      in.IncludeNotes,
		Format:            format,
		Dangerous:         in.Dangerous,
		TokenSet:          token != "",
	}, token, nil
}

// --- Begin: authenticate → authorize → join-or-start (S6/B2/B3) ------------------

// BeginResult is the POST outcome: either a task to attach (202) or an
// idempotent no-op (200).
type BeginResult struct {
	// TaskID is set for 202 (attach via GET .../imports/{id}).
	TaskID string
	// NoOp is set for 200 (import.json already matches).
	NoOp *ImportDoc
	// Joined reports a B2 join (params matched a running import).
	Joined bool
}

// Begin authenticates, authorizes create-on-namespace, then joins a
// params-matching running import (B2), answers the idempotent no-op
// (import.json matches), 409s foreign/different targets (B3), or starts a
// fresh task on the core wal.TaskTable (B6). p is the resolved principal.
func (s *Service) Begin(ctx context.Context, p auth.Principal, params Params, token string) (*BeginResult, *wal.TaskRecord, error) {
	if err := s.checkCreate(ctx, p, params.Owner, params.Name); err != nil {
		return nil, nil, err
	}
	params.importer = p.Name
	target := params.target()
	want := params.scrubbedMap()

	s.mu.Lock()
	if res, rec, err := s.joinOrConflictLocked(target, want); err != nil || res != nil {
		s.mu.Unlock()
		return res, rec, err
	}
	s.mu.Unlock()
	// B3 probe (synchronous, fail fast): manifest × import.json. Runs
	// OUTSIDE the service lock — never hold a lock across store I/O
	// (13 §2 rule 4); the join window is re-checked under the lock
	// below before the running entry is installed.
	manifestPresent, doc, err := s.probe(ctx, params)
	if err != nil {
		return nil, nil, err
	}
	if manifestPresent && doc != nil {
		return &BeginResult{NoOp: doc}, nil, nil
	}
	if manifestPresent {
		return nil, nil, &StatusError{Status: 409, Message: fmt.Sprintf("target %s exists but was not created by import; delete and retry, or pick another name", target)}
	}

	st := newStream()
	st.target = target
	id := newImportID()
	s.mu.Lock()
	// Re-check under the lock (a concurrent Begin may have won).
	if res, rec, err := s.joinOrConflictLocked(target, want); err != nil || res != nil {
		s.mu.Unlock()
		return res, rec, err
	}
	r := &running{id: id, params: want, done: make(chan struct{})}
	s.running[target] = r
	s.streams[id] = st
	pruneExpiredLocked(s.streams)
	s.mu.Unlock()

	go s.drive(target, id, params, token, st, r)
	// The 202 carries the id-keyed stream; the table record lands on the
	// first narration (GET merges whichever is fresher).
	return &BeginResult{TaskID: id}, nil, nil
}

// joinOrConflictLocked implements B2 under the service lock: a running
// import with matching canonical params joins (same outcome, bounded by
// the joiner ctx at the table); anything else on the same target is a
// 409 naming the running import. (nil, nil, nil) means start fresh.
func (s *Service) joinOrConflictLocked(target string, want map[string]string) (*BeginResult, *wal.TaskRecord, error) {
	if r := s.running[target]; r != nil {
		if paramsEqual(r.params, want) {
			return &BeginResult{TaskID: r.id, Joined: true}, r.rec, nil
		}
		return nil, nil, &StatusError{Status: 409, Message: fmt.Sprintf("import to %s already running (task %s) with a different source or options; wait for it or delete and retry", target, r.id)}
	}
	return nil, nil, nil
}

// probe implements B3: manifest present? import.json present + matching?
func (s *Service) probe(ctx context.Context, params Params) (bool, *ImportDoc, error) {
	if s.store == nil {
		return false, nil, &StatusError{Status: 503, Message: "import store not configured"}
	}
	meta, err := s.store.Head(ctx, store.RepoPrefix(params.Owner, params.Name)+store.Manifest)
	if err != nil {
		return false, nil, &StatusError{Status: 500, Message: fmt.Sprintf("probe target: %v", scrubError(err.Error()))}
	}
	if meta == nil {
		return false, nil, nil
	}
	doc, _, err := readImportDoc(ctx, s.store, params.Owner, params.Name)
	if err != nil {
		return false, nil, err
	}
	if doc != nil && doc.SourceURL == params.Source.URL {
		return true, doc, nil
	}
	return true, nil, nil
}

// drive runs the import body on the core table (B6: opsTasks-style —
// WithoutCancel so client disconnect never cancels the leader) and
// publishes narration to the id-keyed ring.
func (s *Service) drive(target, id string, params Params, token string, st *stream, r *running) {
	ctx := context.WithoutCancel(context.Background())
	var rec *wal.TaskRecord
	var runErr error
	in := &importNarr{stream: st}
	if s.reg != nil {
		rec, runErr = s.reg.Tasks().Run(ctx, target, KindRepoImport, params.scrubbedMap(), func(tctx context.Context, task *wal.Task) error {
			in.task = task
			return s.runImport(tctx, in, id, params, token)
		})
	} else {
		runErr = &StatusError{Status: 503, Message: "import registry not configured"}
	}
	var outcome *Outcome
	if runErr != nil {
		outcome = &Outcome{Repo: target, HeadSHAs: map[string]string{}, Err: statusOf(runErr)}
	} else {
		// runImport's contract: nil error ⇒ result set (adopt + commit
		// paths both setResult; a nil here surfaces as the terminal
		// error frame, never a silent close).
		outcome = in.result
	}
	st.setOutcomeRecord(rec)
	st.finish(outcome)
	s.mu.Lock()
	if cur, ok := s.running[target]; ok && cur == r {
		s.finishLocked(target, r, rec, runErr)
	}
	s.mu.Unlock()
}

// Narrator is the narration sink: phase Notices + progress bars over a
// cancellable ctx (*wal.Task satisfies it directly).
type Narrator interface {
	Ctx() context.Context
	Notice(text string)
	Progress(label string, done, total uint64, unit string)
}

// importNarr wraps the run's narrator, fans server-path packets out to the
// id-keyed ring (B4), and collects the terminal result from the body.
type importNarr struct {
	task   *wal.Task // server path (nil on CLI)
	print  *Printer  // CLI path (nil on server)
	stream *stream   // server path ring (nil on CLI)
	result *Outcome
}

func (n *importNarr) Ctx() context.Context {
	if n.task != nil {
		return n.task.Ctx()
	}
	return n.print.Ctx()
}

func (n *importNarr) Notice(text string) {
	text = scrubError(scrubURL(text))
	if n.task != nil {
		n.task.Notice(text)
		n.stream.setRecord(n.task.Record())
		n.stream.send(wal.Progress{Kind: "notice", Text: text})
		rec := n.task.Record()
		n.stream.send(wal.Progress{Kind: "task", Task: &rec})
		return
	}
	n.print.Notice(text)
}

func (n *importNarr) Progress(label string, done, total uint64, unit string) {
	if n.task != nil {
		n.task.Progress(label, done, total, unit)
		n.stream.setRecord(n.task.Record())
		p := wal.Progress{Kind: "progress", Label: label, Done: done, Unit: unit}
		if total > 0 {
			p.Total = &total
			pct := float64(done) / float64(total) * 100
			p.Percent = &pct
		}
		n.stream.send(p)
		return
	}
	n.print.Progress(label, done, total, unit)
}

// setResult records the body's terminal value (heads, format, time).
func (n *importNarr) setResult(o *Outcome) { n.result = o }

// setOutcomeRecord mirrors the final table record before finish.
func (s *stream) setOutcomeRecord(rec *wal.TaskRecord) {
	if rec == nil {
		return
	}
	s.setRecord(*rec)
}

// newImportID mints a 32-hex task id (table ids are 16 random bytes hex).
func newImportID() string {
	// NOTE: the table mints its own id for the wal record; the import id
	// is the SERVICE id (B4 id-keyed ring). Both travel in the outcome.
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("i%x", time.Now().UnixNano())
	}
	return "i" + hex.EncodeToString(b[:15])
}

// --- GET surface ----------------------------------------------------------------------

// Lookup resolves an import id to its ring + record (streams first, then
// the core table for pruned-but-remembered records).
func (s *Service) Lookup(id string) (*stream, *wal.TaskRecord, bool) {
	s.mu.Lock()
	pruneExpiredLocked(s.streams)
	st, ok := s.streams[id]
	s.mu.Unlock()
	if ok {
		rec, _, _ := st.snapshot()
		if rec == nil {
			rec = s.taskRecord(id)
		}
		return st, rec, true
	}
	if rec := s.taskRecord(id); rec != nil {
		return nil, rec, true
	}
	return nil, nil, false
}

// Janitor prunes finished rings past retention (mirrors the wal byID
// janitor; import.json stays the durable truth). Begin/Lookup already
// prune lazily on every call, so this is the explicit-sweep entry point
// (operator/debug use) rather than a required sweeper.
func (s *Service) Janitor() {
	s.mu.Lock()
	defer s.mu.Unlock()
	pruneExpiredLocked(s.streams)
}

// pruneExpiredLocked deletes finished streams past streamRetain. Caller
// holds the service lock; the stream expiry check nests stream.mu under
// it (the same direction as Janitor — never the reverse).
func pruneExpiredLocked(streams map[string]*stream) {
	now := time.Now()
	for id, st := range streams {
		if st.expired(now) {
			delete(streams, id)
		}
	}
}

// --- auth -------------------------------------------------------------------------------

// checkCreate enforces create-on-namespace (plan §4, S6 order — called
// AFTER authenticate, BEFORE join-or-start): anonymous → 401 (real 401,
// so clients erase creds — law 9); host write/admin, org owners, and
// ≥write roles pass; everyone else → 403.
func (s *Service) checkCreate(ctx context.Context, p auth.Principal, owner, repo string) error {
	if p.Anonymous {
		return &StatusError{Status: 401, Message: "authentication required"}
	}
	if s.roles == nil {
		if p.Write || p.Admin {
			return nil
		}
		return &StatusError{Status: 403, Message: "write access required"}
	}
	if cerr := s.roles.CheckRole(ctx, owner, repo, p, identity.RoleWrite); cerr != nil {
		switch cerr.Kind {
		case auth.ErrUnauthorized:
			return &StatusError{Status: 401, Message: cerr.Why}
		default:
			return &StatusError{Status: 403, Message: cerr.Why}
		}
	}
	return nil
}

// checkRead gates task-status reads on the namespace (the task may exist
// before the repo does — gate on namespace, not repo).
func (s *Service) checkRead(ctx context.Context, p auth.Principal, owner, repo string) error {
	if s.roles == nil {
		if p.Anonymous {
			return &StatusError{Status: 401, Message: "authentication required"}
		}
		return nil
	}
	if cerr := s.roles.CheckRead(ctx, owner, repo, p); cerr != nil {
		switch cerr.Kind {
		case auth.ErrUnauthorized:
			return &StatusError{Status: 401, Message: cerr.Why}
		default:
			return &StatusError{Status: 403, Message: cerr.Why}
		}
	}
	return nil
}

// --- headless runner (Seam 7: CLI goes through the same publish/CAS path) --------

// Printer is the CLI narrator: human lines on stdout/stderr.
type Printer struct {
	Context context.Context
}

// Ctx returns the command ctx (SIGINT cancels the clone mid-flight).
func (p *Printer) Ctx() context.Context {
	if p.Context == nil {
		return context.Background()
	}
	return p.Context
}

// Notice prints a phase line.
func (p *Printer) Notice(text string) { fmt.Printf("import: %s\n", scrubError(scrubURL(text))) }

// Progress prints carriage-return bars at 10% steps (quiet otherwise).
func (p *Printer) Progress(label string, done, total uint64, unit string) {
	if total == 0 || done > total {
		return
	}
	pct := done * 100 / total
	if done == total || pct%10 == 0 {
		fmt.Printf("import: %s %d%% (%d/%d %s)\n", label, pct, done, total, unit)
	}
}

// RunHeadless executes the import body synchronously (CLI path — no table,
// no ring; the manifest CAS is still the commit point) and returns the
// outcome. importer names the operator (recorded as repo admin).
func (s *Service) RunHeadless(ctx context.Context, params Params, token, importer string) (*Outcome, error) {
	if importer == "" {
		importer = "operator"
	}
	params.importer = importer
	in := &importNarr{print: &Printer{Context: ctx}}
	if err := s.runImport(ctx, in, newImportID(), params, token); err != nil {
		return &Outcome{Repo: params.target(), HeadSHAs: map[string]string{}, Err: statusOf(err)}, err
	}
	if in.result == nil {
		return &Outcome{Repo: params.target(), HeadSHAs: map[string]string{}}, nil
	}
	return in.result, nil
}

// statusOf normalizes body errors to StatusError (scrubbed).
func statusOf(err error) *StatusError {
	if se, ok := err.(*StatusError); ok {
		return se
	}
	return &StatusError{Status: 500, Message: scrubError(fmt.Sprintf("%v", err))}
}
