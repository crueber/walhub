// avatar.go — auto-generated user avatars (Forgejo #376, restyled #525,
// figures recolored #539, palette widened #550).
//
// Every OIDC login for a user with no avatar enqueues a background
// generation: the DiceBear "rings" style seeded by the verified
// email (seed stays the email even though #370 made the username the
// identity key), over a solid walhub-green (emerald-600 #059669)
// background in the greyscale-preset treatment (flat, no gradients),
// with the rings figures drawn from a greens-to-black override
// palette (emerald/green/teal 300→950 + black, userAvatarRingColors)
// instead of the style's built-in rainbow.
// The SVG lives on the bucket at
// users/<username>/avatar.svg (law 4 — memory is a cache, the bucket is
// truth) with the pointer on profile.json (avatar_content_type /
// avatar_updated_at, the #359 org-avatar shape). Deleting the avatar
// opts out (avatar_disabled) until the user regenerates.
//
// Determinism note: the seed feeds a seeded PRNG only — the same email
// renders the identical SVG every time, so deleting and re-enabling
// yields the same avatar unless the email changes.
//
// ### Concurrency
//
// Hazard: N concurrent logins for one principal stampeding one
// generation (N SVGs, N CAS loops). Avoidance: a process-local dedup
// set — one in-flight generation per principal behind a mutex that is
// never held across a store call. The task table is deliberately NOT
// used: it single-flights (repo, kind) for narrated repo work with
// progress packets, while avatar generation is a per-login
// fire-and-forget local computation with no client-visible progress
// (law 7's "no silent waiting" targets client-visible long work, and
// the login response already answered). The bucket CAS is the commit
// point; a duplicate generation across hosts last-writes byte-identical
// output (deterministic), so no coordination is needed beyond the
// in-process dedup.
package identity

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"

	dicebear "github.com/dicebear/dicebear-go/v10"
	"github.com/dicebear/styles/v10"

	"git.packden.us/crueber/walhub/internal/store"
)

// userAvatarContentType is the only user-avatar media type: generated
// SVGs, never uploads (unlike #359 org avatars, which accept PNG/JPEG/
// GIF/WebP uploads and reject SVG — same-origin served SVG is script
// execution in our origin, but THESE bytes are generated locally from
// the style definition, never authored by the user, and the seed is
// verified-absent from the output before the write).
const userAvatarContentType = "image/svg+xml"

// maxUserAvatarBytes caps one generated avatar (generation output is a
// few KiB; the cap fails closed on a pathological style definition).
const maxUserAvatarBytes = int64(1 << 20)

// userAvatarBackgroundColor is the flat avatar background: Tailwind
// emerald-600 (#059669), the canonical brand green (web/src/ui.css
// .btn.primary). ONE hue for both themes — the SVG is generated once
// server-side and served as static bytes, so it cannot theme-switch;
// emerald-600 reads on light and dark surfaces alike. Bare hex (no
// "#"): the dicebear-go v10 options schema accepts ^#?(3/4/6/8 hex)$
// and the renderer emits the #-prefixed fill itself. There is no
// "backgroundType" key in v10 (a v5-era name) — backgroundColor is
// the core-library option and backgroundColorFill defaults to solid,
// which is the flat greyscale-preset treatment (verified: output
// carries a single <rect> fill, no gradient elements).
const userAvatarBackgroundColor = "059669"

