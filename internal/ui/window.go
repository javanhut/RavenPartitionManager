// Package ui is the GTK 4 window: a disk picker, a map of the selected disk,
// its partitions and free space in a list, the operations for the selection,
// and a log of everything run (or, in dry-run mode, everything that would
// have been run).
package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"raven-partition-manager/internal/disk"
	"raven-partition-manager/internal/ops"
)

// Window is the application's main window.
type Window struct {
	win    *gtk.ApplicationWindow
	runner ops.Runner

	disks    []disk.Disk
	diskIdx  int
	regions  []disk.Region
	selected int
	running  bool

	diskDrop     *gtk.DropDown
	settingDisks bool
	dryRun       *gtk.Switch
	modeBanner   *gtk.Label
	toolsBanner  *gtk.Label
	info         *gtk.Label
	dmap         *diskMap
	list         *gtk.ListBox
	settingRows  bool
	btnTable     *gtk.Button
	btnNew       *gtk.Button
	btnDelete    *gtk.Button
	btnFormat    *gtk.Button
	btnResize    *gtk.Button
	btnUnmount   *gtk.Button
	status       *gtk.Label
	btnRefresh   *gtk.Button
	log          *gtk.TextView
	logEnd       *gtk.TextMark
}

// NewWindow builds the window and loads the disks. Dry run starts on.
func NewWindow(app *gtk.Application) *Window {
	loadCSS()
	w := &Window{win: gtk.NewApplicationWindow(app), selected: -1, runner: ops.Runner{DryRun: true}}
	w.win.SetTitle("Partitions")
	w.win.SetDefaultSize(980, 680)

	// Header: disk picker, refresh, dry-run switch.
	header := gtk.NewHeaderBar()
	w.diskDrop = gtk.NewDropDownFromStrings(nil)
	w.diskDrop.SetSizeRequest(360, -1)
	w.diskDrop.NotifyProperty("selected", func() {
		if !w.settingDisks {
			w.diskIdx = int(w.diskDrop.Selected())
			w.selected = -1
			w.showDisk()
		}
	})
	header.SetTitleWidget(w.diskDrop)

	w.btnRefresh = gtk.NewButtonFromIconName("view-refresh-symbolic")
	w.btnRefresh.SetTooltipText("Read the disks again")
	w.btnRefresh.ConnectClicked(w.refresh)
	header.PackStart(w.btnRefresh)

	dryBox := gtk.NewBox(gtk.OrientationHorizontal, 6)
	dryLabel := gtk.NewLabel("Dry run")
	w.dryRun = gtk.NewSwitch()
	w.dryRun.SetActive(true)
	w.dryRun.SetVAlign(gtk.AlignCenter)
	w.dryRun.SetTooltipText("When on, operations only log the commands they would run")
	w.dryRun.NotifyProperty("active", w.updateMode)
	dryBox.Append(dryLabel)
	dryBox.Append(w.dryRun)
	header.PackEnd(dryBox)
	w.win.SetTitlebar(header)

	// Body.
	body := gtk.NewBox(gtk.OrientationVertical, 0)

	w.modeBanner = gtk.NewLabel("")
	w.modeBanner.AddCSSClass("mode-banner")
	w.modeBanner.SetWrap(true)
	body.Append(w.modeBanner)

	w.toolsBanner = gtk.NewLabel("")
	w.toolsBanner.AddCSSClass("tools-banner")
	w.toolsBanner.SetWrap(true)
	w.toolsBanner.SetSelectable(true)
	w.toolsBanner.SetVisible(false)
	body.Append(w.toolsBanner)

	top := gtk.NewBox(gtk.OrientationVertical, 10)
	top.AddCSSClass("content")
	w.info = gtk.NewLabel("")
	w.info.SetXAlign(0)
	w.info.SetWrap(true)
	top.Append(w.info)
	w.dmap = newDiskMap(w.selectRegion)
	top.Append(w.dmap.area)
	body.Append(top)

	// The header and rows share one scrolled area so they stay aligned, and
	// so the columns' width never becomes the window's minimum width (a
	// tiled window narrower than the columns scrolls instead).
	table := gtk.NewBox(gtk.OrientationVertical, 0)
	table.Append(columnsRow(true, nil, "Partition", "Filesystem", "Label", "Size", "Mount point", "Type"))
	w.list = gtk.NewListBox()
	w.list.SetSelectionMode(gtk.SelectionSingle)
	w.list.AddCSSClass("partition-list")
	w.list.ConnectRowSelected(func(row *gtk.ListBoxRow) {
		if w.settingRows {
			return
		}
		i := -1
		if row != nil {
			i = row.Index()
		}
		w.selected = i
		w.dmap.select_(i)
		w.updateActions()
	})
	table.Append(w.list)
	scroll := gtk.NewScrolledWindow()
	scroll.SetPolicy(gtk.PolicyAutomatic, gtk.PolicyAutomatic)
	scroll.SetChild(table)
	scroll.SetVExpand(true)
	scroll.SetSizeRequest(-1, 150)
	listBox := gtk.NewBox(gtk.OrientationVertical, 0)
	listBox.AddCSSClass("content")
	listBox.Append(scroll)

	actions := gtk.NewActionBar()
	w.btnNew = actionButton("New Partition", "list-add-symbolic", w.onNew)
	w.btnDelete = actionButton("Delete", "edit-delete-symbolic", w.onDelete)
	w.btnFormat = actionButton("Format", "drive-harddisk-symbolic", w.onFormat)
	w.btnResize = actionButton("Resize", "zoom-fit-best-symbolic", w.onResize)
	w.btnUnmount = actionButton("Unmount", "media-eject-symbolic", w.onUnmount)
	w.btnTable = actionButton("New Partition Table", "document-new-symbolic", w.onTable)
	actions.PackStart(w.btnNew)
	actions.PackStart(w.btnDelete)
	actions.PackStart(w.btnFormat)
	actions.PackStart(w.btnResize)
	actions.PackStart(w.btnUnmount)
	actions.PackEnd(w.btnTable)

	// Why the selection's actions are unavailable, in words, so nobody has
	// to hover over greyed-out buttons to find out.
	w.status = gtk.NewLabel("")
	w.status.SetXAlign(0)
	w.status.SetWrap(true)
	w.status.AddCSSClass("status-line")
	w.status.SetVisible(false)

	w.log = gtk.NewTextView()
	w.log.SetEditable(false)
	w.log.SetMonospace(true)
	w.log.SetWrapMode(gtk.WrapWordChar)
	w.log.AddCSSClass("log")
	buf := w.log.Buffer()
	w.logEnd = buf.CreateMark("end", buf.EndIter(), false)
	logScroll := gtk.NewScrolledWindow()
	logScroll.SetChild(w.log)
	logScroll.SetVExpand(true)
	logBox := gtk.NewBox(gtk.OrientationVertical, 0)
	logHeading := gtk.NewLabel("Log")
	logHeading.SetXAlign(0)
	logHeading.AddCSSClass("column-header")
	logHeading.AddCSSClass("log-heading")
	logBox.Append(logHeading)
	logBox.Append(logScroll)
	logBox.SetSizeRequest(-1, 110)

	upper := gtk.NewBox(gtk.OrientationVertical, 0)
	upper.Append(listBox)
	upper.Append(actions)
	upper.Append(w.status)
	upper.SetVExpand(true)

	paned := gtk.NewPaned(gtk.OrientationVertical)
	paned.SetStartChild(upper)
	paned.SetEndChild(logBox)
	// The list and its actions never collapse; the log keeps its natural
	// height and gives way first when the window is short.
	paned.SetShrinkStartChild(false)
	paned.SetResizeEndChild(false)
	paned.SetShrinkEndChild(false)
	paned.SetVExpand(true)
	body.Append(paned)

	w.win.SetChild(body)
	w.updateMode()
	w.refresh()
	w.appendLog(fmt.Sprintf("Found %d disks. Dry run is on: operations only log their commands until you switch it off.", len(w.disks)))
	return w
}

