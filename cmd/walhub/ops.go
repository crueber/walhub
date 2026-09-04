// ops.go — `compact`, the `bundle` family, and the import/synth/mirror entry
// points (11_config_cli.md §6.2; the heavy lifting lives in internal/maintain
// and internal/bundle).
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/bundle"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/maintain"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// ---- compact ------------------------------------------------------------------

// runCompact drives 10_maintenance.md's compaction unit: --once runs a single
// pass (exit 0 even when nothing to do); --base additionally runs the
// base-rebuild path when due; without --once the loop runs on the command ctx.
func runCompact(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("compact")
	once := fs.Bool("once", false, "run a single pass")
	base := fs.Bool("base", false, "include the base-rebuild path (requires --once)")
	mustParse(fs, args)
	if *base && !*once {
		fmt.Fprintln(os.Stderr, "walhub: --base requires --once")
		return exitErr
	}
	reg, _, cleanup := openEngine(ctx, c)
	defer cleanup()
	m := maintain.NewWalMaintainer(reg, maintain.Options{})
	if !*once {
		m.Run(ctx) // the maintainer's cadence loop, ctx-cancelled
		return exitOK
	}
	m.RunPass(ctx)
	fmt.Println("pass complete")
	return exitOK
}

// ---- bundle -------------------------------------------------------------------

// bundleSlotTable plans the slot table for one repo (the same planner the
// maintainer uses; §8.14).
func bundleSlotTable(ctx context.Context, st store.ObjectStore, reg *wal.Registry, repo string, now time.Time) ([]bundle.PlanSlot, []bundle.Strategy, *proto.BundleList, error) {
	eff := reg.Config()
	strategies, err := bundle.FromConfig(eff.Bundles.Strategy, eff.Bundles.MinCommits)
	if err != nil {
		return nil, nil, nil, err
	}
	list := readBundleList(ctx, st, repo)
	adapter := &bundle.WalAdapter{R: reg}
	first, firstOK := adapter.FirstStateAt(repo)
	firstStateAt := func(string) (time.Time, bool) { return first, firstOK }
	hostFits := func(*bundle.Strategy) bool { return true } // CLI planning: host fits
	states := bundle.PlanStates(repo, strategies, list, now, firstStateAt, hostFits)
	return states, strategies, list, nil
}

func readBundleList(ctx context.Context, st store.ObjectStore, repo string) *proto.BundleList {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return &proto.BundleList{}
	}
	body, _, err := store.GetBytes(ctx, st, store.RepoPrefix(owner, name)+store.BundleList, store.GetOptions{})
	if err != nil || body == nil {
		return &proto.BundleList{}
	}
	list, err := proto.UnmarshalBundleList(body)
	if err != nil {
		return &proto.BundleList{}
	}
	return list
}

// buildOneSlot runs the §8.9 pipeline for one (strategy, slot).
func buildOneSlot(ctx context.Context, reg *wal.Registry, repo string, strategies []bundle.Strategy, list *proto.BundleList, strategyName string, slot uint64) error {
	byName := bundle.ByName(strategies)
	st := byName[strategyName]
	if st == nil {
		return fmt.Errorf("unknown strategy %q", strategyName)
	}
	eff := reg.Config()
	h, err := reg.Open(ctx, repo)
	if err != nil {
		return err
	}
	packDir := h.Repo().PackDir()
	deps := bundle.Deps{
		Wal:          &bundle.WalAdapter{R: reg},
		Prim:         &bundle.GitPrimitives{L: h.Layer()},
		St:           reg.Store(),
		Tasks:        bundleTaskRunner{t: reg.Tasks()},
		CacheDir:     eff.Cache.Dir,
		HostID:       reg.InstanceID(),
		RepoDir:      h.Dir(),
		MinCommits:   eff.Bundles.MinCommits,
		List:         list,
		MainOnly:     eff.Bundles.MainOnly,
		ExtraRefs:    eff.Bundles.ExtraRefs,
		ObjectFormat: objectFormatOf(h),
		LocalPack: func(checksum string) (string, bool) {
			path := filepath.Join(packDir, checksum+".pack")
			if _, err := os.Stat(path); err != nil {
				return "", false
			}
			return path, true
		},
	}
	err = bundle.BuildSlot(ctx, &deps, repo, strategies, st, time.Unix(int64(slot), 0).UTC())
	if len(deps.Verdicts) > 0 {
		if verr := bundle.RecordVerdicts(ctx, reg.Store(), deps.Verdicts); verr != nil && err == nil {
			err = verr
		}
	}
	return err
}

// bundleTaskRunner binds the bundle TaskRunner onto wal.TaskTable (§8.9.1).
type bundleTaskRunner struct{ t *wal.TaskTable }

