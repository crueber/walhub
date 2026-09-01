// repo.go — `repo create|list|info|policy|settings` (11_config_cli.md §6.2).
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/policy"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// openEngine loads config+store+registry for a repo command and returns the
// registry (closed by the returned cleanup func).
func openEngine(ctx context.Context, c *cli) (*wal.Registry, store.ObjectStore, func()) {
	cfg, state, err := resolveConfig(c)
	if err != nil {
		code, msg := exitCodeForLoad(state, err)
		fmt.Fprintf(os.Stderr, "walhub: %s\n", msg)
		os.Exit(code)
	}
	dataDir := dataDirFor(c)
	st, err := openStore(cfg, dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: store: %v\n", err)
		os.Exit(exitErr)
	}
	reg := wal.NewRegistry(ctx, st, cfg)
	return reg, st, func() { reg.Close() }
}

// ---- repo create ----------------------------------------------------------------

func runRepoCreate(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("repo create")
	format := fs.String("object-format", "sha1", "sha1 | sha256")
	positional := mustParse(fs, args)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: walhub repo create <OWNER/REPO> [--object-format sha1|sha256]")
		return exitArg
	}
	id, err := git.ParseRepoId(positional[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	f, err := git.ObjectFormatFrom(*format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	reg, _, cleanup := openEngine(ctx, c)
	defer cleanup()
	if _, err := reg.Create(ctx, id.String(), f); err != nil {
		fmt.Fprintf(os.Stderr, "walhub: create %s: %v\n", id.String(), err)
		return exitErr
	}
	fmt.Printf("created %s (%s)\n", id.String(), f)
	return exitOK
}

// ---- repo list ------------------------------------------------------------------

func runRepoList(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("repo list")
	json := fs.Bool("json", false, "emit JSONL")
	mustParse(fs, args)
	reg, st, cleanup := openEngine(ctx, c)
	defer cleanup()

	type row struct{ Owner, Name, Format string }
	var rows []row
	owners, err := listOwners(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	for _, owner := range owners {
		repos, err := listRepos(ctx, st, owner)
		if err != nil {
			continue
		}
		for _, name := range repos {
			format := "sha1"
			if h := reg.Get(owner + "/" + name); h == nil {
				if m := readManifest(ctx, st, owner, name); m != nil {
					format = m.ObjectFormat
				}
			}
			rows = append(rows, row{owner, name, format})
		}
	}
	if *json {
		for _, r := range rows {
			fmt.Printf("{\"owner\":%q,\"name\":%q,\"full_name\":%q,\"object_format\":%q}\n", r.Owner, r.Name, r.Owner+"/"+r.Name, r.Format)
		}
		return exitOK
	}
	for _, r := range rows {
		fmt.Printf("%-40s %s\n", pad(r.Owner+"/"+r.Name, 40), r.Format)
	}
	return exitOK
}

// ---- repo info ------------------------------------------------------------------

func runRepoInfo(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("repo info")
	positional := mustParse(fs, args)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: walhub repo info <OWNER/REPO>")
		return exitArg
	}
	_, st, cleanup := openEngine(ctx, c)
	defer cleanup()
	owner, name, ok := splitRepo(positional[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "walhub: invalid repo id %q\n", positional[0])
		return exitErr
	}
	m := readManifest(ctx, st, owner, name)
	if m == nil {
		fmt.Fprintf(os.Stderr, "walhub: repository not found: %s\n", positional[0])
		return exitErr
	}
	var packBytes uint64
	for _, p := range m.Packs {
		packBytes += p.PackSize
	}
	fmt.Printf("repo:            %s\n", m.Repo)
	fmt.Printf("object format:   %s\n", m.ObjectFormat)
	fmt.Printf("head seq:        %d\n", m.HeadSeq)
	fmt.Printf("min seq:         %d\n", m.MinSeq)
	fmt.Printf("revision:        %d\n", m.Revision)
	fmt.Printf("writer:          %s\n", m.Writer)
	if m.Checkpoint != nil {
		fmt.Printf("checkpoint:      seq %d (%s)\n", m.Checkpoint.Seq, m.Checkpoint.Key)
	} else {
		fmt.Printf("checkpoint:      none\n")
	}
	fmt.Printf("log segments:    %d\n", len(m.LogSegments))
	fmt.Printf("live packs:      %d (%d bytes)\n", len(m.Packs), packBytes)
	fmt.Printf("settings:        ")
	if m.Settings != nil {
		fmt.Printf("revision %d by %s\n", m.Settings.Revision, m.Settings.Author)
	} else {
		fmt.Printf("none\n")
	}
	return exitOK
}

// ---- repo policy ----------------------------------------------------------------

func runRepoPolicy(ctx context.Context, c *cli, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: walhub repo policy get|set --file F|clear <OWNER/REPO>")
		return exitArg
	}
	sub, rest := args[0], args[1:]
	_, st, cleanup := openEngine(ctx, c)
	defer cleanup()
	fs := newFlagSet("repo policy " + sub)
	file := fs.String("file", "", "policy.json path (set)")
	positional := mustParse(fs, rest)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: walhub repo policy get|set --file F|clear <OWNER/REPO>")
		return exitArg
	}
	owner, name, ok := splitRepo(positional[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "walhub: invalid repo id %q\n", positional[0])
		return exitErr
	}
	key := store.PolicyKey(owner, name)
	switch sub {
	case "get":
		body, _, err := store.GetBytes(ctx, st, key, store.GetOptions{})
		if err != nil || body == nil {
			fmt.Println("{}") // missing policy = allow-all
			return exitOK
		}
		os.Stdout.Write(body)
		if body[len(body)-1] != '\n' {
			fmt.Println()
		}
		return exitOK
	case "set":
		if *file == "" {
			fmt.Fprintln(os.Stderr, "walhub: repo policy set requires --file F")
			return exitArg
		}
		body, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
		if _, err := policy.Parse(body); err != nil { // validated on set (14.1)
			fmt.Fprintf(os.Stderr, "walhub: invalid policy: %v\n", err)
			return exitErr
		}
		if _, err := st.Put(ctx, key, store.PutBody{Bytes: body}, store.PutOptions{ContentType: "application/json"}); err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
		return exitOK
	case "clear":
		if err := st.Delete(ctx, key, ""); err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "walhub: unknown repo policy subcommand %q\n", sub)
		return exitArg
	}
}

