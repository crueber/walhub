package pulls

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	walgit "git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/policy"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// --- role helpers (P6, same ladder as internal/issues) -----------------------

// roleRank orders role names on the P6 ladder read < triage < write <
// maintain < admin.
func roleRank(role string) int {
	switch identity.Role(strings.ToLower(role)) {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

// roleOf resolves the actor's repo role ("" when none). Host admin/write
// flags short-circuit through identity's own resolution.
func (s *Service) roleOf(ctx context.Context, owner, repo string, p auth.Principal) string {
	if s.Roles == nil {
		if p.Admin {
			return string(identity.RoleAdmin)
		}
		if p.Write {
			return string(identity.RoleWrite)
		}
		if p.Anonymous {
			return ""
		}
		return string(identity.RoleRead)
	}
	role, _ := s.Roles.Resolve(ctx, owner, repo, p)
	return string(role)
}

// requireRole enforces a minimum repo role: host admin always passes;
// anonymous failures are 401, authenticated-but-insufficient are 403.
func (s *Service) requireRole(ctx context.Context, owner, repo string, p auth.Principal, want string) error {
	if p.Admin {
		return nil
	}
	got := s.roleOf(ctx, owner, repo, p)
	if roleRank(got) >= roleRank(want) {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return fmt.Errorf("%w: need %s", ErrForbidden, want)
}

// requireRead enforces the read gate (identity require_read hook when
// wired: public visibility or role ≥ read).
func (s *Service) requireRead(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Admin || p.Write {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous {
			return fmt.Errorf("%w", ErrUnauthorized)
		}
		return nil
	}
	if aerr := s.Roles.CheckRead(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("identity unavailable: %s", aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// requireAuthenticated rejects anonymous callers (open/comment/merge need a
// principal to attribute).
func requireAuthenticated(p auth.Principal) error {
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return nil
}

// --- protected-ref gate (§5 step 4) ------------------------------------------

// loadPolicy reads and parses repos/<o>/<r>/policy.json. A missing file is
// allow-all (anyone with the role may move any ref) — the only implicit
// default. An unparseable file fails closed (merge refused, never "skip
// policy").
func (s *Service) loadPolicy(ctx context.Context, owner, repo string) (*policy.Document, error) {
	raw, _, err := s.getJSON(ctx, PolicyKey(owner, repo))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return policy.Parse([]byte(`{"version":1,"rules":[]}`))
	}
	doc, perr := policy.Parse(raw)
	if perr != nil {
		return nil, fmt.Errorf("%w: policy.json unparseable: %v", ErrConflict, perr)
	}
	return doc, nil
}

// checkProtectedRef explicitly evaluates policy.json for (principal =
// merger, ref = base.ref, op) because the merge task's base-ref publish is
// NOT a receive-pack push (§5 step 4): protect rules deny the merge exactly
// as they would deny a push (plain-text `rejected by rule '<name>'`), and
// bypass lists (e.g. svc:merge-queue) apply unchanged. Evaluation is pure
// and local (no network in eval). op is create|update|delete|force-push.
func (s *Service) checkProtectedRef(ctx context.Context, owner, repo string, principal, ref, op string) error {
	if walgit.IsManagedRef(ref) {
		// Only the PR task publishes managed refs, server-side; a client
		// principal never satisfies this path (the push pipeline refuses
		// managed refs before policy). The task's own base/head publishes
		// are never managed refs — reaching here is a bug signal.
		return fmt.Errorf("%w: refused to publish managed ref %q through the policy path", ErrForbidden, ref)
	}
	if gerr := s.checkRequiredChecks(ctx, owner, repo); gerr != nil {
		return gerr
	}
	doc, err := s.loadPolicy(ctx, owner, repo)
	if err != nil {
		return err
	}
	// Protect-only evaluation (policy.EvaluateProtect): the merge publish
	// is server-side, NOT a receive-pack push, so observation effects with
	// their own gates (required-reviews — consulted separately by runMerge
	// through the ReviewGate seam) must not deny here. Required-checks
	// runs through its own pre-scan above (pending Wave 05).
	v := policy.EvaluateProtect(ctx, doc, policy.Request{Principal: principal, Ref: ref, Op: op})
	if !v.Allow {
		return fmt.Errorf("rejected by rule '%s'", v.Rule)
	}
	return nil
}

// checkRequiredChecks consults the required-checks gate (doc 05) when a
// rule carries it. It runs BEFORE the strict policy parse, scanning the raw
// policy.json structurally: internal/checks does not exist yet (Wave 05),
// so a rule that actually carries the gate fails closed here ("no checks
// backend") rather than dying opaquely at parse (protect is strict:
// required_checks would be an unknown key) or silently allowing. Plain
// protect rules (the only kind this tree can parse) never trigger it —
// evaluation is the protect rules above, and the gate is recorded as
// pending Wave 05. When 05 lands, this is where the head-sha check verdict
// is consulted instead, failing closed with the named checks.
func (s *Service) checkRequiredChecks(ctx context.Context, owner, repo string) error {
	raw, _, err := s.getJSON(ctx, PolicyKey(owner, repo))
	if err != nil || raw == nil {
		return err
	}
	var doc struct {
		Rules []struct {
			Name   string                     `json:"name"`
			Effect map[string]json.RawMessage `json:"effect"`
		} `json:"rules"`
	}
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil // unparseable ⇒ loadPolicy fails closed next; no gate verdict here
	}
	for _, r := range doc.Rules {
		for _, body := range r.Effect {
			var m map[string]any
			if jerr := json.Unmarshal(body, &m); jerr != nil {
				continue
			}
			for k := range m {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "required") && strings.Contains(lk, "check") {
					name := r.Name
					if name == "" {
						name = "unnamed"
					}
					return fmt.Errorf("%w: rule '%s' requires checks: no checks backend (Wave 05 pending)", ErrConflict, name)
				}
			}
		}
	}
	return nil
}

// --- message templates (§5 step 3) -------------------------------------------

// mergeMessage renders the default commit message for a strategy. Titles:
// merge — `Merge pull request #<num> from <head-ref-shorthand>`;
// squash/rebase — the head's subject (+ body = squashed head messages or
// the request's commit_message). commit_title/commit_message overrides win.
// Default-branch merges of protected refs append `(<full sha>)` per GitHub
// convention — the UI renders the sha link.
func mergeMessage(strategy string, num int, headRef, headSubject, headBody, titleOverride, msgOverride string) (title, body string) {
	short := headRef
	if i := strings.LastIndex(headRef, "/"); i >= 0 {
		short = headRef[i+1:]
	}
	switch strategy {
	case StrategyMerge:
		title = fmt.Sprintf("Merge pull request #%d from %s", num, short)
		body = strings.TrimSpace(headSubject)
		if headBody != "" {
			body += "\n\n" + strings.TrimSpace(headBody)
		}
	case StrategySquash, StrategyRebase:
		title = headSubject
		if title == "" {
			title = fmt.Sprintf("Merge pull request #%d from %s", num, short)
		}
		body = strings.TrimSpace(headBody)
	default:
		title = fmt.Sprintf("Merge pull request #%d from %s", num, short)
	}
	if titleOverride != "" {
		title = titleOverride
	}
	if msgOverride != "" {
		body = msgOverride
	}
	return title, body
}

// fullMessage joins title + body into the commit-tree -m message.
func fullMessage(title, body string) string {
	if strings.TrimSpace(body) == "" {
		return title
	}
	return title + "\n\n" + body
}
