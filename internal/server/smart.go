package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/wal"
)

// gitHeaders stamps the §4.1 no-cache triple on every git response.
func gitHeaders(w http.ResponseWriter, contentType string) {
	h := w.Header()
	h.Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	h.Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	h.Set("Pragma", "no-cache")
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
}

// svcContentType maps a git service onto its advertisement/result content type.
func svcContentType(svc git.Service) string {
	if svc == git.ServiceReceivePack {
		return "application/x-git-receive-pack-advertisement"
	}
	return "application/x-git-upload-pack-advertisement"
}

// pktErrBody builds the pkt-line ERR wire shape for git-ish refusals.
func pktErrBody(msg string) []byte {
	return git.ErrPkt(msg)
}

// pktErrFor returns the pkt writer used by drain/placement gates.
func (s *Server) pktErrFor(w http.ResponseWriter, r *http.Request) func(msg string) {
	if !isGitClient(r.UserAgent()) {
		return nil
	}
	return func(msg string) {
		gitHeaders(w, "application/x-git-upload-pack-result")
		body := pktErrBody(msg)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// gitAuthFailure implements §4.2, the 401-vs-pkt-ERR rule: reply 200 +
// pkt-line ERR only when ALL of (1) git-ish UA, (2) ?service= present,
// (3) Authorization carried, (4) Forbidden/Unavailable (retrying cannot help).
// Invalid/expired credentials MUST get a real 401 (git erases the dead token
// and re-prompts).
func (s *Server) gitAuthFailure(w http.ResponseWriter, r *http.Request, svc git.Service, aerr *auth.AuthError) {
	gitish := isGitClient(r.UserAgent())
	hasService := r.URL.Query().Get("service") != ""
	carriedAuth := r.Header.Get("Authorization") != "" || r.Header.Get("X-Walgit-Authorization") != ""
	retryNeverHelps := aerr.Kind == auth.ErrForbidden || aerr.Kind == auth.ErrUnavailable
	if gitish && hasService && carriedAuth && retryNeverHelps {
		gitHeaders(w, svcContentType(svc))
		body := pktErrBody("walgit: " + aerr.Why + " — see " + s.cfg.Server.PublicURL + "/_auth/tokens for where tokens come from")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	switch aerr.Kind {
	case auth.ErrUnavailable:
		w.Header().Set("Retry-After", "15")
		plainStatus(w, http.StatusServiceUnavailable, aerr.Why)
	case auth.ErrForbidden:
		if !carriedAuth {
			// No credential at all: a real 401 makes git prompt (§4.2).
			w.Header().Set("WWW-Authenticate", `Bearer realm="walgit"`)
			plainStatus(w, http.StatusUnauthorized, aerr.Why)
			return
		}
		plainStatus(w, http.StatusForbidden, aerr.Why)
	default:
		w.Header().Set("WWW-Authenticate", `Bearer realm="walgit"`)
		plainStatus(w, http.StatusUnauthorized, aerr.Why)
	}
}

// gitInfoRefs answers GET|HEAD /{o}/{r}[.git]/info/refs?service= (§3.3):
// upload-pack → require_read; receive-pack → require_write; unknown service →
// 400; Git-Protocol header selects v0/v2.
func (s *Server) gitInfoRefs(w http.ResponseWriter, r *http.Request, id git.RepoId) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		plainStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	svcName := r.URL.Query().Get("service")
	if svcName == "" {
		// Dumb HTTP does not exist (§1): no service → deliberate 404.
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	svc, ok := git.ServiceFromName(svcName)
	if !ok {
		plainStatus(w, http.StatusBadRequest, "unknown service: "+svcName)
		return
	}
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.gitAuthFailure(w, r, svc, aerr)
		return
	}
	p = s.authSvc.identityForward(r, p)
	if svc == git.ServiceUploadPack {
		if aerr := requireRead(p, s.cfg.Server.Auth.AnonymousRead); aerr != nil {
			s.gitAuthFailure(w, r, svc, aerr)
			return
		}
	} else if aerr := requireWrite(p); aerr != nil {
		s.gitAuthFailure(w, r, svc, aerr)
		return
	}
	// Placement gate (not_served_here → 503 + pkt ERR per §4.2/§4.3).
	if !s.placementOK(w, r, id, svc) {
		return
	}
	if rel := s.sem.TryAcquire(id.String()); rel == nil {
		w.Header().Set("Retry-After", "15")
		s.gitAuthFailure(w, r, svc, &auth.AuthError{Kind: auth.ErrUnavailable,
			Why: "repository busy"})
		return
	} else {
		defer rel()
	}
	v2 := git.ProtocolVersion(r.Header.Get("Git-Protocol")) == 2
	if aerr := s.engine.Sync(r.Context(), id, wal.LevelRefs); aerr != nil {
		if isNotFound(aerr) {
			plainStatus(w, http.StatusNotFound, "repository not found")
			return
		}
		s.gitAuthFailure(w, r, svc, &auth.AuthError{Kind: auth.ErrUnavailable, Why: aerr.Error()})
		return
	}
	repo, err := s.engine.Repo(r.Context(), id, false, git.Sha1)
	if err != nil {
		plainStatus(w, http.StatusNotFound, "repository not found")
		return
	}
	advert, err := s.layer.Advertisement(repo, svc, v2, s.Version())
	if err != nil {
		s.gitAuthFailure(w, r, svc, &auth.AuthError{Kind: auth.ErrUnavailable, Why: err.Error()})
		return
	}
	gitHeaders(w, svcContentType(svc))
	w.WriteHeader(http.StatusOK)
	if svc == git.ServiceUploadPack && v2 {
		_, _ = w.Write(advert)
		return
	}
	// v0 advertisement: `# service=…` prefix + flush, then the body.
	_, _ = w.Write(git.Pkt("# service=" + svcName + "\n"))
	_, _ = w.Write(git.Flush())
	_, _ = w.Write(advert)
}

// placementOK applies the §4.3 placement gate: not_served_here → 503 +
// Retry-After: 15 + pkt ERR naming the serving host.
func (s *Server) placementOK(w http.ResponseWriter, r *http.Request, id git.RepoId, svc git.Service) bool {
	if s.engine == nil {
		return true
	}
	pl, err := s.engine.Placement(r.Context(), id)
	if err != nil {
		return true // no placement info → serve
	}
	if pl.Serve {
		return true
	}
	s.metrics.Counter("walgit_not_served_here_total", "requests refused by placement").
		Inc("service", svc.String())
	host := pl.ServedBy
	if host == "" {
		host = "another host"
	}
	msg := "walgit: " + id.String() + " is served by " + host + "; retry shortly"
	if isGitClient(r.UserAgent()) && r.URL.Query().Get("service") != "" &&
		r.Header.Get("Authorization") != "" {
		gitHeaders(w, svcContentType(svc))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pktErrBody(msg))
		return false
	}
	w.Header().Set("Retry-After", "15")
	plainStatus(w, http.StatusServiceUnavailable, msg)
	return false
}

// bodyReader returns the request body, honoring Content-Encoding: gzip via a
// bounded reader (§7: decompress reading directly from r.Body — never into a
// full buffer; corrupt gzip → 400).
func (s *Server) bodyReader(w http.ResponseWriter, r *http.Request) (io.Reader, bool) {
	body := io.Reader(r.Body)
	if hasPrefixFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(body)
		if err != nil {
			plainStatus(w, http.StatusBadRequest, "corrupt gzip stream")
			return nil, false
		}
		body = gz
	}
	// Bound at the git protocol's own ingest caps (04_git.md): no global
	// body-limit middleware exists; limits are per-feature.
	return io.LimitReader(body, int64(s.cfg.Server.MaxPushBytes)+1), true
}

