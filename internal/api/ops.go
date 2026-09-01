package api

import (
	"net/http"
	"strconv"

	"git.packden.us/crueber/walhub/internal/git"
)

// --- ops (§12.2) ----------------------------------------------------------------------

// opsTable is the frozen op/param surface (§12.2).
var opsTable = []OpSpec{
	{Op: "fsck", Params: []OpParam{{Name: "connectivity", Values: []string{"1"}}}},
	{Op: "repair"},
	{Op: "follow"},
	{Op: "rev-index", Params: []OpParam{{Name: "pack"}}},
	{Op: "compact", Params: []OpParam{{Name: "force", Values: []string{"1"}}, {Name: "base", Values: []string{"1"}}}},
	{Op: "bundle", Params: []OpParam{{Name: "strategy"}, {Name: "slot"}}},
	{Op: "checkpoint", Params: []OpParam{{Name: "trigger"}}},
	{Op: "sync"},
	{Op: "rematerialize"},
}

// bundleStrategies lists the configured strategy names/kinds (§12.2).
func bundleStrategies(h *handlers, r *http.Request) []map[string]string {
	out := []map[string]string{}
	if h.env.Cfg == nil {
		return out
	}
	for _, s := range h.env.Cfg.Bundles.Strategy {
		out = append(out, map[string]string{"name": s.Name, "kind": s.Kind})
	}
	return out
}

func (h *handlers) opsList(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	available := opsTable
	if h.env.Tasks != nil {
		if t := h.env.Tasks.Ops(); len(t) > 0 {
			available = t
		}
	}
	_, recent, err := h.env.Tasks.List(r.Context(), RepoOf(r))
	if err != nil {
		recent = []TaskRecord{}
	}
	if recent == nil {
		recent = []TaskRecord{}
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, struct {
		Available        []OpSpec            `json:"available"`
		Recent           []TaskRecord        `json:"recent"`
		BundleStrategies []map[string]string `json:"bundle_strategies"`
	}{Available: available, Recent: recent, BundleStrategies: bundleStrategies(h, r)})
}

// opStart begins (or joins) an op and attaches the SSE stream (§12.2/§10.3).
func (h *handlers) opStart(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthWrite) {
		return
	}
	op := r.PathValue("op")
	spec := findOp(op)
	if spec == nil {
		writePlain(w, http.StatusNotFound, "unknown op: "+op)
		return
	}
	params := map[string]string{}
	qs := r.URL.Query()
	for _, p := range spec.Params {
		v := qs.Get(p.Name)
		if v == "" {
			if p.Values == nil { // bare param names are required (§12.2)
				writePlain(w, http.StatusBadRequest, "missing required param: "+p.Name)
				return
			}
			continue
		}
		if len(p.Values) > 0 {
			okParam := false
			for _, allowed := range p.Values {
				if allowed == v {
					okParam = true
					break
				}
			}
			if !okParam {
				writePlain(w, http.StatusBadRequest, "invalid "+p.Name+": "+v)
				return
			}
		}
		params[p.Name] = v
	}
	st, err := h.env.Tasks.Begin(r.Context(), RepoOf(r), op, params)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	s, ok := NewSSE(w, r)
	if !ok {
		writePlain(w, http.StatusNotAcceptable, "streaming unsupported")
		return
	}
	defer s.Close()
	s.pump(st)
}

func findOp(op string) *OpSpec {
	for i := range opsTable {
		if opsTable[i].Op == op {
			return &opsTable[i]
		}
	}
	return nil
}

// --- tasks (§12.3) ---------------------------------------------------------------------

type tasksBody struct {
	Hostname string       `json:"hostname"`
	Running  []TaskRecord `json:"running"`
	Recent   []TaskRecord `json:"recent"`
}

func (h *handlers) tasksList(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	id := RepoOf(r)
	running, recent, err := h.env.Tasks.List(r.Context(), id)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, tasksBody{
		Hostname: h.env.Hostname,
		Running:  nonNil(running),
		Recent:   nonNil(recent),
	})
}

// taskGet: the TaskRecord JSON, or — with Accept: text/event-stream — the
// attach stream (one task packet, replay, live, terminal; §12.3).
func (h *handlers) taskGet(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	id := RepoOf(r)
	taskID := r.PathValue("id")
	if acceptsSSE(r) {
		st, found, err := h.env.Tasks.Attach(r.Context(), id, taskID)
		if err != nil {
			mapViewErr(w, err)
			return
		}
		if !found {
			writePlain(w, http.StatusNotFound, "unknown task: "+taskID)
			return
		}
		s, ok := NewSSE(w, r)
		if !ok {
			writePlain(w, http.StatusNotAcceptable, "streaming unsupported")
			return
		}
		defer s.Close()
		s.pump(st)
		return
	}
	rec, ok, err := h.env.Tasks.Get(r.Context(), id, taskID)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	if !ok {
		// 404 if unknown on this instance (records are instance-memory only).
		writePlain(w, http.StatusNotFound, "unknown task: "+taskID)
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, rec)
}

// taskRecordFrom converts an engine record (bind_wal.go uses this so the
// TaskRecord conversion lives in exactly one place).
func taskRecordFrom(id, kind, repo, hostname string, startedMs int64, ok *bool, summary string, logTail []string, params map[string]string) TaskRecord {
	return TaskRecord{
		ID:       id,
		Kind:     kind,
		Repo:     repo,
		Hostname: hostname,
		Started:  strconv.FormatInt(startedMs, 10),
		OK:       ok,
		Summary:  summary,
		LogTail:  nonNil(logTail),
		Params:   params,
	}
}

var _ = git.RepoId{} // keep the import if the table changes shape
