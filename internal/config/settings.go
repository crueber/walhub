package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// MaxRepoSettingsBytes is the serialized-settings payload budget (§4.2):
// larger payloads are rejected (400 at publish).
const MaxRepoSettingsBytes = 16 << 10

// MaxRepoDescriptionRunes bounds the per-repo short description (issue #235):
// a header-line display string, not a document. Single-line (no CR/LF);
// longer or multi-line payloads are rejected (400 at publish) like any other
// invalid settings key.
const MaxRepoDescriptionRunes = 512

// RepoSettings are the per-repo settings published into the WAL (D24): the
// allowed sections merged over the host config by Merge ("with_settings").
// Zero pointers mean "not set, inherit host config". Description is display
// metadata (issue #235): it rides the same TOML document — persistence,
// revisioning, authorship, and admin-only writes free — but it never merges
// into the host config (Merge ignores it). Features (Forgejo #522) is the
// same kind of passenger: six per-repo feature flags validated, persisted,
// and revisioned with the doc, but never merged (there is no host-config
// counterpart — an absent section means every feature enabled).
type RepoSettings struct {
	Description  string                    `toml:"description"`
	Features     *RepoFeatures             `toml:"features"`
	Bundles      *Bundles                  `toml:"bundles"`
	Maintenance  *Maintenance              `toml:"maintenance"`
	Compaction   *Compaction               `toml:"compaction"`
	Upstream     *Upstream                 `toml:"upstream"`
	Integrations map[string]toml.Primitive `toml:"integrations"` // accepted, forward-compat, never interpreted
}

// RepoFeatures are the six per-repo feature flags (Forgejo #522), carried
// as the [features] TOML section. Every key is an *enabled* flag: nil (key
// absent) means enabled, so an absent section — every existing repo —
// resolves to all-on with zero migration. Pointers (not plain bools) are
// what keep "unset" distinct from "explicitly disabled": a plain bool
// would read an absent key as false and strand every pre-#522 repo with
// all features off.
type RepoFeatures struct {
	Issues   *bool `toml:"issues"`
	Pulls    *bool `toml:"pulls"`
	Releases *bool `toml:"releases"`
	Forks    *bool `toml:"forks"`
	Watch    *bool `toml:"watch"`
	Star     *bool `toml:"star"`
}

// ResolvedFeatures are the effective flags: plain bools, always fully
// populated (absent keys resolved to enabled). This is what the summary
// projects and what the write guards consult — never a nil-meaning
// question at a read or write boundary.
type ResolvedFeatures struct {
	Issues   bool `json:"issues"`
	Pulls    bool `json:"pulls"`
	Releases bool `json:"releases"`
	Forks    bool `json:"forks"`
	Watch    bool `json:"watch"`
	Star     bool `json:"star"`
}

// AllFeatures is the zero-migration default: every feature enabled.
func AllFeatures() ResolvedFeatures {
	return ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: true, Watch: true, Star: true}
}

// Resolve maps the [features] section onto effective flags: nil section
// or nil key means enabled; an explicit false disables.
func (f *RepoFeatures) Resolve() ResolvedFeatures {
	out := AllFeatures()
	if f == nil {
		return out
	}
	if f.Issues != nil {
		out.Issues = *f.Issues
	}
	if f.Pulls != nil {
		out.Pulls = *f.Pulls
	}
	if f.Releases != nil {
		out.Releases = *f.Releases
	}
	if f.Forks != nil {
		out.Forks = *f.Forks
	}
	if f.Watch != nil {
		out.Watch = *f.Watch
	}
	if f.Star != nil {
		out.Star = *f.Star
	}
	return out
}

// ETagBits renders the resolved flags as six fixed-order 0/1 chars
// (issues, pulls, releases, forks, watch, star): the short summary-ETag
// suffix covering a settings-only flip with no ref move (the #235/#505
// suffix discipline). Fixed order and fixed length — always six chars,
// so the suffix is stable and greppable.
func (f ResolvedFeatures) ETagBits() string {
	bits := []byte{'1', '1', '1', '1', '1', '1'}
	if !f.Issues {
		bits[0] = '0'
	}
	if !f.Pulls {
		bits[1] = '0'
	}
	if !f.Releases {
		bits[2] = '0'
	}
	if !f.Forks {
		bits[3] = '0'
	}
	if !f.Watch {
		bits[4] = '0'
	}
	if !f.Star {
		bits[5] = '0'
	}
	return string(bits)
}

