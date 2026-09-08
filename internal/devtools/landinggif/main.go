// Command landinggif generates the landing-page concept GIFs (issue #187).
//
// Four hand-drawn animated diagrams (stdlib image/gif only — no
// golang.org/x/image, no npm involvement) plus one still each:
//
//	go run ./internal/devtools/landinggif -out web/public/concepts/
//
// Storyboards, timings, palette, and budgets are specified in the issue
// plan §3.2–§3.3; frame-timing acceptance: key-frame holds ≥ 180 units
// (1.8 s), motion steps ≥ 40 (0.4 s, above browser minimum-delay clamping,
// N4), ≤ 3 moving elements per frame, numbered captions on every frame.
// Output is byte-for-byte deterministic (fixed palette order, fixed frame
// order, no timestamps, no map iteration in the encode path).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/gif"
	"os"
	"path/filepath"
	"time"
)

// Delay units are 100ths of a second (GIF convention).
const (
	holdLong   = 200 // key-frame holds (2.0 s)
	holdLonger = 220 // final rest holds (2.2 s)
	holdMid    = 180 // ack/write holds (1.8 s)
	holdShort  = 150 // card-landing holds (1.5 s)
	stepSlow   = 60  // slow motion steps (0.6 s)
	stepFast   = 50  // motion steps (0.5 s)
)

// frame is one animation frame plus its display delay.
type frame struct {
	img   *image.Paletted
	delay int
}

// sceneStrings records every literal drawn by the scenes (via str) so the
// font-coverage test fails loudly on any glyph the font lacks — the
// missing-`3` class from the planning spike can never recur silently.
var sceneStrings []string

func str(s string) string {
	sceneStrings = append(sceneStrings, s)
	return s
}

// --- scene 1: push -----------------------------------------------------------

// pushScene: push → objects land in the bucket (7 frames, ≈ 8 s).
func pushScene() []frame {
	laptop := func(c *canvas, ci uint8) {
		c.labelBox(40, 100, 170, 110, ci, str("YOU"), str("git push"))
	}
	bucket := func(c *canvas, ci uint8, rows ...string) {
		c.labelBox(430, 100, 170, 110, ci, str("BUCKET"), rows...)
	}
	mk := func(draw func(c *canvas), caption string, delay int) frame {
		c := newCanvas()
		draw(c)
		c.caption(str(caption))
		return frame{c.img, delay}
	}
	packArrow := func(c *canvas, x2 int) {
		c.arrowRight(215, x2, 155, cAmber)
		c.text(str("PACK"), 290, 118, cAmber)
	}
	return []frame{
		mk(func(c *canvas) { laptop(c, cAccent); bucket(c, cDim) }, "1. YOU PUSH.", holdLong),
		mk(func(c *canvas) { laptop(c, cAccent); bucket(c, cDim); packArrow(c, 300) }, "2. PACKS TRAVEL.", stepFast),
		mk(func(c *canvas) {
			laptop(c, cAccent)
			bucket(c, cDim)
			packArrow(c, 425)
			c.fill(330, 143, 34, 24, cAmber)
		}, "2. PACKS TRAVEL.", stepFast),
		mk(func(c *canvas) {
			laptop(c, cAccent)
			bucket(c, cDim)
			c.arrowRight(215, 425, 155, cAmber)
			c.fill(400, 143, 34, 24, cAmber)
		}, "3. BUCKET WRITES.", stepSlow),
		mk(func(c *canvas) { laptop(c, cAccent); bucket(c, cAccent, str("+ PACK")) }, "3. BUCKET WRITES.", holdMid),
		mk(func(c *canvas) {
			laptop(c, cAccent)
			bucket(c, cAccent, str("+ PACK"))
			c.arrowLeft(425, 215, 200, cAccent)
			c.text(str("ACK"), 300, 208, cAccent)
		}, "4. BUCKET ACKS FIRST.", holdMid),
		mk(func(c *canvas) { laptop(c, cAccent); bucket(c, cAccent, str("+ PACK")) }, "4. BUCKET ACKS FIRST.", holdLong),
	}
}

// --- scene 2: bucket ---------------------------------------------------------

