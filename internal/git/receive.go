package git

import (
	"bufio"

	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	exec "os/exec"
)

// Receive-pack server flow (04_git.md §7): parse request, ingest, connectivity,
// report status. Publish/policy belong to doc 05/14; this file provides the
// parsing, guards, and report emission they consume.

// PushCommand is one `<old> <new> <ref>` command from the request.
type PushCommand struct {
	Old, New, Ref string
}

// PushRequest is the parsed receive-pack request (04_git.md §7 step 1).
type PushRequest struct {
	Commands    []PushCommand
	Caps        []string
	PushOptions []string
	Pack        []byte // raw pack bytes; empty for a pure delete push
	CapsRaw     string // first-line capabilities after NUL
}

// CapsSet is a lookup helper for the parsed capabilities.
func (r *PushRequest) Has(cap string) bool {
	for _, c := range r.Caps {
		if c == cap {
			return true
		}
	}
	return false
}

// ParsePushRequest parses the full body: first pkt-line `<old> <new>
// <ref>\0<caps>`, then further command lines until flush; push-option lines
// follow when `push-options` was requested; the REMAINDER is raw pack bytes.
// object-format mismatch → protocol error (refuse).
func (l *Layer) ParsePushRequest(repo *LocalRepo, body []byte) (*PushRequest, error) {
	pr := NewPktReader(bytes.NewReader(body))
	req, err := l.parsePushCommands(repo, pr)
	if err != nil {
		return nil, err
	}
	// The remainder of the body is the raw pack (may be absent for deletes).
	rest, err := io.ReadAll(pr.r)
	if err != nil {
		return nil, &GitError{Kind: GitErrProtocol, Detail: "pack read: " + err.Error()}
	}
	req.Pack = rest
	return req, nil
}

// ParsePushRequestStream parses the command (and push-options) sections from
// a stream and leaves the reader positioned at the pack start — for
// transports without body framing (SSH receive-pack, 17_ssh.md §5): the
// caller streams the remaining bytes through IngestStream. The returned
// request has Pack == nil; a pure-delete push has no pack bytes at all (the
// client waits for the report without closing its side).
func (l *Layer) ParsePushRequestStream(repo *LocalRepo, r io.Reader) (*PushRequest, io.Reader, error) {
	pr := NewPktReader(r)
	req, err := l.parsePushCommands(repo, pr)
	if err != nil {
		return nil, nil, err
	}
	return req, pr.r, nil
}

// parsePushCommands reads the update commands, capabilities, and push options.
func (l *Layer) parsePushCommands(repo *LocalRepo, pr *PktReader) (*PushRequest, error) {
	req := &PushRequest{}
	first := true
	for {
		p, kind, err := pr.Next()
		if err != nil {
			if errors.Is(err, ErrMaxBytes) {
				return nil, err // the request cap is a refusal, not a protocol fault
			}
			return nil, &GitError{Kind: GitErrProtocol, Detail: "receive-pack request: " + err.Error()}
		}
		if kind == PktKindFlush {
			break
		}
		if kind != PktKindData || len(p) == 0 {
			continue
		}
		line, caps := FirstNul(p)
		text := strings.TrimRight(string(line), "\n")
		if first {
			first = false
			req.CapsRaw = strings.TrimRight(string(caps), "\n")
			req.Caps = strings.Fields(req.CapsRaw)
			if err := l.checkObjectFormat(repo, req.Caps); err != nil {
				return nil, err
			}
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			return nil, &GitError{Kind: GitErrProtocol, Detail: fmt.Sprintf("bad push command line %q", text)}
		}
		if err := ValidateRefName(fields[2]); err != nil {
			return nil, &GitError{Kind: GitErrInvalidInput, Detail: err.Error()}
		}
		req.Commands = append(req.Commands, PushCommand{Old: fields[0], New: fields[1], Ref: fields[2]})
	}

	if req.Has("push-options") {
		for {
			p, kind, err := pr.Next()
			if err != nil {
				if errors.Is(err, ErrMaxBytes) {
					return nil, err // the request cap is a refusal, not a protocol fault
				}
				return nil, &GitError{Kind: GitErrProtocol, Detail: "push-options: " + err.Error()}
			}
			if kind == PktKindFlush {
				break
			}
			if kind == PktKindData {
				req.PushOptions = append(req.PushOptions, strings.TrimRight(string(p), "\n"))
			}
		}
	}
	return req, nil
}

// checkObjectFormat validates the client's object-format cap against the repo
// format; mismatch → refuse.
func (l *Layer) checkObjectFormat(repo *LocalRepo, caps []string) error {
	for _, c := range caps {
		if v, ok := strings.CutPrefix(c, "object-format="); ok && v != repo.Format().String() {
			return &GitError{Kind: GitErrProtocol,
				Detail: fmt.Sprintf("object-format %s does not match repository format %s", v, repo.Format())}
		}
	}
	return nil
}

