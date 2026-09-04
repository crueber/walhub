package policy

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// effectRegistry is the Seam 3 registry: the effect kinds a policy.json may
// carry. Compiled-in, not dynamic — a registry entry is a Go value added in
// one RegisterEffect call. The built-ins (protect, history, size) register
// in init.
var effectRegistry = map[string]func() Effect{}

// RegisterEffect registers an effect prototype; duplicate kinds are a
// startup panic. Unknown effect kinds in a loaded file are parse errors
// (fail closed: 400 on PUT, REJECT on next push) — deploy the new binary
// fleet-wide BEFORE any repo adopts the new effect, because an old binary
// fails closed on files it cannot parse. That is the cost of open effects;
// it is accepted by design.
//
// The prototype must be a pointer to a struct; Parse runs on a fresh zero
// instance per rule. A new effect documents its own combination rule and
// its cross-rule compatibility check (or "no cross-rule interaction") in
// this doc family at registration time.
func RegisterEffect(e Effect) {
	t := reflect.TypeOf(e)
	if t == nil || t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Struct {
		panic(fmt.Sprintf("policy: effect %T must be a pointer to a struct", e))
	}
	kind := e.Kind()
	if kind == "" {
		panic("policy: effect kind must not be empty")
	}
	if _, dup := effectRegistry[kind]; dup {
		panic(fmt.Sprintf("policy: duplicate effect kind %q", kind))
	}
	effectRegistry[kind] = func() Effect { return reflect.New(t.Elem()).Interface().(Effect) }
}
func init() {
	RegisterEffect((*ProtectEffect)(nil))
	RegisterEffect((*HistoryEffect)(nil))
	RegisterEffect((*SizeEffect)(nil))
	// docs/features/04 §6: the required-reviews push-time half (deny
	// direct pushes to matched refs); the merge-time half lives in
	// internal/review, which scans review events at merge time.
	RegisterEffect((*RequiredReviewsEffect)(nil))
}

// effectNull reports a null/missing effect payload, which is a parse error
// rather than "all defaults".
func effectNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func effectObject(raw json.RawMessage, kind string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, invalidf("effect %s: must be an object", kind)
	}
	return m, nil
}
