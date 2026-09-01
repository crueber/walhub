package policy

import "encoding/json"

// HistoryEffect — parsed and stored, NOT enforced (until this document
// says otherwise). Default if omitted: compiled floors. Combination: per
// field, the first rule that sets it wins.
type HistoryEffect struct {
	AllowedForwards *int64
	AllowUnrelated  *bool
}

func (h *HistoryEffect) Kind() string { return "history" }

func (h *HistoryEffect) Parse(raw json.RawMessage) error {
	if effectNull(raw) {
		return invalidf("effect history: must be an object")
	}
	m, err := effectObject(raw, "history")
	if err != nil {
		return err
	}
	h.AllowedForwards, h.AllowUnrelated = nil, nil
	for k, v := range m {
		switch k {
		case "allowed_forwards":
			var n int64
			if err := json.Unmarshal(v, &n); err != nil || n < 0 {
				return invalidf("effect history.allowed_forwards: must be a non-negative integer")
			}
			h.AllowedForwards = &n
		case "allow_unrelated":
			var b bool
			if err := json.Unmarshal(v, &b); err != nil {
				return invalidf("effect history.allow_unrelated: must be a boolean")
			}
			h.AllowUnrelated = &b
		case "_comment":
		default:
			return invalidf("effect history: unknown key %q", k)
		}
	}
	return nil
}

// SizeEffect — parsed and stored, NOT enforced. Default if omitted:
// compiled ceilings (the effect exists to RAISE a ceiling; most-restrictive
// -wins would delete the feature). Combination: first match.
type SizeEffect struct {
	BlobBytes *int64
	PushBytes *int64
}

func (s *SizeEffect) Kind() string { return "size" }

func (s *SizeEffect) Parse(raw json.RawMessage) error {
	if effectNull(raw) {
		return invalidf("effect size: must be an object")
	}
	m, err := effectObject(raw, "size")
	if err != nil {
		return err
	}
	s.BlobBytes, s.PushBytes = nil, nil
	for k, v := range m {
		switch k {
		case "blob_bytes":
			var n int64
			if err := json.Unmarshal(v, &n); err != nil || n < 0 {
				return invalidf("effect size.blob_bytes: must be a non-negative integer")
			}
			s.BlobBytes = &n
		case "push_bytes":
			var n int64
			if err := json.Unmarshal(v, &n); err != nil || n < 0 {
				return invalidf("effect size.push_bytes: must be a non-negative integer")
			}
			s.PushBytes = &n
		case "_comment":
		default:
			return invalidf("effect size: unknown key %q", k)
		}
	}
	return nil
}
