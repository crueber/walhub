package git

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Pack ingest (04_git.md §3): git index-pack in a scratch git-dir per ingest —
// never the serving copy.

// IngestMetrics are advisory (a malformed trace file is ignored, never an error).
type IngestMetrics struct {
	GitSeconds  float64            // t_abs of the last region_leave
	WallSeconds float64            // Go-measured around the exec
	FeedSeconds float64            // measured by the Go feeder goroutine (phase="feed")
	Phases      map[string]float64 // region label → duration
}

// IngestResult (04_git.md §3.3).
type IngestResult struct {
	Checksum    Oid
	ObjectCount uint64
	Trace       *IngestMetrics
}

// ingestMu serializes scratch-name nanos within a process (collision safety).
var ingestMu sync.Mutex
var lastNanos int64

func nextSuffix() int64 {
	ingestMu.Lock()
	defer ingestMu.Unlock()
	n := time.Now().UnixNano()
	if n <= lastNanos {
		n = lastNanos + 1
	}
	lastNanos = n
	return n
}

// Ingest ingests one pack: index-pack --stdin --keep --rev-index --threads=0
// [--fix-thin] [--fsck-objects] in <repo>/walgit-ingest-<pid>-<nanos>/ with
// objects/info/alternates, streamed stdin under max_bytes enforcement, moves
// idx → rev → pack LAST, and removes the scratch on every exit path.
func (l *Layer) Ingest(ctx context.Context, repo *LocalRepo, pack io.Reader, maxBytes int64, thin, fsck bool) (*IngestResult, error) {
	nanos := nextSuffix()
	scratch := filepath.Join(repo.Path, fmt.Sprintf("walgit-ingest-%d-%d", os.Getpid(), nanos))
	scratchPack := filepath.Join(scratch, "objects", "pack")
	tracePath := filepath.Join(os.TempDir(), fmt.Sprintf("walgit-index-pack-%d.jsonl", nanos))
	started := time.Now()

	defer os.RemoveAll(scratch) // EVERY exit path; also sweeps git tmp_* debris
	defer l.sweepTmp(scratch)
	defer os.Remove(tracePath)

	// Build the scratch git-dir by hand (no subprocess).
	if err := os.MkdirAll(scratchPack, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(scratch, "refs"), 0o755); err != nil {
		return nil, err
	}
	if err := writeHeadSeed(scratch); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(scratch, "objects", "info"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"),
		[]byte(repo.ObjectsDir()+"\n"), 0o644); err != nil {
		return nil, err
	}
	if err := copyFile(filepath.Join(scratch, "config"), filepath.Join(repo.Path, "config")); err != nil {
		return nil, err
	}

	// Stream stdin into a temp file with max_bytes enforcement (64 KiB chunks),
	// aborting with ErrMaxBytes the moment the cap is crossed.
	staged, err := l.stagePack(pack, maxBytes)
	if err != nil {
		return nil, err
	}
	defer os.Remove(staged.path)
	feedSeconds := staged.feedSecs

	argv := []string{"index-pack", "--stdin", "--keep", "--rev-index", "--threads=0"}
	if thin {
		argv = append(argv, "--fix-thin")
	}
	if fsck {
		argv = append(argv, "--fsck-objects")
	}

	var out bytes.Buffer
	ingestCtx, cancel := context.WithTimeout(ctx, l.IngestTTL)
	defer cancel()
	stderr, runErr := l.runPooled(ingestCtx, execSpec{
		argv:   argv,
		dir:    scratch,
		env:    []string{"GIT_DIR=" + scratch, "GIT_TRACE2_EVENT=" + tracePath},
		stdin:  staged.reader(),
		stdout: &out,
	})
	if runErr != nil {
		if errors.Is(staged.overErr, ErrMaxBytes) {
			return nil, &GitError{Kind: GitErrPack, Detail: "pack exceeds max_bytes"}
		}
		if ctxErr(ingestCtx) {
			return nil, &GitError{Kind: GitErrIo, Detail: "ingest timed out or cancelled", Cmd: strings.Join(argv, " "), Stderr: stderr}
		}
		return nil, &PackRejectedError{Detail: strings.TrimSpace(stderr)}
	}

	// Parse the trailing checksum from index-pack's stdout: take the LAST
	// 40/64-hex token as the pack checksum.
	checksum := lastHexToken(out.String())
	if checksum == "" {
		return nil, &PackRejectedError{Detail: "index-pack printed no checksum: " + out.String()}
	}

	// Move order idx → rev → pack, atomic renames into the repo's
	// objects/pack/pack-<hex>.*; pack LAST so an interrupt never leaves a pack
	// without an idx. The .keep file is discarded.
	keepPath := filepath.Join(scratchPack, "pack-"+checksum+".keep")
	idxSrc := filepath.Join(scratchPack, "pack-"+checksum+".idx")
	revSrc := filepath.Join(scratchPack, "pack-"+checksum+".rev")
	packSrc := filepath.Join(scratchPack, "pack-"+checksum+".pack")
	dstBase := filepath.Join(repo.PackDir(), "pack-"+checksum)
	for _, mv := range []struct{ src, dst string }{
		{idxSrc, dstBase + ".idx"},
		{revSrc, dstBase + ".rev"},
		{packSrc, dstBase + ".pack"},
	} {
		if _, err := os.Stat(mv.src); err == nil {
			if err := os.Rename(mv.src, mv.dst); err != nil {
				return nil, err
			}
		}
	}
	os.Remove(keepPath)

	// object_count from the idx fanout: after \xfftOc + version, the next 1024
	// bytes are 256 BE fanout counts; count = fanout[255]. No subprocess.
	count, err := idxObjectCount(dstBase + ".idx")
	if err != nil {
		return nil, err
	}
	if count == 0 {
		// Zero-object pack (ref-only push): install nothing.
		os.Remove(dstBase + ".idx")
		os.Remove(dstBase + ".rev")
		os.Remove(dstBase + ".pack")
	}

	trace := l.parseTrace(tracePath, started, feedSeconds)
	return &IngestResult{Checksum: Oid(checksum), ObjectCount: count, Trace: trace}, nil
}