// bucketScene: the bucket is the only database (7 frames, ≈ 8 s).
func bucketScene() []frame {
	rows := []string{str("REFS"), str("PACKS"), str("CONFIG"), str("POLICY")}
	bucket := func(c *canvas, ci uint8, hi int) {
		c.labelBox(220, 140, 200, 130, ci, str("BUCKET"))
		for i, row := range rows {
			col := ci
			if i == hi {
				col = cAmber
			}
			c.text(row, 240, 168+i*22, col)
		}
	}
	inst := func(c *canvas, ci uint8, name string) {
		c.labelBox(250, 30, 140, 56, ci, str(name))
	}
	link := func(c *canvas, ci uint8, broken bool) {
		if broken {
			c.vline(320, 92, 110, ci)
			c.vline(320, 118, 136, ci)
			c.text(str("X"), 312, 108, cAmber)
		} else {
			c.vline(320, 92, 136, ci)
		}
	}
	mk := func(draw func(c *canvas), caption string, delay int) frame {
		c := newCanvas()
		draw(c)
		c.caption(str(caption))
		return frame{c.img, delay}
	}
	return []frame{
		mk(func(c *canvas) { bucket(c, cAccent, -1); inst(c, cDim, str("A")); link(c, cDim, false) }, "1. ONE BUCKET.", holdLong),
		mk(func(c *canvas) { bucket(c, cAccent, -1); inst(c, cAmber, str("A")); link(c, cDim, true) }, "2. INSTANCE DIES.", stepSlow),
		mk(func(c *canvas) { bucket(c, cAccent, -1) }, "2. INSTANCE DIES.", holdLong),
		mk(func(c *canvas) { bucket(c, cAccent, -1); inst(c, cAccent, str("B")); link(c, cAccent, false) }, "3. NEW INSTANCE.", stepSlow),
		mk(func(c *canvas) { bucket(c, cAccent, 0); inst(c, cAccent, str("B")); link(c, cAccent, false) }, "4. NOTHING LOST.", stepFast),
		mk(func(c *canvas) { bucket(c, cAccent, 2); inst(c, cAccent, str("B")); link(c, cAccent, false) }, "4. NOTHING LOST.", stepFast),
		mk(func(c *canvas) {
			bucket(c, cAccent, -1)
			inst(c, cAccent, str("B"))
			link(c, cAccent, false)
			c.text(str("WARMTH ONLY"), 252, 284, cDim)
		}, "4. NOTHING LOST.", holdLonger),
	}
}

// --- scene 3: fetch ----------------------------------------------------------

// fetchScene: fetch/clone reads objects back (6 frames, ≈ 7 s).
func fetchScene() []frame {
	bucket := func(c *canvas, ci uint8) {
		c.labelBox(40, 100, 170, 130, ci, str("BUCKET"), str("REFS"), str("PACKS"))
	}
	clone := func(c *canvas, ci uint8, rows ...string) {
		c.labelBox(430, 100, 170, 130, ci, str("CLONE"), rows...)
	}
	mk := func(draw func(c *canvas), caption string, delay int) frame {
		c := newCanvas()
		draw(c)
		c.caption(str(caption))
		return frame{c.img, delay}
	}
	return []frame{
		mk(func(c *canvas) { bucket(c, cAccent); clone(c, cDim) }, "1. EMPTY CLONE.", holdLong),
		mk(func(c *canvas) {
			bucket(c, cAccent)
			clone(c, cDim)
			c.arrowRight(215, 425, 140, cDim)
			c.text(str("REFS (1)"), 280, 104, cDim)
		}, "2. REFS FIRST.", stepSlow),
		mk(func(c *canvas) {
			bucket(c, cAccent)
			clone(c, cAmber, str("main"), str("v1.0"))
		}, "2. REFS FIRST.", holdShort),
		mk(func(c *canvas) {
			bucket(c, cAccent)
			clone(c, cAccent, str("main"), str("v1.0"))
			c.fill(215, 168, 210, 10, cAmber)
			c.fill(300, 156, 34, 24, cAmber)
			c.text(str("PACKS"), 290, 190, cAmber)
		}, "3. PACKS FOLLOW.", stepFast),
		mk(func(c *canvas) {
			bucket(c, cAccent)
			clone(c, cAccent, str("main"), str("v1.0"), str("src/"), str("docs/"))
		}, "4. WORKING COPY.", holdLong),
		mk(func(c *canvas) {
			bucket(c, cAccent)
			clone(c, cAccent, str("main"), str("v1.0"), str("src/"), str("docs/"))
		}, "4. WORKING COPY.", holdLong),
	}
}

// --- scene 4: collab ---------------------------------------------------------

