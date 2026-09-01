package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/policy"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- policy endpoints (§10). The policy document lives in the object store
// at repos/<o>/<r>/policy.json (CAS discipline); parse via internal/policy —
// fail closed: an unparseable file is a 400 on PUT.

func policyKey(id git.RepoId) string { return id.StorePrefix() + "policy.json" }

const allowAllPolicy = `{"version":1,"groups":[],"rules":[]}`

// loadPolicy reads and parses the stored policy; missing = allow-all (nil doc).
func loadPolicy(ctx context.Context, e *Env, id git.RepoId) (*policy.Document, []byte, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, e.Store, policyKey(id), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, []byte(allowAllPolicy), "", nil
		}
		return nil, nil, "", err
	}
	doc, err := policy.Parse(raw)
	if err != nil {
		return nil, raw, meta.Version, err
	}
	return doc, raw, meta.Version, nil
}

func (h *handlers) policyGet(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	_, raw, _, err := loadPolicy(r.Context(), h.env, RepoOf(r))
	if err != nil {
		// Fail closed: surface the parse error as plain text (GET of a
		// corrupt doc is a 503 so nobody mistakes it for allow-all).
		writePlain(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, json.RawMessage(raw))
}

func (h *handlers) policyPut(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthAdmin) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writePlain(w, http.StatusRequestEntityTooLarge, "policy body too large")
		return
	}
	if _, err := policy.Parse(body); err != nil {
		// Invalid → 400 with a plain-text reason list; nothing written (§10).
		writePlain(w, http.StatusBadRequest, err.Error())
		return
	}
	id := RepoOf(r)
	ctx := r.Context()
	_, _, cur, err := loadPolicy(ctx, h.env, id)
	if err != nil {
		writePlain(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	mode := store.PutCreate
	opts := store.PutOptions{Mode: mode, ContentType: "application/json"}
	if cur != "" {
		opts.Mode = store.PutUpdate
		opts.IfVersion = cur
	}
	if _, err := h.env.Store.Put(ctx, policyKey(id), store.PutBody{Bytes: body}, opts); err != nil {
		if store.IsPreconditionFailed(err) {
			writePlain(w, http.StatusConflict, "policy changed concurrently; retry")
			return
		}
		writePlain(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, json.RawMessage(body))
}

func (h *handlers) policyDelete(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthAdmin) {
		return
	}
	if err := h.env.Store.Delete(r.Context(), policyKey(RepoOf(r)), ""); err != nil && !store.IsNotFound(err) {
		writePlain(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST …/policy/validate (§10) ----------------------------------------------------

type protectSummary struct {
	Rule   string   `json:"rule"`
	Refs   []string `json:"refs"`
	Ops    []string `json:"ops"`
	Bypass []string `json:"bypass"`
}

type policyValidateBody struct {
	OK      bool             `json:"ok"`
	Errors  []string         `json:"errors"`
	Rules   int              `json:"rules"`
	Groups  int              `json:"groups"`
	Protect []protectSummary `json:"protect"`
}

// protectSummaries compiles the protect-rule summary (§10: `protect` = the
// compiled protect-rule summary).
func protectSummaries(d *policy.Document) []protectSummary {
	out := []protectSummary{}
	if d == nil {
		return out
	}
	for _, rule := range d.Rules {
		p := rule.Protect()
		if p == nil {
			continue
		}
		refs := nonNil(rule.Match.Refs)
		out = append(out, protectSummary{
			Rule:   rule.Name,
			Refs:   refs,
			Ops:    nonNil(p.RestrictOps),
			Bypass: nonNil(p.Bypass),
		})
	}
	return out
}

func (h *handlers) policyValidate(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	res := validatePolicy(r.Context(), h.env, RepoOf(r), bytes.TrimSpace(body))
	writeCached(w, r, ccNoStore, "", http.StatusOK, res)
}

// validatePolicy parses the body (or, when empty, the stored policy) and
// reports {ok, errors[], rules, groups, protect}.
func validatePolicy(ctx context.Context, e *Env, id git.RepoId, body []byte) policyValidateBody {
	res := policyValidateBody{OK: true, Errors: []string{}, Protect: []protectSummary{}}
	doc, _, _, err := loadPolicyBytes(ctx, e, id, body)
	if err != nil {
		res.OK = false
		res.Errors = []string{err.Error()}
		return res
	}
	if doc == nil {
		return res // allow-all: zero rules, zero groups
	}
	res.Rules = len(doc.Rules)
	res.Groups = len(doc.Groups)
	res.Protect = protectSummaries(doc)
	return res
}

// loadPolicyBytes parses `body`, or the stored policy when body is empty.
func loadPolicyBytes(ctx context.Context, e *Env, id git.RepoId, body []byte) (*policy.Document, []byte, store.Version, error) {
	if len(body) == 0 {
		return loadPolicy(ctx, e, id)
	}
	doc, err := policy.Parse(body)
	if err != nil {
		return nil, nil, "", err
	}
	return doc, body, "", nil
}

// --- POST …/policy/dry-run?last=N (§10) -----------------------------------------------

type dryRunRef struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
	Force  bool   `json:"force"`
}

type dryRunResult struct {
	Seq       uint64      `json:"seq"`
	At        string      `json:"at"`
	Principal string      `json:"principal"`
	Atomic    bool        `json:"atomic"`
	Refs      []dryRunRef `json:"refs"`
}

type dryRunBody struct {
	Pushes  int            `json:"pushes"`
	Allowed int            `json:"allowed"`
	Denied  int            `json:"denied"`
	Results []dryRunResult `json:"results"`
}

func (h *handlers) policyDryRun(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	last := 10
	if s := r.URL.Query().Get("last"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 1000 {
			last = n
		}
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	ctx := r.Context()
	id := RepoOf(r)
	doc, _, _, err := loadPolicyBytes(ctx, h.env, id, bytes.TrimSpace(body))
	if err != nil {
		// An unparseable candidate policy cannot be evaluated: fail closed.
		writePlain(w, http.StatusBadRequest, err.Error())
		return
	}
	pushes, err := h.env.Repo.PushHistory(ctx, id, last)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	out := dryRunBody{Results: []dryRunResult{}}
	for _, p := range pushes {
		res := dryRunResult{
			Seq:       p.Seq,
			Principal: p.Principal,
			Atomic:    p.Atomic,
			At:        p.At.UTC().Format(time.RFC3339),
		}
		allOK := true
		for _, ref := range p.Refs {
			dr := dryRunRef{Name: ref.Name, Force: ref.Force}
			dr.OK, dr.Reason = evalPushRef(doc, p.Principal, ref)
			if !dr.OK {
				allOK = false
			}
			res.Refs = append(res.Refs, dr)
		}
		out.Pushes++
		if allOK {
			out.Allowed++
		} else {
			out.Denied++
		}
		out.Results = append(out.Results, res)
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, out)
}

// evalPushRef evaluates one ref update: op derived from the wire triple
// (create/delete/update/force-push; force pre-derived by the view via
// merge-base --is-ancestor semantics — §10). The dry-run never enforces.
func evalPushRef(d *policy.Document, principal string, ref PushRef) (bool, string) {
	op := "update"
	switch {
	case ref.Old == "" || isZeroHex(ref.Old):
		op = policy.OpCreate
	case ref.New == "" || isZeroHex(ref.New):
		op = policy.OpDelete
	case ref.Force:
		op = policy.OpForcePush
	}
	v := policy.Evaluate(context.Background(), d, policy.Request{Principal: principal, Ref: ref.Name, Op: op, Force: ref.Force})
	if v.Allow {
		return true, ""
	}
	return false, "rejected by rule '" + v.Rule + "'"
}

// isZeroHex reports the all-zero absent marker.
func isZeroHex(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