func (r bundleTaskRunner) RunBundle(ctx context.Context, repo string, params map[string]string, fn func(ctx context.Context, tr bundle.Reporter) error) error {
	_, err := r.t.Run(ctx, repo, "bundle", params, func(ctx context.Context, t *wal.Task) error {
		return fn(ctx, t) // *wal.Task satisfies bundle.Reporter
	})
	return err
}

func objectFormatOf(h *wal.RepoHandle) string {
	m, _ := h.ManifestSnapshot()
	if m != nil && m.ObjectFormat != "" {
		return m.ObjectFormat
	}
	return "sha1"
}

func runBundleRun(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("bundle run")
	repoFilter := fs.String("repo", "", "limit to one repo")
	strategyFilter := fs.String("strategy", "", "limit to one strategy name")
	mustParse(fs, args)
	reg, st, cleanup := openEngine(ctx, c)
	defer cleanup()

	repos := []string{}
	if *repoFilter != "" {
		repos = append(repos, *repoFilter)
	} else {
		owners, err := listOwners(ctx, st)
		if err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
		for _, owner := range owners {
			names, _ := listRepos(ctx, st, owner)
			for _, name := range names {
				repos = append(repos, owner+"/"+name)
			}
		}
	}
	now := time.Now().UTC()
	anyErr := false
	for _, repo := range repos {
		states, strategies, list, err := bundleSlotTable(ctx, st, reg, repo, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %s: %v\n", repo, err)
			anyErr = true
			continue
		}
		byName := bundle.ByName(strategies)
		// Oldest missing slot first, backfill_max respected (§8.10).
		sort.SliceStable(states, func(i, j int) bool { return states[i].Slot < states[j].Slot })
		perStrategy := map[string]int{}
		for _, s := range states {
			if s.State != "missing" {
				continue
			}
			if *strategyFilter != "" && s.Strategy != *strategyFilter {
				continue
			}
			stt := byName[s.Strategy]
			if stt != nil && stt.BackfillMax > 0 && perStrategy[s.Strategy] >= stt.BackfillMax {
				fmt.Printf("%s strategy=%s slot=%d skipped backfill_max=%d\n", repo, s.Strategy, s.Slot, stt.BackfillMax)
				continue
			}
			perStrategy[s.Strategy]++
			if err := buildOneSlot(ctx, reg, repo, strategies, list, s.Strategy, s.Slot); err != nil {
				fmt.Printf("%s strategy=%s slot=%d error: %v\n", repo, s.Strategy, s.Slot, err)
				anyErr = true
				continue
			}
			fmt.Printf("%s strategy=%s slot=%d built\n", repo, s.Strategy, s.Slot)
		}
	}
	if anyErr {
		return exitErr
	}
	return exitOK
}

func runBundlePlan(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("bundle plan")
	json := fs.Bool("json", false, "emit JSONL")
	positional := mustParse(fs, args)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: walhub bundle plan <OWNER/REPO>")
		return exitArg
	}
	reg, st, cleanup := openEngine(ctx, c)
	defer cleanup()
	states, _, _, err := bundleSlotTable(ctx, st, reg, positional[0], time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	if *json {
		for _, s := range states {
			fmt.Printf("{\"strategy\":%q,\"slot\":%d,\"state\":%q,\"detail\":%q}\n", s.Strategy, s.Slot, s.State, s.Detail)
		}
		return exitOK
	}
	fmt.Printf("%-12s %-16s %-12s %s\n", "STRATEGY", "SLOT", "STATE", "DETAIL")
	for _, s := range states {
		fmt.Printf("%-12s %-16d %-12s %s\n", s.Strategy, s.Slot, s.State, s.Detail)
	}
	return exitOK
}

func runBundleCompose(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("bundle compose")
	strategy := fs.String("strategy", "", "strategy name (default: the first declared)")
	positional := mustParse(fs, args)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: walhub bundle compose <OWNER/REPO> [--strategy N]")
		return exitArg
	}
	repo := positional[0]
	reg, st, cleanup := openEngine(ctx, c)
	defer cleanup()
	states, strategies, list, err := bundleSlotTable(ctx, st, reg, repo, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	name := *strategy
	if name == "" && len(strategies) > 0 {
		name = strategies[0].Name
	}
	// Latest slot of the strategy whose state is not built (§8.14 compose).
	var chosen *bundle.PlanSlot
	for i := range states {
		if states[i].Strategy == name {
			chosen = &states[i]
		}
	}
	if chosen == nil {
		fmt.Fprintf(os.Stderr, "walhub: strategy %q not found\n", name)
		return exitErr
	}
	if err := buildOneSlot(ctx, reg, repo, strategies, list, name, chosen.Slot); err != nil {
		fmt.Fprintf(os.Stderr, "walhub: compose: %v\n", err)
		return exitErr
	}
	fmt.Printf("%s strategy=%s slot=%d built\n", repo, name, chosen.Slot)
	return exitOK
}

