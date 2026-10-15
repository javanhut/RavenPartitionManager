package ui

import (
	"fmt"
	"math"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"raven-partition-manager/internal/disk"
	"raven-partition-manager/internal/ops"
)

// dialog is a small modal window with a body and Cancel/OK buttons. onOK
// returns false to keep the dialog open (after showing an error in it).
type dialog struct {
	win    *gtk.Window
	body   *gtk.Box
	error  *gtk.Label
	ok     *gtk.Button
	cancel *gtk.Button
}

func newDialog(parent *gtk.Window, title, okLabel string, destructive bool) *dialog {
	d := &dialog{win: gtk.NewWindow()}
	d.win.SetTitle(title)
	d.win.SetModal(true)
	d.win.SetTransientFor(parent)
	d.win.SetDefaultSize(520, -1)
	d.win.SetResizable(false)

	outer := gtk.NewBox(gtk.OrientationVertical, 12)
	outer.AddCSSClass("dialog-body")

	d.body = gtk.NewBox(gtk.OrientationVertical, 10)
	outer.Append(d.body)

	d.error = gtk.NewLabel("")
	d.error.AddCSSClass("error-text")
	d.error.SetXAlign(0)
	d.error.SetWrap(true)
	d.error.SetVisible(false)
	outer.Append(d.error)

	buttons := gtk.NewBox(gtk.OrientationHorizontal, 8)
	buttons.SetHAlign(gtk.AlignEnd)
	buttons.SetMarginTop(6)
	d.cancel = gtk.NewButtonWithLabel("Cancel")
	d.cancel.ConnectClicked(d.win.Close)
	d.ok = gtk.NewButtonWithLabel(okLabel)
	if destructive {
		d.ok.AddCSSClass("destructive-action")
	} else {
		d.ok.AddCSSClass("suggested-action")
	}
	buttons.Append(d.cancel)
	buttons.Append(d.ok)
	outer.Append(buttons)

	d.win.SetChild(outer)
	return d
}

func (d *dialog) onOK(f func() bool) {
	d.ok.ConnectClicked(func() {
		if f() {
			d.win.Close()
		}
	})
}

func (d *dialog) fail(err error) bool {
	d.error.SetText(err.Error())
	d.error.SetVisible(true)
	return false
}

func (d *dialog) text(s string) *gtk.Label {
	l := gtk.NewLabel(s)
	l.SetXAlign(0)
	l.SetWrap(true)
	d.body.Append(l)
	return l
}

// form lays out labelled fields in a two-column grid.
type form struct {
	grid *gtk.Grid
	row  int
}

func (d *dialog) form() *form {
	g := gtk.NewGrid()
	g.SetRowSpacing(8)
	g.SetColumnSpacing(12)
	d.body.Append(g)
	return &form{grid: g}
}

func (f *form) add(label string, w gtk.Widgetter) {
	l := gtk.NewLabel(label)
	l.SetXAlign(1)
	l.AddCSSClass("dim-label")
	f.grid.Attach(l, 0, f.row, 1, 1)
	if hw, ok := w.(interface{ SetHExpand(bool) }); ok {
		hw.SetHExpand(true)
	}
	f.grid.Attach(w, 1, f.row, 1, 1)
	f.row++
}

// mibSpin is a size field in MiB.
func mibSpin(minBytes, maxBytes, value uint64) *gtk.SpinButton {
	lo, hi := float64(minBytes/disk.MiB), float64(maxBytes/disk.MiB)
	if hi < lo {
		hi = lo
	}
	s := gtk.NewSpinButtonWithRange(lo, hi, 1)
	s.SetDigits(0)
	s.SetValue(math.Min(hi, math.Max(lo, float64(value/disk.MiB))))
	return s
}

func spinBytes(s *gtk.SpinButton) uint64 {
	return uint64(math.Round(s.Value())) * disk.MiB
}