// ---- repo settings (D24) ----------------------------------------------------------

func runRepoSettings(ctx context.Context, c *cli, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: walhub repo settings show [--effective]|set --file F [-m MSG]|clear|history <OWNER/REPO>")
		return exitArg
	}
	sub, rest := args[0], args[1:]
	fs := newFlagSet("repo settings " + sub)
	file := fs.String("file", "", "settings TOML path (set)")
	msg := fs.String("m", "", "change message")
	effective := fs.Bool("effective", false, "print the merged host+repo config (show)")
	positional := mustParse(fs, rest)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: walhub repo settings show|set|clear|history <OWNER/REPO>")
		return exitArg
	}
	repoID := positional[0]

	cfg, state, err := resolveConfig(c)
	if err != nil {
		code, msg := exitCodeForLoad(state, err)
		fmt.Fprintf(os.Stderr, "walhub: %s\n", msg)
		os.Exit(code)
	}
	reg, _, cleanup := openEngine(ctx, c)
	defer cleanup()
	h, err := reg.Open(ctx, repoID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: open %s: %v\n", repoID, err)
		return exitErr
	}

	switch sub {
	case "show":
		m, _ := h.ManifestSnapshot()
		if *effective {
			if m != nil && m.Settings != nil {
				rs, err := config.ParseRepoSettings([]byte(m.Settings.Toml))
				if err != nil {
					fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
					return exitErr
				}
				merged, err := rs.Merge(cfg)
				if err != nil {
					fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
					return exitErr
				}
				printTomlSections(merged, "bundles", "maintenance", "compaction", "upstream")
				return exitOK
			}
			printTomlSections(cfg, "bundles", "maintenance", "compaction", "upstream")
			return exitOK
		}
		if m == nil || m.Settings == nil {
			fmt.Println("# no settings published (revision 0)")
			return exitOK
		}
		fmt.Printf("# revision %d by %s: %s\n", m.Settings.Revision, m.Settings.Author, m.Settings.Message)
		fmt.Println(m.Settings.Toml)
		return exitOK
	case "set":
		if *file == "" {
			fmt.Fprintln(os.Stderr, "walhub: repo settings set requires --file F")
			return exitArg
		}
		body, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
		author := os.Getenv("USER")
		if author == "" {
			author = "unknown"
		}
		if err := h.PublishSettings(ctx, string(body), author, *msg, nil); err != nil {
			fmt.Fprintf(os.Stderr, "walhub: publish settings: %v\n", err)
			return exitErr
		}
		return exitOK
	case "clear":
		if err := h.PublishSettings(ctx, "", os.Getenv("USER"), "clear", nil); err != nil {
			fmt.Fprintf(os.Stderr, "walhub: clear settings: %v\n", err)
			return exitErr
		}
		return exitOK
	case "history":
		m, _ := h.ManifestSnapshot()
		if m == nil {
			return exitOK
		}
		entries, err := h.ReadLog(ctx, m.MinSeq, m.HeadSeq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
		for _, e := range entries {
			if e.Kind != proto.EntryKindSettings || e.Settings == nil {
				continue
			}
			fmt.Printf("seq %d  revision %d  %s  %s\n", e.Seq, e.Settings.Revision, e.Settings.Author, e.Settings.Message)
		}
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "walhub: unknown repo settings subcommand %q\n", sub)
		return exitArg
	}
}

