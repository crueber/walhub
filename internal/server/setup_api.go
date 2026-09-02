package server

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"git.packden.us/crueber/walhub/internal/config"
)

// setupSaveMu serializes Saves (process-wide mutex, §3.4 concurrency note).
var setupSaveMu sync.Mutex

// setupBaseConfig resolves the file-visible base the setup API edits on top
// of (§3.4): defaults ⊕ the first existing candidate config file; with no
// candidate the zero-config first-run shape; an unparseable file also falls
// back to the first-run shape (its errors are reported separately through the
// boot state). This is what makes setup EDIT the current configuration
// instead of replacing it with defaults plus a diff.
func (s *Server) setupBaseConfig() *config.Config {
	base, err := config.LoadSetupBase(s.cfg.DataDir, s.boot.ConfigPaths)
	if err != nil {
		return config.FirstRunDefaults(s.cfg.DataDir)
	}
	return base
}

// setupSchemaEntry is one key row of the GET /api/v1/setup groups.
type setupSchemaEntry struct {
	Key     string `json:"key"`
	Value   any    `json:"value"`
	Default any    `json:"default"`
	Type    string `json:"type"`
	Secret  bool   `json:"secret"`
	Doc     string `json:"doc"`
}

type setupSchemaGroup struct {
	Section string             `json:"section"`
	Keys    []setupSchemaEntry `json:"keys"`
}

type setupError struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// setupGet answers GET /api/v1/setup: full config schema + effective values +
// file state + validation errors (§3.4).
func (s *Server) setupGet(w http.ResponseWriter, r *http.Request) {
	if !s.setupAccess(w, r) {
		return
	}
	fileState := "absent"
	if s.boot.Mode == "normal" {
		fileState = "valid"
	} else if s.boot.InSetupOnly() {
		fileState = "invalid"
	}
	errs := []setupError{}
	for _, e := range s.boot.Errors {
		errs = append(errs, setupError{Message: e})
	}
	def := config.Defaults()
	groups := buildSchemaGroups(s.cfg, def)
	writeJSONBody(w, http.StatusOK, map[string]any{
		"file_state": fileState,
		"errors":     errs,
		"groups":     groups,
	})
}

// buildSchemaGroups walks the Config struct via its toml tags: section =
// top-level field, key = "<section>.<field>[.<subfield>…]". Nested section
// structs (server.auth.*) flatten into dotted keys — they are real editable
// settings, not one opaque row. Types: string|int|bool|duration|list.
func buildSchemaGroups(effective, def *config.Config) []setupSchemaGroup {
	rv, dv := reflect.ValueOf(*effective), reflect.ValueOf(*def)
	rt := rv.Type()
	groups := []setupSchemaGroup{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		section := f.Tag.Get("toml")
		if section == "" || section == "-" || f.Type.Kind() != reflect.Struct {
			continue
		}
		g := setupSchemaGroup{Section: section, Keys: []setupSchemaEntry{}}
		appendSchemaKeys(&g, section+".", rv.Field(i), dv.Field(i))
		if len(g.Keys) > 0 {
			groups = append(groups, g)
		}
	}
	return groups
}

func appendSchemaKeys(g *setupSchemaGroup, prefix string, sv, dv reflect.Value) {
	st := sv.Type()
	for j := 0; j < st.NumField(); j++ {
		sf := st.Field(j)
		name := sf.Tag.Get("toml")
		if name == "" || name == "-" {
			continue
		}
		key := prefix + name
		if sf.Type.Kind() == reflect.Struct {
			appendSchemaKeys(g, key+".", sv.Field(j), dv.Field(j))
			continue
		}
		typ := "string"
		switch sf.Type.Kind() {
		case reflect.Bool:
			typ = "bool"
		case reflect.Int, reflect.Int64, reflect.Int32:
			typ = "int"
		case reflect.Slice:
			typ = "list"
		}
		if sf.Type == reflect.TypeOf(config.Duration(0)) {
			typ = "duration"
		}
		g.Keys = append(g.Keys, setupSchemaEntry{
			Key:     key,
			Value:   fmtAny(sv.Field(j)),
			Default: fmtAny(dv.Field(j)),
			Type:    typ,
			Secret:  secretKey(name),
			Doc:     sf.Name,
		})
	}
}

func fmtAny(v reflect.Value) any {
	if !v.IsValid() || !v.CanInterface() {
		return nil
	}
	switch x := v.Interface().(type) {
	case config.Duration:
		return x.String()
	case config.ByteSize:
		return int64(x)
	default:
		return x
	}
}