// gitService answers POST /{o}/{r}[.git]/git-{upload,receive}-pack (§3.3/§4).
func (s *Server) gitService(w http.ResponseWriter, r *http.Request, id git.RepoId, svc git.Service, hadGit bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		plainStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if svc == git.ServiceReceivePack && !hadGit {
		// receive-pack refuses a non-.git URL with a pkt ERR (§4.3).
		gitHeaders(w, svcContentType(svc))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pktErrBody("walgit: push requires the .git suffix: " + id.String() + ".git"))
		return
	}
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.gitAuthFailure(w, r, svc, aerr)
		return
	}
	p = s.authSvc.identityForward(r, p)
	if svc == git.ServiceUploadPack {
		if aerr := requireRead(p, s.cfg.Server.Auth.AnonymousRead); aerr != nil {
			s.gitAuthFailure(w, r, svc, aerr)
			return
		}
	} else if aerr := requireWrite(p); aerr != nil {
		s.gitAuthFailure(w, r, svc, aerr)
		return
	}
	if !s.placementOK(w, r, id, svc) {
		return
	}
	if rel := s.sem.TryAcquire(id.String()); rel == nil {
		w.Header().Set("Retry-After", "15")
		s.gitAuthFailure(w, r, svc, &auth.AuthError{Kind: auth.ErrUnavailable,
			Why: "repository busy"})
		return
	} else {
		defer rel()
	}

	if svc == git.ServiceUploadPack {
		s.uploadPack(w, r, id)
		return
	}
	s.receivePack(w, r, id, p)
}