// sizeHint keeps a "= 12.3 GiB" label next to a MiB field up to date.
func sizeHint(s *gtk.SpinButton) gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationHorizontal, 8)
	hint := gtk.NewLabel("")
	hint.AddCSSClass("dim-label")
	update := func() { hint.SetText("MiB  = " + disk.HumanSize(spinBytes(s))) }
	s.ConnectValueChanged(update)
	update()
	s.SetHExpand(true)
	box.Append(s)
	box.Append(hint)
	return box
}

// fsChoice is a filesystem dropdown limited to installed tools, optionally
// with an "Unformatted" entry first.
type fsChoice struct {
	drop *gtk.DropDown
	ids  []string
}

func newFSChoice(allowNone bool, preferred string) *fsChoice {
	c := &fsChoice{}
	var names []string
	if allowNone {
		c.ids = append(c.ids, "")
		names = append(names, "Unformatted")
	}
	sel := 0
	for _, f := range ops.AvailableFilesystems() {
		if f.ID == preferred {
			sel = len(c.ids)
		}
		c.ids = append(c.ids, f.ID)
		names = append(names, f.Name)
	}
	c.drop = gtk.NewDropDownFromStrings(names)
	c.drop.SetSelected(uint(sel))
	return c
}

func (c *fsChoice) id() string {
	i := int(c.drop.Selected())
	if i < 0 || i >= len(c.ids) {
		return ""
	}
	return c.ids[i]
}

// confirmPlan shows exactly what a plan will run and calls apply when the
// user agrees.
func confirmPlan(parent *gtk.Window, plan *ops.Plan, dryRun bool, apply func()) {
	okLabel := "Apply"
	if dryRun {
		okLabel = "Simulate"
	}
	d := newDialog(parent, plan.Title, okLabel, plan.Destructive && !dryRun)
	d.text(plan.Summary)
	if dryRun {
		d.text("Dry run is on: these commands are only written to the log. Nothing on the disk changes.").AddCSSClass("dim-label")
	} else {
		warn := "These commands run as root as soon as you press Apply."
		if plan.Destructive {
			warn += " This cannot be undone."
		}
		d.text(warn).AddCSSClass("warning-text")
	}
	cmds := gtk.NewLabel(plan.Commands())
	cmds.SetXAlign(0)
	cmds.SetSelectable(true)
	cmds.SetWrap(true)
	cmds.AddCSSClass("monospace")
	cmds.AddCSSClass("command-list")
	d.body.Append(cmds)
	d.onOK(func() bool { apply(); return true })
	d.win.Present()
	// Focus a button, not the selectable command list (which would select
	// all of it); Cancel when Enter would otherwise erase something.
	if plan.Destructive && !dryRun {
		d.cancel.GrabFocus()
	} else {
		d.ok.GrabFocus()
	}
}

func errorDialog(parent *gtk.Window, title string, err error) {
	d := newDialog(parent, title, "Close", false)
	d.text(err.Error())
	d.onOK(func() bool { return true })
	d.win.Present()
}

// The dialogs below collect what each operation needs, build its plan and
// hand it to confirmPlan via next.

func newTableDialog(parent *gtk.Window, dk *disk.Disk, next func(*ops.Plan)) {
	d := newDialog(parent, "New partition table on "+dk.Path, "Continue", false)
	d.text(fmt.Sprintf("Write an empty partition table to %s. Everything on the disk is erased.", dk.DisplayName()))
	choice := gtk.NewDropDownFromStrings([]string{"GPT (recommended)", "MBR / msdos (legacy BIOS, at most 4 partitions)"})
	d.form().add("Type", choice)
	d.onOK(func() bool {
		t := ops.TableGPT
		if choice.Selected() == 1 {
			t = ops.TableMSDOS
		}
		plan, err := ops.NewTable(dk, t)
		if err != nil {
			return d.fail(err)
		}
		next(plan)
		return true
	})
	d.win.Present()
}