// Present shows the window.
func (w *Window) Present() { w.win.Present() }

func actionButton(label, icon string, onClick func()) *gtk.Button {
	b := gtk.NewButton()
	box := gtk.NewBox(gtk.OrientationHorizontal, 6)
	box.Append(gtk.NewImageFromIconName(icon))
	box.Append(gtk.NewLabel(label))
	b.SetChild(box)
	b.ConnectClicked(onClick)
	return b
}

// Column widths in pixels, shared by the header and every row. Pixels rather
// than characters, so the bold header lines up with the rows.
var columnWidths = []int{130, 90, 120, 85, 170, 140}

func columnsRow(header bool, sw gtk.Widgetter, cells ...string) *gtk.Box {
	row := gtk.NewBox(gtk.OrientationHorizontal, 12)
	row.AddCSSClass("columns")
	if header {
		row.AddCSSClass("column-header")
	}
	if sw == nil {
		spacer := gtk.NewBox(gtk.OrientationHorizontal, 0)
		spacer.SetSizeRequest(14, -1)
		sw = spacer
	}
	row.Append(sw)
	for i, c := range cells {
		l := gtk.NewLabel(c)
		l.SetXAlign(0)
		l.SetEllipsize(pango.EllipsizeEnd)
		// A one-character natural width lets the size request alone set
		// the column, and long text ellipsizes instead of pushing columns.
		l.SetSizeRequest(columnWidths[i], -1)
		l.SetMaxWidthChars(1)
		if i == len(cells)-1 {
			l.SetHExpand(true)
		}
		if i == 0 && !header {
			l.AddCSSClass("part-name")
		}
		row.Append(l)
	}
	return row
}