// FeaturesOf extracts the resolved feature flags from a stored settings
// TOML body, returning all-enabled for empty bodies and for bodies that
// no longer parse (display metadata must never fail a read path — the
// summary renders all-on then, exactly like DescriptionOf renders "").
func FeaturesOf(body []byte) ResolvedFeatures {
	if len(bytes.TrimSpace(body)) == 0 {
		return AllFeatures()
	}
	rs, err := ParseRepoSettings(body)
	if err != nil {
		return AllFeatures()
	}
	return rs.Features.Resolve()
}

// hostOnlyRepoSettings sections produce a clearer error than "unknown".
var hostOnlyRepoSettings = map[string]bool{
	"server": true,
	"store":  true,
	"wal":    true,
	"cache":  true,
	"auth":   true,
}

// ParseRepoSettings validates and decodes a settings payload (§4.2): size
// budget, allowed sections only ([features] [bundles] [maintenance]
// [compaction] [upstream], plus the top-level description key and
// [integrations] stored verbatim), and host-only keys
func ParseRepoSettings(payload []byte) (*RepoSettings, error) {
	if len(payload) > MaxRepoSettingsBytes {
		return nil, fmt.Errorf("repo settings payload is %d bytes; limit is %d", len(payload), MaxRepoSettingsBytes)
	}
	var rs RepoSettings
	md, err := toml.NewDecoder(bytes.NewReader(payload)).Decode(&rs)
	if err != nil {
		return nil, fmt.Errorf("repo settings: %v", err)
	}
	if rs.Upstream != nil && rs.Upstream.TokenEnv != "" {
		return nil, errors.New("repo settings: upstream.token_env is host-only and not settable via settings")
	}
	if err := ValidateRepoDescription(rs.Description); err != nil {
		return nil, err
	}
	for _, key := range md.Undecoded() {
		head := key[0]
		if head == "integrations" {
			continue // stored verbatim, forward-compat, never interpreted
		}
		if hostOnlyRepoSettings[head] {
			return nil, fmt.Errorf("repo settings: %q is not settable via settings", head)
		}
		return nil, fmt.Errorf("repo settings: unknown key %q", key.String())
	}
	return &rs, nil
}

// ValidateRepoDescription enforces the issue-#235 description shape: a
// single-line display string of at most MaxRepoDescriptionRunes runes. The
// empty string means "unset" and is always valid.
func ValidateRepoDescription(d string) error {
	if strings.ContainsAny(d, "\r\n\x00") {
		return errors.New("repo settings: description must be a single line")
	}
	if n := utf8.RuneCountInString(d); n > MaxRepoDescriptionRunes {
		return fmt.Errorf("repo settings: description is %d characters; limit is %d", n, MaxRepoDescriptionRunes)
	}
	return nil
}

// DescriptionOf extracts the description from a stored settings TOML body,
// returning "" for empty bodies and for bodies that no longer parse (display
// metadata must never fail a read path — the summary renders "" then).
func DescriptionOf(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	rs, err := ParseRepoSettings(body)
	if err != nil {
		return ""
	}
	return rs.Description
}

// Merge ("with_settings"): pointer-set fields override base; unset sections
// inherit. upstream.token_env never comes from settings — the base value is
// preserved. Description and Features never merge: they are display/behavior
// metadata, not config (there is no host-config counterpart).
func (r *RepoSettings) Merge(base *Config) (*Config, error) {
	c := *base
	if r.Bundles != nil {
		c.Bundles = *r.Bundles
	}
	if r.Maintenance != nil {
		c.Maintenance = *r.Maintenance
	}
	if r.Compaction != nil {
		c.Compaction = *r.Compaction
	}
	if r.Upstream != nil {
		u := base.Upstream
		if r.Upstream.Git != "" {
			u.Git = r.Upstream.Git
		}
		if r.Upstream.Lfs != "" {
			u.Lfs = r.Upstream.Lfs
		}
		if len(r.Upstream.Follow) != 0 {
			u.Follow = r.Upstream.Follow
		}
		c.Upstream = u
	}
	// Integrations are stored verbatim and never interpreted.
	return &c, nil
}

// ValidateAgainst runs the host build's with_settings merge + full §5
// validation against the would-be effective config (§4.2): a settings payload
// must pass the same fail-closed validation as a host config, restricted to
// the merged sections.
func (r *RepoSettings) ValidateAgainst(base *Config) error {
	merged, err := r.Merge(base)
	if err != nil {
		return err
	}
	_, errs := Validate(merged)
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return errors.New(strings.Join(msgs, "; "))
}
