package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Maintenance plumbing (04_git.md §9): repack, commit-graph, history pack.
// All commands run in the repo git-dir, Pool.Run, 1800 s timeout, under the
// per-repo pack_mutex owned by doc 05/10.

// PackDiff is the before/after objects/pack/*.idx set difference returned to
// the caller for store reconciliation.
type PackDiff struct {
	New     []string // idx basenames ("pack-<hex>.idx") present after only
	Removed []string // idx basenames present before only
}

func idxSet(repo *LocalRepo) (map[string]bool, error) {
	entries, err := os.ReadDir(repo.PackDir())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".idx") {
			out[e.Name()] = true
		}
	}
	return out, nil
}

func diffIdx(before map[string]bool, repo *LocalRepo) (*PackDiff, error) {
	after, err := idxSet(repo)
	if err != nil {
		return nil, err
	}
	d := &PackDiff{}
	for name := range after {
		if !before[name] {
			d.New = append(d.New, name)
		}
	}
	for name := range before {
		if !after[name] {
			d.Removed = append(d.Removed, name)
		}
	}
	return d, nil
}

// GeometricRepack runs the maintainer compaction:
//
//	git repack -d --geometric=<factor> --write-midx [--write-bitmap-index] [--keep-pack <name>]…
//
// and diffs the objects/pack/*.idx set before/after.
func (l *Layer) GeometricRepack(ctx context.Context, repo *LocalRepo, factor int, bitmap bool, keepPacks []string) (*PackDiff, error) {
	ctx, cancel := context.WithTimeout(ctx, l.MaintTTL)
	defer cancel()
	before, err := idxSet(repo)
	if err != nil {
		return nil, err
	}
	argv := []string{"repack", "-d", fmt.Sprintf("--geometric=%d", factor), "--write-midx"}
	if bitmap {
		argv = append(argv, "--write-bitmap-index")
	}
	for _, k := range keepPacks {
		argv = append(argv, "--keep-pack", k)
	}
	if _, err := l.runPooled(ctx, execSpec{argv: argv, dir: repo.Path, timeout: l.MaintTTL}); err != nil {
		return nil, err
	}
	return diffIdx(before, repo)
}

// FullRepack runs the base rebuild:
//
//	git repack -a -d --threads=0 --write-bitmap-index --write-midx [--keep-pack …]
//
// after deleting stray *.keep markers.
func (l *Layer) FullRepack(ctx context.Context, repo *LocalRepo, keepPacks []string) (*PackDiff, error) {
	ctx, cancel := context.WithTimeout(ctx, l.MaintTTL)
	defer cancel()
	if err := removeKeepMarkers(repo); err != nil {
		return nil, err
	}
	before, err := idxSet(repo)
	if err != nil {
		return nil, err
	}
	argv := []string{"repack", "-a", "-d", "--threads=0", "--write-bitmap-index", "--write-midx"}
	for _, k := range keepPacks {
		argv = append(argv, "--keep-pack", k)
	}
	if _, err := l.runPooled(ctx, execSpec{argv: argv, dir: repo.Path, timeout: l.MaintTTL}); err != nil {
		return nil, err
	}
	return diffIdx(before, repo)
}

func removeKeepMarkers(repo *LocalRepo) error {
	matches, _ := filepath.Glob(filepath.Join(repo.PackDir(), "*.keep"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			return err
		}
	}
	return nil
}

// --- commit-graph --------------------------------------------------------------------

// WriteCommitGraph runs the split commit-graph write:
//
//	git commit-graph write --reachable --split=replace [--changed-paths]
//
// and returns the last chain layer's trailing checksum (the file name under
// objects/info/commit-graphs/), copied out as the wal/<checksum>.commit-graph
// side-file at sideDir.
func (l *Layer) WriteCommitGraph(ctx context.Context, repo *LocalRepo, changedPaths bool, sideDir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, l.MaintTTL)
	defer cancel()
	argv := []string{"commit-graph", "write", "--reachable", "--split=replace"}
	if changedPaths {
		argv = append(argv, "--changed-paths")
	}
	if _, err := l.runPooled(ctx, execSpec{argv: argv, dir: repo.Path, timeout: l.MaintTTL}); err != nil {
		return "", err
	}
	return copyLastChainLayer(repo, sideDir)
}

// copyLastChainLayer identifies the last chain layer by its trailing checksum
// (the file name under objects/info/commit-graphs/) and copies it out.
func copyLastChainLayer(repo *LocalRepo, sideDir string) (string, error) {
	graphDir := filepath.Join(repo.Path, "objects", "info", "commit-graphs")
	if _, err := os.Stat(graphDir); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	// commit-graph-chain names the layers; the LAST layer is its last line.
	var chain []string
	if data, err := os.ReadFile(filepath.Join(graphDir, "commit-graph-chain")); err == nil {
		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			if line != "" {
				chain = append(chain, strings.TrimSpace(line))
			}
		}
	}
	name := ""
	if len(chain) > 0 {
		name = chain[len(chain)-1]
	} else {
		// non-split layout: commit-graph itself, no side-file needed
		return "", nil
	}
	src := filepath.Join(graphDir, "graph-"+name+".graph")
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	checksum := strings.TrimSuffix(name, ".graph")
	if sideDir != "" {
		if err := os.MkdirAll(sideDir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(sideDir, checksum+".commit-graph"), data, 0o644); err != nil {
			return "", err
		}
	}
	return checksum, nil
}

