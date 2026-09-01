package bundle

import (
	"context"
	"fmt"

	"path"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Serving (§8.11): serve_via = "proxy" → URIs are …/bundles/<strategy>/<file>
// through the static object contract (ETag/Range/immutable); "signed_url" →
// presigned store URIs (TTL bundles.signed_url_ttl, 1 h) for repos in
// bundles.signed_url_for; on any signing error fall back to proxy URIs and
// warn once per repo (in-memory flag). List responses are no-cache (presigned
// URIs expire).

// ServeVia values.
const (
	ServeProxy     = "proxy"
	ServeSignedURL = "signed_url"
)

// DefaultSignedURLTTL is bundles.signed_url_ttl's default (§8.11).
const DefaultSignedURLTTL = time.Hour

// Server renders bundle URIs for list responses and advertisement.
type Server struct {
	St store.ObjectStore

	ServeVia     string        // proxy | signed_url
	SignedURLTTL time.Duration // 0 → DefaultSignedURLTTL
	SignedURLFor map[string]bool

	// PublicBase is the proxy base, e.g. "https://git.example.com" — repo
	// paths are appended as <base>/<owner>/<repo>[.git]/bundles/….
	PublicBase string

	// WarnOnce is called at most once per repo on a signing fallback; nil → silent.
	WarnOnce func(repo, msg string)

	mu     sync.Mutex
	warned map[string]bool
}

// ProxyURI renders the proxy URI for an entry (§8.11): …/bundles/<strategy>/<file>.
func (s *Server) ProxyURI(repo string, e *proto.BundleEntry) string {
	return fmt.Sprintf("%s/%s.git/bundles/%s/%s", s.PublicBase, repo, e.Strategy, path.Base(e.Key))
}

// URI picks proxy vs presigned for one entry, with the signing-failure
// fallback (§8.11). repo is "<owner>/<name>".
func (s *Server) URI(ctx context.Context, repo string, e *proto.BundleEntry) string {
	if s.ServeVia == ServeSignedURL && s.SignedURLFor[repo] && s.St != nil {
		ttl := s.SignedURLTTL
		if ttl <= 0 {
			ttl = DefaultSignedURLTTL
		}
		u, err := s.St.SignedGetURL(ctx, e.Key, ttl)
		if err == nil && u != nil {
			return *u
		}
		s.warnOnce(repo, fmt.Sprintf("presign failed for %s (%v); falling back to proxy URIs", e.Key, err))
	}
	return s.ProxyURI(repo, e)
}

func (s *Server) warnOnce(repo, msg string) {
	if s.WarnOnce == nil {
		return
	}
	s.mu.Lock()
	if s.warned == nil {
		s.warned = make(map[string]bool)
	}
	first := !s.warned[repo]
	s.warned[repo] = true
	s.mu.Unlock()
	if first {
		s.WarnOnce(repo, msg)
	}
}

// ListResponse is one rendered list (clone or catchup) with its URIs resolved.
type ListResponse struct {
	Body    string
	Entries []*proto.BundleEntry
}

// Render renders the git-config text for one family (§8.11): entries ascending
// creationToken, orphaned incrementals dropped, filter families never mixed.
// clone=true renders the kept fulls + chain (bundles/list); clone=false
// renders the same without fulls (bundles/catchup).
func (s *Server) Render(ctx context.Context, repo string, list *proto.BundleList, clone bool, filter string) (string, error) {
	entries, err := FamilyFilter(list, filter)
	if err != nil {
		return "", err
	}
	if clone {
		entries = intersectIDs(entries, CloneEntries(list))
	} else {
		entries = intersectIDs(entries, CatchupEntries(list))
	}
	var b []byte
	b = append(b, "[bundle]\n"...)
	b = append(b, "    version = 1\n"...)
	b = append(b, "    mode = "+modeOf(list)+"\n"...)
	b = append(b, "    heuristic = "+heuristicOf(list)+"\n"...)
	for _, e := range entries {
		b = append(b, fmt.Sprintf("[bundle %q]\n", e.ID)...)
		b = append(b, "    uri = "+uriLine(s.URI(ctx, repo, e))+"\n"...)
		b = append(b, fmt.Sprintf("    creationToken = %d\n", e.CreationToken)...)
		if e.Filter != "" {
			b = append(b, "    filter = "+e.Filter+"\n"...)
		}
	}
	return string(b), nil
}

func modeOf(list *proto.BundleList) string {
	if list != nil && list.Mode != "" {
		return list.Mode
	}
	return "all"
}

func heuristicOf(list *proto.BundleList) string {
	if list != nil && list.Heuristic != "" {
		return list.Heuristic
	}
	return "creationToken"
}

// uriLine quotes a URI for git-config text when it could confuse the parser.
func uriLine(u string) string {
	if u == "" {
		return `""`
	}
	for _, r := range u {
		if r <= ' ' || r == '#' || r == ';' || r == '[' || r == '"' || r == '\\' {
			return urlQuote(u)
		}
	}
	return u
}

func urlQuote(u string) string {
	q := make([]byte, 0, len(u)+2)
	q = append(q, '"')
	for _, c := range []byte(u) {
		switch c {
		case '"', '\\':
			q = append(q, '\\', c)
		default:
			q = append(q, c)
		}
	}
	return string(append(q, '"'))
}

func intersectIDs(a, b []*proto.BundleEntry) []*proto.BundleEntry {
	if len(b) == 0 {
		return nil
	}
	ids := make(map[string]bool, len(b))
	for _, e := range b {
		ids[e.ID] = true
	}
	out := make([]*proto.BundleEntry, 0, len(a))
	for _, e := range a {
		if ids[e.ID] {
			out = append(out, e)
		}
	}
	return out
}