// ParseShallow returns the `shallow <oid>` lines from a request body
// (collected, NOT enforced — 04_git.md §7 step 1).
func ParseShallow(body []byte) []Oid {
	var out []Oid
	for _, p := range func() [][]byte { ps, _ := ReadAllPkts(bytes.NewReader(body)); return ps }() {
		if v, ok := strings.CutPrefix(strings.TrimRight(string(p), "\n"), "shallow "); ok {
			out = append(out, Oid(v))
		}
	}
	return out
}

// --- report-status (§7.2) -----------------------------------------------------------

// RefReport is the per-ref result: OK or a rejection reason.
type RefReport struct {
	Ref    string
	OK     bool
	Reason string // ng reason when !OK
}

// Report is the full report-status payload.
type Report struct {
	UnpackOK  bool
	UnpackMsg string
	Refs      []RefReport
	Quiet     bool // suppresses only band-2 chatter, never the report
	Sideband  bool // client negotiated side-band-64k
}

// EncodeReport renders the report: `unpack ok` / `unpack ng <msg>`, then per-ref
// `ok <ref>` / `ng <ref> <reason>`. Plain report-status lines are ALWAYS used,
// even when report-status-v2 was negotiated (§7.2 normative). When Sideband,
// the entire report is wrapped in band-1 frames (no empty band-1 frame).
func (r *Report) EncodeReport() []byte {
	var inner bytes.Buffer
	if r.UnpackOK {
		inner.Write(Pkt("unpack ok"))
	} else {
		inner.Write(Pkt("unpack ng " + r.UnpackMsg))
	}
	for _, ref := range r.Refs {
		if ref.OK {
			inner.Write(Pkt("ok " + ref.Ref))
		} else {
			inner.Write(Pkt("ng " + ref.Ref + " " + ref.Reason))
		}
	}
	inner.Write(Flush())
	if !r.Sideband {
		return inner.Bytes()
	}
	var b bytes.Buffer
	b.Write(EncodeSideband(1, inner.Bytes()))
	b.Write(Flush())
	return b.Bytes()
}

// Band2 wraps a failure message for band-2 emission (sent BEFORE the report).
func Band2(msg string) []byte { return EncodeSideband(2, []byte(msg+"\n")) }

// --- connectivity (§7.1) --------------------------------------------------------------

// CheckConnectivity verifies the pushed objects connect: tips fed to
// `git rev-list --objects --stdin --not --all`, stdout piped into
// `git cat-file --batch-check`; missing oids (≤ 16 retained) →
// ErrMissingObject. rev-list's non-zero exit (unable to read/missing) is also
// ErrMissingObject.
func (l *Layer) CheckConnectivity(ctx context.Context, repo *LocalRepo, tips []Oid) error {
	ctx, cancel := context.WithTimeout(ctx, l.ConnectTTL)
	defer cancel()
	return l.Pool.Run(ctx, func() error { return l.connectivity(ctx, repo, tips) })
}

