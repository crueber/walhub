package api

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
)

// --- fakes (handlers stay independent of internal/wal — 15_testing.md) --------------

const (
	fakeSHA  = "cb38da1b23e56a2b3c4d5e6f708192a3b4c5d6e7"
	fakeSHA2 = "1111111111111111111111111111111111111111"
	fakeTag  = "2222222222222222222222222222222222222222"
)

type fakeView struct {
	mu        sync.Mutex
	resolves  map[string]Resolution
	heads     map[string]Ref
	lists     map[string][]Ref
	more      map[string]bool
	trees     map[string]TreeResult
	blobs     map[string]BlobResult
	blobRaw   map[string][]byte
	commitPg  map[string]CommitPage
	commits   map[string]CommitDetail
	summaries map[string]SummaryData
	overviews map[string]OverviewData
	settings  map[string]SettingsDoc
	history   map[string]SettingsHistory
	headSeq   map[string]uint64
	pushes    map[string][]PushRecord
	published map[string][]byte
	synced    []string
}

func newFakeView() *fakeView {
	return &fakeView{
		resolves:  map[string]Resolution{},
		heads:     map[string]Ref{},
		lists:     map[string][]Ref{},
		more:      map[string]bool{},
		trees:     map[string]TreeResult{},
		blobs:     map[string]BlobResult{},
		blobRaw:   map[string][]byte{},
		commitPg:  map[string]CommitPage{},
		commits:   map[string]CommitDetail{},
		summaries: map[string]SummaryData{},
		overviews: map[string]OverviewData{},
		history:   map[string]SettingsHistory{},
		published: map[string][]byte{},
		headSeq:   map[string]uint64{},
		pushes:    map[string][]PushRecord{},
	}
}

func (f *fakeView) Sync(_ context.Context, id git.RepoId, level SyncLevel) error {
	if id.Owner == "missing" {
		return fmt.Errorf("%w: unknown repository", ErrNotFound)
	}
	f.mu.Lock()
	f.synced = append(f.synced, id.String()+"/"+level.String())
	f.mu.Unlock()
	return nil
}

func (f *fakeView) Resolve(_ context.Context, id git.RepoId, rest string) (Resolution, error) {
	r, ok := f.resolves[id.String()+"/"+rest]
	if !ok {
		return Resolution{}, fmt.Errorf("%w: unknown revision", ErrNotFound)
	}
	return r, nil
}

func (f *fakeView) Head(_ context.Context, id git.RepoId) (Ref, bool, error) {
	r, ok := f.heads[id.String()]
	return r, ok, nil
}

