// fsck.go — unit 6 (§9.1): periodic connectivity audit. Runs only on a host
// whose local copy holds the whole pack set (gated in plan.go), over the
// serving copy, and writes fsck.pb as an Overwrite put.
package maintain

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// runFsck audits connectivity over the serving copy and publishes the report.
// The interval + full-copy predicates were already checked by Select.
func (m *Maintainer) runFsck(ctx context.Context, rep Repo, snap *Snapshot, t TaskLogger) (Outcome, string) {
	if m.opt.Fscker == nil {
		return OutcomeError, "fsck runner not wired"
	}
	binary := "git"
	if snap.Eff.Git.Binary != "" {
		binary = snap.Eff.Git.Binary
	}
	missing, problems, err := m.opt.Fscker.Fsck(ctx, binary, rep.Dir())
	if err != nil {
		return OutcomeError, err.Error()
	}
	missingTotal := uint64(len(missing))
	bounded := missing
	if len(bounded) > fsckMissingBound {
		bounded = bounded[:fsckMissingBound]
	}
	report := &proto.FsckReport{
		Seq:          snap.Manifest.HeadSeq,
		At:           ptrTs(m.now()),
		Host:         m.host(),
		Missing:      bounded,
		MissingTotal: missingTotal,
		Problems:     uint64(problems),
	}
	if st := m.store(); st != nil {
		if err := putFsckReport(ctx, st, rep.Prefix(), report); err != nil {
			return OutcomeError, "fsck.pb write: " + err.Error()
		}
	}
	m.metrics.missingObjects.Store(int64(missingTotal))
	t.Notice(fmt.Sprintf("fsck: %d missing, %d problems", missingTotal, problems))
	return OutcomeOK, fmt.Sprintf("%d missing, %d problems", missingTotal, problems)
}

// execFscker runs `git fsck --connectivity-only --no-dangling` (§9.1 exact
// argv) via exec.CommandContext — drain cancels the audit mid-run.
type execFscker struct{}

func (execFscker) Fsck(ctx context.Context, binary, dir string) (missing []string, problems int, err error) {
	cmd := exec.CommandContext(ctx, binary, "fsck", "--connectivity-only", "--no-dangling")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		// git fsck exits non-zero when it finds problems; the report still
		// parses. Only a launch failure (binary missing, context canceled)
		// is an error here.
		if _, lookErr := exec.LookPath(binary); lookErr != nil {
			return nil, 0, fmt.Errorf("fsck: %v", err)
		}
	}
	var all []byte
	all = append(all, stderr.Bytes()...)
	all = append(all, stdout.Bytes()...)
	sc := bufio.NewScanner(bytes.NewReader(all))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if oid, ok := missingOidFromLine(line); ok {
			missing = append(missing, oid)
			continue
		}
		if line != "" && !strings.HasPrefix(line, "Checking") && !strings.HasPrefix(line, "notice:") {
			problems++
		}
	}
	return missing, problems, nil
}

func missingOidFromLine(line string) (string, bool) {
	for _, kind := range []string{"missing blob", "missing tree", "missing commit", "missing tag"} {
		if oid, ok := strings.CutPrefix(line, kind+" "); ok {
			return strings.TrimSpace(oid), true
		}
	}
	return "", false
}