func (w *Window) current() *disk.Disk {
	if w.diskIdx < 0 || w.diskIdx >= len(w.disks) {
		return nil
	}
	return &w.disks[w.diskIdx]
}

func (w *Window) selectedRegion() *disk.Region {
	if w.selected < 0 || w.selected >= len(w.regions) {
		return nil
	}
	return &w.regions[w.selected]
}

// refresh reads the disks again, keeping the same disk and region selected
// when they still exist.
func (w *Window) refresh() {
	w.checkTools()
	var keepDisk string
	var keepStart uint64
	if d := w.current(); d != nil {
		keepDisk = d.Path
	}
	if r := w.selectedRegion(); r != nil {
		keepStart = r.Start
	}

	disks, err := disk.Probe()
	if err != nil {
		w.appendLog("Error: " + err.Error())
	}
	w.disks = disks
	w.diskIdx = 0
	names := make([]string, len(disks))
	for i := range disks {
		names[i] = disks[i].DisplayName()
		if disks[i].Path == keepDisk {
			w.diskIdx = i
		}
	}
	w.settingDisks = true
	w.diskDrop.SetModel(gtk.NewStringList(names))
	if len(disks) > 0 {
		w.diskDrop.SetSelected(uint(w.diskIdx))
	}
	w.settingDisks = false

	w.selected = -1
	w.showDisk()
	for i, r := range w.regions {
		if keepStart != 0 && r.Start == keepStart {
			w.selectRegion(i)
			break
		}
	}
}

// showDisk fills the info line, map and list for the current disk.
func (w *Window) showDisk() {
	w.settingRows = true
	w.list.RemoveAll()
	w.settingRows = false

	d := w.current()
	if d == nil {
		w.info.SetText("No disks found.")
		w.regions = nil
		w.dmap.set(nil, -1, "No disks")
		w.updateActions()
		return
	}

	w.info.SetMarkup(diskInfoMarkup(d))
	w.regions = d.Regions()
	note := ""
	switch {
	case d.Table == "" && d.FSType != "":
		note = fmt.Sprintf("No partition table: the whole disk is one %s filesystem", d.FSType)
	case d.Table == "":
		note = "No partition table"
	}
	w.dmap.set(w.regions, w.selected, note)

	for _, r := range w.regions {
		row := gtk.NewListBoxRow()
		row.SetChild(regionRow(r))
		w.list.Append(row)
	}
	if len(w.regions) == 0 {
		empty := gtk.NewLabel(note + ". Create a partition table to add partitions.")
		empty.AddCSSClass("dim-label")
		empty.SetMarginTop(24)
		empty.SetMarginBottom(24)
		w.list.SetPlaceholder(empty)
	}
	w.updateActions()
}