func (f *fakeView) RefList(_ context.Context, id git.RepoId, ns string, q RefQuery) ([]Ref, bool, error) {
	all := f.lists[id.String()+"/"+ns]
	if all == nil {
		return []Ref{}, false, nil
	}
	short := func(name string) string {
		return strings.TrimPrefix(name, "refs/heads/")
	}
	lower := strings.ToLower(q.Q)
	out := []Ref{}
	for _, r := range all {
		if !strings.HasPrefix(r.Name, "refs/"+ns+"/"+q.Prefix) {
			continue
		}
		if lower != "" && !strings.Contains(strings.ToLower(short(r.Name)), lower) {
			continue
		}
		if q.After != "" && r.Name <= q.After {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	more := len(out) > q.N
	if more {
		out = out[:q.N]
	}
	return out, more, nil
}

func (f *fakeView) Tree(_ context.Context, id git.RepoId, rev, path string) (TreeResult, error) {
	tr, ok := f.trees[id.String()+"|"+rev+"|"+path]
	if !ok {
		return TreeResult{}, fmt.Errorf("%w: not a tree object", ErrNotFound)
	}
	return tr, nil
}

func (f *fakeView) Blob(_ context.Context, id git.RepoId, rev, path string, raw bool) (BlobResult, error) {
	if raw {
		b, ok := f.blobRaw[id.String()+"|"+rev+"|"+path]
		if !ok {
			return BlobResult{}, fmt.Errorf("%w: path does not exist", ErrNotFound)
		}
		return BlobResult{Contents: b, Size: int64(len(b))}, nil
	}
	br, ok := f.blobs[id.String()+"|"+rev+"|"+path]
	if !ok {
		return BlobResult{}, fmt.Errorf("%w: path does not exist", ErrNotFound)
	}
	return br, nil
}

func (f *fakeView) Commits(_ context.Context, id git.RepoId, ref, path string, skip, n int) (CommitPage, error) {
	p, ok := f.commitPg[id.String()+"|"+ref+"|"+path+"|"+strconv.Itoa(skip)+"|"+strconv.Itoa(n)]
	if !ok {
		return CommitPage{}, fmt.Errorf("%w: unknown revision", ErrNotFound)
	}
	return p, nil
}

func (f *fakeView) Commit(_ context.Context, id git.RepoId, sha string) (CommitDetail, error) {
	d, ok := f.commits[id.String()+"|"+sha]
	if !ok {
		return CommitDetail{}, fmt.Errorf("%w: bad revision", ErrNotFound)
	}
	return d, nil
}

func (f *fakeView) Summary(_ context.Context, id git.RepoId) (SummaryData, error) {
	s, ok := f.summaries[id.String()]
	if !ok {
		return SummaryData{}, fmt.Errorf("%w: unknown repository", ErrNotFound)
	}
	return s, nil
}

func (f *fakeView) Overview(_ context.Context, id git.RepoId) (OverviewData, error) {
	ov, ok := f.overviews[id.String()]
	if !ok {
		return OverviewData{}, fmt.Errorf("%w: unknown repository", ErrNotFound)
	}
	return ov, nil
}

func (f *fakeView) Settings(_ context.Context, id git.RepoId) (SettingsDoc, error) {
	if body, ok := f.published[id.String()]; ok {
		return SettingsDoc{
			Revision:  3,
			Author:    "jane",
			UpdatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			Message:   "set",
			TOML:      string(body),
		}, nil
	}
	return SettingsDoc{}, nil // revision 0 = none ever published
}

func (f *fakeView) PublishSettings(_ context.Context, id git.RepoId, body []byte, _, _ string) (uint64, error) {
	f.published[id.String()] = body
	return 4, nil
}

func (f *fakeView) SettingsHistory(_ context.Context, id git.RepoId) (SettingsHistory, error) {
	h, ok := f.history[id.String()]
	if !ok {
		return SettingsHistory{Entries: []SettingsEntry{}}, nil
	}
	return h, nil
}

func (f *fakeView) HeadSeq(_ context.Context, id git.RepoId) (uint64, error) {
	return f.headSeq[id.String()], nil
}

func (f *fakeView) PushHistory(_ context.Context, id git.RepoId, last int) ([]PushRecord, error) {
	return f.pushes[id.String()], nil
}

type fakeRegistry struct {
	owners  []string
	repos   map[string][]string
	created map[string]bool
	fail    error
}

func (f *fakeRegistry) Owners(_ context.Context) ([]string, error) {
	out := []string{}
	for o := range f.repos {
		out = append(out, o)
	}
	sort.Strings(out)
	return out, f.fail
}

func (f *fakeRegistry) Repos(_ context.Context, owner string) ([]string, error) {
	return append([]string{}, f.repos[owner]...), f.fail
}

func (f *fakeRegistry) Exists(_ context.Context, id git.RepoId) (bool, error) {
	return f.created[id.String()], f.fail
}

func (f *fakeRegistry) Create(_ context.Context, id git.RepoId, _ git.ObjectFormat) error {
	if f.created[id.String()] {
		return fmt.Errorf("%w: exists", ErrExists)
	}
	f.created[id.String()] = true
	return nil
}

func (f *fakeRegistry) Delete(_ context.Context, id git.RepoId) error {
	delete(f.created, id.String())
	return f.fail
}

type fakeTasks struct {
	ops     []OpSpec
	running []TaskRecord
	recent  []TaskRecord
	records map[string]TaskRecord
	streams map[string]TaskStream
	listErr error
}

func (f *fakeTasks) Ops() []OpSpec { return nil } // ops handler falls back to the frozen table

func (f *fakeTasks) List(_ context.Context, _ git.RepoId) ([]TaskRecord, []TaskRecord, error) {
	return f.running, f.recent, f.listErr
}

func (f *fakeTasks) Get(_ context.Context, _ git.RepoId, id string) (TaskRecord, bool, error) {
	r, ok := f.records[id]
	return r, ok, nil
}

func (f *fakeTasks) Begin(_ context.Context, _ git.RepoId, op string, params map[string]string) (TaskStream, error) {
	return f.streams["op:"+op], nil
}

func (f *fakeTasks) Attach(_ context.Context, _ git.RepoId, id string) (TaskStream, bool, error) {
	st, ok := f.streams[id]
	return st, ok, nil
}

func strptr(s string) *string { return new(s) }