// ---- store listing helpers --------------------------------------------------------

func listOwners(ctx context.Context, st store.ObjectStore) ([]string, error) {
	out := []string{}
	err := st.ListPrefixes(ctx, "repos/", func(m string) error {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(m, "repos/"), "/"))
		return nil
	})
	sort.Strings(out)
	return out, err
}

func listRepos(ctx context.Context, st store.ObjectStore, owner string) ([]string, error) {
	out := []string{}
	prefix := "repos/" + owner + "/"
	err := st.ListPrefixes(ctx, prefix, func(m string) error {
		name := strings.TrimPrefix(m, prefix)
		if strings.HasSuffix(name, "/") {
			out = append(out, strings.TrimSuffix(name, "/"))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func splitRepo(s string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(strings.TrimSuffix(s, ".git"), "/")
	return owner, name, ok && owner != "" && name != ""
}

// readManifest loads a manifest straight from the store (no repo open).
func readManifest(ctx context.Context, st store.ObjectStore, owner, name string) *proto.Manifest {
	body, _, err := store.GetBytes(ctx, st, store.RepoPrefix(owner, name)+store.Manifest, store.GetOptions{})
	if err != nil || body == nil {
		return nil
	}
	m, err := proto.UnmarshalManifest(body)
	if err != nil {
		return nil
	}
	return m
}

// printTomlSections prints a config restricted to the named sections.
func printTomlSections(cfg *config.Config, sections ...string) {
	for _, s := range sections {
		fmt.Printf("[%s]\n", s)
		switch s {
		case "bundles":
			fmt.Printf("min_commits = %d\n", cfg.Bundles.MinCommits)
			fmt.Printf("min_bytes = %q\n", fmtBytes(int64(cfg.Bundles.MinBytes)))
			fmt.Printf("main_only = %v\n", cfg.Bundles.MainOnly)
		case "maintenance":
			fmt.Printf("interval = %q\n", fmtDur(time.Duration(cfg.Maintenance.Interval)))
			fmt.Printf("checkpoints = %v\n", cfg.Maintenance.Checkpoints)
			fmt.Printf("follow_interval = %q\n", fmtDur(time.Duration(cfg.Maintenance.FollowInterval)))
		case "compaction":
			fmt.Printf("enabled = %v\n", cfg.Compaction.Enabled)
			fmt.Printf("factor = %d\n", cfg.Compaction.Factor)
		case "upstream":
			fmt.Printf("git = %q\n", cfg.Upstream.Git)
			fmt.Printf("lfs = %q\n", cfg.Upstream.Lfs)
		}
		fmt.Println()
	}
}
