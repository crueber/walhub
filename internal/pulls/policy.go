package pulls

import (
	"context"
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
	doc, err := s.loadPolicy(ctx, owner, repo)
	if err != nil {
		return err
	}
	// Protect-only evaluation (policy.EvaluateProtect): the merge publish
	// is server-side, NOT a receive-pack push, so observation effects with
	// their own gates (required-reviews — consulted separately by runMerge
	// through the ReviewGate seam) must not deny here. Required-checks
	// runs through its own gate next (checkRequiredChecksGate in
	// checks.go — 05 §6 merge-time half, never the push path).
	v := policy.EvaluateProtect(ctx, doc, policy.Request{Principal: principal, Ref: ref, Op: op})
	if !v.Allow {
		return fmt.Errorf("rejected by rule '%s'", v.Rule)
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
