// Drawing primitives for the landing concept GIFs (issue #187): filled
// rectangles, 1px box outlines, horizontal/vertical arrows, and 2x-scaled
// bitmap text. Single-threaded; no locks, no goroutines.
package main

import (
	"image"
	"image/color"
)

const (
	imgW = 640
	imgH = 360
	// textScale renders the 5x7 font at 2x (10x14 px per glyph — the legible
	// size proven by the planning spike).
	textScale = 2
	// captionY is the baseline row of the baked bottom-center caption.
	captionY = imgH - 30
)

// Palette (dark-theme-friendly, fixed order — determinism; GIFs ship in
// dark-framed cards in both themes, one asset set).
var palette = []color.Color{
	color.RGBA{0x09, 0x09, 0x0b, 0xff}, // 0 bg zinc-950
	color.RGBA{0xe4, 0xe4, 0xe7, 0xff}, // 1 fg zinc-200
	color.RGBA{0x10, 0xb9, 0x81, 0xff}, // 2 accent emerald-500
	color.RGBA{0xf5, 0x9e, 0x0b, 0xff}, // 3 in-flight amber
	color.RGBA{0x71, 0x71, 0x7a, 0xff}, // 4 dim zinc-500
}

const (
	cBG     uint8 = 0
	cFG     uint8 = 1
	cAccent uint8 = 2
	cAmber  uint8 = 3
	cDim    uint8 = 4
)

// canvas is one 640x360 paletted frame.
type canvas struct {
	img *image.Paletted
}

func newCanvas() *canvas {
	return &canvas{img: image.NewPaletted(image.Rect(0, 0, imgW, imgH), palette)}
}

func (c *canvas) set(x, y int, ci uint8) {
	if x < 0 || y < 0 || x >= imgW || y >= imgH {
		return
	}
	c.img.SetColorIndex(x, y, ci)
}

// fill paints a filled rectangle (clipped to the frame).
func (c *canvas) fill(x, y, w, h int, ci uint8) {
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			c.set(x+dx, y+dy, ci)
		}
	}
}

// box draws a 1px outline rectangle.
func (c *canvas) box(x, y, w, h int, ci uint8) {
	for dx := 0; dx < w; dx++ {
		c.set(x+dx, y, ci)
		c.set(x+dx, y+h-1, ci)
	}
	for dy := 0; dy < h; dy++ {
		c.set(x, y+dy, ci)
		c.set(x+w-1, y+dy, ci)
	}
}

// hline draws a 2px-tall horizontal line (visible at GIF scale).
func (c *canvas) hline(x1, x2, y int, ci uint8) {
	if x1 > x2 {
		x1, x2 = x2, x1
	}
	c.fill(x1, y-1, x2-x1+1, 3, ci)
}

// vline draws a 2px-wide vertical line.
func (c *canvas) vline(x, y1, y2 int, ci uint8) {
	if y1 > y2 {
		y1, y2 = y2, y1
	}
	c.fill(x-1, y1, 3, y2-y1+1, ci)
}

// arrowRight draws a rightward arrow from x1 to x2 at height y.
func (c *canvas) arrowRight(x1, x2, y int, ci uint8) {
	c.hline(x1, x2-8, y, ci)
	for i := 0; i < 8; i++ {
		c.fill(x2-8+i, y-1-i/2-1, 2, 3+i, ci)
	}
}

// arrowLeft draws a leftward arrow from x1 to x2 (x1 > x2) at height y.
func (c *canvas) arrowLeft(x1, x2, y int, ci uint8) {
	c.hline(x2+8, x1, y, ci)
	for i := 0; i < 8; i++ {
		c.fill(x2+i, y-1-(7-i)/2-1, 2, 3+(7-i), ci)
	}
}

// textWidth is the pixel width of s at 2x (5px cells + 1px tracking).
func textWidth(s string) int {
	if s == "" {
		return 0
	}
	return len(s)*(5*textScale+textScale) - textScale
}

// text renders s with the top-left ink pixel at (x, y), color ci.
func (c *canvas) text(s string, x, y int, ci uint8) {
	cx := x
	for i := 0; i < len(s); i++ {
		g, ok := font[s[i]]
		if !ok {
			cx += 5*textScale + textScale // missing glyph: advance, never crash
			continue
		}
		for row := 0; row < 7; row++ {
			for col := 0; col < 5; col++ {
				if g[row][col] == '#' {
					c.fill(cx+col*textScale, y+row*textScale, textScale, textScale, ci)
				}
			}
		}
		cx += 5*textScale + textScale
	}
}

// textCentered renders s centered on cx at row y.
func (c *canvas) textCentered(s string, cx, y int, ci uint8) {
	c.text(s, cx-textWidth(s)/2, y, ci)
}

// caption bakes the numbered bottom-center caption every frame carries.
func (c *canvas) caption(s string) {
	c.fill(0, imgH-44, imgW, 44, cBG)
	c.textCentered(s, imgW/2, captionY, cFG)
}

// labelBox draws a labeled box: outline + centered title on the top edge +
// optional body lines inside.
func (c *canvas) labelBox(x, y, w, h int, ci uint8, title string, body ...string) {
	c.fill(x, y, w, h, cBG)
	c.box(x, y, w, h, ci)
	if title != "" {
		tw := textWidth(title)
		c.fill(x+(w-tw)/2-6, y-10, tw+12, 20, cBG) // knock out the top edge
		c.text(title, x+(w-tw)/2, y-10, ci)
	}
	by := y + 22
	for _, line := range body {
		c.text(line, x+12, by, ci)
		by += 7*textScale + 8
	}
}