// uploadPack streams the v0/v2 fetch: the body is piped to the git layer and
// the result streamed out (ctx-bound: client disconnect kills the child).
func (s *Server) uploadPack(w http.ResponseWriter, r *http.Request, id git.RepoId) {
	body, ok := s.bodyReader(w, r)
	if !ok {
		return
	}
	if err := s.engine.Sync(r.Context(), id, wal.LevelServe); err != nil {
		if isNotFound(err) {
			plainStatus(w, http.StatusNotFound, "repository not found")
			return
		}
		plainStatus(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	repo, err := s.engine.Repo(r.Context(), id, false, git.Sha1)
	if err != nil {
		plainStatus(w, http.StatusNotFound, "repository not found")
		return
	}
	protocol := ""
	if git.ProtocolVersion(r.Header.Get("Git-Protocol")) == 2 {
		protocol = "version=2"
	}
	gitHeaders(w, "application/x-git-upload-pack-result")
	w.WriteHeader(http.StatusOK)
	if err := s.layer.UploadPack(r.Context(), repo, body, w, protocol); err != nil && r.Context().Err() == nil {
		s.log.Warn("upload-pack failed", "repo", id.String(), "err", err)
	}
}

// pushBroker buffers the receive-pack body up to wal.push_broker_buffer_bytes
// for replay on broker-down fallback (§4.3: streamed passthrough above the cap
// cannot be replayed — fall back only when buffered).
func (s *Server) pushBroker() (url string, bufMax int64) {
	return s.cfg.WAL.PushBrokerURL, int64(s.cfg.WAL.PushBrokerBufferBytes)
}

// forwardToBroker forwards the receive-pack body to the broker: method+path
// preserved, X-Walgit-Forwarded: 1 + X-Walgit-Principal added; a request
// carrying X-Walgit-Forwarded is refused (loop guard).
func (s *Server) forwardToBroker(ctx context.Context, r *http.Request, body []byte, principal string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, r.Method, s.cfg.WAL.PushBrokerURL+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range r.Header {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("X-Walgit-Forwarded", "1")
	req.Header.Set("X-Walgit-Principal", principal)
	return http.DefaultClient.Do(req)
}

// receivePack implements the push path: parse commands → ingest the pack →
// connectivity → engine.Publish; .git suffix + placement + drain gates run
// before any sync work (§4.3).
func (s *Server) receivePack(w http.ResponseWriter, r *http.Request, id git.RepoId, p auth.Principal) {
	// Loop guard: a request the broker already forwarded is refused here.
	if r.Header.Get("X-Walgit-Forwarded") != "" && s.cfg.WAL.PushBrokerURL != "" {
		s.metrics.Counter("walgit_push_refused_total", "pushes refused").Inc("reason", "loop")
		plainStatus(w, http.StatusBadRequest, "forwarded push loop refused")
		return
	}

	var fwd bool
	if s.cfg.WAL.PushBrokerURL != "" {
		pl, err := s.engine.Placement(r.Context(), id)
		fwd = err == nil && !pl.Serve && !pl.Maintain
	}
	if fwd {
		s.receivePackForward(w, r, id, p)
		return
	}
	s.receivePackLocal(w, r, id, p)
}

// receivePackForward buffers the body (bounded) and forwards; broker down →
// local fallback (only when buffered).
func (s *Server) receivePackForward(w http.ResponseWriter, r *http.Request, id git.RepoId, p auth.Principal) {
	_, bufMax := s.pushBroker()
	buffered, err := io.ReadAll(io.LimitReader(r.Body, bufMax))
	if err != nil || int64(len(buffered)) > bufMax {
		s.metrics.Counter("walgit_push_refused_total", "pushes refused").Inc("reason", "spill")
		plainStatus(w, http.StatusRequestEntityTooLarge,
			"push too large to forward (spill-to-deny above "+strconv.FormatInt(bufMax, 10)+" bytes)")
		return
	}
	resp, ferr := s.forwardToBroker(r.Context(), r, buffered, p.Name)
	if ferr == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		gitHeaders(w, "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if ferr == nil {
		resp.Body.Close()
	}
	// Broker down → local fallback (we buffered, so replay works).
	s.log.Warn("push broker down; local fallback", "repo", id.String(), "err", ferr)
	s.receivePackLocal(w, r, id, p)
}

// receivePackLocal runs the git-layer push pipeline (§4): parse, ingest
// (bounded), connectivity, then the engine publish (WAL commit).
func (s *Server) receivePackLocal(w http.ResponseWriter, r *http.Request, id git.RepoId, p auth.Principal) {
	body, ok := s.bodyReader(w, r)
	if !ok {
		return
	}
	all, err := io.ReadAll(body)
	if err != nil {
		plainStatus(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if int64(len(all)) > int64(s.cfg.Server.MaxPushBytes) {
		s.metrics.Counter("walgit_push_refused_total", "pushes refused").Inc("reason", "too_large")
		plainStatus(w, http.StatusRequestEntityTooLarge, "push exceeds max_push_bytes")
		return
	}
	create := s.cfg.Server.AutoCreateOnPush || s.engine.AutoCreate(r.Context(), id)
	repo, rerr := s.engine.Repo(r.Context(), id, create, git.Sha1)
	if rerr != nil {
		if isNotFound(rerr) {
			plainStatus(w, http.StatusNotFound, "repository not found")
			return
		}
		plainStatus(w, http.StatusServiceUnavailable, rerr.Error())
		return
	}
	req, perr := s.layer.ParsePushRequest(repo, all)
	if perr != nil {
		plainStatus(w, http.StatusBadRequest, "malformed push request: "+perr.Error())
		return
	}
	// ParsePushRequest consumed the command section; the remainder is the
	// raw pack (req.Pack, possibly empty for deletes).
	if len(req.Pack) > 0 {
		if _, ierr := s.layer.Ingest(r.Context(), repo, bytes.NewReader(req.Pack),
			int64(s.cfg.Server.MaxPushBytes), req.Has("thin-pack"), true); ierr != nil {
			gitHeaders(w, "application/x-git-receive-pack-result")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(git.Band2("pack rejected: " + ierr.Error()))
			return
		}
	}
	tips := make([]git.Oid, 0, len(req.Commands))
	for _, c := range req.Commands {
		if c.New != repo.ZeroOid() && !isZeroOidStr(c.New) {
			tips = append(tips, c.New)
		}
	}
	if len(tips) > 0 {
		if cerr := s.layer.CheckConnectivity(r.Context(), repo, tips); cerr != nil {
			gitHeaders(w, "application/x-git-receive-pack-result")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(git.Band2("connectivity check failed: " + cerr.Error()))
			return
		}
	}
	res, pubErr := s.engine.Publish(r.Context(), id, req, p.Name, wal.ObjectAccess{Local: repo})
	gitHeaders(w, "application/x-git-receive-pack-result")
	w.WriteHeader(http.StatusOK)
	if pubErr != nil {
		_, _ = w.Write(git.ErrPkt("walgit: publish failed: " + pubErr.Error()))
		return
	}
	report := git.Report{UnpackOK: true, Sideband: req.Has("side-band-64k")}
	for _, rr := range res.PerRef {
		if rr.Err != nil {
			report.Refs = append(report.Refs, git.RefReport{Ref: rr.Name, OK: false, Reason: rr.Err.Error()})
		} else {
			report.Refs = append(report.Refs, git.RefReport{Ref: rr.Name, OK: true})
		}
	}
	_, _ = w.Write(report.EncodeReport())
}

func isZeroOidStr(s string) bool {
	for i := range s {
		if s[i] != '0' {
			return false
		}
	}
	return len(s) > 0
}

// isNotFound maps engine errors onto 404.
func isNotFound(err error) bool {
	var we *wal.WalError
	if errorsAs(err, &we) {
		return we.Kind == wal.WalErrNotFound
	}
	return false
}

func errorsAs(err error, target **wal.WalError) bool {
	for err != nil {
		if e, ok := err.(*wal.WalError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// bundlesDispatch answers the bundle routes (§3.3): list/catchup (JSON of the
// git-config bundle list) and /bundles/{strategy}/{name} (the bundle object,
// full static contract).
func (s *Server) bundlesDispatch(w http.ResponseWriter, r *http.Request, id git.RepoId, rest []string, hadGit bool) {
	if len(rest) == 0 {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.gitAuthFailure(w, r, git.ServiceUploadPack, aerr)
		return
	}
	if aerr := requireRead(p, s.cfg.Server.Auth.AnonymousRead); aerr != nil {
		s.gitAuthFailure(w, r, git.ServiceUploadPack, aerr)
		return
	}
	switch {
	case rest[0] == "list" || rest[0] == "catchup":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			plainStatus(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		filter := r.URL.Query().Get("filter")
		if filter != "" && filter != "blob:none" {
			plainStatus(w, http.StatusBadRequest, "only ?filter=blob:none is accepted")
			return
		}
		if rel := s.sem.TryAcquire(id.String()); rel == nil {
			w.Header().Set("Retry-After", "15")
			plainStatus(w, http.StatusServiceUnavailable, "repository busy")
			return
		} else {
			defer rel()
		}
		bl, err := s.engine.Bundles(r.Context(), id, filter)
		if err != nil {
			if isNotFound(err) {
				plainStatus(w, http.StatusNotFound, "repository not found")
				return
			}
			plainStatus(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		entries := bl.Fulls
		if rest[0] == "catchup" {
			entries = bl.Chain // same list without the fulls
		}
		// Record the principal for D17 (bundle-require bookkeeping lives in
		// the engine's upload path).
		_ = p.Name
		gitHeaders(w, "application/json")
		writeJSONBody(w, http.StatusOK, map[string]any{
			"repo":     id.String(),
			"filter":   filter,
			"bundles":  entries,
			"recorded": time.Now().UTC().Format(time.RFC3339),
		})
	case len(rest) == 2:
		s.serveBundleObject(w, r, id, BundleEntry{Strategy: rest[0], Name: rest[1]})
	default:
		plainStatus(w, http.StatusNotFound, "not found")
	}
}

// serveBundleObject streams one bundle object through the §5 static contract.
func (s *Server) serveBundleObject(w http.ResponseWriter, r *http.Request, id git.RepoId, e BundleEntry) {
	key := bundleKey(id, e)
	s.serveStatic(w, r, key, "application/x-git-bundle")
}
