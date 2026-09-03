package server

// bind_ssh.go — the SSH transport seam (17_ssh.md). The server implements
// internal/sshd.Transport so SSH sessions reuse the exact git pipeline the
// HTTP handlers run: sync → upload-pack for fetches, parse → ingest →
// connectivity → publish → report for pushes. Every gate the HTTP route
// applies (placement §4.3, drain §12, max_push_bytes) applies here too.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/sshd"
	"git.packden.us/crueber/walhub/internal/wal"
)

// Pre-pipeline push failures, shared by both transports. The SSH session
// handler maps them to stderr text; the HTTP handler maps them to statuses.
var (
	errPushNotFound    = sshd.ErrNotFound
	errPushUnavailable = sshd.ErrUnavailable
	errPushTooLarge    = errors.New("push exceeds max_push_bytes")
)

// sshPlacement applies the §4.3 placement gate: a repo not served here is
// refused before any sync work (HTTP answers 503; SSH reports unavailable).
func (s *Server) sshPlacement(ctx context.Context, id git.RepoId) error {
	pl, err := s.engine.Placement(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: %v", errPushUnavailable, err)
	}
	if !pl.Serve && !pl.Maintain {
		return fmt.Errorf("%w: %s is served by %s; retry shortly",
			errPushUnavailable, id.String(), pl.ServedBy)
	}
	return nil
}

// SSHUploadPack implements sshd.Transport: gates → sync → open → the git
// layer's upload-pack streaming (protocol v0 or v2 via GIT_PROTOCOL).
func (s *Server) SSHUploadPack(ctx context.Context, id git.RepoId, protocol string, stdin io.Reader, stdout, stderr io.Writer) error {
	if s.drain.Draining() {
		return fmt.Errorf("%w: draining; retry shortly", errPushUnavailable)
	}
	if err := s.sshPlacement(ctx, id); err != nil {
		return err
	}
	repo, err := s.uploadPackPrepare(ctx, id)
	if err != nil {
		return err
	}
	return s.layer.UploadPackSSH(ctx, repo, stdin, stdout, protocol)
}

// SSHReceivePack implements sshd.Transport: gates → streaming parse of the
// command section → IngestStream (the pack is consumed while the client
// waits) → connectivity → publish → report. Mirrors receivePackLocal (§3.3)
// without body framing: git send-pack never closes its side before the
// report, so nothing here reads the channel to EOF.
func (s *Server) SSHReceivePack(ctx context.Context, id git.RepoId, principal string, stdin io.Reader, stdout, stderr io.Writer) error {
	if s.drain.Draining() {
		return fmt.Errorf("%w: draining; retry shortly", errPushUnavailable)
	}
	if err := s.sshPlacement(ctx, id); err != nil {
		return err
	}
	p := auth.Principal{Name: principal}

	create := s.cfg.Server.AutoCreateOnPush || s.engine.AutoCreate(ctx, id)
	repo, rerr := s.engine.Repo(ctx, id, create, git.Sha1)
	if rerr != nil {
		if isNotFound(rerr) {
			return fmt.Errorf("%w: %s", errPushNotFound, id.String())
		}
		return fmt.Errorf("%w: %v", errPushUnavailable, rerr)
	}

	// Over SSH, receive-pack OPENS with the server's ref advertisement: the
	// client sends nothing until it has read it (the hang a missing
	// advertisement produces is the classic mistake). v2 does not cover push,
	// so the advertisement is always v0. Auto-created repos advertise an
	// empty ref list, which is what makes the push create them.
	advert, aerr := s.layer.Advertisement(repo, git.ServiceReceivePack, false, s.Version())
	if aerr != nil {
		return fmt.Errorf("%w: %v", errPushUnavailable, aerr)
	}
	s.log.Debug("ssh receive: advertisement sent", "repo", id.String(), "bytes", len(advert))
	if _, werr := stdout.Write(advert); werr != nil {
		return fmt.Errorf("client went away: %v", werr)
	}
	s.log.Debug("ssh receive: advertisement on the wire", "repo", id.String())

	// The limit caps the whole request (commands + pack); the pack itself is
	// re-capped inside IngestStream.
	lr := io.LimitReader(stdin, int64(s.cfg.Server.MaxPushBytes)+1)

	req, packReader, perr := s.layer.ParsePushRequestStream(repo, lr)
	if perr != nil {
		return fmt.Errorf("malformed push request: %w", perr)
	}
	// A pure-delete push sends no pack bytes at all: the client waits for the
	// report with the channel open, so the reader must never be touched.
	allDeletes := true
	for _, c := range req.Commands {
		if !isZeroOidStr(c.New) {
			allDeletes = false
			break
		}
	}
	var pack io.Reader
	if !allDeletes {
		pack = packReader
	}
	return s.pushPipeline(ctx, id, p, repo, req, pack, stdout)
}

