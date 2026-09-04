// collab.go — Feature 08 §4: the ONE repo collaboration stream.
//
// GET /{o}/{r}/api/collab/stream (both lanes) carries every live
// collaboration event for a repo. One connection per page; frames
// invalidate data-layer cache keys, they never carry full state (the
// timeline and lists stay the source of truth; frames are the
// invalidation transport only).
//
// Frame: `event: <kind>` with data {kind, num?, seq?, sha?, tag?, actor?,
// at, ...} (kinds: issue|issue_event|pull|review|thread|check|release|
// access). Envelope is the 07 §6 dialect verbatim (: walgit opener,
// 10 s keepalives, no-store). Read auth (anonymous when anonymous_read);
// the require_read gate decides.
//
// ### Concurrency
// Hazard: a slow page stalls the repo bus or pins a goroutine after the
// client leaves. Avoidance: the bus is drop-oldest (publish never
// blocks); this handler exits via the request context (13 channel rule:
// SubscribeRepo's unsubscribe closes the channel); every SSE write
// carries a 15 s deadline.
package notify

import (
	"encoding/json"
	"net/http"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// collabKinds is the 08 §4 kind set. Frames with other names are dropped
// (forward-compatible: a new publisher must not break old pages).
var collabKinds = map[string]bool{
	"issue": true, "issue_event": true, "pull": true, "review": true,
	"thread": true, "check": true, "release": true, "access": true,
}

// collabFrame is the wire data for one frame: the RepoFrame plus the
// explicit kind (08 §4: data carries kind even though the event name
// already names it, so coalescing keys off one field).
type collabFrame struct {
	Kind     string `json:"kind"`
	Num      int    `json:"num,omitempty"`
	Seq      int    `json:"seq,omitempty"`
	Sha      string `json:"sha,omitempty"`
	Tag      string `json:"tag,omitempty"`
	Actor    string `json:"actor,omitempty"`
	Action   string `json:"action,omitempty"`
	Title    string `json:"title,omitempty"`
	State    string `json:"state,omitempty"`
	At       string `json:"at"`
	Tid      string `json:"thread_id,omitempty"`
	Context  string `json:"context,omitempty"`
	Combined string `json:"combined_state,omitempty"`
}

// frameFor maps one bus frame to its wire frame; ok=false drops unknown
// kinds (never break the page on a new publisher).
func frameFor(f RepoFrame) (collabFrame, bool) {
	if !collabKinds[f.Name] {
		return collabFrame{}, false
	}
	return collabFrame{
		Kind: f.Name, Num: f.Num, Seq: f.Seq, Sha: f.Sha, Tag: f.Tag,
		Actor: f.Actor, Action: f.Action, Title: f.Title, State: f.State,
		At: f.At, Tid: f.Tid, Context: f.Context, Combined: f.Combined,
	}, true
}

// collabStream serves GET /{o}/{r}/api/collab/stream: the recent ring
// replays first (late attachers backfill), then live frames ride until
// the client cancels. Errors are plain-text (fail closed downstream).
func (h *Handler) collabStream(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal) {
	if r.Method != http.MethodGet {
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := h.Svc.requireRead(r.Context(), owner, repo, p); err != nil {
		writePlain(w, statusFor(err), err.Error())
		return
	}
	s, ok := newSSEWriter(w, r)
	if !ok {
		writePlain(w, http.StatusNotAcceptable, "streaming unsupported")
		return
	}
	defer s.close()
	full := owner + "/" + repo
	ch, recent, unsub := h.Svc.SubscribeRepo(full)
	defer unsub()
	for _, f := range recent {
		if f.Repo != full {
			continue
		}
		cf, ok := frameFor(f)
		if !ok {
			continue
		}
		if !s.event(cf.Kind, string(collabJSON(cf))) {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case f, ok := <-ch:
			if !ok {
				return
			}
			cf, ok := frameFor(f)
			if !ok {
				continue
			}
			if !s.event(cf.Kind, string(collabJSON(cf))) {
				return
			}
		}
	}
}

func collabJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
