//go:build windows

package capture

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/rfb"
	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procGetDC                         = user32.NewProc("GetDC")
	procReleaseDC                     = user32.NewProc("ReleaseDC")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetCursorInfo                 = user32.NewProc("GetCursorInfo")
	procGetIconInfo                   = user32.NewProc("GetIconInfo")
	procDrawIconEx                    = user32.NewProc("DrawIconEx")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procGetDIBits              = gdi32.NewProc("GetDIBits")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
)

const (
	smCXScreen = 0
	smCYScreen = 1

	srcCopy = 0x00CC0020
	// captureBlt makes BitBlt include layered (transparent) windows. Without
	// it, anything drawn with WS_EX_LAYERED — which on modern Windows includes
	// a lot of UI — comes back missing from the capture.
	captureBlt = 0x40000000

	biRGB        = 0
	dibRGBColors = 0

	cursorShowing = 0x00000001
	diNormal      = 0x0003

	// dpiAwarenessPerMonitorV2 makes GetSystemMetrics report real pixels on a
	// scaled display. Without it Windows lies to the process about the screen
	// size and the capture comes back stretched and blurry.
	dpiAwarenessPerMonitorV2 = ^uintptr(3) // (DPI_AWARENESS_CONTEXT)-4
)

type point struct{ X, Y int32 }

type cursorInfo struct {
	Size      uint32
	Flags     uint32
	Cursor    windows.Handle
	ScreenPos point
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  windows.Handle
	HbmColor windows.Handle
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

var dpiOnce sync.Once

// setDPIAware opts this process into true-pixel coordinates. It is safe to call
// more than once but only the first call in a process takes effect, which is
// why it is guarded.
func setDPIAware() {
	dpiOnce.Do(func() {
		// Per-monitor v2 exists on Windows 10 1703 and later; older systems
		// fall back to the process-wide (system DPI) flag.
		if err := procSetProcessDpiAwarenessContext.Find(); err == nil {
			if r, _, _ := procSetProcessDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2); r != 0 {
				return
			}
		}
		_, _, _ = procSetProcessDPIAware.Call()
	})
}

// windowsCapturer grabs the primary display with GDI.
//
// GDI rather than DXGI Desktop Duplication: Desktop Duplication is faster, but
// reaching it from Go means hand-rolling COM vtable calls, and it fails
// outright on the secure desktop and across some session transitions — exactly
// the moments a support tool must not go blind. GDI is a few hundred lines of
// plain syscalls, works everywhere, and at the 10-20 fps a helpdesk session
// needs it is not the bottleneck. Desktop Duplication is the optimisation to
// reach for if frame rate ever becomes one.
type windowsCapturer struct {
	w, h int

	screenDC windows.Handle
	memDC    windows.Handle
	bitmap   windows.Handle
	oldObj   windows.Handle

	img *rfb.Image
	bmi bitmapInfoHeader
}

func newCapturer() (Capturer, error) {
	setDPIAware()

	w, _, _ := procGetSystemMetrics.Call(smCXScreen)
	h, _, _ := procGetSystemMetrics.Call(smCYScreen)
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("GetSystemMetrics reported a %dx%d screen", w, h)
	}

	c := &windowsCapturer{w: int(w), h: int(h)}
	if err := c.alloc(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *windowsCapturer) alloc() error {
	dc, _, err := procGetDC.Call(0)
	if dc == 0 {
		return fmt.Errorf("GetDC: %w", err)
	}
	c.screenDC = windows.Handle(dc)

	mem, _, err := procCreateCompatibleDC.Call(dc)
	if mem == 0 {
		c.release()
		return fmt.Errorf("CreateCompatibleDC: %w", err)
	}
	c.memDC = windows.Handle(mem)

	bmp, _, err := procCreateCompatibleBitmap.Call(dc, uintptr(c.w), uintptr(c.h))
	if bmp == 0 {
		c.release()
		return fmt.Errorf("CreateCompatibleBitmap: %w", err)
	}
	c.bitmap = windows.Handle(bmp)

	old, _, _ := procSelectObject.Call(mem, bmp)
	c.oldObj = windows.Handle(old)

	c.img = rfb.NewImage(c.w, c.h)
	c.bmi = bitmapInfoHeader{
		Size:  uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width: int32(c.w),
		// Negative height asks GDI for a top-down bitmap. Left positive, the
		// rows come back bottom-up and the image is upside down.
		Height:      -int32(c.h),
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}
	return nil
}

