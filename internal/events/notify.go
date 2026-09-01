// notify.go — POST /_events/notify (09 §6.1): the route lives in internal/server
// (chi, auth: read, 404 when this instance has no bridge); the handler here
// parses the accepted bodies (GCS Pub/Sub push envelope, S3 event notification,
// {key}/{repo} glue), schedules catch-ups, and answers with the report array.
// The handler never blocks on the bridge: wakes are non-blocking, and a
// "dropped" wake is covered by the sweep backstop.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// notifyReport is one entry of the 200 response (09 §6.3; field names are the
// Go-side choice recorded in the doc's deviations).
type notifyReport struct {
	Repo   string `json:"repo"`
	Status string `json:"status"` // "queued" | "dropped" | "ignored"
}

// Notify statuses.
const (
	StatusQueued  = "queued"
	StatusDropped = "dropped"
	StatusIgnored = "ignored"
)

// ---- accepted body shapes (tolerant of extra fields) ------------------------

// gcsEnvelope is the GCS Pub/Sub push envelope: the trigger is a message whose
// attributes say eventType == "OBJECT_FINALIZE"; the object key is
// message.attributes.objectId (also checked directly under message).
type gcsEnvelope struct {
	Message *gcsMessage `json:"message"`
}

type gcsMessage struct {
	Attributes map[string]string `json:"attributes"`
	ObjectID   string            `json:"objectId"`
}

// s3Notification is the S3 bucket-notification shape.
type s3Notification struct {
	Records []s3Record `json:"Records"`
}

type s3Record struct {
	EventName string `json:"eventName"`
	S3        struct {
		Object struct {
			Key string `json:"key"`
		} `json:"object"`
	} `json:"s3"`
}

// glueBody is the walhub glue: {"key": "..."} or {"repo": "o/r"}.
type glueBody struct {
	Key  string `json:"key"`
	Repo string `json:"repo"`
}

// ---- parsing -----------------------------------------------------------------

// trigger identifies one wake-up candidate extracted from a notify body.
type trigger struct {
	repo string // "owner/name" when recognized; "" → ignored
	key  string // original key, for tests/diagnostics
}

// repoFromKey implements the trigger rule (09 §6.1): an object key ending
// repos/<owner>/<repo>/manifest.pb schedules catchUp(owner/name); everything
// else is ignored.
func repoFromKey(key string) (string, bool) {
	if !strings.HasSuffix(key, "/manifest.pb") {
		return "", false
	}
	rest := key[:len(key)-len("/manifest.pb")]
	// Accept both repo-relative ("repos/o/r") and bucket-absolute
	// ("…/repos/o/r") keys; the repo segment is the last "repos/<owner>/<repo>".
	if i := strings.LastIndex(rest, "/repos/"); i >= 0 {
		rest = rest[i+len("/repos/"):]
	} else if !strings.HasPrefix(rest, "repos/") {
		return "", false
	} else {
		rest = rest[len("repos/"):]
	}
	return validRepo(rest)
}

// validRepo validates "owner/name" (exactly one '/', both parts non-empty).
func validRepo(s string) (string, bool) {
	i := strings.Index(s, "/")
	if i <= 0 || i == len(s)-1 || strings.Contains(s[i+1:], "/") {
		return "", false
	}
	return s, true
}

// repoFromRepo validates the {"repo": "o/r"} glue form.
func repoFromRepo(s string) (string, bool) {
	return validRepo(s)
}

// parseNotify turns one accepted body into one report entry per recognized
// trigger, in body order. Unparseable-but-valid JSON and non-trigger content
// ack with no triggers (the caller reports "ignored" as appropriate).
func parseNotify(body []byte) []trigger {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}

	// GCS Pub/Sub push envelope.
	if _, ok := root["message"]; ok {
		var env gcsEnvelope
		if err := json.Unmarshal(body, &env); err == nil && env.Message != nil {
			if env.Message.Attributes["eventType"] == "OBJECT_FINALIZE" {
				key := env.Message.Attributes["objectId"]
				if key == "" {
					key = env.Message.ObjectID
				}
				return keyTrigger(key)
			}
			return nil // other event types: acked and ignored
		}
	}

	// S3 event notification.
	if _, ok := root["Records"]; ok {
		var n s3Notification
		if err := json.Unmarshal(body, &n); err == nil && len(n.Records) > 0 {
			var out []trigger
			for i := range n.Records {
				if strings.HasPrefix(n.Records[i].EventName, "ObjectCreated:") {
					out = append(out, keyTrigger(n.Records[i].S3.Object.Key)...)
				}
			}
			return out
		}
	}

	// Glue forms.
	var g glueBody
	if err := json.Unmarshal(body, &g); err != nil {
		return nil
	}
	if g.Key != "" {
		return keyTrigger(g.Key)
	}
	if g.Repo != "" {
		if repo, ok := repoFromRepo(g.Repo); ok {
			return []trigger{{repo: repo}}
		}
		return nil
	}
	return nil
}

func keyTrigger(key string) []trigger {
	if key == "" {
		return nil
	}
	if repo, ok := repoFromKey(key); ok {
		return []trigger{{repo: repo, key: key}}
	}
	return []trigger{{key: key}} // recognized object event, non-trigger key
}

// ---- HTTP handler ---------------------------------------------------------------

// HandleNotify serves POST /_events/notify for a live bridge (09 §6.1). The
// server owns routing, auth (require_read), and the 404 when no bridge exists.
func (b *Bridge) HandleNotify(w http.ResponseWriter, r *http.Request) {
	body, err := readAllLimited(r, 1<<20)
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	// Unparseable JSON bodies are rejected: every accepted shape is valid JSON.
	var probe any
	if err := json.Unmarshal(body, &probe); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	trigs := parseNotify(body)
	reports := make([]notifyReport, 0, len(trigs))
	for _, t := range trigs {
		if t.repo == "" {
			reports = append(reports, notifyReport{Status: StatusIgnored})
			continue
		}
		status := b.Wake(t.repo)
		if status == StatusQueued && b.lastFail(t.repo) != nil {
			// A previous catch-up for this repo failed at a sink; run this
			// catch-up synchronously (bounded by the request context) so the
			// notifier redelivers on failure (09 §6.3).
			if _, cerr := b.catchUp(r.Context(), t.repo); cerr != nil {
				http.Error(w, "sink delivery failing", http.StatusServiceUnavailable)
				return
			}
		}
		reports = append(reports, notifyReport{Repo: t.repo, Status: status})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	if err := enc.Encode(reports); err != nil {
		b.log.Warn("events: notify response encode failed", "err", err)
	}
}

// readAllLimited reads the request body up to limit bytes.
func readAllLimited(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("nil body")
	}
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return data, nil
}
