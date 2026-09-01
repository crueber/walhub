package bundle

import (
	"fmt"
	"path"
	"sort"

	"git.packden.us/crueber/walhub/internal/config"
)

// Strategy kinds (§8.5).
const (
	KindFull        = "full"
	KindIncremental = "incremental"
)

// FilterBlobNone is the only supported filter family (§8.5: Filter is "" | "blob:none").
const FilterBlobNone = "blob:none"

// DefaultMinCommits is bundles.min_commits when unset (§8.7).
const DefaultMinCommits = 25

// DefaultKeep applies when a full strategy leaves keep unset (§8.5: default 2).
const DefaultKeep = 2

// Strategy is the validated per-strategy bundle plan (§8.5). Config conversion
// in FromConfigStrategy; defaults in DefaultStrategies.
type Strategy struct {
	Name        string   // unique, e.g. "weekly"
	Kind        string   // "full" | "incremental"
	Base        string   // required for incremental: name of the base strategy
	Schedule    Cron     // parsed §8.3 schedule
	Keep        int      // fulls only; fulls listed (default 2); keep on an incremental = config error
	BackfillMax int      // missing slots per pass; 0 = unlimited
	Chain       bool     // dailies default true
	Filter      string   // "" | "blob:none"
	Refs        []string // glob overrides of the global ref rule (§8.8)
	MinCommits  int      // 0 → bundles.min_commits
}

// EffectiveKeep resolves the listed-fulls count for a full strategy.
func (s *Strategy) EffectiveKeep() int {
	if s.Keep > 0 {
		return s.Keep
	}
	return DefaultKeep
}

// EffectiveMinCommits resolves the per-strategy gate against the host default.
func (s *Strategy) EffectiveMinCommits(defaultMin int) int {
	if s.MinCommits > 0 {
		return s.MinCommits
	}
	if defaultMin > 0 {
		return defaultMin
	}
	return DefaultMinCommits
}

// FromConfigStrategy converts one host/repo config strategy (§8.5), parsing the
// schedule fail-closed. defaultMinCommits fills MinCommits when unset.
func FromConfigStrategy(bs config.BundleStrategy, defaultMinCommits int) (Strategy, error) {
	cron, err := ParseSchedule(bs.Schedule)
	if err != nil {
		return Strategy{}, fmt.Errorf("strategy %q: %w", bs.Name, err)
	}
	s := Strategy{
		Name:        bs.Name,
		Kind:        bs.Kind,
		Base:        bs.Base,
		Schedule:    cron,
		Keep:        bs.Keep,
		BackfillMax: bs.BackfillMax,
		Chain:       bs.Chain,
		Filter:      bs.Filter,
		Refs:        bs.Refs,
		MinCommits:  bs.MinCommits,
	}
	return s, nil
}

