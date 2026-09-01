package git

import (
	"bytes"
	"sort"
	"strings"
)

// Advertisements (04_git.md §6). The HTTP layer prepends `# service=<svc>\n` +
// flush for info/refs; Advertisement returns only the body.

// Service selects the smart-http service.
type Service int

const (
	ServiceUploadPack Service = iota
	ServiceReceivePack
)

func (s Service) String() string {
	if s == ServiceReceivePack {
		return "git-receive-pack"
	}
	return "git-upload-pack"
}

// ServiceFromName parses "git-upload-pack"/"git-receive-pack".
func ServiceFromName(s string) (Service, bool) {
	switch s {
	case "git-upload-pack":
		return ServiceUploadPack, true
	case "git-receive-pack":
		return ServiceReceivePack, true
	}
	return 0, false
}

// ProtocolVersion from the Git-Protocol header (case-insensitive token match):
// `version=2` → 2, otherwise 0 (04_git.md §6).
func ProtocolVersion(header string) int {
	for _, tok := range strings.Fields(header) {
		if strings.EqualFold(tok, "version=2") {
			return 2
		}
	}
	return 0
}

const (
	capsReceivePack = "report-status report-status-v2 delete-refs side-band-64k quiet atomic ofs-delta push-options object-format="
	capsUploadPack  = "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow deepen-since deepen-not no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want filter object-format="
	agentSuffix     = " agent=walgit/"
)

// Advertisement renders the v0 ref advertisement body (no `# service=` prefix,
// ends with flush) or, when v2, the hand-rendered capability advertisement
// (04_git.md §6.1/§6.2).
func (l *Layer) Advertisement(repo *LocalRepo, svc Service, v2 bool, version string) ([]byte, error) {
	if v2 {
		return l.capabilityAdvert(repo, version), nil
	}
	return l.v0Advertisement(repo, svc, version)
}

func (l *Layer) v0Advertisement(repo *LocalRepo, svc Service, version string) ([]byte, error) {
	snap, err := l.Snapshot(repo)
	if err != nil {
		return nil, err
	}
	caps := capsUploadPack
	if svc == ServiceReceivePack {
		caps = capsReceivePack
	}
	caps += repo.Format().String() + agentSuffix + version

	var b bytes.Buffer
	if len(snap.Refs) == 0 {
		// Empty repo → single capabilities^{} line.
		b.Write(Pkt(repo.Format().ZeroHex() + " capabilities^{}\x00" + caps))
		b.Write(Flush())
		return b.Bytes(), nil
	}
	for i, e := range snap.Refs {
		if i == 0 {
			b.Write(Pkt(string(e.Oid) + " " + e.Name + "\x00" + caps))
		} else {
			b.Write(Pkt(string(e.Oid) + " " + e.Name))
		}
		if e.Peeled != "" {
			b.Write(Pkt(string(e.Peeled) + " " + e.Name + "{}"))
		}
	}
	if svc == ServiceUploadPack && snap.HeadOid != "" {
		b.Write(Pkt(string(snap.HeadOid) + " HEAD"))
	}
	b.Write(Flush())
	return b.Bytes(), nil
}

// capabilityAdvert is the v2 capability advertisement, rendered by hand
// (ls-refs is ours; fetch delegates to stock git — §6.2).
func (l *Layer) capabilityAdvert(repo *LocalRepo, version string) []byte {
	var b bytes.Buffer
	b.Write(Pkt("version 2"))
	b.Write(Pkt("agent=walgit/" + version))
	b.Write(Pkt("ls-refs=unborn"))
	b.Write(Pkt("fetch=thin-pack ofs-delta sideband-all wait-for-done shallow deepen-since deepen-not deepen-relative filter include-tag"))
	b.Write(Pkt("object-format=" + repo.Format().String()))
	b.Write(Flush())
	return b.Bytes()
}

// --- ls-refs (§6.3) ----------------------------------------------------------------

// LsRefsArgs are the client's ls-refs arguments.
type LsRefsArgs struct {
	Symrefs  bool
	Peel     bool
	Unborn   bool
	Prefixes []string
}

