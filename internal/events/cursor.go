// cursor.go — the per-repo delivery cursor, events/cursor.json (09 §5.1): same
// JSON fields and CAS semantics as the Rust implementation. The cursor is the
// bridge's only direct store access besides its own object.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
)

// cursorDoc is events/cursor.json (bucket-format compatible with Rust).
type cursorDoc struct {
	PublishedSeq uint64 `json:"published_seq"`
	UpdatedAt    string `json:"updated_at"` // RFC3339
}

// cursorKey is the full store key for a repo's cursor (repo-relative
// events/cursor.json under the repo's store prefix).
func cursorKey(repo string) (string, error) {
	id, err := git.ParseRepoId(repo)
	if err != nil {
		return "", err
	}
	return id.StorePrefix() + "events/cursor.json", nil
}

// loadCursor returns the stored cursor and its CAS version; found=false when
// the object is absent (cold cursor).
func loadCursor(ctx context.Context, st store.ObjectStore, key string) (cursorDoc, store.Version, bool, error) {
	res, err := st.Get(ctx, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return cursorDoc{}, "", false, nil
		}
		return cursorDoc{}, "", false, fmt.Errorf("events: read %s: %w", key, err)
	}
	obj, ok := res.(store.Object)
	if !ok {
		return cursorDoc{}, "", false, fmt.Errorf("events: read %s: unexpected result %T", key, res)
	}
	defer obj.Body.Close()
	var doc cursorDoc
	if err := json.NewDecoder(obj.Body).Decode(&doc); err != nil {
		return cursorDoc{}, "", false, fmt.Errorf("events: decode %s: %w", key, err)
	}
	return doc, obj.Meta.Version, true, nil
}

// casCursor advances the cursor to seq. A lost CAS (another bridge instance
// advanced it first) is SUCCESS: our emission was a duplicate and the dedup key
// (repo, _walgit.seq, ref_name) holds (09 §5.1 step 5). A cold cursor is
// created with PutCreate; losing that create is likewise a lost race.
func casCursor(ctx context.Context, st store.ObjectStore, key string, ver store.Version, found bool, seq uint64) error {
	body, err := json.Marshal(cursorDoc{PublishedSeq: seq, UpdatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return fmt.Errorf("events: encode %s: %w", key, err)
	}
	mode := store.PutUpdate
	opts := store.PutOptions{Mode: mode, IfVersion: ver}
	if !found {
		opts.Mode, opts.IfVersion = store.PutCreate, ""
	}
	if _, err := st.Put(ctx, key, store.PutBody{Bytes: body}, opts); err != nil {
		if store.IsPreconditionFailed(err) {
			return nil // lost CAS = success (09 §5.1 step 5)
		}
		return fmt.Errorf("events: CAS %s: %w", key, err)
	}
	return nil
}