// FromConfig converts and validates a whole strategy table (§8.5). Order is
// preserved: it is the backfill priority (§8.10).
func FromConfig(strategies []config.BundleStrategy, defaultMinCommits int) ([]Strategy, error) {
	out := make([]Strategy, 0, len(strategies))
	for _, bs := range strategies {
		s, err := FromConfigStrategy(bs, defaultMinCommits)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := ValidateStrategies(out); err != nil {
		return nil, err
	}
	return out, nil
}

// DefaultStrategies is the §8.5 default table: weekly full + daily + hourly.
func DefaultStrategies() []Strategy {
	weekly, _ := ParseSchedule("@weekly")
	daily, _ := ParseSchedule("@daily")
	hourly, _ := ParseSchedule("@hourly")
	return []Strategy{
		{Name: "weekly", Kind: KindFull, Schedule: weekly, Keep: DefaultKeep, BackfillMax: 1},
		{Name: "daily", Kind: KindIncremental, Base: "weekly", Schedule: daily, Chain: true, BackfillMax: 7},
		{Name: "hourly", Kind: KindIncremental, Base: "daily", Schedule: hourly, BackfillMax: 48},
	}
}

// ValidateStrategies is the §8.5 fail-closed validation: incremental requires
// base naming an existing strategy; the base graph must be acyclic; a whole
// chain shares one filter; keep on an incremental is an error; min_commits ≥ 0;
// backfill_max ≥ 0; filter is "" or blob:none; kind is full|incremental.
func ValidateStrategies(strats []Strategy) error {
	byName := make(map[string]*Strategy, len(strats))
	for i := range strats {
		s := &strats[i]
		if s.Name == "" {
			return fmt.Errorf("bundles.strategy[%d]: name is required", i)
		}
		if _, dup := byName[s.Name]; dup {
			return fmt.Errorf("bundles.strategy: duplicate name %q", s.Name)
		}
		byName[s.Name] = s
	}
	for i := range strats {
		s := &strats[i]
		switch s.Kind {
		case KindFull:
		case KindIncremental:
			if s.Base == "" {
				return fmt.Errorf("bundles.strategy %q: incremental strategy requires base", s.Name)
			}
			if _, ok := byName[s.Base]; !ok {
				return fmt.Errorf("bundles.strategy %q: base %q does not name a strategy", s.Name, s.Base)
			}
			if s.Keep != 0 {
				return fmt.Errorf("bundles.strategy %q: keep is a full-strategy concept", s.Name)
			}
		default:
			return fmt.Errorf("bundles.strategy %q: kind must be %q or %q", s.Name, KindFull, KindIncremental)
		}
		if s.BackfillMax < 0 {
			return fmt.Errorf("bundles.strategy %q: backfill_max must be >= 0", s.Name)
		}
		if s.MinCommits < 0 {
			return fmt.Errorf("bundles.strategy %q: min_commits must be >= 0", s.Name)
		}
		if s.Filter != "" && s.Filter != FilterBlobNone {
			return fmt.Errorf("bundles.strategy %q: filter must be %q", s.Name, FilterBlobNone)
		}
		for _, g := range s.Refs {
			if g == "" {
				return fmt.Errorf("bundles.strategy %q: empty ref glob", s.Name)
			}
		}
	}
	// Acyclic base graph + one shared filter per chain (§8.5).
	for i := range strats {
		seen := map[string]bool{strats[i].Name: true}
		cur := &strats[i]
		for cur.Base != "" {
			if seen[cur.Base] {
				return fmt.Errorf("bundles.strategy %q: base graph has a cycle at %q", strats[i].Name, cur.Base)
			}
			seen[cur.Base] = true
			cur = byName[cur.Base]
			if strats[i].Filter != cur.Filter {
				return fmt.Errorf("bundles.strategy %q mixes filter values in its base chain: a filtered chain is all-or-nothing", strats[i].Name)
			}
		}
	}
	return nil
}

// ChainRoot walks base links to the strategy at the root of s's chain.
func ChainRoot(strats []Strategy, s *Strategy) *Strategy {
	byName := ByName(strats)
	cur := s
	for cur.Base != "" {
		b, ok := byName[cur.Base]
		if !ok {
			return cur
		}
		cur = b
	}
	return cur
}

// ByName indexes strategies by name.
func ByName(strats []Strategy) map[string]*Strategy {
	m := make(map[string]*Strategy, len(strats))
	for i := range strats {
		m[strats[i].Name] = &strats[i]
	}
	return m
}

// SelectRefs applies the §8.8 ref rules: main_only → HEAD + refs/heads/main;
// otherwise refs/heads/*, refs/tags/*, HEAD plus bundles.extra_refs globs; a
// strategy's own refs override all of it. Glob matching is path.Match on the
// full ref name; unmatched globs are not an error. Returns the selected subset
// of `refs` (sorted by name, HEAD first).
func SelectRefs(s *Strategy, mainOnly bool, extraRefs []string, refs []string) []string {
	var globs []string
	switch {
	case s != nil && len(s.Refs) > 0:
		globs = s.Refs
	case mainOnly:
		globs = []string{"HEAD", "refs/heads/main"}
	default:
		globs = append([]string{"HEAD", "refs/heads/*", "refs/tags/*"}, extraRefs...)
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		for _, g := range globs {
			if ok, _ := path.Match(g, r); ok {
				out = append(out, r)
				break
			}
		}
	}
	sort.Strings(out)
	// HEAD first, then the sorted rest (§8.9.4 header ordering).
	for i, r := range out {
		if r == "HEAD" && i != 0 {
			copy(out[1:i+1], out[0:i])
			out[0] = "HEAD"
			break
		}
	}
	return out
}