func diskInfoMarkup(d *disk.Disk) string {
	var parts []string
	model := d.Model
	if model == "" {
		model = d.Name
	}
	parts = append(parts, "<b>"+glib.MarkupEscapeText(model)+"</b>")
	parts = append(parts, glib.MarkupEscapeText(d.Path))
	parts = append(parts, disk.HumanSize(d.Size))
	switch d.Table {
	case "gpt":
		parts = append(parts, "GPT")
	case "dos":
		parts = append(parts, "MBR")
	case "":
		parts = append(parts, "no partition table")
	default:
		parts = append(parts, glib.MarkupEscapeText(d.Table))
	}
	if d.Transport != "" {
		parts = append(parts, strings.ToUpper(d.Transport))
	}
	parts = append(parts, fmt.Sprintf("%d-byte sectors", d.SectorSize))
	if d.Removable {
		parts = append(parts, "removable")
	}
	if d.ReadOnly {
		parts = append(parts, "<span foreground=\"#c0392b\">read-only</span>")
	}
	return strings.Join(parts, "  ·  ")
}

func regionRow(r disk.Region) *gtk.Box {
	if r.Kind == disk.RegionFree {
		row := columnsRow(false, swatch(colorFree), "Unallocated", "", "", disk.HumanSize(r.Size), "", "")
		row.AddCSSClass("free-row")
		return row
	}
	p := r.Part
	label := p.Label
	if label == "" {
		label = p.PartLabel
	}
	typ := p.TypeName
	if p.Extended() {
		typ = "Extended"
	}
	mount := strings.Join(p.Mountpoints, ", ")
	if mount == "" && len(p.Holders) > 0 {
		mount = "in use by " + strings.Join(p.Holders, ", ")
	}
	return columnsRow(false, swatch(regionColor(r)), p.Name, p.FSType, label, disk.HumanSize(p.Size), mount, typ)
}

func (w *Window) selectRegion(i int) {
	if i < 0 || i >= len(w.regions) {
		return
	}
	w.selected = i
	w.dmap.select_(i)
	if row := w.list.RowAtIndex(i); row != nil {
		w.list.SelectRow(row)
	}
	w.updateActions()
}

// updateActions enables each operation only when it can apply to the
// selection, and says why not in its tooltip when it cannot.
func (w *Window) updateActions() {
	d := w.current()
	r := w.selectedRegion()
	set := func(b *gtk.Button, ok bool, tip, why string) {
		b.SetSensitive(ok && !w.running)
		if ok || why == "" {
			b.SetTooltipText(tip)
		} else {
			b.SetTooltipText(tip + " — " + why)
		}
	}
	w.btnRefresh.SetSensitive(!w.running)
	w.diskDrop.SetSensitive(!w.running)

	all := []*gtk.Button{w.btnTable, w.btnNew, w.btnDelete, w.btnFormat, w.btnResize, w.btnUnmount}
	if d == nil {
		for _, b := range all {
			b.SetSensitive(false)
		}
		w.setStatus("")
		return
	}

	switch {
	case d.ReadOnly:
		set(w.btnTable, false, "Erase the disk and write an empty partition table", "the disk is read-only")
	case d.Busy():
		set(w.btnTable, false, "Erase the disk and write an empty partition table", "something on the disk is mounted or in use")
	default:
		set(w.btnTable, true, "Erase the disk and write an empty partition table", "")
	}

	newOK, newWhy := false, "select unallocated space"
	if r != nil && r.Kind == disk.RegionFree {
		newOK, newWhy = d.CanAddPartition()
		if d.ReadOnly {
			newOK, newWhy = false, "the disk is read-only"
		}
	}
	set(w.btnNew, newOK, "Create a partition in the selected free space", newWhy)

	partWhy := "select a partition"
	var p *disk.Partition
	if r != nil && r.Kind == disk.RegionPartition {
		p = r.Part
		switch {
		case d.ReadOnly:
			partWhy = "the disk is read-only"
		case p.Busy():
			partWhy = "it is mounted or in use; unmount it first"
		default:
			partWhy = ""
		}
	}
	partOK := p != nil && partWhy == ""
	set(w.btnDelete, partOK, "Remove the selected partition", partWhy)

	fmtOK, fmtWhy := partOK, partWhy
	if fmtOK && p.Extended() {
		fmtOK, fmtWhy = false, "an extended partition cannot hold a filesystem"
	}
	set(w.btnFormat, fmtOK, "Write a new filesystem to the selected partition", fmtWhy)

	resOK, resWhy := partOK, partWhy
	if resOK {
		if _, err := ops.Limits(d, p); err != nil {
			resOK, resWhy = false, err.Error()
		}
	}
	set(w.btnResize, resOK, "Grow or shrink the selected partition", resWhy)

	// Unmounting changes nothing on the disk, so a read-only disk is no reason
	// not to.
	umOK, umWhy := false, "select a mounted partition"
	if p != nil {
		if _, err := ops.Unmount(p); err == nil {
			umOK, umWhy = true, ""
		} else {
			umWhy = err.Error()
		}
	}
	set(w.btnUnmount, umOK, "Unmount the selected partition so it can be changed", umWhy)

	w.setStatus(statusText(d, p, resOK, resWhy))
}

