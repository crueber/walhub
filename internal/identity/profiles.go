package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// Profile is users/<principal>/profile.json.
type Profile struct {
	Version     int    `json:"version"`
	Principal   string `json:"principal"`
	DisplayName string `json:"display_name"`
	Bio         string `json:"bio"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func encodeProfile(p *Profile) []byte {
	raw, _ := json.Marshal(p)
	return raw
}

func parseProfile(raw []byte) (*Profile, error) {
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: profile.json: %v", ErrInvalid, err)
	}
	return &p, nil
}

// GetProfile reads a profile; nil when absent. GET on a missing profile is
// the canonical "does this principal exist" probe — an O(1) GET, and there
// is deliberately no users LIST.
func (s *Service) GetProfile(ctx context.Context, principal string) (*Profile, error) {
	raw, _, err := store.GetBytes(ctx, s.Store, ProfileKey(principal), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseProfile(raw)
}

// EnsureProfile creates the profile lazily when absent (first authenticated
// request or first role granted); returns the existing one otherwise.
func (s *Service) EnsureProfile(ctx context.Context, principal string) (*Profile, error) {
	principal = normPrincipal(principal)
	if !ValidPrincipal(principal) {
		return nil, fmt.Errorf("%w: invalid principal %q", ErrInvalid, principal)
	}
	if p, err := s.GetProfile(ctx, principal); err != nil || p != nil {
		return p, err
	}
	now := s.nowUTC().Format(time.RFC3339)
	p := &Profile{Version: 1, Principal: principal, CreatedAt: now, UpdatedAt: now}
	if _, err := store.PutBytes(ctx, s.Store, ProfileKey(principal), encodeProfile(p),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		if store.IsPreconditionFailed(err) {
			return s.GetProfile(ctx, principal)
		}
		return nil, err
	}
	return p, nil
}

// PutProfile updates display_name/bio (self-or-admin, checked by the
// handler) via CAS.
func (s *Service) PutProfile(ctx context.Context, principal, displayName, bio string) (*Profile, error) {
	principal = normPrincipal(principal)
	var result *Profile
	_, err := s.casUpdate(ctx, ProfileKey(principal), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		now := s.nowUTC().Format(time.RFC3339)
		if cur == nil {
			result = &Profile{Version: 1, Principal: principal, DisplayName: displayName, Bio: bio, CreatedAt: now, UpdatedAt: now}
			return encodeProfile(result), true, nil
		}
		prev, perr := parseProfile(cur)
		if perr != nil {
			return nil, false, perr
		}
		prev.Version++
		prev.DisplayName = displayName
		prev.Bio = bio
		prev.UpdatedAt = now
		result = prev
		return encodeProfile(prev), true, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