func newPartitionDialog(parent *gtk.Window, dk *disk.Disk, free disk.Region, next func(*ops.Plan)) {
	d := newDialog(parent, "New partition on "+dk.Path, "Continue", false)
	d.text(fmt.Sprintf("%s of unallocated space. The partition starts at the beginning of it.", disk.HumanSize(free.Size)))
	f := d.form()
	size := mibSpin(disk.MiB, free.Size, free.Size)
	f.add("Size", sizeHint(size))
	fs := newFSChoice(true, "ext4")
	f.add("Filesystem", fs.drop)
	label := gtk.NewEntry()
	label.SetPlaceholderText("optional")
	f.add("Label", label)
	esp := gtk.NewCheckButtonWithLabel("EFI system partition (FAT32)")
	if ops.FilesystemByID("fat32").Available() {
		f.add("", esp)
		esp.ConnectToggled(func() {
			if esp.Active() {
				for i, id := range fs.ids {
					if id == "fat32" {
						fs.drop.SetSelected(uint(i))
					}
				}
			}
		})
	}
	d.onOK(func() bool {
		plan, err := ops.Create(dk, ops.CreateRequest{
			Start: free.Start,
			Size:  spinBytes(size),
			FS:    fs.id(),
			Label: label.Text(),
			ESP:   esp.Active(),
		})
		if err != nil {
			return d.fail(err)
		}
		next(plan)
		return true
	})
	d.win.Present()
}

func formatDialog(parent *gtk.Window, dk *disk.Disk, p *disk.Partition, next func(*ops.Plan)) {
	d := newDialog(parent, "Format "+p.Path, "Continue", false)
	d.text(fmt.Sprintf("Write a new filesystem to %s (%s). Everything on it is erased.", p.Path, disk.HumanSize(p.Size)))
	f := d.form()
	pref := p.FSType
	if pref == "vfat" {
		pref = "fat32"
	}
	if pref == "" {
		pref = "ext4"
	}
	fs := newFSChoice(false, pref)
	f.add("Filesystem", fs.drop)
	label := gtk.NewEntry()
	label.SetText(p.Label)
	label.SetPlaceholderText("optional")
	f.add("Label", label)
	d.onOK(func() bool {
		plan, err := ops.Format(dk, p, fs.id(), label.Text())
		if err != nil {
			return d.fail(err)
		}
		next(plan)
		return true
	})
	d.win.Present()
}

func resizeDialog(parent *gtk.Window, dk *disk.Disk, p *disk.Partition, limits ops.ResizeLimits, next func(*ops.Plan)) {
	d := newDialog(parent, "Resize "+p.Path, "Continue", false)
	what := "The partition keeps its start; only its end moves."
	switch {
	case limits.CanGrow && limits.CanShrink:
		what += fmt.Sprintf(" It can grow by up to %s or shrink.", disk.HumanSize(limits.Max-min(limits.Max, p.Size)))
	case limits.CanGrow:
		what += fmt.Sprintf(" It can grow by up to %s; %s filesystems cannot be shrunk here.", disk.HumanSize(limits.Max-p.Size), fsName(p))
	default:
		what += " There is no free space after it, so it can only shrink."
	}
	d.text(what)
	hi := max(limits.Max, p.Size/disk.MiB*disk.MiB)
	size := mibSpin(limits.Min, hi, p.Size)
	f := d.form()
	f.add("Current", gtk.NewLabel(disk.HumanSize(p.Size)))
	f.add("New size", sizeHint(size))
	if limits.CanShrink && p.FSType != "" {
		d.text("Shrinking fails safely if the filesystem holds more than the new size: the filesystem is shrunk first and the partition is left alone if that fails.").AddCSSClass("dim-label")
	}
	d.onOK(func() bool {
		plan, err := ops.Resize(dk, p, spinBytes(size))
		if err != nil {
			return d.fail(err)
		}
		next(plan)
		return true
	})
	d.win.Present()
}

func fsName(p *disk.Partition) string {
	if p.FSType == "" {
		return "unformatted"
	}
	return p.FSType
}