// InstallCommitGraphBase installs a downloaded side-file as the chain BASE on
// a reader: objects/info/commit-graphs/graph-<hash>.graph + commit-graph-chain
// naming only it.
func InstallCommitGraphBase(repo *LocalRepo, checksum string, data []byte) error {
	graphDir := filepath.Join(repo.Path, "objects", "info", "commit-graphs")
	if err := os.MkdirAll(graphDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(graphDir, "graph-"+checksum+".graph"), data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(graphDir, "commit-graph-chain"), []byte(checksum+"\n"), 0o644)
}

// FoldCommitGraphs runs the incremental fold over downloaded packs:
//
//	git commit-graph write --split --stdin-packs
//
// fed the pack idx names on stdin (close stdin, drain, Wait).
func (l *Layer) FoldCommitGraphs(ctx context.Context, repo *LocalRepo, idxNames []string) error {
	ctx, cancel := context.WithTimeout(ctx, l.MaintTTL)
	defer cancel()
	var b bytes.Buffer
	for _, n := range idxNames {
		b.WriteString(filepath.Base(n) + "\n") // git resolves idx names under objects/pack
	}
	_, err := l.runPooled(ctx, execSpec{
		argv:   []string{"commit-graph", "write", "--split", "--stdin-packs"},
		dir:    repo.Path,
		stdin:  &b,
		stdout: io.Discard,
	})
	return err
}

// --- history pack (D18) ----------------------------------------------------------------

// HistoryPack builds the blobless history pack (git.history_pack): all ref
// oids on stdin fed to
//
//	git pack-objects --filter=blob:none --revs --delta-base-offset --stdout -q
//
// piped into `git index-pack --stdin` in a scratch with alternates; renames to
// pack-<sha>.pack/.idx/.rev and writes the .history marker naming the base.
// Returns "" when there are no refs (nothing to pack).
func (l *Layer) HistoryPack(ctx context.Context, repo *LocalRepo, base string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, l.MaintTTL)
	defer cancel()
	snap, err := l.Snapshot(repo)
	if err != nil {
		return "", err
	}
	if len(snap.Refs) == 0 {
		return "", nil
	}

	scratch := filepath.Join(repo.Path, fmt.Sprintf("walgit-history-%d-%d", os.Getpid(), nextSuffix()))
	packDir := filepath.Join(scratch, "objects", "pack")
	defer os.RemoveAll(scratch)
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(scratch, "refs"), 0o755); err != nil {
		return "", err
	}
	if err := writeHeadSeed(scratch); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(scratch, "objects", "info"), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"),
		[]byte(repo.ObjectsDir()+"\n"), 0o644); err != nil {
		return "", err
	}

	var tips bytes.Buffer
	for _, e := range snap.Refs {
		tips.WriteString(string(e.Oid) + "\n")
	}
	var packOut bytes.Buffer
	if _, err := l.runPooled(ctx, execSpec{
		argv:  []string{"pack-objects", "--filter=blob:none", "--revs", "--delta-base-offset", "--stdout", "-q"},
		dir:   repo.Path,
		stdin: &tips, stdout: &packOut, timeout: l.MaintTTL,
	}); err != nil {
		return "", err
	}
	if packOut.Len() == 0 {
		return "", nil
	}
	if _, err := l.runPooled(ctx, execSpec{
		argv:  []string{"index-pack", "--stdin", "--rev-index", "--threads=0"},
		dir:   scratch,
		env:   []string{"GIT_DIR=" + scratch},
		stdin: &packOut, stdout: io.Discard, timeout: l.MaintTTL,
	}); err != nil {
		return "", err
	}
	// Move idx/rev first, pack LAST (crash safety: a pack without idx is inert).
	basename := ""
	for _, pattern := range []string{"pack-*.idx", "pack-*.rev", "pack-*.pack"} {
		matches, err := filepath.Glob(filepath.Join(packDir, pattern))
		if err != nil {
			return "", err
		}
		if len(matches) == 0 && pattern == "pack-*.pack" {
			return "", &PackRejectedError{Detail: "history index-pack produced no pack"}
		}
		for _, src := range matches {
			if err := os.Rename(src, filepath.Join(repo.PackDir(), filepath.Base(src))); err != nil {
				return "", err
			}
		}
		if len(matches) > 0 && pattern == "pack-*.pack" {
			basename = filepath.Base(matches[0])
		}
	}
	marker := strings.TrimSuffix(basename, ".pack") + ".history"
	if err := os.WriteFile(filepath.Join(repo.PackDir(), marker), []byte(base+"\n"), 0o644); err != nil {
		return "", err
	}
	return strings.TrimSuffix(basename, ".pack"), nil
}

// WriteHistoryMidx builds the history midx:
//
//	git multi-pack-index write --stdin-packs --preferred-pack=<history idx>
//
// covering history packs + installed bases; removed when no history packs exist.
func (l *Layer) WriteHistoryMidx(ctx context.Context, repo *LocalRepo, idxNames []string, preferred string) error {
	ctx, cancel := context.WithTimeout(ctx, l.MaintTTL)
	defer cancel()
	if len(idxNames) == 0 {
		os.Remove(filepath.Join(repo.Path, "objects", "pack", "multi-pack-index"))
		return nil
	}
	var b bytes.Buffer
	for _, n := range idxNames {
		b.WriteString(filepath.Base(n) + "\n")
	}
	argv := []string{"multi-pack-index", "write", "--stdin-packs", "--preferred-pack=" + filepath.Base(preferred)}
	_, err := l.runPooled(ctx, execSpec{
		argv: argv, dir: repo.PackDir(), stdin: &b, stdout: io.Discard, timeout: l.MaintTTL,
	})
	return err
}
