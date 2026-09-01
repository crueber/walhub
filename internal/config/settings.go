package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// MaxRepoSettingsBytes is the serialized-settings payload budget (§4.2):
// larger payloads are rejected (400 at publish).
const MaxRepoSettingsBytes = 16 << 10

// RepoSettings are the per-repo settings published into the WAL (D24): the
// allowed sections merged over the host config by Merge ("with_settings").
// Zero pointers mean "not set, inherit host config".
type RepoSettings struct {
	Bundles      *Bundles                  `toml:"bundles"`
	Maintenance  *Maintenance              `toml:"maintenance"`
	Compaction   *Compaction               `toml:"compaction"`
	Upstream     *Upstream                 `toml:"upstream"`
	Integrations map[string]toml.Primitive `toml:"integrations"` // accepted, forward-compat, never interpreted
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
// budget, allowed sections only ([bundles] [maintenance] [compaction]
// [upstream], plus [integrations] stored verbatim), and host-only keys
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

// Merge ("with_settings"): pointer-set fields override base; unset sections
// inherit. upstream.token_env never comes from settings — the base value is
// preserved.
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