func secretKey(name string) bool {
	l := strings.ToLower(name)
	return strings.Contains(l, "secret") || strings.Contains(l, "password") ||
		strings.Contains(l, "token") || l == "key"
}

// parseSetupBody accepts raw TOML or {"overrides": {key: value}} (§3.4) and
// applies them ON TOP OF base — the file-visible config (setupBaseConfig), so
// untouched keys keep their current values. Never mutates base's callers: base
// is a freshly-loaded object per request.
func parseSetupBody(r *http.Request, base *config.Config) (*config.Config, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	c := base
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "{") {
		var req struct {
			Overrides map[string]any `json:"overrides"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		for k, v := range req.Overrides {
			parts := strings.Split(k, ".")
			if len(parts) < 2 {
				return nil, fmt.Errorf("key %q must be section.field", k)
			}
			var raw string
			switch x := v.(type) {
			case string:
				raw = x
			case bool:
				raw = fmt.Sprintf("%t", x)
			case float64:
				raw = strconv.FormatFloat(x, 'f', -1, 64) // no exponent: 1073741824, not "1.07e+09"
			case []any:
				items := make([]string, len(x))
				for i := range x {
					items[i] = fmt.Sprintf("%v", x[i])
				}
				raw = strings.Join(items, ",")
			default:
				raw = fmt.Sprintf("%v", x)
			}
			if err := configSetPath(c, parts, raw); err != nil {
				return nil, err
			}
		}
		return c, nil
	}
	// Raw TOML: merge over defaults via the toml decoder's primitive map walk.
	var overlay map[string]any
	if _, derr := toml.Decode(string(body), &overlay); derr != nil {
		return nil, derr
	}
	for section, fields := range overlay {
		m, ok := fields.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("section %q must be a table", section)
		}
		for name, v := range m {
			var raw string
			switch x := v.(type) {
			case string:
				raw = x
			case bool:
				raw = fmt.Sprintf("%t", x)
			case int64:
				raw = fmt.Sprintf("%d", x)
			case float64:
				raw = strconv.FormatFloat(x, 'f', -1, 64)
			case []any:
				items := make([]string, len(x))
				for i := range x {
					items[i] = fmt.Sprintf("%v", x[i])
				}
				raw = strings.Join(items, ",")
			default:
				raw = fmt.Sprintf("%v", v)
			}
			if err := configSetPath(c, []string{section, name}, raw); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

// configSetPath sets one dotted key ("server.auth.mode") by reflection over
// the toml tags, walking nested section structs.
func configSetPath(c *config.Config, parts []string, raw string) error {
	if len(parts) < 2 {
		return fmt.Errorf("key %q must be section.field", strings.Join(parts, "."))
	}
	return setPathSegments(reflect.ValueOf(c).Elem(), parts, raw, "")
}

func setPathSegments(sv reflect.Value, parts []string, raw, path string) error {
	st := sv.Type()
	for j := 0; j < st.NumField(); j++ {
		name := st.Field(j).Tag.Get("toml")
		if name == "" || name == "-" || name != parts[0] {
			continue
		}
		next := path
		if next != "" {
			next += "."
		}
		next += name
		fv := sv.Field(j)
		if len(parts) == 1 {
			if fv.Kind() == reflect.Struct {
				return fmt.Errorf("%s: is a section, not a value", next)
			}
			return configCoerce(fv, raw, next)
		}
		if fv.Kind() != reflect.Struct {
			return fmt.Errorf("%s: is not a section", next)
		}
		return setPathSegments(fv, parts[1:], raw, next)
	}
	return fmt.Errorf("unknown key %q", path+"."+parts[0])
}

func configCoerce(fv reflect.Value, raw, path string) error {
	raw = strings.TrimSpace(raw)
	// Named types first: the schema surfaces Duration as "1h0m0s" and
	// ByteSize as a plain integer — Sscanf("%d") would silently truncate
	// both ("1h0m0s" → 1ns, "1.07e+09" → 1).
	switch fv.Type() {
	case reflect.TypeOf(config.Duration(0)):
		if d, err := time.ParseDuration(raw); err == nil {
			fv.SetInt(int64(d))
			return nil
		}
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil { // bare int = seconds (§2 parser rule)
			fv.SetInt(n * int64(time.Second))
			return nil
		}
		return fmt.Errorf("%s: not a duration", path)
	case reflect.TypeOf(config.ByteSize(0)):
		n, err := parseSettingInt(raw)
		if err != nil {
			return fmt.Errorf("%s: not an int", path)
		}
		fv.SetInt(n)
		return nil
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		if raw == "true" {
			fv.SetBool(true)
		} else if raw == "false" {
			fv.SetBool(false)
		} else {
			return fmt.Errorf("%s: not a bool", path)
		}
	case reflect.Int, reflect.Int64, reflect.Int32:
		n, err := parseSettingInt(raw)
		if err != nil {
			return fmt.Errorf("%s: not an int", path)
		}
		if fv.OverflowInt(n) {
			return fmt.Errorf("%s: %d out of range", path, n)
		}
		fv.SetInt(n)
	case reflect.Slice:
		items := strings.Split(raw, ",")
		out := reflect.MakeSlice(fv.Type(), len(items), len(items))
		for i := range items {
			if out.Index(i).Kind() == reflect.String {
				out.Index(i).SetString(strings.TrimSpace(items[i]))
			} else {
				return fmt.Errorf("%s: unsupported list element", path)
			}
		}
		fv.Set(out)
	default:
		return fmt.Errorf("%s: unsupported type", path)
	}
	return nil
}

// parseSettingInt accepts plain decimals and the scientific notation that
// JSON numbers produce when formatted with %v ("1.073741824e+09").
func parseSettingInt(raw string) (int64, error) {
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f != math.Trunc(f) {
		return 0, fmt.Errorf("not an integer: %q", raw)
	}
	return int64(f), nil
}

// setupTest answers POST /api/v1/setup/test: run the full validator WITHOUT
// saving → 200 {errors: []} or 422 {errors: [{key,message}]} (§3.4).
func (s *Server) setupTest(w http.ResponseWriter, r *http.Request) {
	if !s.setupAccess(w, r) {
		return
	}
	c, err := parseSetupBody(r, s.setupBaseConfig())
	if err != nil {
		writeJSONBody(w, http.StatusUnprocessableEntity, map[string]any{
			"errors": []setupError{{Message: err.Error()}},
		})
		return
	}
	if _, errs := config.Validate(c); len(errs) > 0 {
		out := []setupError{}
		for _, e := range errs {
			out = append(out, setupError{Message: e.Error()})
		}
		writeJSONBody(w, http.StatusUnprocessableEntity, map[string]any{"errors": out})
		return
	}
	writeJSONBody(w, http.StatusOK, map[string]any{"errors": []setupError{}})
}

// setupPut answers PUT /api/v1/setup: validate + atomically write
// <data-dir>/walhub.toml via config.SaveSetup, then respond
// 200 {saved, requires_restart, errors} (§3.4).
func (s *Server) setupPut(w http.ResponseWriter, r *http.Request) {
	if !s.setupAccess(w, r) {
		return
	}
	c, err := parseSetupBody(r, s.setupBaseConfig())
	if err != nil {
		writeJSONBody(w, http.StatusUnprocessableEntity, map[string]any{
			"errors": []setupError{{Message: err.Error()}},
		})
		return
	}
	if _, errs := config.Validate(c); len(errs) > 0 {
		out := []setupError{}
		for _, e := range errs {
			out = append(out, setupError{Message: e.Error()})
		}
		writeJSONBody(w, http.StatusUnprocessableEntity, map[string]any{"errors": out})
		return
	}
	setupSaveMu.Lock()
	serr := config.SaveSetup(c, s.cfg.DataDir)
	setupSaveMu.Unlock()
	if serr != nil {
		writeJSONBody(w, http.StatusInternalServerError, map[string]any{
			"errors": []setupError{{Message: serr.Error()}},
		})
		return
	}
	// requires_restart: every key whose effective value differs from the
	// just-saved file (the process does NOT hot-reload; §3.4).
	writeJSONBody(w, http.StatusOK, map[string]any{
		"saved":            true,
		"requires_restart": restartKeys(s.cfg, c),
		"errors":           []setupError{},
	})
}

// restartKeys diffs the effective config against the candidate.
func restartKeys(effective, candidate *config.Config) []string {
	groups := buildSchemaGroups(candidate, effective)
	out := []string{}
	for _, g := range groups {
		for _, k := range g.Keys {
			if anyToString(effectiveValue(effective, k.Key)) != anyToString(k.Value) {
				out = append(out, k.Key)
			}
		}
	}
	return out
}

func effectiveValue(c *config.Config, key string) any {
	parts := strings.SplitN(key, ".", 2)
	if len(parts) != 2 {
		return nil
	}
	groups := buildSchemaGroups(c, c)
	for _, g := range groups {
		if g.Section != parts[0] {
			continue
		}
		for _, k := range g.Keys {
			if k.Key == key {
				return k.Value
			}
		}
	}
	return nil
}

func anyToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return fmt.Sprintf("%t", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case nil:
		return "<nil>"
	default:
		return fmt.Sprintf("%v", x)
	}
}