// statusText explains, most fundamental reason first, what is stopping
// changes to the selected disk and partition.
func statusText(d *disk.Disk, p *disk.Partition, resOK bool, resWhy string) string {
	if d.ReadOnly {
		return fmt.Sprintf("%s is read-only: the kernel will not write to it, so nothing on it can be changed. "+
			"For an SD card, slide the lock switch on its side away from \"Lock\", then unplug the reader, plug it back in and press Refresh. "+
			"If it stays read-only with the switch unlocked, the card may have locked itself after wearing out.", d.Path)
	}
	if p == nil {
		return ""
	}
	if len(p.Mountpoints) > 0 {
		return fmt.Sprintf("%s is mounted at %s. Unmount it before deleting, formatting or resizing it.", p.Name, strings.Join(p.Mountpoints, ", "))
	}
	if len(p.Holders) > 0 {
		return fmt.Sprintf("%s is in use by %s. Close or deactivate that before changing it.", p.Name, strings.Join(p.Holders, ", "))
	}
	if !resOK && !p.Extended() {
		return "Resize is unavailable: " + resWhy + "."
	}
	return ""
}

func (w *Window) setStatus(text string) {
	w.status.SetText(text)
	w.status.SetVisible(text != "")
}

// checkTools says which of the tools every change needs are missing, and
// keeps dry run on until they are installed. Refresh checks again.
func (w *Window) checkTools() {
	msg := ops.MissingMessage(ops.MissingTools())
	if msg != "" {
		msg += "\nPress Refresh once they are installed."
		w.dryRun.SetActive(true)
		w.dryRun.SetTooltipText("Changes need the tools named above")
	} else {
		w.dryRun.SetTooltipText("When on, operations only log the commands they would run")
	}
	w.dryRun.SetSensitive(msg == "")
	w.toolsBanner.SetText(msg)
	w.toolsBanner.SetVisible(msg != "")
}

func (w *Window) updateMode() {
	w.runner.DryRun = w.dryRun.Active()
	if w.runner.DryRun {
		w.modeBanner.SetText("Dry run — operations only show the commands they would run. Nothing on any disk is changed.")
		w.modeBanner.RemoveCSSClass("live")
		w.modeBanner.AddCSSClass("dry")
	} else {
		w.modeBanner.SetText("Live — operations are written to disk as root after you confirm them.")
		w.modeBanner.RemoveCSSClass("dry")
		w.modeBanner.AddCSSClass("live")
	}
}

func (w *Window) parent() *gtk.Window { return &w.win.Window }

func (w *Window) onTable() {
	if d := w.current(); d != nil {
		newTableDialog(w.parent(), d, w.confirm)
	}
}

func (w *Window) onNew() {
	d, r := w.current(), w.selectedRegion()
	if d != nil && r != nil && r.Kind == disk.RegionFree {
		newPartitionDialog(w.parent(), d, *r, w.confirm)
	}
}

func (w *Window) onDelete() {
	d, r := w.current(), w.selectedRegion()
	if d == nil || r == nil || r.Part == nil {
		return
	}
	plan, err := ops.Delete(d, r.Part)
	if err != nil {
		errorDialog(w.parent(), "Cannot delete "+r.Part.Path, err)
		return
	}
	w.confirm(plan)
}