func runBundleRm(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("bundle rm")
	positional := mustParse(fs, args)
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "usage: walhub bundle rm <OWNER/REPO> <IDS…>")
		return exitArg
	}
	_, st, cleanup := openEngine(ctx, c)
	defer cleanup()
	repo := positional[0]
	owner, name, ok := splitRepo(repo)
	if !ok {
		fmt.Fprintf(os.Stderr, "walhub: invalid repo id %q\n", repo)
		return exitErr
	}
	ids := positional[1:]
	removed, err := bundle.RemoveEntries(ctx, st, ids)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: rm: %v\n", err)
		return exitErr
	}
	for _, key := range removed {
		full := store.RepoPrefix(owner, name) + key
		if err := st.Delete(ctx, full, ""); err != nil {
			fmt.Fprintf(os.Stderr, "walhub: delete %s: %v\n", full, err)
		}
	}
	for _, id := range ids {
		fmt.Printf("removed %s\n", id)
	}
	return exitOK
}

// ---- synth / import / mirror ----------------------------------------------------

func runSynth(ctx context.Context, c *cli, args []string) int {
	return notImplemented("synth")
}

// runImport implements the classic import (§6.2): publish the source refs +
// packs (reusing the source's packs), then a full repack published as the
// tier-2 base. --direct (bucket-direct striped import) is not built yet.
func runImport(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("import")
	from := fs.String("from", "", "source git directory (bare or worktree)")
	direct := fs.Bool("direct", false, "bucket-direct import of ready-made packs")
	reusePacks := fs.Bool("reuse-packs", true, "reuse the source's pack files")
	var refGlobs multiString
	fs.Var(&refGlobs, "refs", "only import refs matching the glob (repeatable)")
	// Feature 10 (docs/features/10 §CLI): URL import through the same
	// Service (same publish/CAS path as the server, 14 §14.9).
	sourceURL := fs.String("url", "", "source git URL to clone (owner/repo shorthand or full URL)")
	var onlyRefs multiString
	fs.Var(&onlyRefs, "ref", "only import this exact ref (repeatable)")
	defaultBranchOnly := fs.Bool("default-branch-only", false, "import only the source default branch")
	includePullHeads := fs.Bool("include-pull-heads", false, "also import refs/pull/N/head (never /merge)")
	includeNotes := fs.Bool("include-notes", false, "also import refs/notes/*")
	tokenEnv := fs.String("token-env", "", "env var NAME holding the source token (never the token)")
	format := fs.String("format", "", "require source object format (sha1|sha256; empty follows source)")
	dangerous := fs.Bool("dangerous", false, "confirm a non-allowlisted non-GitHub source")
	mustParse(fs, args)
	positional := fs.Args()
	if *sourceURL != "" {
		return runImportURL(ctx, c, importURLArgs{
			url: *sourceURL, repo: firstPositional(positional),
			refs: onlyRefs.values, defaultBranchOnly: *defaultBranchOnly,
			includePullHeads: *includePullHeads, includeNotes: *includeNotes,
			tokenEnv: *tokenEnv, format: *format, dangerous: *dangerous,
		})
	}
	if *direct {
		return notImplemented("import --direct")
	}
	if *from == "" || len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: walhub import --from GITDIR owner/name [--reuse-packs] [--refs GLOB]…")
		return exitArg
	}
	repoID := positional[0]
	id, err := git.ParseRepoId(repoID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	reg, _, cleanup := openEngine(ctx, c)
	defer cleanup()

	h, err := reg.Create(ctx, id.String(), git.Sha1)
	if err != nil {
		var we *wal.WalError
		if errAsExists(err, &we) {
			if h, err = reg.Open(ctx, id.String()); err != nil {
				fmt.Fprintf(os.Stderr, "walhub: open %s: %v\n", id.String(), err)
				return exitErr
			}
		} else {
			fmt.Fprintf(os.Stderr, "walhub: create %s: %v\n", id.String(), err)
			return exitErr
		}
	}
	layer := h.Layer()

	// 1. Source refs (filtered).
	srcRefs, err := sourceRefs(*from, refGlobs.values)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}

	// 2. Packs: reuse the source's pack files (published as tier-0 entries).
	packDir := filepath.Join(*from, "objects", "pack")
	packs, _ := filepath.Glob(filepath.Join(packDir, "*.pack"))
	sort.Strings(packs)
	imported := map[string]bool{}
	for _, pack := range packs {
		checksum, cerr := packTrailerChecksum(pack, "sha1")
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", cerr)
			return exitErr
		}
		if imported[checksum] {
			continue
		}
		if !*reusePacks {
			// Re-pack via pack-objects into a fresh pack (all objects).
			tmp := filepath.Join(os.TempDir(), "walhub-import-"+checksum+".pack")
			if err := packAll(ctx, *from, tmp); err != nil {
				fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
				return exitErr
			}
			pack = tmp
			if checksum, cerr = packTrailerChecksum(pack, "sha1"); cerr != nil {
				fmt.Fprintf(os.Stderr, "walhub: %v\n", cerr)
				return exitErr
			}
		}
		if _, err := h.AddPack(ctx, pack, checksum, 0, map[string]string{"imported_from": *from}); err != nil {
			fmt.Fprintf(os.Stderr, "walhub: publish pack %s: %v\n", checksum, err)
			return exitErr
		}
		imported[checksum] = true
	}

	// 3. Refs (applied to the serving copy last; §6.2 import ordering).
	txn := &proto.RefTransaction{}
	for _, r := range srcRefs {
		txn.Updates = append(txn.Updates, &proto.RefUpdate{Name: r.name, OldOid: git.Sha1.ZeroHex(), NewOid: r.oid})
	}
	if len(txn.Updates) > 0 {
		if _, err := h.Publish(ctx, wal.PublishRequest{Txn: txn, Meta: map[string]string{"imported_from": *from}}); err != nil {
			fmt.Fprintf(os.Stderr, "walhub: publish refs: %v\n", err)
			return exitErr
		}
	}

	// 4. Full bitmap'd repack published as the tier-2 base (§6.2 import tail).
	diff, err := layer.FullRepack(ctx, h.Repo(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: full repack: %v\n", err)
		return exitErr
	}
	for _, idxBase := range diff.New {
		checksum := strings.TrimSuffix(idxBase, ".idx")
		if imported[checksum] {
			continue
		}
		pack := filepath.Join(h.Repo().PackDir(), checksum+".pack")
		if _, err := h.AddPack(ctx, pack, checksum, 2, map[string]string{"history": "base"}); err != nil {
			fmt.Fprintf(os.Stderr, "walhub: publish base %s: %v\n", checksum, err)
			return exitErr
		}
	}
	fmt.Printf("imported %s: %d refs, %d source packs\n", id.String(), len(srcRefs), len(imported))
	return exitOK
}

