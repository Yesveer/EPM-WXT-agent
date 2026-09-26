package capture

import (
	"bytes"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/rfb"
)

// tileSize is the granularity of change detection. It matches ZRLE's tile size
// so a dirty tile maps onto exactly one unit of work in the encoder.
const tileSize = 64

// maxRects caps how many rectangles one update may carry. Each rectangle costs
// a 12-byte header plus ZRLE framing, and a client has to process each one, so
// past a few dozen it is cheaper to send one larger rectangle that covers them
// all than to itemise the damage.
const maxRects = 48

// Differ finds what changed between consecutive frames.
//
// No capture API used here reports damage: Windows GDI has no concept of it,
// and CGDisplayCreateImage returns a whole screen each time. Diffing is
// therefore how a mostly-static desktop stays cheap — a user reading a document
// produces a handful of dirty tiles per second instead of a full frame.
type Differ struct {
	prev   []byte // previous frame pixels, tightly packed
	w, h   int
	stride int
	dirty  []bool // one flag per tile, row-major
	tilesX int
	tilesY int
}

// NewDiffer returns an empty Differ. The first Damage call after construction
// reports the whole frame.
func NewDiffer() *Differ { return &Differ{} }

// Reset adopts img as the baseline without reporting any damage.
func (d *Differ) Reset(img *rfb.Image) {
	d.w, d.h, d.stride = img.Width, img.Height, img.Stride
	d.tilesX = (img.Width + tileSize - 1) / tileSize
	d.tilesY = (img.Height + tileSize - 1) / tileSize
	d.dirty = make([]bool, d.tilesX*d.tilesY)

	need := len(img.Pix)
	if cap(d.prev) < need {
		d.prev = make([]byte, need)
	}
	d.prev = d.prev[:need]
	copy(d.prev, img.Pix)
}

// Damage returns the regions of img that differ from the previous frame, and
// adopts img as the new baseline.
func (d *Differ) Damage(img *rfb.Image) []rfb.Rect {
	// A resolution change invalidates the whole baseline.
	if img.Width != d.w || img.Height != d.h || img.Stride != d.stride {
		d.Reset(img)
		return []rfb.Rect{img.Bounds()}
	}

	for i := range d.dirty {
		d.dirty[i] = false
	}

	any := false
	for ty := 0; ty < d.tilesY; ty++ {
		y0 := ty * tileSize
		y1 := min(y0+tileSize, img.Height)
		for tx := 0; tx < d.tilesX; tx++ {
			x0 := tx * tileSize
			x1 := min(x0+tileSize, img.Width)

			if d.tileChanged(img, x0, y0, x1, y1) {
				d.dirty[ty*d.tilesX+tx] = true
				any = true
			}
		}
	}

	if !any {
		return nil
	}

	copy(d.prev, img.Pix)
	return d.coalesce()
}

// tileChanged compares one tile row by row, stopping at the first difference.
func (d *Differ) tileChanged(img *rfb.Image, x0, y0, x1, y1 int) bool {
	lo, hi := x0*4, x1*4
	for y := y0; y < y1; y++ {
		off := y * img.Stride
		if !bytes.Equal(img.Pix[off+lo:off+hi], d.prev[off+lo:off+hi]) {
			return true
		}
	}
	return false
}

// coalesce turns the dirty-tile grid into rectangles.
//
// Horizontally adjacent dirty tiles in a row become one run; runs in
// consecutive rows that cover the same columns are then stacked into a single
// taller rectangle. That collapses the common cases — a blinking cursor, a
// scrolling window, a repainting panel — into one or two rectangles.
func (d *Differ) coalesce() []rfb.Rect {
	type run struct{ x0, x1 int } // in tile units, half-open

	var rects []rfb.Rect
	// pending holds runs from the previous row that are still growing
	// downwards, together with the tile row they started on.
	type openRun struct {
		run
		startY int
	}
	var open []openRun

	flush := func(o openRun, endY int) {
		x := o.x0 * tileSize
		y := o.startY * tileSize
		rects = append(rects, rfb.Rect{
			X: x,
			Y: y,
			W: min(o.x1*tileSize, d.w) - x,
			H: min(endY*tileSize, d.h) - y,
		})
	}

	for ty := 0; ty < d.tilesY; ty++ {
		// Collect this row's runs.
		var cur []run
		tx := 0
		for tx < d.tilesX {
			if !d.dirty[ty*d.tilesX+tx] {
				tx++
				continue
			}
			start := tx
			for tx < d.tilesX && d.dirty[ty*d.tilesX+tx] {
				tx++
			}
			cur = append(cur, run{start, tx})
		}

		// Extend open runs that continue identically into this row; close the
		// rest.
		var next []openRun
		used := make([]bool, len(cur))
		for _, o := range open {
			matched := false
			for i, c := range cur {
				if !used[i] && c.x0 == o.x0 && c.x1 == o.x1 {
					next = append(next, o)
					used[i] = true
					matched = true
					break
				}
			}
			if !matched {
				flush(o, ty)
			}
		}
		for i, c := range cur {
			if !used[i] {
				next = append(next, openRun{run: c, startY: ty})
			}
		}
		open = next
	}
	for _, o := range open {
		flush(o, d.tilesY)
	}

	// Too many pieces to itemise. Collapsing them into a single bounding box
	// is the obvious move and a bad one: a few scattered changes in opposite
	// corners would then redraw the entire screen, which is exactly how a
	// mostly-idle desktop produces an enormous recording.
	//
	// Row bands are far tighter — each spans only the dirty columns of its own
	// tile row — while still costing one rectangle per row at worst.
	if len(rects) > maxRects {
		return d.rowBands()
	}
	return rects
}

// rowBands returns one rectangle per tile row that has any dirty tile, spanning
// that row's leftmost to rightmost dirty column.
func (d *Differ) rowBands() []rfb.Rect {
	var bands []rfb.Rect

	for ty := 0; ty < d.tilesY; ty++ {
		first, last := -1, -1
		for tx := 0; tx < d.tilesX; tx++ {
			if !d.dirty[ty*d.tilesX+tx] {
				continue
			}
			if first == -1 {
				first = tx
			}
			last = tx
		}
		if first == -1 {
			continue
		}

		x := first * tileSize
		y := ty * tileSize
		band := rfb.Rect{
			X: x,
			Y: y,
			W: min((last+1)*tileSize, d.w) - x,
			H: min((ty+1)*tileSize, d.h) - y,
		}

		// Merge with the previous band when they cover the same columns, so a
		// tall change stays one rectangle rather than one per row.
		if n := len(bands); n > 0 {
			prev := bands[n-1]
			if prev.X == band.X && prev.W == band.W && prev.Y+prev.H == band.Y {
				bands[n-1].H += band.H
				continue
			}
		}
		bands = append(bands, band)
	}
	return bands
}