func (w *Window) onUnmount() {
	r := w.selectedRegion()
	if r == nil || r.Part == nil {
		return
	}
	plan, err := ops.Unmount(r.Part)
	if err != nil {
		errorDialog(w.parent(), "Cannot unmount "+r.Part.Path, err)
		return
	}
	w.confirm(plan)
}

func (w *Window) onFormat() {
	d, r := w.current(), w.selectedRegion()
	if d != nil && r != nil && r.Part != nil {
		formatDialog(w.parent(), d, r.Part, w.confirm)
	}
}

func (w *Window) onResize() {
	d, r := w.current(), w.selectedRegion()
	if d == nil || r == nil || r.Part == nil {
		return
	}
	limits, err := ops.Limits(d, r.Part)
	if err != nil {
		errorDialog(w.parent(), "Cannot resize "+r.Part.Path, err)
		return
	}
	resizeDialog(w.parent(), d, r.Part, limits, w.confirm)
}

// confirm shows the plan and runs it once the user agrees.
func (w *Window) confirm(plan *ops.Plan) {
	confirmPlan(w.parent(), plan, w.runner.DryRun, func() { w.execute(plan) })
}

// execute runs a plan off the main loop, streaming its output to the log,
// then reads the disks again.
func (w *Window) execute(plan *ops.Plan) {
	w.running = true
	w.updateActions()
	runner := w.runner
	go func() {
		err := runner.Run(context.Background(), plan, func(line string) {
			glib.IdleAdd(func() { w.appendLog(line) })
		})
		glib.IdleAdd(func() {
			w.running = false
			if err != nil {
				w.appendLog("Error: " + err.Error())
			}
			if runner.DryRun {
				w.updateActions()
			} else {
				w.refresh()
			}
			switch {
			case errors.Is(err, ops.ErrTableMismatch):
				w.offerReload(plan.Title+" did not finish", plan.Disk)
			case err != nil:
				errorDialog(w.parent(), plan.Title+" did not finish", err)
			}
		})
	}()
}

// offerReload follows a plan that stopped because the partition table on the
// disk is not the one shown, and offers to have the kernel re-read it.
func (w *Window) offerReload(title, path string) {
	var dk *disk.Disk
	for i := range w.disks {
		if w.disks[i].Path == path {
			dk = &w.disks[i]
		}
	}
	if dk == nil {
		errorDialog(w.parent(), title, ops.ErrTableMismatch)
		return
	}
	plan, err := ops.ReloadTable(dk)
	if err != nil {
		errorDialog(w.parent(), title, fmt.Errorf("%w; it cannot be re-read while %v", ops.ErrTableMismatch, err))
		return
	}
	reloadDialog(w.parent(), title, dk, plan, w.confirm)
}

func (w *Window) appendLog(line string) {
	buf := w.log.Buffer()
	buf.Insert(buf.EndIter(), time.Now().Format("15:04:05  ")+line+"\n")
	w.log.ScrollToMark(w.logEnd, 0, false, 0, 1)
}

const css = `
.content { padding: 12px 16px; }
.mode-banner { padding: 6px 16px; font-size: 0.92em; }
.mode-banner.dry { background: alpha(#3584e4, 0.14); }
.mode-banner.live { background: alpha(#e01b24, 0.22); font-weight: bold; }
.tools-banner { padding: 6px 16px; font-size: 0.92em; background: alpha(#e5a50a, 0.22); }
.columns { padding: 6px 10px; }
.column-header { font-weight: bold; opacity: 0.65; }
.log-heading { padding: 8px 12px 0 12px; }
.partition-list { border-radius: 8px; }
.part-name { font-weight: bold; }
.free-row { opacity: 0.75; }
.status-line { padding: 4px 16px 8px 16px; color: #c64600; }
.log { padding: 8px 12px; font-size: 0.9em; }
.dialog-body { padding: 18px 20px; }
.command-list { padding: 10px 12px; border-radius: 6px; background: alpha(currentColor, 0.07); font-size: 0.92em; }
.monospace { font-family: monospace; }
.error-text { color: #e01b24; }
.warning-text { color: #c64600; font-weight: bold; }
`

func loadCSS() {
	p := gtk.NewCSSProvider()
	p.LoadFromString(css)
	gtk.StyleContextAddProviderForDisplay(gdk.DisplayGetDefault(), p, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
}
