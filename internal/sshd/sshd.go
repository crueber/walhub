// Package sshd serves git over SSH (17_ssh.md): the standard transport every
// git client speaks — `git-upload-pack` for clone/fetch (out) and
// `git-receive-pack` for push (in). Built directly on golang.org/x/crypto/ssh
// (the one Law-1 amendment; hand-rolling SSH is not on the table), the same
// primitive Gitea and Forgejo build on.
//
// The session handler parses the client's command string and dispatches to a
// Transport implemented by internal/server, so SSH reuses the exact same git
// pipeline as HTTP: sync → upload-pack for fetches, parse → ingest →
// connectivity → publish → report for pushes.
//
// # Concurrency
//
// Hazard: every connection is its own goroutine and every exec streams into a
// git subprocess — unbounded sessions exhaust CPU/memory exactly like
// unbounded HTTP git requests would.
//
// Avoidance: one goroutine per connection and per session channel, all bound
// to a context canceled on disconnect or shutdown; a semaphore caps
// concurrent exec sessions (Config.MaxSessions); x/crypto's channel windowing
// bounds output buffering so a slow client cannot balloon server memory; the
// heavy git execs run inside the git layer's blocking pool exactly as on the
// HTTP path.
package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"git.packden.us/crueber/walhub/internal/git"
	gossh "golang.org/x/crypto/ssh"
)

// Sentinel errors a Transport may wrap; the session handler turns them into
// stderr text and exit codes.
var (
	ErrNotFound    = errors.New("repository not found")
	ErrUnavailable = errors.New("service unavailable")
)

// Principal is the authenticated identity for one connection, resolved from
// the configured public key.
type Principal struct {
	Name  string
	Write bool
	Admin bool
}

// Transport is the server-side git pipeline, implemented by internal/server.
// It carries no HTTP types: both transports meet here.
type Transport interface {
	// SSHUploadPack serves clone/fetch; protocol is "version=2" or "" (v0),
	// forwarded from the client's GIT_PROTOCOL env.
	SSHUploadPack(ctx context.Context, id git.RepoId, protocol string, stdin io.Reader, stdout, stderr io.Writer) error
	// SSHReceivePack serves push; implementations enforce placement, drain,
	// and max_push_bytes exactly like the HTTP route.
	SSHReceivePack(ctx context.Context, id git.RepoId, principal string, stdin io.Reader, stdout, stderr io.Writer) error
}

// KeyEntry is the resolved identity for one public key: the principal and
// the flags its credential carries.
type KeyEntry struct {
	Principal string
	Write     bool
	Admin     bool
}

// Config is the listener construction input.
type Config struct {
	Listen string
	// HostKey is private-key PEM bytes. When nil, HostKeyPath is loaded; when
	// that file does not exist, a fresh ed25519 key is generated and persisted
	// there (auto-generation keeps zero-config boots whole).
	HostKey     []byte
	HostKeyPath string
	// KeyLookup resolves a public-key fingerprint to its principal at auth
	// time — keys are user-managed in the object store (17_ssh.md §3), so the
	// lookup is one store GET behind this callback, not config.
	KeyLookup func(ctx context.Context, fingerprint string) (KeyEntry, error)
	// MaxSessions bounds concurrent git exec sessions (0 = 64).
	MaxSessions int
	Log         *slog.Logger
}

// Server is the SSH listener. Construct with New; run with ListenAndServe;
// stop by canceling the context.
type Server struct {
	cfg      Config
	tr       Transport
	log      *slog.Logger
	signer   gossh.Signer
	sessions chan struct{}
	maxSess  int

	mu    sync.Mutex
	ln    net.Listener
	conns map[gossh.Conn]struct{}
	live  map[*gossh.ServerConn]int
}

// New validates the construction input; the host key is resolved here (a
// misconfigured key fails at boot), while key lookup stays per-connection
// (it needs the connection context and hits the object store).
func New(cfg Config, tr Transport) (*Server, error) {
	if cfg.KeyLookup == nil {
		return nil, fmt.Errorf("sshd: KeyLookup is required")
	}
	s := &Server{
		cfg:     cfg,
		tr:      tr,
		log:     cfg.Log,
		conns:   map[gossh.Conn]struct{}{},
		live:    map[*gossh.ServerConn]int{},
		maxSess: cfg.MaxSessions,
	}
	if s.maxSess <= 0 {
		s.maxSess = 64
	}
	s.sessions = make(chan struct{}, s.maxSess)
	if s.log == nil {
		s.log = slog.Default()
	}
	signer, err := hostSigner(cfg)
	if err != nil {
		return nil, err
	}
	s.signer = signer
	return s, nil
}

// Addr reports the listen address (for tests binding :0).
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// ListenAndServe blocks until the context is canceled or the listener fails.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.cfg.KeyLookup == nil {
		return fmt.Errorf("sshd: KeyLookup is required")
	}
	sshCfg := &gossh.ServerConfig{
		PublicKeyCallback: func(meta gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			// The key's principal and flags come from the store-backed registry
			// (17_ssh.md §3): one lookup by fingerprint. An unknown key ends the
			// handshake with a clean permission-denied.
			fp := gossh.FingerprintSHA256(key)
			entry, err := s.cfg.KeyLookup(ctx, fp)
			if err != nil || entry.Principal == "" {
				s.log.Warn("ssh key refused", "fingerprint", fp, "remote", meta.RemoteAddr())
				return nil, fmt.Errorf("walhub: unknown public key %s", fp)
			}
			ext := map[string]string{"principal": entry.Principal}
			if entry.Write {
				ext["write"] = "1"
			}
			if entry.Admin {
				ext["admin"] = "1"
			}
			s.log.Info("ssh key accepted", "principal", entry.Principal, "fingerprint", fp, "remote", meta.RemoteAddr())
			return &gossh.Permissions{Extensions: ext}, nil
		},
	}
	sshCfg.AddHostKey(s.signer)

	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("ssh listen %s: %w", s.cfg.Listen, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		s.mu.Lock()
		ln.Close()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	}()

	s.log.Info("ssh listening", "addr", s.cfg.Listen)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ssh accept: %w", err)
		}
		go s.handleConn(ctx, sshCfg, c)
	}
}

