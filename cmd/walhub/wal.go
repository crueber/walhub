// wal.go — `wal ls|show|materialize|add-pack|annotate-pack|rev-index`
// (11_config_cli.md §6.2).
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// chunkLog streams the log in bounded batches (§6.2: no full-log read).
func chunkLog(ctx context.Context, h *walHandleT, from, to uint64, fn func([]*proto.LogEntry) error) error {
	const batch = 500
	for from <= to {
		end := from + batch - 1
		if end > to {
			end = to
		}
		entries, err := h.ReadLog(ctx, from, end)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		if err := fn(entries); err != nil {
			return err
		}
		from = end + 1
	}
	return nil
}

func runWalLs(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("wal ls")
	from := fs.Int64("from", 0, "first seq (0 = min_seq)")
	to := fs.Int64("to", 0, "last seq (0 = head_seq)")
	json := fs.Bool("json", false, "emit JSONL")
	positional := mustParse(fs, args)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: walhub wal ls <OWNER/REPO> [--from N] [--to N]")
		return exitArg
	}
	h, err := openHandle(ctx, c, positional[0])
	if err != nil {
		return exitErr
	}
	defer h.close()
	m := h.manifest()
	lo, hi := uint64(*from), uint64(*to)
	if lo == 0 {
		lo = m.MinSeq
	}
	if hi == 0 {
		hi = m.HeadSeq
	}
	err = chunkLog(ctx, h, lo, hi, func(entries []*proto.LogEntry) error {
		for _, e := range entries {
			pack := "-"
			if e.Pack != nil {
				pack = short12(e.Pack.Checksum)
			}
			sups := 0
			if e.Kind == proto.EntryKindCompact {
				sups = len(e.Supersedes)
			}
			refs := 0
			if e.Txn != nil {
				refs = len(e.Txn.Updates)
			}
			at := ""
			if e.CreatedAt != nil {
				at = e.CreatedAt.Go().UTC().Format(time.RFC3339)
			}
			if *json {
				fmt.Printf("{\"seq\":%d,\"kind\":%q,\"pack\":%q,\"supersedes\":%d,\"refs\":%d,\"created_at\":%q}\n",
					e.Seq, string(e.Kind), pack, sups, refs, at)
				continue
			}
			fmt.Printf("%8d  %-10s  %-12s  supersedes %2d  refs %2d  %s\n", e.Seq, e.Kind, pack, sups, refs, at)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	return exitOK
}

func runWalShow(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("wal show")
	positional := mustParse(fs, args)
	if len(positional) != 2 {
		fmt.Fprintln(os.Stderr, "usage: walhub wal show <OWNER/REPO> <SEQ>")
		return exitArg
	}
	seq, err := strconv.ParseUint(positional[1], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: invalid seq %q\n", positional[1])
		return exitErr
	}
	h, err := openHandle(ctx, c, positional[0])
	if err != nil {
		return exitErr
	}
	defer h.close()
	entries, err := h.ReadLog(ctx, seq, seq)
	if err != nil || len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "walhub: seq %d not in the live log (folded into a checkpoint?)\n", seq)
		return exitErr
	}
	e := entries[0]
	fmt.Printf("seq:        %d\n", e.Seq)
	fmt.Printf("kind:       %s\n", e.Kind)
	fmt.Printf("writer:     %s\n", e.Writer)
	if e.CreatedAt != nil {
		fmt.Printf("created_at: %s\n", e.CreatedAt.Go().UTC().Format(time.RFC3339Nano))
	}
	if e.Pack != nil {
		fmt.Printf("pack:       %s (%d bytes, tier %d)\n", e.Pack.Checksum, e.Pack.PackSize, e.Pack.Tier)
	}
	if len(e.Supersedes) > 0 {
		fmt.Printf("supersedes: %s\n", strings.Join(e.Supersedes, ", "))
	}
	if e.Txn != nil {
		fmt.Printf("atomic:     %v\n", e.Txn.Atomic)
		for _, u := range e.Txn.Updates {
			fmt.Printf("  %s  %s -> %s\n", u.Name, displayOid(u.OldOid), displayOid(u.NewOid))
		}
		if len(e.Txn.PushOptions) > 0 {
			fmt.Printf("push-options: %s\n", strings.Join(e.Txn.PushOptions, ", "))
		}
	}
	if e.Checkpoint != nil {
		fmt.Printf("checkpoint: seq %d (%s)\n", e.Checkpoint.Seq, e.Checkpoint.Key)
	}
	if len(e.Meta) > 0 {
		fmt.Println("meta:")
		for _, k := range sortedKeys(e.Meta) {
			fmt.Printf("  %s = %s\n", k, e.Meta[k])
		}
	}
	return exitOK
}

