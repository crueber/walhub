package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	exec "os/exec"
)

// Upload-pack (04_git.md §8): stock git, byte-for-byte pass-through after
// guard parsing. v0 advertisement and ls-refs are ours (advert.go); fetch is
// delegated.

// UploadPackErrRefusal is a pkt-line refusal (ERR … + flush, HTTP 503 at the
// HTTP layer).
type Refusal struct {
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// ErrPkt renders a refusal as `ERR <msg>` + flush.
func ErrPkt(msg string) []byte {
	var b bytes.Buffer
	b.Write(Pkt("ERR " + msg))
	b.Write(Flush())
	return b.Bytes()
}

// UploadPack runs stock git upload-pack for the negotiated protocol version:
//
//	git -c uploadpack.allowSidebandAll=true upload-pack --stateless-rpc .
//
// with GIT_DIR=<repo>, GIT_PROTOCOL=version=2|0, GIT_TERMINAL_PROMPT=0, on
// Pool.Run. The body is passed through byte-for-byte — walhub never re-encodes
// client pkt-lines. stderr on non-zero exit → Subprocess error.
func (l *Layer) UploadPack(ctx context.Context, repo *LocalRepo, body io.Reader, out io.Writer, protocol string) error {
	protocolVersion := "version=0"
	if protocol == "2" || strings.Contains(protocol, "version=2") {
		protocolVersion = "version=2"
	}
	argv := []string{"-c", "uploadpack.allowSidebandAll=true", "upload-pack", "--stateless-rpc", "."}
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	return l.Pool.Run(uploadCtx, func() error {
		// upload-pack lifetime = request lifetime: the request context cancels
		// it. No internal timeout beyond server.request_timeout (doc 06).
		stderr, err := l.runPiped(uploadCtx, repo, argv, protocolVersion, body, out)
		if err != nil {
			return err
		}
		_ = stderr
		return nil
	})
}

// runPiped is the §2 discipline for upload-pack: feeder goroutine streams the
// body into stdin and closes; stdout is copied to the consumer as it arrives;
// stderr drains to a bounded buffer; then Wait.
func (l *Layer) runPiped(ctx context.Context, repo *LocalRepo, argv []string, gitProtocol string, body io.Reader, out io.Writer) (string, error) {
	cmd := exec.CommandContext(ctx, l.Binary, argv...)
	cmd.Dir = repo.Path
	cmd.Env = l.baseEnv(repo.Path, "GIT_PROTOCOL="+gitProtocol)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderrBuf := newBounded(8 << 10)
	cmd.Stderr = stderrBuf
	if err := cmd.Start(); err != nil {
		return stderrBuf.String(), err
	}
	// stdin: feeder goroutine closes at EOF (close errors swallowed — the
	// child may have exited).
	go func() {
		defer stdin.Close() //nolint:errcheck
		_, _ = io.Copy(stdin, body)
	}()
	// stdout: copy to the client as it arrives (git's own band framing
	// provides flush boundaries; no re-buffering).
	_, copyErr := io.Copy(out, stdout)
	waitErr := cmd.Wait()
	if ctxErr(ctx) {
		return stderrBuf.String(), &GitError{Kind: GitErrIo, Detail: "upload-pack cancelled (client disconnect)"}
	}
	if copyErr != nil && waitErr == nil {
		return stderrBuf.String(), copyErr
	}
	if waitErr != nil {
		return stderrBuf.String(), errSubprocess(argv, stderrBuf.String())
	}
	return stderrBuf.String(), nil
}

// Guards parse the v2 fetch request (or v0 want lines) BEFORE spawning.
// The request is then passed through unchanged.

// FetchGuards is what the guards read out of a request.
type FetchGuards struct {
	Wants   []Oid
	Haves   []Oid
	Deepen  bool // any deepen/deepen-since/deepen-not
	Filter  bool
	Command string // "fetch" (v2) or "" (v0)
}

// ParseFetchGuards counts/extracts guards from a raw request body (v0 or v2);
// it never mutates the request.
func ParseFetchGuards(body []byte) (*FetchGuards, error) {
	g := &FetchGuards{}
	pr := NewPktReader(bytes.NewReader(body))
	inCommand := false
	for {
		p, kind, err := pr.Next()
		if err != nil {
			if err == io.EOF {
				return g, nil
			}
			return nil, &GitError{Kind: GitErrProtocol, Detail: "fetch request: " + err.Error()}
		}
		if kind == PktKindFlush {
			inCommand = true // v2: args section after flush
			continue
		}
		if kind != PktKindData || len(p) == 0 {
			continue
		}
		line := strings.TrimRight(string(p), "\n")
		switch {
		case strings.HasPrefix(line, "command="):
			g.Command = strings.TrimPrefix(line, "command=")
		case strings.HasPrefix(line, "want "):
			g.Wants = append(g.Wants, Oid(strings.TrimPrefix(line, "want ")))
		case strings.HasPrefix(line, "have "):
			g.Haves = append(g.Haves, Oid(strings.TrimPrefix(line, "have ")))
		case line == "thin-pack", line == "ofs-delta", line == "no-progress",
			line == "include-tag", line == "sideband-all", line == "wait-for-done",
			strings.HasPrefix(line, "shallow "), strings.HasPrefix(line, "want-ref "),
			strings.HasPrefix(line, "have"):
			// tolerated, no guard state
		case strings.HasPrefix(line, "deepen"), line == "deepen-since", line == "deepen-not",
			line == "deepen-relative":
			g.Deepen = true
		case strings.HasPrefix(line, "filter"):
			g.Filter = true
		}
		_ = inCommand
	}
}

// max_wants guard (§8.2): when git.max_wants > 0, exceeding the cap → pkt ERR,
// no git spawn.
func (l *Layer) CheckMaxWants(g *FetchGuards) error {
	if l.MaxWants > 0 && len(g.Wants) > l.MaxWants {
		return &TooManyWantsError{Cap: l.MaxWants}
	}
	return nil
}

// CheckBundleRequire implements D17 (§8.2): an unbounded zero-have fetch (no
// deepen*, no filter, no have) of a repo listed in bundles.require is refused
// with the exact fix in the error text. Bounded zero-have fetches (CI
// --depth/--filter) and all fetches with haves proceed.
func (l *Layer) CheckBundleRequire(repo *LocalRepo, principal string, g *FetchGuards) error {
	if !containsRepo(l.BundlesRequire, repo.ID.String()) {
		return nil
	}
	if len(g.Haves) > 0 || g.Deepen || g.Filter {
		return nil
	}
	// One-shot fallback: a principal that fetched bundles/list within the last
	// hour demonstrably tried bundle-uri → ONE upload-pack full clone per 6 h.
	if l.BundleLedger != nil && principal != "" {
		if l.BundleLedger.CanFallback(principal, repo.ID.String()) {
			l.BundleLedger.RecordFallback(principal, repo.ID.String())
			return nil
		}
	}
	return &Refusal{Message: fmt.Sprintf(
		"ERR walgit: %s requires bundle-uri clones; use bundle-uri (pass -c transfer.bundleURI=false for shallow/CI fetches)",
		repo.ID.String())}
}

func containsRepo(list []string, name string) bool {
	for _, v := range list {
		if v == name {
			return true
		}
	}
	return false
}

// RemoteServedRefusal is the §8.1 refusal for a remote-served base (no store
// mount): pkt ERR + HTTP 503/Retry-After at the HTTP layer.
func RemoteServedRefusal(repo *LocalRepo) *Refusal {
	return &Refusal{Message: fmt.Sprintf(
		"walgit: %s is served through a remote base that is not mounted on this host; fetch from the serving host, or set cache.store_mount so the base is local (this walhub build has no remote-reader fetch engine)",
		repo.ID.String())}
}
