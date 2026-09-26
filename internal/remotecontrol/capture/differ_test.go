package capture

import (
	"testing"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/rfb"
)

func fill(im *rfb.Image, r rfb.Rect, b, g, rr byte) {
	for y := r.Y; y < r.Y+r.H; y++ {
		for x := r.X; x < r.X+r.W; x++ {
			off := y*im.Stride + x*4
			im.Pix[off], im.Pix[off+1], im.Pix[off+2] = b, g, rr
		}
	}
}

// covers reports whether the damage rectangles together contain r.
func covers(damage []rfb.Rect, r rfb.Rect) bool {
	for y := r.Y; y < r.Y+r.H; y++ {
		for x := r.X; x < r.X+r.W; x++ {
			in := false
			for _, d := range damage {
				if x >= d.X && x < d.X+d.W && y >= d.Y && y < d.Y+d.H {
					in = true
					break
				}
			}
			if !in {
				return false
			}
		}
	}
	return true
}

func TestDifferReportsNothingForIdenticalFrames(t *testing.T) {
	im := rfb.NewImage(256, 256)
	fill(im, im.Bounds(), 0x10, 0x20, 0x30)

	d := NewDiffer()
	d.Reset(im)

	if got := d.Damage(im); got != nil {
		t.Errorf("damage on an unchanged frame = %+v, want none", got)
	}
}

func TestDifferFindsASmallChange(t *testing.T) {
	im := rfb.NewImage(256, 256)
	fill(im, im.Bounds(), 0, 0, 0)

	d := NewDiffer()
	d.Reset(im)

	// One pixel, deliberately not on a tile boundary.
	changed := rfb.Rect{X: 100, Y: 70, W: 1, H: 1}
	fill(im, changed, 0xFF, 0xFF, 0xFF)

	damage := d.Damage(im)
	if len(damage) == 0 {
		t.Fatal("a changed pixel produced no damage")
	}
	if !covers(damage, changed) {
		t.Errorf("damage %+v does not cover the changed pixel %+v", damage, changed)
	}
	// It should stay tile-sized rather than escalating to the whole screen.
	for _, r := range damage {
		if r.W > tileSize || r.H > tileSize {
			t.Errorf("damage rect %+v is larger than one tile for a 1px change", r)
		}
	}
}

func TestDifferAdoptsNewBaseline(t *testing.T) {
	im := rfb.NewImage(128, 128)
	d := NewDiffer()
	d.Reset(im)

	fill(im, rfb.Rect{X: 0, Y: 0, W: 10, H: 10}, 1, 2, 3)
	if got := d.Damage(im); len(got) == 0 {
		t.Fatal("expected damage after a change")
	}
	// The same frame again must now be clean: the previous call took it as the
	// new baseline. Getting this wrong makes every frame a full redraw.
	if got := d.Damage(im); got != nil {
		t.Errorf("damage repeated for an unchanged frame: %+v", got)
	}
}

func TestDifferCoalescesVerticallyAdjacentRuns(t *testing.T) {
	im := rfb.NewImage(256, 256)
	d := NewDiffer()
	d.Reset(im)

	// A tall stripe spanning four tile rows in the same columns should come
	// back as ONE rectangle, not four.
	stripe := rfb.Rect{X: 64, Y: 0, W: 64, H: 256}
	fill(im, stripe, 9, 9, 9)

	damage := d.Damage(im)
	if len(damage) != 1 {
		t.Fatalf("damage = %d rects, want 1 coalesced rect: %+v", len(damage), damage)
	}
	if !covers(damage, stripe) {
		t.Errorf("damage %+v does not cover the stripe %+v", damage, stripe)
	}
}

func TestDifferClipsToImageEdges(t *testing.T) {
	// 100x100 is not a multiple of the 64px tile size, so the right and bottom
	// tiles are partial. Damage must stop at the image edge rather than
	// reporting coordinates past it.
	im := rfb.NewImage(100, 100)
	d := NewDiffer()
	d.Reset(im)

	fill(im, rfb.Rect{X: 95, Y: 95, W: 5, H: 5}, 7, 7, 7)

	damage := d.Damage(im)
	if len(damage) == 0 {
		t.Fatal("expected damage")
	}
	for _, r := range damage {
		if r.X+r.W > 100 || r.Y+r.H > 100 {
			t.Errorf("damage rect %+v extends past the 100x100 image", r)
		}
	}
}

func TestDifferHandlesResolutionChange(t *testing.T) {
	small := rfb.NewImage(64, 64)
	d := NewDiffer()
	d.Reset(small)

	big := rfb.NewImage(256, 128)
	damage := d.Damage(big)

	if len(damage) != 1 || damage[0] != big.Bounds() {
		t.Fatalf("resolution change damage = %+v, want the full new bounds %+v", damage, big.Bounds())
	}
	// And the new size must have become the baseline.
	if got := d.Damage(big); got != nil {
		t.Errorf("damage after adopting the new size = %+v, want none", got)
	}
}