func runWalMaterialize(ctx context.Context, c *cli, args []string) int {
	return notImplemented("wal materialize")
}

func runWalAddPack(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("wal add-pack")
	tier := fs.Uint("tier", 0, "pack tier (0 = recovery base)")
	historyOf := fs.String("history-of", "", "checksum whose history this pack carries")
	positional := mustParse(fs, args)
	if len(positional) != 2 {
		fmt.Fprintln(os.Stderr, "usage: walhub wal add-pack <OWNER/REPO> <PACK> [--tier N] [--history-of CHECKSUM]")
		return exitArg
	}
	h, err := openHandle(ctx, c, positional[0])
	if err != nil {
		return exitErr
	}
	defer h.close()
	path := positional[1]
	checksum, err := packTrailerChecksum(path, h.manifest().ObjectFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	meta := map[string]string{}
	if *historyOf != "" {
		meta["history_of"] = *historyOf
	}
	if _, err := h.AddPack(ctx, path, checksum, uint32(*tier), meta); err != nil {
		fmt.Fprintf(os.Stderr, "walhub: add-pack: %v\n", err)
		return exitErr
	}
	fmt.Printf("published pack %s (tier %d)\n", checksum, *tier)
	return exitOK
}

func runWalAnnotatePack(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("wal annotate-pack")
	rev := fs.Bool("rev", false, "pack has a .rev side file")
	bitmap := fs.Bool("bitmap", false, "pack has a .bitmap side file")
	commitGraph := fs.Bool("commit-graph", false, "pack has a .commit-graph side file")
	positional := mustParse(fs, args)
	if len(positional) != 2 {
		fmt.Fprintln(os.Stderr, "usage: walhub wal annotate-pack <OWNER/REPO> <CHECKSUM> [--rev] [--bitmap] [--commit-graph]")
		return exitArg
	}
	h, err := openHandle(ctx, c, positional[0])
	if err != nil {
		return exitErr
	}
	defer h.close()
	if err := h.AnnotatePack(ctx, positional[1], *rev, *bitmap, *commitGraph); err != nil {
		fmt.Fprintf(os.Stderr, "walhub: annotate-pack: %v\n", err)
		return exitErr
	}
	return exitOK
}

// runWalRevIndex writes a .rev from an .idx: git's own index-pack --rev-index
// writes a byte-identical side file next to the pack (04_git.md owns the
// algorithm; the writer here is the git binary).
func runWalRevIndex(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("wal rev-index")
	out := fs.String("out", "", "write the .rev to P instead of alongside the pack")
	positional := mustParse(fs, args)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: walhub wal rev-index <IDX> [--out P]")
		return exitArg
	}
	indexPath := positional[0]
	packPath := strings.TrimSuffix(indexPath, ".idx") + ".pack"
	bin := os.Getenv("WALGIT_GIT_BINARY")
	if bin == "" {
		bin = "git"
	}
	cmd := exec.Command(bin, "-c", "pack.writeReverseIndex=true", "index-pack", "--rev-index", packPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "walhub: rev-index: %v (git >= 2.31 required)\n", err)
		return exitErr
	}
	produced := strings.TrimSuffix(packPath, ".pack") + ".rev"
	if *out != "" {
		if err := os.Rename(produced, *out); err != nil {
			// cross-device moves fall back to copy
			body, rerr := os.ReadFile(produced)
			if rerr != nil || os.WriteFile(*out, body, 0o644) != nil {
				fmt.Fprintf(os.Stderr, "walhub: move %s: %v\n", produced, err)
				return exitErr
			}
			_ = os.Remove(produced)
		}
		fmt.Println(*out)
	} else {
		fmt.Println(filepath.Base(produced))
	}
	return exitOK
}

// packTrailerChecksum computes the pack trailing SHA (hex) from the file tail.
func packTrailerChecksum(path, objectFormat string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // operator-provided path
	if err != nil {
		return "", err
	}
	defer f.Close()
	size := 20 // sha1
	if objectFormat == "sha256" {
		size = 32
	}
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() < int64(size) {
		return "", fmt.Errorf("%s: not a pack file", filepath.Base(path))
	}
	if _, err := f.Seek(-int64(size), 2); err != nil { //nolint:mnd // trailer at EOF
		return "", err
	}
	trailer := make([]byte, size)
	if _, err := f.Read(trailer); err != nil { //nolint:gosec // fixed-size read
		return "", err
	}
	return hex.EncodeToString(trailer), nil
}

func displayOid(oid string) string {
	if oid == "" {
		return "(absent)"
	}
	if strings.Trim(oid, "0") == "" {
		return "(delete)"
	}
	return oid
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ { // insertion sort: tiny maps
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
