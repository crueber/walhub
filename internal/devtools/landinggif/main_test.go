// Tests for the landing-gif generator (issue #187): full-ASCII font +
// coverage (the missing-`3` class can never recur silently), determinism
// (byte-identical re-encodes), and freshness (regeneration is byte-equal to
// the checked-in web/public/concepts/ artifacts — regeneration is always
// intentional + reviewed).
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestFontFullASCII: the font table covers every printable ASCII codepoint.
func TestFontFullASCII(t *testing.T) {
	for c := byte(32); c < 127; c++ {
		g, ok := font[c]
		if !ok {
			t.Fatalf("font missing glyph for %q (%d)", c, c)
		}
		for row, line := range g {
			if len(line) != 5 {
				t.Fatalf("glyph %q row %d: want 5 cells, got %q", c, row, line)
			}
			for _, ch := range line {
				if ch != '#' && ch != '.' {
					t.Fatalf("glyph %q row %d: illegal cell %q", c, row, ch)
				}
			}
		}
	}
	if len(font) != 95 {
		t.Fatalf("font must hold exactly the 95 printable ASCII glyphs, got %d", len(font))
	}
}

// TestFontCoversSceneText: every character drawn by every scene has a glyph.
func TestFontCoversSceneText(t *testing.T) {
	sceneStrings = nil
	for _, sc := range scenes() {
		_ = sc.frames // built above → literals recorded via str()
	}
	seen := map[byte]bool{}
	for _, s := range sceneStrings {
		for i := 0; i < len(s); i++ {
			seen[s[i]] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("no scene literals recorded — str() wiring broken")
	}
	for c := range seen {
		if c < 32 || c > 126 {
			t.Fatalf("scene literal uses non-printable-ASCII byte %d", c)
		}
		if _, ok := font[c]; !ok {
			t.Fatalf("scene text uses %q with no font glyph (the missing-3 class)", c)
		}
	}
	t.Logf("scene text uses %d distinct glyphs", len(seen))
}

// TestDeterministic: two encodes of the same scene are byte-identical.
func TestDeterministic(t *testing.T) {
	for _, sc := range scenes() {
		a, b := encodeGIF(sc.frames), encodeGIF(sc.frames)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: re-encode drifted (%d vs %d bytes)", sc.name, len(a), len(b))
		}
	}
}

// TestFrameTimingFloor: holds ≥ 180, steps ≥ 40 (storyboard acceptance, N4).
func TestFrameTimingFloor(t *testing.T) {
	for _, sc := range scenes() {
		for i, f := range sc.frames {
			if f.delay < 40 {
				t.Fatalf("%s frame %d: delay %d below the 40 floor", sc.name, i, f.delay)
			}
		}
		holds := 0
		for _, f := range sc.frames {
			if f.delay >= 180 {
				holds++
			}
		}
		if holds == 0 {
			t.Fatalf("%s: no key-frame hold ≥ 180", sc.name)
		}
	}
}

// repoRoot walks up from this file to the module root (go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up")
		}
		dir = parent
	}
}

// TestFreshness: regeneration is byte-equal to the checked-in artifacts.
func TestFreshness(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "web", "public", "concepts")
	for _, sc := range scenes() {
		for _, f := range []struct {
			path string
			body []byte
		}{{sc.name + ".gif", encodeGIF(sc.frames)}, {sc.name + "-still.gif", encodeGIF(sc.frames[:1])}} {
			want, err := os.ReadFile(filepath.Join(dir, f.path))
			if err != nil {
				t.Fatalf("%s missing — run: make landing-gifs", f.path)
			}
			if !bytes.Equal(want, f.body) {
				t.Fatalf("%s drifted — regenerate with make landing-gifs and review the diff", f.path)
			}
		}
	}
}

// TestBudgets: per-asset byte budgets from the plan §3.3.
func TestBudgets(t *testing.T) {
	budgets := map[string]int{
		"push": 30 << 10, "bucket": 30 << 10, "fetch": 30 << 10, "collab": 40 << 10,
	}
	total := 0
	for _, sc := range scenes() {
		anim, still := encodeGIF(sc.frames), encodeGIF(sc.frames[:1])
		if len(anim) > budgets[sc.name] {
			t.Fatalf("%s.gif = %d bytes, budget %d", sc.name, len(anim), budgets[sc.name])
		}
		if len(still) > 4<<10 {
			t.Fatalf("%s-still.gif = %d bytes, budget 4096", sc.name, len(still))
		}
		t.Logf("%s.gif = %d bytes, %s-still.gif = %d bytes", sc.name, len(anim), sc.name, len(still))
		total += len(anim) + len(still)
	}
	if total > 150<<10 {
		t.Fatalf("total GIF weight = %d bytes, budget 150 KiB", total)
	}
	t.Logf("total = %d bytes", total)
}