// userAvatarRingColors is the rings-figure palette: the Tailwind
// emerald, green, and teal runs 300→950 plus black (bare hex, no "#",
// same spelling the options schema accepts), ordered so the lightness
// ramp interleaves by step (all 300s, then 400s, …) down to the dark
// terminus. Passed as the "ringColor"
// core-library option (name+"Color" in the vendored
// dicebear-go/internal/render/options.go — a user-supplied color
// list overrides the style collection per resolver.go's
// r.options.color(name)), replacing the style's built-in 16-color
// rainbow (coral→blue→purple→pink) whose indigo-family figures
// clashed with the app-green background (Forgejo #539). Greens to
// black only, all from the app palette (web/src/ui.css) + black —
// no blue-adjacent hues (cyan reads as blue at avatar size), no
// rainbow colors — pinned by TestGenerateGreensOnlyFigures, which
// asserts none of the 16 style defaults appears in output across
// seeds. Every step is a real Tailwind shade; the SVG is generated
// once server-side and served as static bytes, so every entry must
// read on both light and dark surfaces. The 600 steps sit near the
// emerald-600 background hue — figure-on-figure there reads via the
// ring gaps.
var userAvatarRingColors = []string{
	"6ee7b7", // emerald-300
	"86efac", // green-300
	"5eead4", // teal-300
	"34d399", // emerald-400
	"4ade80", // green-400
	"2dd4bf", // teal-400
	"10b981", // emerald-500
	"22c55e", // green-500
	"14b8a6", // teal-500
	"059669", // emerald-600 (background hue — figure-on-figure reads via the ring gaps)
	"16a34a", // green-600 (near-background hue — reads via the ring gaps)
	"0d9488", // teal-600 (near-background hue — reads via the ring gaps)
	"047857", // emerald-700
	"15803d", // green-700
	"0f766e", // teal-700
	"065f46", // emerald-800
	"166534", // green-800
	"115e59", // teal-800
	"064e3b", // emerald-900
	"14532d", // green-900
	"134e4a", // teal-900
	"022c22", // emerald-950
	"052e16", // green-950
	"042f2e", // teal-950
	"000000", // black
}

// ringsStyle parses the DiceBear "rings" definition
// once per process: the definition is static JSON, so re-parsing per
// login would burn CPU on a hot path (law 6).
var ringsStyle = sync.OnceValues(func() (*dicebear.Style, error) {
	def, ok := styles.Get("rings")
	if !ok {
		return nil, fmt.Errorf("dicebear: unknown style %q", "rings")
	}
	return dicebear.NewStyle([]byte(def))
})

// GenerateUserAvatarSVG renders the deterministic avatar for seed
// (the user's verified email): DiceBear "rings" with a solid
// walhub-green background (userAvatarBackgroundColor) and
// greens-to-black figures (userAvatarRingColors via the "ringColor"
// core-library option — verified against the dicebear-go/v10 render
// + validation sources: the style defines no per-style options, but
// backgroundColor and <colorName>Color are core-library option keys
// and user-supplied colors override the style collection).
// The seed feeds the PRNG only and is deliberately excluded from the
// resolved options and the markup (verified by TestGenerateSeedAbsent);
// sanitize-first, the write is still refused when the raw seed appears
// in the output, when the document is not an <svg>, or when it carries
// a <script> element (fail closed — generated bytes must never become
// a script-injection vector in our origin).
func GenerateUserAvatarSVG(seed string) (string, error) {
	st, err := ringsStyle()
	if err != nil {
		return "", err
	}
	av, err := dicebear.NewAvatar(st, map[string]any{
		"seed":            seed,
		"backgroundColor": []string{userAvatarBackgroundColor},
		"ringColor":       userAvatarRingColors,
	})
	if err != nil {
		return "", fmt.Errorf("dicebear: generate: %w", err)
	}
	svg := av.SVG()
	if err := checkAvatarSVG(svg, seed); err != nil {
		return "", err
	}
	return svg, nil
}

// checkAvatarSVG is the sanitize-first gate on generated bytes.
func checkAvatarSVG(svg, seed string) error {
	if int64(len(svg)) > maxUserAvatarBytes {
		return fmt.Errorf("%w: generated avatar exceeds %d bytes", ErrTooLarge, maxUserAvatarBytes)
	}
	t := strings.TrimSpace(svg)
	if !strings.HasPrefix(t, "<svg") {
		return fmt.Errorf("%w: generated avatar is not an SVG document", ErrInvalid)
	}
	if strings.Contains(strings.ToLower(svg), "<script") {
		return fmt.Errorf("%w: generated avatar carries a script element", ErrInvalid)
	}
	if seed != "" && strings.Contains(svg, seed) {
		return fmt.Errorf("%w: seed leaked into generated avatar", ErrInvalid)
	}
	return nil
}

// UserAvatarKey returns users/<username>/avatar.svg. Usernames are
// key-safe (ValidUsername charset), so no encoding is needed — the
// UserKey precedent.
func UserAvatarKey(username string) string {
	return "users/" + normPrincipal(username) + "/avatar.svg"
}