type sourceRef struct {
	name string
	oid  string
}

// sourceRefs reads refs via git for-each-ref, filtered by trailing-path globs.
func sourceRefs(gitDir string, globs []string) ([]sourceRef, error) {
	bin := os.Getenv("WALGIT_GIT_BINARY")
	if bin == "" {
		bin = "git"
	}
	cmd := execGit(bin, "--git-dir="+gitDir, "for-each-ref", "--format=%(objectname) %(refname)")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("for-each-ref on %s: %w", gitDir, err)
	}
	var refs []sourceRef
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		oid, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if len(globs) > 0 && !matchAnyGlobRef(globs, strings.TrimPrefix(name, "refs/")) {
			continue
		}
		refs = append(refs, sourceRef{name: name, oid: oid})
	}
	return refs, nil
}

// packAll writes one pack holding every reachable object of the source repo.
func packAll(ctx context.Context, gitDir, outPath string) error {
	bin := os.Getenv("WALGIT_GIT_BINARY")
	if bin == "" {
		bin = "git"
	}
	out, err := os.Create(outPath) //nolint:gosec // operator-provided path
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := execGit(bin, "--git-dir="+gitDir, "pack-objects", "--all", "--delta-base-offset")
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func execGit(bin string, args ...string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// globMatch is the ref-glob matcher (path.Match semantics).
func globMatch(pattern, s string) bool {
	ok, _ := path.Match(pattern, s)
	return ok
}

// multiString accumulates repeatable string flags.
type multiString struct{ values []string }

func (m *multiString) String() string { return strings.Join(m.values, ",") }
func (m *multiString) Set(v string) error {
	m.values = append(m.values, v)
	return nil
}

func errAsExists(err error, target **wal.WalError) bool {
	for err != nil {
		if e, ok := err.(*wal.WalError); ok {
			*target = e
			return e.Kind == wal.WalErrAlreadyExists
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func runMirror(ctx context.Context, c *cli, args []string) int {
	return notImplemented("mirror")
}

// matchAnyGlob matching for ref globs (owner-free: suffix or prefix match).
func matchAnyGlobRef(globs []string, name string) bool {
	for _, g := range globs {
		if ok := globMatch(g, name); ok {
			return true
		}
	}
	return false
}