func (c *windowsCapturer) release() {
	if c.memDC != 0 && c.oldObj != 0 {
		_, _, _ = procSelectObject.Call(uintptr(c.memDC), uintptr(c.oldObj))
		c.oldObj = 0
	}
	if c.bitmap != 0 {
		_, _, _ = procDeleteObject.Call(uintptr(c.bitmap))
		c.bitmap = 0
	}
	if c.memDC != 0 {
		_, _, _ = procDeleteDC.Call(uintptr(c.memDC))
		c.memDC = 0
	}
	if c.screenDC != 0 {
		_, _, _ = procReleaseDC.Call(0, uintptr(c.screenDC))
		c.screenDC = 0
	}
}

func (c *windowsCapturer) Size() (int, int) { return c.w, c.h }

func (c *windowsCapturer) Capture() (*rfb.Image, error) {
	// A resolution change (docking, display swap, RDP resize) invalidates every
	// cached handle, so rebuild rather than blit into a stale bitmap.
	w, _, _ := procGetSystemMetrics.Call(smCXScreen)
	h, _, _ := procGetSystemMetrics.Call(smCYScreen)
	if int(w) != c.w || int(h) != c.h {
		if w == 0 || h == 0 {
			return nil, fmt.Errorf("screen reported as %dx%d", w, h)
		}
		c.release()
		c.w, c.h = int(w), int(h)
		if err := c.alloc(); err != nil {
			return nil, err
		}
	}

	r, _, err := procBitBlt.Call(
		uintptr(c.memDC), 0, 0, uintptr(c.w), uintptr(c.h),
		uintptr(c.screenDC), 0, 0, srcCopy|captureBlt,
	)
	if r == 0 {
		return nil, fmt.Errorf("BitBlt: %w", err)
	}

	c.drawCursor()

	got, _, err := procGetDIBits.Call(
		uintptr(c.memDC), uintptr(c.bitmap), 0, uintptr(c.h),
		uintptr(unsafe.Pointer(&c.img.Pix[0])),
		uintptr(unsafe.Pointer(&c.bmi)),
		dibRGBColors,
	)
	if got == 0 {
		return nil, fmt.Errorf("GetDIBits: %w", err)
	}
	return c.img, nil
}

// drawCursor composites the mouse pointer into the captured frame.
//
// BitBlt never includes the cursor, and a remote desktop without one is
// disorienting for both sides: the admin cannot see what the user is pointing
// at, and the user cannot follow what the admin is doing. RFB does have a
// client-side cursor pseudo-encoding, but compositing works with every viewer
// including the one guacd presents.
func (c *windowsCapturer) drawCursor() {
	ci := cursorInfo{Size: uint32(unsafe.Sizeof(cursorInfo{}))}
	if r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 {
		return
	}
	if ci.Flags&cursorShowing == 0 || ci.Cursor == 0 {
		return
	}

	// The hotspot is where the cursor actually points; drawing at ScreenPos
	// without subtracting it puts the arrow tip in the wrong place.
	var ii iconInfo
	var hotX, hotY int32
	if r, _, _ := procGetIconInfo.Call(uintptr(ci.Cursor), uintptr(unsafe.Pointer(&ii))); r != 0 {
		hotX, hotY = int32(ii.XHotspot), int32(ii.YHotspot)
		// GetIconInfo hands over two bitmaps that belong to the caller.
		if ii.HbmMask != 0 {
			_, _, _ = procDeleteObject.Call(uintptr(ii.HbmMask))
		}
		if ii.HbmColor != 0 {
			_, _, _ = procDeleteObject.Call(uintptr(ii.HbmColor))
		}
	}

	_, _, _ = procDrawIconEx.Call(
		uintptr(c.memDC),
		uintptr(ci.ScreenPos.X-hotX),
		uintptr(ci.ScreenPos.Y-hotY),
		uintptr(ci.Cursor),
		0, 0, 0, 0, diNormal,
	)
}

func (c *windowsCapturer) Close() error {
	c.release()
	return nil
}