// UserAvatarURL is the stable serving URL for username's avatar; v is
// the profile's avatar_updated_at — the ?v cache-buster (the #359
// pattern: immutable cache class, pointer timestamp busts it).
func UserAvatarURL(username, v string) string {
	u := "/api/v1/users/" + normPrincipal(username) + "/avatar"
	if v != "" {
		u += "?v=" + v
	}
	return u
}

// putUserAvatar stores svg bytes at users/<username>/avatar.svg and
// points profile.json at them (bytes first, pointer second — the
// #359 philosophy: orphaned bytes after a crashed pointer write are
// inert, while a pointer without bytes can never render). Creates the
// profile when absent (a first-login generation must not depend on an
// earlier EnsureProfile). Clears AvatarDisabled: a successful install
// is the user having an avatar.
func (s *Service) putUserAvatar(ctx context.Context, username, svg string) (*Profile, error) {
	username = normPrincipal(username)
	if !ValidPrincipal(username) {
		return nil, fmt.Errorf("%w: invalid principal %q", ErrInvalid, username)
	}
	if err := checkAvatarSVG(svg, ""); err != nil {
		return nil, err
	}
	if _, err := store.PutBytes(ctx, s.Store, UserAvatarKey(username), []byte(svg),
		store.PutOptions{Mode: store.PutOverwrite, ContentType: userAvatarContentType}); err != nil {
		return nil, err
	}
	var result *Profile
	_, err := s.casUpdate(ctx, ProfileKey(username), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		now := s.nowUTC().Format(time.RFC3339)
		if cur == nil {
			result = &Profile{Version: 1, Principal: username,
				AvatarContentType: userAvatarContentType, AvatarUpdatedAt: now,
				CreatedAt: now, UpdatedAt: now}
			return encodeProfile(result), true, nil
		}
		prev, perr := parseProfile(cur)
		if perr != nil {
			return nil, false, perr
		}
		prev.Version++
		prev.AvatarContentType = userAvatarContentType
		prev.AvatarUpdatedAt = now
		prev.AvatarDisabled = false
		prev.UpdatedAt = now
		result = prev
		return encodeProfile(prev), true, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetUserAvatar reads the avatar bytes plus the pointer profile; prof
// is nil when the user has no avatar (pointer unset or object missing
// — a pruned bucket still renders the no-avatar state, never an
// error, the #359 rule).
func (s *Service) GetUserAvatar(ctx context.Context, username string) ([]byte, *Profile, error) {
	username = normPrincipal(username)
	if !ValidPrincipal(username) {
		return nil, nil, fmt.Errorf("%w: invalid principal %q", ErrInvalid, username)
	}
	got, err := s.GetProfile(ctx, username)
	if err != nil {
		return nil, nil, err
	}
	if got == nil || got.AvatarContentType == "" {
		return nil, nil, nil
	}
	raw, _, err := store.GetBytes(ctx, s.Store, UserAvatarKey(username), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return raw, got, nil
}

// DeleteUserAvatar removes the avatar and opts out: the pointer clears
// and AvatarDisabled sets, so later logins must NOT regenerate until
// the user regenerates (opt back in). Idempotent: a missing avatar
// still records the opt-out (deleting "nothing" is the user saying
// "no avatar"). Unknown principals 404 (no profile to carry the flag
// — synthesis must not manufacture authority, the userExists rule).
func (s *Service) DeleteUserAvatar(ctx context.Context, username string) (*Profile, error) {
	username = normPrincipal(username)
	if !ValidPrincipal(username) {
		return nil, fmt.Errorf("%w: invalid principal %q", ErrInvalid, username)
	}
	var result *Profile
	_, err := s.casUpdate(ctx, ProfileKey(username), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown principal %q", ErrNotFound, username)
		}
		prev, perr := parseProfile(cur)
		if perr != nil {
			return nil, false, perr
		}
		if prev.AvatarContentType == "" && prev.AvatarDisabled {
			result = prev
			return nil, false, nil
		}
		prev.Version++
		prev.AvatarContentType = ""
		prev.AvatarUpdatedAt = ""
		prev.AvatarDisabled = true
		prev.UpdatedAt = s.nowUTC().Format(time.RFC3339)
		result = prev
		return encodeProfile(prev), true, nil
	})
	if err != nil {
		return nil, err
	}
	// Best-effort: the pointer is already clear, so leftover bytes are
	// inert (GetUserAvatar gates on the pointer).
	_ = s.Store.Delete(ctx, UserAvatarKey(username), "")
	return result, nil
}

// RegenerateUserAvatar regenerates the avatar synchronously (explicit
// user action — the POST /avatar path): clears the opt-out and
// installs a fresh deterministic render. The seed resolves through
// the username registry (EmailForUsername); a legacy email spelling
// seeds itself. An unmappable principal 404s — generation without a
// verified email would break the seed=email contract.
func (s *Service) RegenerateUserAvatar(ctx context.Context, username string) (*Profile, error) {
	username = normPrincipal(username)
	email, err := s.EmailForUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	if email == "" {
		if _, merr := mail.ParseAddress(username); merr == nil {
			email = username
		} else {
			return nil, fmt.Errorf("%w: no verified email for %q", ErrNotFound, username)
		}
	}
	svg, err := s.generate(email)
	if err != nil {
		return nil, err
	}
	return s.putUserAvatar(ctx, username, svg)
}

// generate is the injectable render step (tests stub counting/
// blocking renders for the single-flight proof); production is
// GenerateUserAvatarSVG.
func (s *Service) generate(seed string) (string, error) {
	if s.Generate != nil {
		return s.Generate(seed)
	}
	return GenerateUserAvatarSVG(seed)
}

// EnsureAvatarAsync enqueues a background generation for a login that
// found no avatar. It never blocks the caller (the login response
// already answered) and never generates twice for one principal:
// opted-out users and avatar holders return before the dedup set, and
// concurrent logins share one in-flight entry. Generation failures are
// best-effort drops — the next login retries (no error surface on a
// fire-and-forget path; the pointer write is the success signal).
func (s *Service) EnsureAvatarAsync(username, email string) {
	username = normPrincipal(username)
	email = normPrincipal(email)
	if username == "" || email == "" {
		return
	}
	s.avatarMu.Lock()
	if s.avatarBusy == nil {
		s.avatarBusy = map[string]struct{}{}
	}
	if _, ok := s.avatarBusy[username]; ok {
		s.avatarMu.Unlock()
		return
	}
	s.avatarBusy[username] = struct{}{}
	s.avatarMu.Unlock()
	go func() {
		defer func() {
			s.avatarMu.Lock()
			delete(s.avatarBusy, username)
			s.avatarMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 30*time.Second)
		defer cancel()
		if got, err := s.GetProfile(ctx, username); err != nil || got != nil &&
			(got.AvatarContentType != "" || got.AvatarDisabled) {
			return
		}
		svg, err := s.generate(email)
		if err != nil {
			return
		}
		// Re-check the pointer before the write: a concurrent explicit
		// opt-out between the probe and the render must win (the
		// delete's CAS is the authority, but skipping a doomed write
		// keeps the loser's bytes off the bucket).
		if got, err := s.GetProfile(ctx, username); err != nil || got != nil &&
			(got.AvatarContentType != "" || got.AvatarDisabled) {
			return
		}
		_, _ = s.putUserAvatar(ctx, username, svg)
	}()
}

// UserAvatarURL reports the stable avatar URL for username ("" when
// the user has none): the api.Env Avatars hook behind GET /api/v1/me's
// avatar_url (law 8 — core never imports this package). One exact-key
// profile GET; probe errors fail closed to "" (display metadata must
// never fail the me() call — the Orgs fail-open precedent inverted to
// fail-closed is wrong here: "" renders the username fallback, which
// is the correct no-avatar state, never an error).
func (s *Service) UserAvatarURL(ctx context.Context, username string) string {
	got, err := s.GetProfile(ctx, normPrincipal(username))
	if err != nil || got == nil || got.AvatarContentType == "" {
		return ""
	}
	return UserAvatarURL(username, got.AvatarUpdatedAt)
}
