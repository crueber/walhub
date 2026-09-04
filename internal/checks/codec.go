package checks

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidState marks a state outside the v1 enum (→ 409: the enum is
// closed and a re-report with a typo'd state must not silently become
// pending).
var ErrInvalidState = errors.New("invalid state")

// encodeStatus renders a status record (arrays stay []-shaped by
// construction — this object carries no arrays).
func encodeStatus(st *StatusDoc) []byte {
	raw, _ := json.Marshal(st)
	return raw
}

// parseStatus parses a status record (fail closed on corrupt bytes).
func parseStatus(raw []byte) (*StatusDoc, error) {
	var st StatusDoc
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("%w: status: %v", ErrCorrupt, err)
	}
	if st.SHA == "" || st.Context == "" || st.Creator == "" {
		return nil, fmt.Errorf("%w: status: missing required fields", ErrCorrupt)
	}
	return &st, nil
}

// encodeIndex renders the index projection.
func encodeIndex(ix *IndexDoc) []byte {
	raw, _ := json.Marshal(ix)
	return raw
}

// parseIndex parses the index projection (fail closed on corrupt bytes).
func parseIndex(raw []byte) (*IndexDoc, error) {
	var ix IndexDoc
	if err := json.Unmarshal(raw, &ix); err != nil {
		return nil, fmt.Errorf("%w: checks/index.json: %v", ErrCorrupt, err)
	}
	if ix.SHAs == nil {
		ix.SHAs = []IndexSHA{}
	}
	for i := range ix.SHAs {
		if ix.SHAs[i].Contexts == nil {
			ix.SHAs[i].Contexts = []IndexContext{}
		}
	}
	return &ix, nil
}

// encodeToken renders a CI token record.
func encodeToken(tok *CITokenDoc) []byte {
	raw, _ := json.Marshal(tok)
	return raw
}

// parseToken parses a CI token record (fail closed on corrupt bytes).
func parseToken(raw []byte) (*CITokenDoc, error) {
	var tok CITokenDoc
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("%w: ci token: %v", ErrCorrupt, err)
	}
	if tok.ID == "" || tok.TokenHash == "" {
		return nil, fmt.Errorf("%w: ci token: missing required fields", ErrCorrupt)
	}
	if tok.Scopes == nil {
		tok.Scopes = []string{}
	}
	return &tok, nil
}
