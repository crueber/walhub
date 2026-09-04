package social

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file owns the §§4–6 shapes. SocialDoc is field-compatible with
// internal/notify's SocialDoc (06 §2 consumes watcher_list and
// watchers_truncated; this package MUST round-trip them on every write).

// SocialDoc is repos/<o>/<r>/meta/social.json (§§4–6, CAS'd): counters stay
// 07-compatible (stars/watchers/forks are COUNTS); the 06 §2 watcher array
// lives in watcher_list (capped at 1 000 by the writer, truncated-flagged).
// A repo with no social object reports all counts as 0 (§6).
type SocialDoc struct {
	Stars             int      `json:"stars"`
	Watchers          int      `json:"watchers"`
	Forks             int      `json:"forks"`
	WatcherList       []string `json:"watcher_list,omitempty"`
	WatchersTruncated bool     `json:"watchers_truncated,omitempty"`
	UpdatedAt         string   `json:"updated_at"`
}

// StarRecord is users/<principal>/starred/<o>/<r>.json (§4): Create/Delete
// only, never rewritten. The record is the source of truth for "did
// principal P star this repo"; the count is denormalized.
type StarRecord struct {
	Repo      string `json:"repo"`
	StarredAt string `json:"starred_at"`
}

// WatchRecord is users/<principal>/watching/<o>/<r>.json (§5, read-only
// here — 06 owns the write path).
type WatchRecord struct {
	Repo      string `json:"repo"`
	WatchedAt string `json:"watched_at"`
}

// StarEntry is one starred-list row (§7): the record plus nothing derived
// (repos that no longer exist are tolerated and returned as-is).
type StarEntry struct {
	Repo      string `json:"repo"`
	StarredAt string `json:"starred_at"`
}

// parseSocial decodes social.json (absent ⇒ zeros, never an error).
func parseSocial(raw []byte) (*SocialDoc, error) {
	var d SocialDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%w: social: %v", ErrCorrupt, err)
	}
	return &d, nil
}

// encodeSocial serializes social.json.
func encodeSocial(d *SocialDoc) []byte {
	raw, _ := json.Marshal(d)
	return raw
}

// parseStarRecord decodes a star record.
func parseStarRecord(raw []byte) (*StarRecord, error) {
	var r StarRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%w: star record: %v", ErrCorrupt, err)
	}
	return &r, nil
}

// splitStarKey parses users/<p>/starred/<o>/<r>.json into (o, r); false
// when the key is not a star record (index objects, if any, are skipped).
func splitStarKey(prefix, key string) (string, string, bool) {
	rest := strings.TrimPrefix(key, prefix)
	if rest == key || !strings.HasSuffix(rest, ".json") {
		return "", "", false
	}
	rest = strings.TrimSuffix(rest, ".json")
	o, r, ok := strings.Cut(rest, "/")
	if !ok || o == "" || r == "" || strings.Contains(r, "/") {
		return "", "", false
	}
	return o, r, true
}
