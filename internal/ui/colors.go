package ui

import (
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"raven-partition-manager/internal/disk"
)

type rgb struct{ r, g, b float64 }

func hex(v uint32) rgb {
	return rgb{float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255}
}

// Filesystem colours, picked to stay readable with white text on light and
// dark themes alike.
var fsColors = map[string]rgb{
	"ext2":        hex(0x2f6fb5),
	"ext3":        hex(0x2f6fb5),
	"ext4":        hex(0x2f6fb5),
	"btrfs":       hex(0x1f8a8a),
	"xfs":         hex(0x7a5bb5),
	"vfat":        hex(0x3d8b40),
	"exfat":       hex(0x5f8f2a),
	"ntfs":        hex(0x2e8b6a),
	"swap":        hex(0xb8463a),
	"crypto_LUKS": hex(0x8a6d1f),
	"LVM2_member": hex(0xa0522d),
}

var (
	colorFree     = hex(0x7d7d7d)
	colorUnknown  = hex(0x6b6b80)
	colorExtended = hex(0x4a4a5a)
)

func regionColor(r disk.Region) rgb {
	if r.Kind == disk.RegionFree {
		return colorFree
	}
	if r.Part.Extended() {
		return colorExtended
	}
	if c, ok := fsColors[r.Part.FSType]; ok {
		return c
	}
	return colorUnknown
}

// swatch is the small colour square at the start of each partition row.
func swatch(c rgb) *gtk.DrawingArea {
	a := gtk.NewDrawingArea()
	a.SetSizeRequest(14, 14)
	a.SetVAlign(gtk.AlignCenter)
	a.SetDrawFunc(func(_ *gtk.DrawingArea, cr *cairo.Context, w, h int) {
		roundedRect(cr, 0, 0, float64(w), float64(h), 3)
		cr.SetSourceRGB(c.r, c.g, c.b)
		cr.Fill()
	})
	return a
}

func roundedRect(cr *cairo.Context, x, y, w, h, r float64) {
	if w < 2*r {
		r = w / 2
	}
	if h < 2*r {
		r = h / 2
	}
	const pi = 3.141592653589793
	cr.NewPath()
	cr.Arc(x+w-r, y+r, r, -pi/2, 0)
	cr.Arc(x+w-r, y+h-r, r, 0, pi/2)
	cr.Arc(x+r, y+h-r, r, pi/2, pi)
	cr.Arc(x+r, y+r, r, pi, 3*pi/2)
	cr.ClosePath()
}
