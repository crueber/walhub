// repoimport.go — Feature 10 composition (docs/features/10_git_import.md):
// the import service over store/registry/identity/config (Seam 5 kind
// registered once — duplicate registration panics, the
// maintain.RegisterKind contract in code terms per R1 B6), the Seam 1
// chain (both lanes, top-level twins), and the discovery entries.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// newImportService builds the import surface over st/ident/reg/cfg and
// registers the task kind + discovery templates (called once per process
// from buildCollab — 09 §4 touch point 3, one block for this package).
func newImportService(st store.ObjectStore, ident *identity.Service, reg *wal.Registry, cfg *config.Config) (*repoimport.Service, *repoimport.Handler) {
	repoimport.RegisterKind(repoimport.KindRepoImport)
	api.RegisterExposed(repoimport.ExposedTemplates...)
	svc := repoimport.New(repoimport.Deps{
		Store:     st,
		Reg:       reg,
		Roles:     ident,
		Cfg:       cfg,
		GitBinary: cfg.Git.Binary,
		Hostname:  instanceID(cfg),
	})
	return svc, &repoimport.Handler{Svc: svc}
}

// chainImport fronts the core mux with the import surface (Seam 1);
// authentication resolves through the server chain (Seam 2).
func chainImport(srv *server.Server, h *repoimport.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().Authenticate(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// ---- `walhub import --url` (Seam 7) ---------------------------------------------------

// importURLArgs carries the --url flag set (11_config_cli.md §6.2;
// docs/features/10 §CLI).
type importURLArgs struct {
	url               string
	repo              string
	refs              []string
	defaultBranchOnly bool
	includePullHeads  bool
	includeNotes      bool
	tokenEnv          string
	format            string
	dangerous         bool
}

func firstPositional(positional []string) string {
	if len(positional) == 0 {
		return ""
	}
	return positional[0]
}

// runImportURL clones the source URL into owner/name through the same
// Service (same publish/CAS path as the server — 14 §14.9): manifest
// Create arbitrates, import.json records provenance, the operator is
// recorded as admin. Exit codes per 11 §6.3 (0 ok, 1 error, 2 argv).
func runImportURL(ctx context.Context, c *cli, a importURLArgs) int {
	if a.url == "" || a.repo == "" {
		fmt.Fprintln(os.Stderr, "usage: walhub import --url URL owner/name [--ref REF]… [--default-branch-only] [--include-pull-heads] [--include-notes] [--token-env VAR] [--format sha1|sha256] [--dangerous]")
		return exitArg
	}
	owner, name, ok := splitRepo(a.repo)
	if !ok {
		fmt.Fprintf(os.Stderr, "walhub: bad repo %q: want owner/name\n", a.repo)
		return exitArg
	}
	reg, st, cleanup := openEngine(ctx, c)
	defer cleanup()
	cfg := reg.Config()
	bin := cfg.Git.Binary
	if bin == "" {
		bin = "git"
	}
	host := instanceID(cfg)
	svc := repoimport.New(repoimport.Deps{
		Store: st, Reg: reg, Roles: identity.New(st, cfg),
		Cfg: cfg, GitBinary: bin, Hostname: host,
	})
	body := map[string]any{
		"source_url": a.url, "owner": owner, "name": name,
		"refs":                nonNilStrings(a.refs),
		"default_branch_only": a.defaultBranchOnly,
		"include_pull_heads":  a.includePullHeads,
		"include_notes":       a.includeNotes,
		"format":              a.format,
		"dangerous":           a.dangerous,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	params, token, err := repoimport.ParseRequest(raw, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	if a.tokenEnv != "" {
		token = os.Getenv(a.tokenEnv)
		if token == "" {
			fmt.Fprintf(os.Stderr, "walhub: env %s is empty\n", a.tokenEnv)
			return exitErr
		}
		if err := repoimport.ValidateTransport(params.Source, true); err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
	}
	importer := os.Getenv("USER")
	if importer == "" {
		importer = "operator"
	}
	outcome, err := svc.RunHeadless(ctx, params, token, importer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: import failed: %v\n", err)
		return exitErr
	}
	fmt.Printf("imported %s: %d refs (%s)\n", outcome.Repo, len(outcome.HeadSHAs), outcome.Format)
	return exitOK
}

// nonNilStrings keeps JSON refs []-not-null.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
