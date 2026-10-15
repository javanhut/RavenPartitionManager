package ui

import (
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"raven-partition-manager/internal/disk"
)

// minSegment keeps tiny partitions (an ESP on a 4 TB disk) visible and
// clickable in the map.
const minSegment = 18.0

// diskMap draws the selected disk as one bar, each region as wide as its
// share of the disk, and reports clicks on a region.
type diskMap struct {
	area     *gtk.DrawingArea
	regions  []disk.Region
	selected int
	note     string // shown instead of the bar when there are no regions
	onSelect func(i int)
}

func newDiskMap(onSelect func(int)) *diskMap {
	m := &diskMap{area: gtk.NewDrawingArea(), selected: -1, onSelect: onSelect}
	m.area.SetSizeRequest(-1, 72)
	m.area.SetHExpand(true)
	m.area.SetDrawFunc(m.draw)
	click := gtk.NewGestureClick()
	click.ConnectPressed(func(_ int, x, _ float64) {
		if i := m.hit(x, float64(m.area.Width())); i >= 0 && m.onSelect != nil {
			m.onSelect(i)
		}
	})
	m.area.AddController(click)
	return m
}

func (m *diskMap) set(regions []disk.Region, selected int, note string) {
	m.regions, m.selected, m.note = regions, selected, note
	m.area.QueueDraw()
}

func (m *diskMap) select_(i int) {
	m.selected = i
	m.area.QueueDraw()
}

// layout gives each region's x offset and width in a bar w pixels wide.
func (m *diskMap) layout(w float64) [][2]float64 {
	n := len(m.regions)
	out := make([][2]float64, n)
	if n == 0 {
		return out
	}
	var total float64
	for _, r := range m.regions {
		total += float64(r.Size)
	}
	// Give every region at least minSegment, then share the rest by size.
	spare := w - minSegment*float64(n)
	if spare < 0 {
		spare = 0
	}
	x := 0.0
	for i, r := range m.regions {
		width := minSegment
		if total > 0 {
			width += spare * float64(r.Size) / total
		}
		out[i] = [2]float64{x, width}
		x += width
	}
	return out
}

func (m *diskMap) hit(x, w float64) int {
	for i, seg := range m.layout(w) {
		if x >= seg[0] && x < seg[0]+seg[1] {
			return i
		}
	}
	return -1
}

func (m *diskMap) draw(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	w, h := float64(width), float64(height)
	cr.SelectFontFace("Sans", cairo.FontSlantNormal, cairo.FontWeightBold)
	cr.SetFontSize(12)

	if len(m.regions) == 0 {
		roundedRect(cr, 1, 1, w-2, h-2, 6)
		cr.SetSourceRGBA(0.5, 0.5, 0.5, 0.25)
		cr.Fill()
		centerText(cr, m.note, w/2, h/2, rgb{0.5, 0.5, 0.5})
		return
	}

	segs := m.layout(w)
	for i, r := range m.regions {
		x, sw := segs[i][0], segs[i][1]
		c := regionColor(r)
		roundedRect(cr, x+1, 1, sw-2, h-2, 5)
		cr.SetSourceRGB(c.r, c.g, c.b)
		cr.Fill()

		if i == m.selected {
			roundedRect(cr, x+2.5, 2.5, sw-5, h-5, 4)
			cr.SetSourceRGB(1, 0.78, 0.2)
			cr.SetLineWidth(3)
			cr.Stroke()
		}

		cr.Save()
		cr.Rectangle(x+4, 0, sw-8, h)
		cr.Clip()
		name, size := regionCaption(r)
		if sw > 60 {
			centerText(cr, name, x+sw/2, h/2-8, rgb{1, 1, 1})
			centerText(cr, size, x+sw/2, h/2+10, rgb{1, 1, 1})
		}
		cr.Restore()
	}
}

func regionCaption(r disk.Region) (string, string) {
	size := disk.HumanSize(r.Size)
	if r.Kind == disk.RegionFree {
		return "Unallocated", size
	}
	name := r.Part.Name
	if r.Part.FSType != "" {
		name += " · " + r.Part.FSType
	}
	return name, size
}

func centerText(cr *cairo.Context, s string, cx, cy float64, c rgb) {
	ext := cr.TextExtents(s)
	cr.MoveTo(cx-ext.Width/2-ext.XBearing, cy-ext.Height/2-ext.YBearing)
	cr.SetSourceRGB(c.r, c.g, c.b)
	cr.ShowText(s)
}