// handleConn runs one SSH connection: handshake with public-key auth, then
// serve session channels until the client disconnects.
func (s *Server) handleConn(ctx context.Context, sshCfg *gossh.ServerConfig, c net.Conn) {
	sconn, chans, reqs, err := gossh.NewServerConn(c, sshCfg)
	if err != nil {
		s.log.Debug("ssh handshake failed", "remote", c.RemoteAddr(), "err", err)
		return
	}
	s.mu.Lock()
	s.conns[sconn] = struct{}{}
	s.live[sconn] = 0
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, sconn)
		delete(s.live, sconn)
		s.mu.Unlock()
		sconn.Close()
	}()

	go gossh.DiscardRequests(reqs) // no global requests: keepalive answers are automatic
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			// No tcpip forwarding, no sftp subsystem channel, nothing else.
			_ = newCh.Reject(gossh.UnknownChannelType, "walhub serves git only")
			continue
		}
		go s.handleSession(ctx, sconn, newCh)
	}
}

// maxSessionsPerConn bounds live session channels on one connection: an
// authenticated principal must not farm parked goroutines via never-exec'd
// channels (17_ssh.md §6).
const maxSessionsPerConn = 16

// handleSession runs one session channel: exec requests only. PTY, shell,
// and subsystem requests are refused — this host runs git, not shells.
func (s *Server) handleSession(ctx context.Context, sconn *gossh.ServerConn, newCh gossh.NewChannel) {
	// The cap is per connection, checked under the same lock that increments:
	// a server-wide pre-check here would serialize unrelated connections.
	s.mu.Lock()
	s.live[sconn]++
	if s.live[sconn] > maxSessionsPerConn {
		s.live[sconn]--
		s.mu.Unlock()
		_ = newCh.Reject(gossh.Prohibited, "walhub: too many sessions on this connection")
		return
	}
	s.mu.Unlock()
	ch, reqs, err := newCh.Accept()
	if err != nil {
		s.mu.Lock()
		s.live[sconn]--
		s.mu.Unlock()
		return
	}
	defer func() {
		s.mu.Lock()
		s.live[sconn]--
		s.mu.Unlock()
	}()
	defer ch.Close()

	var principal Principal
	if p, ok := sconn.Permissions.Extensions["principal"]; ok {
		principal.Name = p
	}
	perms := sconn.Permissions
	principal.Write = perms.Extensions["write"] == "1"
	principal.Admin = perms.Extensions["admin"] == "1"

	for req := range reqs {
		switch req.Type {
		case "env":
			// Accept only GIT_PROTOCOL (protocol v2 negotiation); drop the rest.
			var env struct{ Name, Value string }
			if err := gossh.Unmarshal(req.Payload, &env); err == nil && env.Name == "GIT_PROTOCOL" {
				ctx = withGitProtocol(ctx, env.Value)
			}
			req.Reply(true, nil)
		case "exec":
			var payload struct{ Command string }
			if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			s.exec(ctx, principal, ch, payload.Command)
			return // one command per session, then the channel closes
		case "pty-req", "shell", "subsystem":
			fmt.Fprint(ch.Stderr(), "walhub: interactive shells and subsystems are not served\r\n")
			_, _ = ch.SendRequest("exit-status", false, gossh.Marshal(struct{ Code uint32 }{1}))
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

// exec parses and dispatches one git command, then reports the exit status.
func (s *Server) exec(ctx context.Context, p Principal, ch gossh.Channel, command string) {
	exit := func(code int) {
		_, _ = ch.SendRequest("exit-status", false, gossh.Marshal(struct{ Code uint32 }{uint32(code)}))
	}
	verb, id, err := ParseGitCommand(command)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "walhub: %v\r\n", err)
		exit(1)
		return
	}
	select {
	case s.sessions <- struct{}{}:
		defer func() { <-s.sessions }()
	default:
		fmt.Fprint(ch.Stderr(), "walhub: too many concurrent sessions\r\n")
		exit(1)
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var terr error
	switch verb {
	case "git-upload-pack":
		terr = s.tr.SSHUploadPack(runCtx, id, gitProtocol(ctx), ch, ch, ch.Stderr())
	case "git-receive-pack":
		if !p.Write {
			fmt.Fprint(ch.Stderr(), "walhub: write access required to push\r\n")
			exit(1)
			return
		}
		terr = s.tr.SSHReceivePack(runCtx, id, p.Name, ch, ch, ch.Stderr())
	}
	if terr != nil {
		if runCtx.Err() != nil {
			exit(1) // client went away; nothing to report
			return
		}
		fmt.Fprintf(ch.Stderr(), "walhub: %v\r\n", terr)
		exit(1)
		return
	}
	exit(0)
}