// collabScene: collaboration as objects alongside git data (7 frames, ≈ 9 s).
// Tightest scene (most distinct elements) — implemented and reviewed first
// (S3); fallback would be splitting into two GIFs, not enlarging.
func collabScene() []frame {
	gitLane := func(c *canvas, walPulse bool) {
		ci := cAccent
		c.labelBox(60, 50, 520, 92, ci, str("GIT"), str("REFS  PACKS"))
		c.text(str("WAL"), 500, 78, cDim)
		if walPulse {
			c.box(492, 68, 56, 34, cAmber)
		}
	}
	collabLane := func(c *canvas, ci uint8, issue, pr, check bool) {
		c.labelBox(60, 162, 520, 118, ci, str("COLLAB"))
		c.text(str("ISSUES"), 84, 196, cFG)
		c.text(str("PRS"), 264, 196, cFG)
		c.text(str("CHECKS"), 424, 196, cFG)
		if issue {
			c.fill(84, 222, 130, 30, cBG)
			c.box(84, 222, 130, 30, cAccent)
			c.text(str("#7 OPENED"), 92, 230, cAccent)
		}
		if pr {
			c.fill(264, 222, 110, 30, cBG)
			c.box(264, 222, 110, 30, cAccent)
			c.text(str("PR 8"), 272, 230, cAccent)
			c.hline(214, 264, 237, cDim)
			c.text(str("FIXES 7"), 197, 256, cDim)
		}
		if check {
			c.fill(424, 222, 130, 30, cBG)
			c.box(424, 222, 130, 30, cAccent)
			c.text(str("CI: GREEN"), 432, 230, cAccent)
		}
	}
	mk := func(draw func(c *canvas), caption string, delay int) frame {
		c := newCanvas()
		draw(c)
		c.caption(str(caption))
		return frame{c.img, delay}
	}
	return []frame{
		mk(func(c *canvas) { gitLane(c, false) }, "1. GIT LIVES HERE.", holdLong),
		mk(func(c *canvas) { gitLane(c, false); collabLane(c, cAmber, false, false, false) }, "2. COLLAB MOVES IN.", stepSlow),
		mk(func(c *canvas) { gitLane(c, false); collabLane(c, cDim, true, false, false) }, "3. ISSUE 7 OPENS.", holdShort),
		mk(func(c *canvas) { gitLane(c, false); collabLane(c, cDim, true, true, false) }, "4. PR 8 FIXES 7.", holdShort),
		mk(func(c *canvas) { gitLane(c, false); collabLane(c, cDim, true, true, true) }, "5. CHECKS REPORT.", holdShort),
		mk(func(c *canvas) { gitLane(c, true); collabLane(c, cDim, true, true, true) }, "6. WAL STAYS GIT-ONLY.", holdLong),
		mk(func(c *canvas) { gitLane(c, false); collabLane(c, cAccent, true, true, true) }, "6. WAL STAYS GIT-ONLY.", holdLong),
	}
}

// --- encode + CLI ------------------------------------------------------------

func encodeGIF(frames []frame) []byte {
	g := &gif.GIF{LoopCount: 0}
	for _, f := range frames {
		g.Image = append(g.Image, f.img)
		g.Delay = append(g.Delay, f.delay)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// scenes in review order (S3: collab first).
func scenes() []struct {
	name   string
	frames []frame
} {
	return []struct {
		name   string
		frames []frame
	}{
		{"collab", collabScene()},
		{"push", pushScene()},
		{"bucket", bucketScene()},
		{"fetch", fetchScene()},
	}
}

func main() {
	out := flag.String("out", "web/public/concepts", "output directory for the GIFs + stills")
	flag.Parse()
	start := time.Now()
	total := 0
	for _, sc := range scenes() {
		anim := encodeGIF(sc.frames)
		still := encodeGIF(sc.frames[:1])
		for _, f := range []struct {
			path string
			body []byte
		}{{sc.name + ".gif", anim}, {sc.name + "-still.gif", still}} {
			p := filepath.Join(*out, f.path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				panic(err)
			}
			if err := os.WriteFile(p, f.body, 0o644); err != nil {
				panic(err)
			}
			fmt.Printf("%-18s %7d bytes\n", f.path, len(f.body))
			total += len(f.body)
		}
	}
	fmt.Printf("%-18s %7d bytes (wall %s)\n", "TOTAL", total, time.Since(start).Round(time.Millisecond))
}
