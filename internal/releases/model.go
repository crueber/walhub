package releases

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// This file owns the §1 shapes: the release header, asset entries, the
// latest pointer, and their validation. Wire mapping (browser_download_url,
// []-not-null) lives in http.go; the service owns CAS discipline.

// Release is the CAS'd header (§1.1). Tag and TagSHA are immutable after
// creation; everything else mutates through the PUT upsert CAS loop.
// PublishedAt is nil while draft.
type Release struct {
	Tag         string       `json:"tag"`
	TagSHA      string       `json:"tag_sha"`
	Name        string       `json:"name"`
	Body        string       `json:"body"`
	Draft       bool         `json:"draft"`
	Prerelease  bool         `json:"prerelease"`
	Author      string       `json:"author"`
	CreatedAt   string       `json:"created_at"`
	PublishedAt *string      `json:"published_at"`
	UpdatedAt   string       `json:"updated_at"`
	Assets      []AssetEntry `json:"assets"`
	Version     int          `json:"version"`
}

// AssetEntry is one row of Release.Assets (§1.1): sha256 + size live in the
// header; the byte object itself carries no metadata.
type AssetEntry struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	UploadedAt  string `json:"uploaded_at"`
	Uploader    string `json:"uploader"`
}

// LatestPointer is releases/latest.json (§2): the O(1) hot-read target.
// CreatedAt is the TARGET release's created_at (the monotonic field).
type LatestPointer struct {
	Tag       string `json:"tag"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ReleaseInput is the PUT body (§3, all optional): name defaults to the
// tag, body to "", draft/prerelease to false.
type ReleaseInput struct {
	Name       *string `json:"name"`
	Body       *string `json:"body"`
	Draft      *bool   `json:"draft"`
	Prerelease *bool   `json:"prerelease"`
}

// encodeTag percent-encodes a tag into one key segment (§1: `%` → `%25`,
// `/` → `%2F`; walhub does NOT lowercase or otherwise normalize tags).
// `%` is encoded first so the `/` escape never double-encodes.
func encodeTag(tag string) string {
	return strings.ReplaceAll(strings.ReplaceAll(tag, "%", "%25"), "/", "%2F")
}

// decodeTag reverses encodeTag (API paths decode per segment per 07_api
// §2; handlers MUST re-encode when building the key).
func decodeTag(enc string) string {
	d, err := url.PathUnescape(enc)
	if err != nil {
		return enc
	}
	return d
}

// validateTag requires a non-empty tag that fits one key segment. Git tag
// names MAY contain `/` (encoded, never rejected here); resolution against
// refs/tags/<tag> happens at write time (§1.1).
func validateTag(tag string) (string, error) {
	t := strings.TrimSpace(tag)
	if t == "" {
		return "", fmt.Errorf("%w: tag must not be empty", ErrInvalid)
	}
	if len(t) > 500 {
		return "", fmt.Errorf("%w: tag exceeds 500 bytes", ErrInvalid)
	}
	return t, nil
}

// validateAssetName enforces the single-segment rule (§1.2): no `/`, no
// leading `.`, 1–200 bytes, decoded exactly once by the caller.
func validateAssetName(name string) (string, error) {
	if len(name) < 1 || len(name) > MaxAssetNameLen {
		return "", fmt.Errorf("%w: asset name must be 1–%d bytes", ErrInvalid, MaxAssetNameLen)
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: asset name must be a single path segment", ErrInvalid)
	}
	if strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("%w: asset name must not start with '.'", ErrInvalid)
	}
	if name != strings.TrimSpace(name) {
		return "", fmt.Errorf("%w: asset name must not have surrounding whitespace", ErrInvalid)
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7f {
			return "", fmt.Errorf("%w: asset name must not contain control characters", ErrInvalid)
		}
	}
	return name, nil
}

// normalizeSHA256 lowercases and validates a 64-hex asset digest.
func normalizeSHA256(hexSHA string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(hexSHA))
	if len(s) != 64 {
		return "", fmt.Errorf("%w: X-Walgit-Asset-Sha256 must be 64 hex characters", ErrInvalid)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("%w: X-Walgit-Asset-Sha256 must be hex", ErrInvalid)
	}
	return s, nil
}

// normalizeContentType defaults an empty upload content type and bounds it.
func normalizeContentType(ct string) (string, error) {
	c := strings.TrimSpace(ct)
	if c == "" {
		return "application/octet-stream", nil
	}
	if len(c) > MaxContentTypeLen {
		return "", fmt.Errorf("%w: content type exceeds %d bytes", ErrInvalid, MaxContentTypeLen)
	}
	return c, nil
}

// validateReleaseInput bounds the mutable PUT fields.
func validateReleaseInput(in ReleaseInput) (name, body string, err error) {
	name = ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
		if len([]rune(name)) > MaxNameLen {
			return "", "", fmt.Errorf("%w: name exceeds %d characters", ErrInvalid, MaxNameLen)
		}
		if name == "" {
			return "", "", fmt.Errorf("%w: name must not be empty", ErrInvalid)
		}
	}
	body = ""
	if in.Body != nil {
		body = *in.Body
		if len(body) > MaxBodyBytes {
			return "", "", fmt.Errorf("%w: body exceeds %d bytes", ErrInvalid, MaxBodyBytes)
		}
	}
	return name, body, nil
}

// parseRelease decodes a release header (unknown fields ignored on read).
func parseRelease(raw []byte) (*Release, error) {
	var r Release
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%w: release: %v", ErrCorrupt, err)
	}
	if r.Assets == nil {
		r.Assets = []AssetEntry{}
	}
	return &r, nil
}

// encodeRelease serializes a header ([]-not-null for assets).
func encodeRelease(r *Release) []byte {
	if r.Assets == nil {
		r.Assets = []AssetEntry{}
	}
	raw, _ := json.Marshal(r)
	return raw
}

// findAsset returns the index of name in r.Assets, or -1.
func findAsset(r *Release, name string) int {
	for i, a := range r.Assets {
		if a.Name == name {
			return i
		}
	}
	return -1
}

// parseLatest decodes the latest pointer.
func parseLatest(raw []byte) (*LatestPointer, error) {
	var p LatestPointer
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: latest: %v", ErrCorrupt, err)
	}
	return &p, nil
}

// AutodraftPR is one merged-PR row of the autodraft response (§3).
type AutodraftPR struct {
	Num    int    `json:"num"`
	Title  string `json:"title"`
	Author string `json:"author"`
}

// Autodraft is the GET autodraft response (§3): suggested body plus the
// merged PRs it was built from (newest merge first, capped at 100,
// more=true beyond).
type Autodraft struct {
	Tag   string        `json:"tag"`
	Since string        `json:"since"`
	Body  string        `json:"body"`
	PRs   []AutodraftPR `json:"prs"`
	More  bool          `json:"more"`
}

// draftBody renders the suggested body: "- #12 Title (@author)" lines,
// newest merge first.
func draftBody(prs []AutodraftPR) string {
	lines := make([]string, 0, len(prs))
	for _, p := range prs {
		lines = append(lines, fmt.Sprintf("- #%d %s (@%s)", p.Num, p.Title, p.Author))
	}
	return strings.Join(lines, "\n")
}