// uploadPackPrepare resolves a repo to Serve level for upload-pack, mapping
// failures onto the shared sentinels. HTTP maps them to 404/503.
func (s *Server) uploadPackPrepare(ctx context.Context, id git.RepoId) (*git.LocalRepo, error) {
	if err := s.engine.Sync(ctx, id, wal.LevelServe); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", errPushNotFound, id.String())
		}
		return nil, fmt.Errorf("%w: %v", errPushUnavailable, err)
	}
	repo, err := s.engine.Repo(ctx, id, false, git.Sha1)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", errPushNotFound, id.String())
		}
		return nil, fmt.Errorf("%w: %v", errPushUnavailable, err)
	}
	return repo, nil
}

// pushPipeline runs the §4 push stages over a parsed request: ingest the
// pack, check connectivity, publish to the WAL, and write the report-status
// result to out. Mid-pipeline refusals are git-wire responses (nil error);
// HTTP (receivePackLocal §3.3) and SSH (§17) both land here.
func (s *Server) pushPipeline(ctx context.Context, id git.RepoId, p auth.Principal, repo *git.LocalRepo, req *git.PushRequest, pack io.Reader, out io.Writer) error {
	if pack != nil {
		if _, ierr := s.layer.IngestStream(ctx, repo, pack,
			int64(s.cfg.Server.MaxPushBytes), req.Has("thin-pack"), true); ierr != nil {
			band2Failure(out, req, "pack rejected: "+ierr.Error())
			return nil
		}
	}
	tips := make([]git.Oid, 0, len(req.Commands))
	for _, c := range req.Commands {
		if c.New != repo.ZeroOid() && !isZeroOidStr(c.New) {
			tips = append(tips, c.New)
		}
	}
	if len(tips) > 0 {
		if cerr := s.layer.CheckConnectivity(ctx, repo, tips); cerr != nil {
			band2Failure(out, req, "connectivity check failed: "+cerr.Error())
			return nil
		}
	}
	res, pubErr := s.engine.Publish(ctx, id, req, p.Name, wal.ObjectAccess{Local: repo})
	if pubErr != nil {
		_, _ = out.Write(git.ErrPkt("walgit: publish failed: " + pubErr.Error()))
		return nil
	}
	report := git.Report{UnpackOK: true, Sideband: req.Has("side-band-64k")}
	for _, rr := range res.PerRef {
		if rr.Err != nil {
			report.Refs = append(report.Refs, git.RefReport{Ref: rr.Name, OK: false, Reason: rr.Err.Error()})
		} else {
			report.Refs = append(report.Refs, git.RefReport{Ref: rr.Name, OK: true})
		}
	}
	_, _ = out.Write(report.EncodeReport())
	return nil
}

// gitResultWriter emits the §4.1 git headers and the 200 status on the first
// wire byte, then passes through. The shared push core stays transport-
// agnostic while HTTP responses keep their exact header ordering.
type gitResultWriter struct {
	w    http.ResponseWriter
	once sync.Once
}

func (g *gitResultWriter) Write(p []byte) (int, error) {
	g.once.Do(func() {
		gitHeaders(g.w, "application/x-git-receive-pack-result")
		g.w.WriteHeader(http.StatusOK)
	})
	return g.w.Write(p)
}

// SSH builds the sshd.Server from [server.ssh] config. Returns (nil, nil)
// when disabled (listen empty) or in setup-only mode (no engine). Host key
// resolution order: host_key_env → host_key path → auto-generated ed25519
// under <data-dir>/ssh/ (persisted, so clients can pin it). Errors are
// boot-fatal: a configured SSH that cannot come up must not boot silently.
func (s *Server) SSH() (*sshd.Server, error) {
	sc := s.cfg.Server.SSH
	if sc.Listen == "" || s.engine == nil {
		return nil, nil
	}
	keys := make([]sshd.KeyEntry, 0, len(sc.Keys))
	for _, k := range sc.Keys {
		line := k.Key
		if k.KeyEnv != "" {
			line = os.Getenv(k.KeyEnv)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return nil, fmt.Errorf("server.ssh.keys[%q]: no key material (key or a set key_env is required)", k.Principal)
		}
		keys = append(keys, sshd.KeyEntry{Principal: k.Principal, Write: k.Write, Admin: k.Admin, Line: line})
	}
	cfg := sshd.Config{Listen: sc.Listen, Keys: keys, Log: s.log}
	if sc.HostKeyEnv != "" {
		raw := os.Getenv(sc.HostKeyEnv)
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("server.ssh.host_key_env %q is not set", sc.HostKeyEnv)
		}
		cfg.HostKey = []byte(raw)
	} else {
		cfg.HostKeyPath = sc.HostKey
		if cfg.HostKeyPath == "" {
			cfg.HostKeyPath = filepath.Join(s.dataDir, "ssh", "ed25519_host_key")
		}
	}
	return sshd.New(cfg, s)
}