// area sums the pixels a damage set covers, which is what actually determines
// how much gets encoded, sent and recorded.
func area(rects []rfb.Rect) int {
	total := 0
	for _, r := range rects {
		total += r.W * r.H
	}
	return total
}

// TestDifferFragmentedDamageStaysTight covers the fallback used when damage is
// too scattered to itemise.
//
// Collapsing everything into one bounding box was the original behaviour and
// it is what made a nearly-idle desktop produce enormous recordings: two small
// changes in opposite corners redrew the whole screen. Row bands keep the
// rectangle count bounded without paying for the pixels in between.
func TestDifferFragmentedDamageStaysTight(t *testing.T) {
	const w, h = 1024, 1024
	im := rfb.NewImage(w, h)
	d := NewDiffer()
	d.Reset(im)

	// A checkerboard of small changes: far more runs than maxRects, spread
	// across the whole screen.
	var changed []rfb.Rect
	for ty := 0; ty < 16; ty++ {
		for tx := 0; tx < 16; tx++ {
			if (tx+ty)%2 != 0 {
				continue
			}
			r := rfb.Rect{X: tx * tileSize, Y: ty * tileSize, W: 8, H: 8}
			fill(im, r, 5, 5, 5)
			changed = append(changed, r)
		}
	}

	damage := d.Damage(im)
	if len(damage) == 0 {
		t.Fatal("no damage reported")
	}
	if len(damage) > maxRects {
		t.Errorf("damage = %d rects, want at most %d", len(damage), maxRects)
	}
	for _, c := range changed {
		if !covers(damage, c) {
			t.Fatalf("damage does not cover the change at %+v", c)
		}
	}

	// Every row is dirty in a checkerboard, so bands span the full width and
	// this particular pattern cannot beat the bounding box. What matters is
	// that it is never WORSE — the old code could only ever be the whole screen.
	if got, full := area(damage), w*h; got > full {
		t.Errorf("damage area %d exceeds the full screen %d", got, full)
	}
}

// TestDifferFragmentedDamageAvoidsWholeScreen is the case the bounding box
// handled badly: a handful of changes clustered in two rows, which a global box
// would inflate to the entire display.
func TestDifferFragmentedDamageAvoidsWholeScreen(t *testing.T) {
	const w, h = 1024, 1024
	im := rfb.NewImage(w, h)
	d := NewDiffer()
	d.Reset(im)

	// Alternating tiles along the top row and the bottom row only.
	for _, ty := range []int{0, 15} {
		for tx := 0; tx < 16; tx += 1 {
			if tx%2 != 0 {
				continue
			}
			fill(im, rfb.Rect{X: tx * tileSize, Y: ty * tileSize, W: 8, H: 8}, 7, 7, 7)
		}
	}
	// Enough separate runs to trip the fallback.
	for tx := 0; tx < 16; tx += 2 {
		fill(im, rfb.Rect{X: tx * tileSize, Y: 8 * tileSize, W: 8, H: 8}, 7, 7, 7)
	}

	damage := d.Damage(im)
	got := area(damage)
	full := w * h

	// Three dirty tile rows out of sixteen: the result must be a fraction of
	// the screen, not all of it.
	if got >= full {
		t.Fatalf("damage area %d covers the whole %d-pixel screen", got, full)
	}
	if got > full/4 {
		t.Errorf("damage area %d is more than a quarter of the screen (%d) for three dirty rows", got, full)
	}
	t.Logf("%d rects covering %d px of %d (%.0f%%)", len(damage), got, full, 100*float64(got)/float64(full))
}

func TestSourceForcesFullUpdateOnFirstFrame(t *testing.T) {
	im := rfb.NewImage(64, 48)
	src := NewSource(&stubCapturer{img: im})

	got, damage, err := src.Frame(t.Context())
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	if got != im {
		t.Error("Frame did not return the capturer's image")
	}
	// The viewer starts with an empty framebuffer, so the first frame has to
	// be sent whole even though nothing "changed".
	if len(damage) != 1 || damage[0] != im.Bounds() {
		t.Fatalf("first-frame damage = %+v, want full bounds %+v", damage, im.Bounds())
	}

	if _, damage, err = src.Frame(t.Context()); err != nil {
		t.Fatalf("second Frame: %v", err)
	}
	if damage != nil {
		t.Errorf("second frame reported damage %+v for an unchanged screen", damage)
	}
}

type stubCapturer struct{ img *rfb.Image }

func (s *stubCapturer) Size() (int, int)             { return s.img.Width, s.img.Height }
func (s *stubCapturer) Capture() (*rfb.Image, error) { return s.img, nil }
func (s *stubCapturer) Close() error                 { return nil }