// connectivity runs the two-process pipeline:
//
//	git rev-list --objects --stdin --not --all  |  git cat-file --batch-check
//
// Discipline (§7.1): rev-list stdin written by a feeder goroutine (closes on
// tips-EOF); io.Pipe connects rev-list stdout → cat-file stdin (copier closes
// the write end); the caller drains cat-file stdout, then Wait rev-list first
// (its exit carries the walk verdict), then cat-file.
func (l *Layer) connectivity(ctx context.Context, repo *LocalRepo, tips []Oid) error {
	rev := exec.CommandContext(ctx, l.Binary, "rev-list", "--objects", "--stdin", "--not", "--all")
	rev.Dir = repo.Path
	rev.Env = l.baseEnv(repo.Path)
	revIn, err := rev.StdinPipe()
	if err != nil {
		return err
	}
	revOut, err := rev.StdoutPipe()
	if err != nil {
		return err
	}
	revStderr := newBounded(8 << 10)
	rev.Stderr = revStderr

	cat := exec.CommandContext(ctx, l.Binary, "cat-file", "--batch-check")
	cat.Dir = repo.Path
	cat.Env = l.baseEnv(repo.Path)
	catIn, err := cat.StdinPipe()
	if err != nil {
		return err
	}
	catOut, err := cat.StdoutPipe()
	if err != nil {
		return err
	}
	catStderr := newBounded(8 << 10)
	cat.Stderr = catStderr

	if err := rev.Start(); err != nil {
		return err
	}
	if err := cat.Start(); err != nil {
		rev.Process.Kill()
		rev.Wait()
		return err
	}

	// rev-list stdout → cat-file stdin; copier closes the write end.
	go func() {
		// rev-list --objects emits "<oid> <path>"; cat-file --batch-check
		// treats everything after the first space as a path filter, which
		// marks every blob/tree "missing" (§7.1). Split at the first space.
		sc := bufio.NewScanner(revOut)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if i := strings.IndexByte(line, ' '); i >= 0 {
				line = line[:i]
			}
			if _, err := io.WriteString(catIn, line+"\n"); err != nil {
				break
			}
		}
		catIn.Close()
	}()
	// rev-list stdin: tips one per line; closes on tips-EOF.
	go func() {
		defer revIn.Close()
		for _, t := range tips {
			if isZeroOid(string(t)) {
				continue
			}
			if _, err := io.WriteString(revIn, string(t)+"\n"); err != nil {
				return
			}
		}
	}()

	// Drain cat-file stdout on this goroutine, collecting `<oid> missing`.
	var missing []string
	buf := make([]byte, 32<<10)
	var tail []byte
	for {
		n, rerr := catOut.Read(buf)
		if n > 0 {
			tail = append(tail, buf[:n]...)
			lines := splitLines(&tail)
			for _, line := range lines {
				if oids := missingFromBatch(line); len(oids) > 0 {
					missing = append(missing, oids...)
					if len(missing) >= 16 {
						break
					}
				}
			}
			if len(missing) >= 16 {
				rev.Process.Kill()
				cat.Process.Kill()
				rev.Wait()
				cat.Wait()
				return missingObjects(dedupeStrings(missing[:16]))
			}
		}
		if rerr != nil {
			break
		}
	}

	revErr := rev.Wait()
	catErr := cat.Wait()
	if len(missing) > 0 {
		return missingObjects(dedupeStrings(missing))
	}
	if ctxErr(ctx) {
		return &GitError{Kind: GitErrIo, Detail: "connectivity timed out or cancelled"}
	}
	if revErr != nil {
		// rev-list failed: unable to read / missing tree or commit.
		return missingObjects([]string{firstMissingFromStderr(revStderr.String())})
	}
	_ = catErr
	return nil
}

// splitLines drains complete lines out of *tail, leaving the partial tail.
func splitLines(tail *[]byte) []string {
	var lines []string
	for {
		i := bytes.IndexByte(*tail, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, string((*tail)[:i]))
		*tail = (*tail)[i+1:]
	}
	return lines
}

// firstMissingFromStderr extracts the first 40/64-hex oid from rev-list's
// stderr ("unable to read <sha>" / "missing object <sha>").
func firstMissingFromStderr(stderr string) string {
	run, start := 0, 0
	for i := range len(stderr) + 1 {
		if i < len(stderr) && isHex(stderr[i]) {
			if run == 0 {
				start = i
			}
			run++
			continue
		}
		if run == 40 || run == 64 {
			return strings.ToLower(stderr[start : start+run])
		}
		run = 0
	}
	return "unknown"
}

// missingFromBatch extracts oids reported missing by cat-file --batch-check
// (`<oid> missing`), capped at 16 retained.
func missingFromBatch(out string) []string {
	var missing []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "missing" && isHexOid(fields[0]) {
			missing = append(missing, fields[0])
			if len(missing) >= 16 {
				return missing
			}
		}
	}
	return missing
}

func isHexOid(s string) bool { return (len(s) == 40 || len(s) == 64) && isHexRun(s) }

func isHexRun(s string) bool {
	for i := range s {
		if !isHex(s[i]) {
			return false
		}
	}
	return true
}

func dedupeStrings(s []string) []string {
	seen := map[string]bool{}
	out := s[:0]
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ForceCheck classifies an update as fast-forward via
// `git merge-base --is-ancestor <old> <new>`: exit 0 = FF, exit 1 = force,
// other = treat as force (force-with-verification-needed → force).
func (l *Layer) ForceCheck(ctx context.Context, repo *LocalRepo, old, new Oid) (bool, error) {
	if isZeroOid(string(old)) || isZeroOid(string(new)) {
		return true, nil // create/delete: not a fast-forward question
	}
	_, stderr, err := l.runCollect(ctx, execSpec{
		argv: []string{"merge-base", "--is-ancestor", string(old), string(new)},
		dir:  repo.Path,
	})
	if err == nil {
		return false, nil
	}
	ge, ok := err.(*GitError)
	if ok && ge.Kind == GitErrSubprocess {
		if ge.Detail == "exit status 1" {
			return true, nil
		}
	}
	_ = stderr
	return true, nil // other exit → force
}