func (l *Layer) sweepTmp(scratch string) {
	patterns := []string{filepath.Join(scratch, "tmp_*"), filepath.Join(scratch, "objects", "pack", "tmp_*")}
	for _, p := range patterns {
		matches, _ := filepath.Glob(p)
		for _, m := range matches {
			os.Remove(m)
		}
	}
}

type stagedPack struct {
	path     string
	size     int64
	feedSecs float64
	overErr  error // ErrMaxBytes when the cap was crossed
}

func (s stagedPack) reader() io.Reader {
	f, err := os.Open(s.path)
	if err != nil {
		return strings.NewReader("")
	}
	return f
}

// stagePack copies the streamed pack to a temp file (64 KiB chunks) with the
// max_bytes cap enforced WHILE streaming.
func (l *Layer) stagePack(pack io.Reader, maxBytes int64) (stagedPack, error) {
	tmp, err := os.CreateTemp("", "walgit-packfeed-*")
	if err != nil {
		return stagedPack{}, err
	}
	started := time.Now()
	buf := make([]byte, 64<<10)
	var total int64
	for {
		n, rerr := pack.Read(buf)
		if n > 0 {
			total += int64(n)
			if maxBytes > 0 && total > maxBytes {
				tmp.Close()
				os.Remove(tmp.Name())
				return stagedPack{path: tmp.Name(), overErr: ErrMaxBytes}, nil
			}
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				tmp.Close()
				os.Remove(tmp.Name())
				return stagedPack{}, werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return stagedPack{}, rerr
		}
	}
	return stagedPack{path: tmp.Name(), size: total, feedSecs: time.Since(started).Seconds()}, tmp.Close()
}

// idxObjectCount reads fanout[255] from a pack idx (no subprocess).
func idxObjectCount(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var hdr [8]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return 0, err
	}
	if !bytes.Equal(hdr[:4], []byte{0xff, 't', 'O', 'c'}) {
		return 0, &PackRejectedError{Detail: "bad idx magic"}
	}
	fanout := make([]byte, 1024)
	if _, err := io.ReadFull(f, fanout); err != nil {
		return 0, err
	}
	return uint64(binary.BigEndian.Uint32(fanout[255*4:])), nil
}

// lastHexToken returns the last 40/64-hex token in s (index-pack stdout).
func lastHexToken(s string) string {
	tok := ""
	run := 0
	for i := range len(s) + 1 {
		if i < len(s) && isHex(s[i]) {
			run++
			continue
		}
		if run == 40 || run == 64 {
			tok = strings.ToLower(s[i-run : i])
		}
		run = 0
	}
	return tok
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func ctxErr(ctx context.Context) bool { return ctx.Err() != nil }

// parseTrace parses GIT_TRACE2_EVENT JSONL for metrics: git duration = t_abs
// of the last region_leave; phases from region_enter/leave pairs. Advisory
// only — a malformed trace file is ignored.
func (l *Layer) parseTrace(path string, started time.Time, feedSecs float64) *IngestMetrics {
	m := &IngestMetrics{WallSeconds: time.Since(started).Seconds(), FeedSeconds: feedSecs, Phases: map[string]float64{}}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	opens := map[string]float64{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var ev struct {
			Event  string          `json:"event"`
			Region string          `json:"region"`
			Repo   int             `json:"repo"`
			TAbs   json.RawMessage `json:"t_abs"`
			Data   string          `json:"data"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		var tAbs float64
		if json.Unmarshal(ev.TAbs, &tAbs) != nil {
			tAbs = 0
		}
		switch ev.Event {
		case "region_enter":
			opens[ev.Region] = tAbs
		case "region_leave":
			if open, ok := opens[ev.Region]; ok {
				m.Phases[ev.Region] += tAbs - open
				delete(opens, ev.Region)
			}
			if tAbs > m.GitSeconds {
				m.GitSeconds = tAbs
			}
		}
	}
	return m
}

func copyFile(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