// ParseLsRefsArgs decodes the request pkt-lines: symrefs/peel/unborn flags,
// `ref-prefix <p>` (also tolerating `ref-prefix=<p>`), terminated by flush.
func ParseLsRefsArgs(pkts [][]byte) LsRefsArgs {
	var args LsRefsArgs
	for _, p := range pkts {
		line := strings.TrimRight(string(p), "\n")
		switch {
		case line == "symrefs":
			args.Symrefs = true
		case line == "peel":
			args.Peel = true
		case line == "unborn":
			args.Unborn = true
		}
		if v, ok := strings.CutPrefix(line, "ref-prefix "); ok {
			args.Prefixes = append(args.Prefixes, v)
		} else if v, ok := strings.CutPrefix(line, "ref-prefix="); ok {
			args.Prefixes = append(args.Prefixes, v)
		}
	}
	return args
}

// LsRefs renders the v2 ls-refs response with O(log n + k) prefix filtering
// over the name-sorted snapshot (04_git.md §6.3). HEAD is resolved from
// head_target BEFORE prefix filtering.
func (l *Layer) LsRefs(repo *LocalRepo, args LsRefsArgs) ([]byte, error) {
	snap, err := l.Snapshot(repo)
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	emit := func(oid, name, symrefTarget, peeled string) {
		line := oid + " " + name
		if symrefTarget != "" {
			line += " symref-target:" + symrefTarget
		}
		if peeled != "" {
			line += " peeled:" + peeled
		}
		b.Write(Pkt(line))
	}
	headAdvertised := len(args.Prefixes) == 0
	for _, p := range args.Prefixes {
		// HEAD resolved BEFORE filtering: a prefix that excludes the target
		// must not hide HEAD — advertise when a prefix covers "HEAD" itself
		// or the resolved target.
		if p == "HEAD" || strings.HasPrefix("HEAD", p) ||
			(snap.HeadTarget != "" && strings.HasPrefix(snap.HeadTarget, p)) {
			headAdvertised = true
		}
	}
	if headAdvertised {
		switch {
		case snap.HeadTarget != "":
			target := snap.HeadTarget
			oid := snap.HeadOid
			if oid == "" {
				if e, ok := snap.Get(target); ok {
					oid = string(e.Oid)
				}
			}
			if oid != "" {
				if args.Symrefs {
					emit(string(oid), "HEAD", target, "")
				} else {
					emit(string(oid), "HEAD", "", "")
				}
			} else if args.Unborn {
				// unborn HEAD → the unborn pseudo-oid + symref-target when requested.
				emit("unborn", "HEAD", symOr(args.Symrefs, target), "")
			}
		case snap.HeadOid != "":
			emit(string(snap.HeadOid), "HEAD", "", "")
		case args.Unborn && snap.HeadTarget != "":
			emit("unborn", "HEAD", symOr(args.Symrefs, snap.HeadTarget), "")
		}
	}

	prefixes := append([]string(nil), args.Prefixes...)
	if len(prefixes) == 0 {
		// No prefixes: every ref is advertised.
		for _, e := range snap.Refs {
			peeled := ""
			if args.Peel && strings.HasPrefix(e.Name, "refs/tags/") && e.Peeled != "" {
				peeled = string(e.Peeled)
			}
			emit(string(e.Oid), e.Name, "", peeled)
		}
		b.Write(Flush())
		return b.Bytes(), nil
	}
	sort.Strings(prefixes)
	var lastEmittedIdx = -1
	seen := map[string]bool{}
	for _, p := range prefixes {
		lo := lowerBound(snap.Refs, p)
		for i := lo; i < len(snap.Refs); i++ {
			name := snap.Refs[i].Name
			if !strings.HasPrefix(name, p) {
				if name > p {
					break
				}
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			lastEmittedIdx = i
			peeled := ""
			if args.Peel && strings.HasPrefix(name, "refs/tags/") && snap.Refs[i].Peeled != "" {
				peeled = string(snap.Refs[i].Peeled)
			}
			emit(string(snap.Refs[i].Oid), name, "", peeled)
		}
	}
	_ = lastEmittedIdx
	b.Write(Flush())
	return b.Bytes(), nil
}

func symOr(cond bool, target string) string {
	if cond {
		return target
	}
	return ""
}

// lowerBound is the first index whose name >= p.
func lowerBound(refs []RefEntry, p string) int {
	lo, hi := 0, len(refs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if refs[mid].Name < p {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func dedupe(s []string) []string {
	out := s[:0]
	prev := ""
	for i, v := range s {
		if i == 0 || v != prev {
			out = append(out, v)
		}
		prev = v
	}
	return out
}
