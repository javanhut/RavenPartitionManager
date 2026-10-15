// Package ops turns each change the window offers (new partition table, new
// partition, delete, format, resize) into a Plan: the exact commands that make
// it, in order. Building a plan touches nothing; the runner decides whether to
// show it (dry run) or execute it.
package ops

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"raven-partition-manager/internal/disk"
)

// Step is one command of a plan.
type Step struct {
	Desc  string
	Argv  []string
	Stdin string // fed to the command when not empty
	// OKExit lists exit codes besides 0 that still count as success
	// (e2fsck exits 1 after correcting errors, which is what -y asks for).
	OKExit []int
}

// Plan is everything one operation will run, with what it will do in words.
type Plan struct {
	Title       string
	Summary     string
	Destructive bool // erases data, as opposed to only rewriting table entries
	Steps       []Step
}

// TableType is a partition table layout parted can write.
type TableType string

const (
	TableGPT   TableType = "gpt"
	TableMSDOS TableType = "msdos"
)

// NewTable wipes every signature on the disk and writes an empty partition
// table.
func NewTable(d *disk.Disk, t TableType) (*Plan, error) {
	if err := writable(d); err != nil {
		return nil, err
	}
	if d.Busy() {
		return nil, fmt.Errorf("%s is in use; unmount everything on it first", d.Path)
	}
	if t != TableGPT && t != TableMSDOS {
		return nil, fmt.Errorf("unknown partition table type %q", t)
	}
	return &Plan{
		Title:       fmt.Sprintf("New %s partition table on %s", tableName(t), d.Path),
		Summary:     fmt.Sprintf("Every partition and filesystem on %s (%s) will be erased.", d.Path, disk.HumanSize(d.Size)),
		Destructive: true,
		Steps: []Step{
			{Desc: "Erase filesystem and partition table signatures", Argv: []string{"wipefs", "--all", d.Path}},
			{Desc: "Write an empty " + tableName(t) + " partition table", Argv: []string{"parted", "--script", d.Path, "mklabel", string(t)}},
			settle(),
		},
	}, nil
}

// CreateRequest describes a new partition.
type CreateRequest struct {
	Start uint64 // bytes, MiB aligned
	Size  uint64 // bytes, MiB aligned
	FS    string // filesystem id from Filesystems, or "" to leave it unformatted
	Label string // filesystem label, and the GPT partition name
	ESP   bool   // mark as EFI system partition (boot flag on MBR)
}

// Create adds a partition in free space and, if asked, formats it. parted
// gives the new partition the lowest free number; before anything is
// formatted the plan checks that the device with that number starts where the
// partition was asked to, so a misprediction stops the plan instead of
// formatting the wrong partition.
func Create(d *disk.Disk, req CreateRequest) (*Plan, error) {
	if err := writable(d); err != nil {
		return nil, err
	}
	if ok, why := d.CanAddPartition(); !ok {
		return nil, errors.New(why)
	}
	if req.Size < disk.MiB {
		return nil, errors.New("a partition must be at least 1 MiB")
	}
	if req.Start%disk.MiB != 0 || req.Size%disk.MiB != 0 {
		return nil, errors.New("start and size must be whole MiB")
	}
	if !fitsInFree(d, req.Start, req.Size) {
		return nil, errors.New("the partition does not fit in unallocated space")
	}
	var fs *Filesystem
	if req.FS != "" {
		if fs = FilesystemByID(req.FS); fs == nil {
			return nil, fmt.Errorf("unknown filesystem %q", req.FS)
		}
		if !fs.Available() {
			return nil, fmt.Errorf("%s is not installed", fs.Tool)
		}
		if err := fs.CheckLabel(req.Label); err != nil {
			return nil, err
		}
	}
	if req.ESP && (fs == nil || fs.ID != "fat32") {
		return nil, errors.New("an EFI system partition must be FAT32")
	}

	n := d.NextPartitionNumber()
	path := d.PartitionPath(n)
	end := req.Start + req.Size - 1 // parted's end is inclusive

	mkpart := []string{"parted", "--script", "--align", "optimal", d.Path, "unit", "B", "mkpart"}
	if d.Table == "gpt" {
		name := req.Label
		if name == "" {
			name = "primary"
		}
		mkpart = append(mkpart, name)
	} else {
		mkpart = append(mkpart, "primary")
	}
	if fs != nil && fs.PartedType != "" {
		mkpart = append(mkpart, fs.PartedType)
	}
	mkpart = append(mkpart, fmt.Sprintf("%dB", req.Start), fmt.Sprintf("%dB", end))

	desc := fmt.Sprintf("Create partition %d (%s)", n, disk.HumanSize(req.Size))
	steps := []Step{{Desc: desc, Argv: mkpart}}
	if req.ESP {
		flag := "esp"
		if d.Table != "gpt" {
			flag = "boot"
		}
		steps = append(steps, Step{
			Desc: "Mark it as the EFI system partition",
			Argv: []string{"parted", "--script", d.Path, "set", strconv.Itoa(n), flag, "on"},
		})
	}
	steps = append(steps, settle())
	if fs != nil {
		steps = append(steps,
			Step{
				Desc: fmt.Sprintf("Check that %s is the new partition", path),
				Argv: []string{"grep", "-qx", strconv.FormatUint(req.Start/512, 10), sysfsStart(path)},
			},
			Step{Desc: "Format as " + fs.Name, Argv: fs.Mkfs(path, req.Label)},
		)
	}

	summary := fmt.Sprintf("A %s partition will be created on %s as %s.", disk.HumanSize(req.Size), d.Path, path)
	if fs != nil {
		summary = fmt.Sprintf("A %s %s partition will be created on %s as %s.", disk.HumanSize(req.Size), fs.Name, d.Path, path)
	}
	return &Plan{
		Title:   fmt.Sprintf("New partition on %s", d.Path),
		Summary: summary,
		Steps:   steps,
	}, nil
}

// Delete removes a partition from the table.
func Delete(d *disk.Disk, p *disk.Partition) (*Plan, error) {
	if err := writable(d); err != nil {
		return nil, err
	}
	if p.Busy() {
		return nil, fmt.Errorf("%s is in use; unmount it first", p.Path)
	}
	if p.Extended() {
		for _, q := range d.Partitions {
			if q.Number > 4 {
				return nil, fmt.Errorf("%s still holds logical partitions; delete them first", p.Path)
			}
		}
	}
	return &Plan{
		Title:       fmt.Sprintf("Delete %s", p.Path),
		Summary:     fmt.Sprintf("%s (%s%s) will be removed and everything on it lost.", p.Path, disk.HumanSize(p.Size), fsSuffix(p)),
		Destructive: true,
		Steps: []Step{
			{Desc: fmt.Sprintf("Remove partition %d", p.Number), Argv: []string{"parted", "--script", d.Path, "rm", strconv.Itoa(p.Number)}},
			settle(),
		},
	}, nil
}

// Unmount detaches a partition from everywhere it is mounted, or turns it
// off as swap, so that it can be changed. Nothing on it is touched.
func Unmount(p *disk.Partition) (*Plan, error) {
	if len(p.Mountpoints) == 0 {
		if len(p.Holders) > 0 {
			return nil, fmt.Errorf("%s is used by %s, not mounted; close or deactivate that first", p.Path, strings.Join(p.Holders, ", "))
		}
		return nil, fmt.Errorf("%s is not mounted", p.Path)
	}
	var steps []Step
	swap, mounted := false, false
	for _, m := range p.Mountpoints {
		if m == "[SWAP]" {
			swap = true
		} else {
			mounted = true
		}
	}
	if swap {
		steps = append(steps, Step{Desc: "Stop using it as swap", Argv: []string{"swapoff", p.Path}})
	}
	if mounted {
		steps = append(steps, Step{Desc: "Unmount it everywhere it is mounted", Argv: []string{"umount", "--all-targets", p.Path}})
	}
	return &Plan{
		Title:   fmt.Sprintf("Unmount %s", p.Path),
		Summary: fmt.Sprintf("%s will be unmounted from %s. Nothing on it is changed; files open from it must be closed first.", p.Path, strings.Join(p.Mountpoints, ", ")),
		Steps:   steps,
	}, nil
}

// Format writes a new filesystem over a partition.
func Format(d *disk.Disk, p *disk.Partition, fsID, label string) (*Plan, error) {
	if err := writable(d); err != nil {
		return nil, err
	}
	if p.Busy() {
		return nil, fmt.Errorf("%s is in use; unmount it first", p.Path)
	}
	if p.Extended() {
		return nil, errors.New("an extended partition cannot hold a filesystem")
	}
	fs := FilesystemByID(fsID)
	if fs == nil {
		return nil, fmt.Errorf("unknown filesystem %q", fsID)
	}
	if !fs.Available() {
		return nil, fmt.Errorf("%s is not installed", fs.Tool)
	}
	if err := fs.CheckLabel(label); err != nil {
		return nil, err
	}
	return &Plan{
		Title:       fmt.Sprintf("Format %s as %s", p.Path, fs.Name),
		Summary:     fmt.Sprintf("Everything on %s (%s%s) will be erased.", p.Path, disk.HumanSize(p.Size), fsSuffix(p)),
		Destructive: true,
		Steps: []Step{
			{Desc: "Erase old filesystem signatures", Argv: []string{"wipefs", "--all", p.Path}},
			{Desc: "Format as " + fs.Name, Argv: fs.Mkfs(p.Path, label)},
		},
	}, nil
}

// ResizeLimits is how far a partition can be resized in place.
type ResizeLimits struct {
	Min, Max  uint64 // bytes
	CanGrow   bool
	CanShrink bool
}

// Limits works out how a partition may be resized: its start stays put, so
// it can grow into the free space right after it, and shrink only when its
// filesystem can be shrunk first. Unformatted partitions can do both.
func Limits(d *disk.Disk, p *disk.Partition) (ResizeLimits, error) {
	if err := writable(d); err != nil {
		return ResizeLimits{}, err
	}
	if p.Busy() {
		return ResizeLimits{}, fmt.Errorf("%s is in use; unmount it first", p.Path)
	}
	if p.Extended() {
		return ResizeLimits{}, errors.New("extended partitions cannot be resized here")
	}
	fs, err := resizer(p.FSType)
	if err != nil {
		return ResizeLimits{}, err
	}
	max := (p.Size + d.FreeAfter(p)) / disk.MiB * disk.MiB
	l := ResizeLimits{Min: p.Size, Max: max, CanGrow: max > p.Size}
	if fs.shrink != nil || fs.whole != nil {
		l.CanShrink = true
		l.Min = disk.MiB
	}
	if !l.CanGrow && !l.CanShrink {
		return l, errors.New("there is no free space after the partition and its filesystem cannot shrink")
	}
	return l, nil
}

// Resize moves the end of a partition, keeping its start. When shrinking, the
// filesystem is shrunk before the partition; when growing, the partition
// grows first and the filesystem is grown to fill it. The steps run in order
// and stop at the first failure, so a filesystem that refuses to shrink that
// far leaves the partition untouched.
func Resize(d *disk.Disk, p *disk.Partition, newSize uint64) (*Plan, error) {
	l, err := Limits(d, p)
	if err != nil {
		return nil, err
	}
	if newSize%disk.MiB != 0 {
		return nil, errors.New("the new size must be whole MiB")
	}
	// The current size may not be MiB aligned; any other size must be.
	if newSize == p.Size || (newSize/disk.MiB == p.Size/disk.MiB && p.Size%disk.MiB != 0) {
		return nil, errors.New("the size is unchanged")
	}
	if newSize > l.Max {
		return nil, fmt.Errorf("at most %s fits", disk.HumanSize(l.Max))
	}
	shrinking := newSize < p.Size
	if shrinking && !l.CanShrink {
		return nil, fmt.Errorf("%s filesystems cannot be shrunk here", p.FSType)
	}
	if newSize < l.Min {
		return nil, fmt.Errorf("at least %s is needed", disk.HumanSize(l.Min))
	}

	fs, err := resizer(p.FSType)
	if err != nil {
		return nil, err
	}
	sectors := newSize / d.SectorSize
	table := Step{
		Desc:  fmt.Sprintf("Set partition %d to %s", p.Number, disk.HumanSize(newSize)),
		Argv:  []string{"sfdisk", "--no-reread", "-N", strconv.Itoa(p.Number), d.Path},
		Stdin: fmt.Sprintf(",%d\n", sectors),
	}
	tell := Step{Desc: "Tell the kernel about the new size", Argv: []string{"partx", "--update", "--nr", strconv.Itoa(p.Number), d.Path}}

	var steps []Step
	switch {
	case fs.whole != nil:
		steps = append(steps, fs.whole(d.Path, p.Number, newSize)...)
		steps = append(steps, tell, settle())
	case shrinking:
		steps = append(steps, fs.shrink(p.Path, newSize)...)
		steps = append(steps, table, tell, settle())
	default:
		steps = append(steps, table, tell, settle())
		steps = append(steps, fs.grow(p.Path)...)
	}
	verb := "grow"
	if shrinking {
		verb = "shrink"
	}
	return &Plan{
		Title:   fmt.Sprintf("Resize %s", p.Path),
		Summary: fmt.Sprintf("%s will %s from %s to %s%s.", p.Path, verb, disk.HumanSize(p.Size), disk.HumanSize(newSize), fsSuffix(p)),
		// A failed or interrupted shrink can leave the filesystem damaged, and
		// so can an interrupted FAT resize, which rewrites the FAT either way.
		Destructive: shrinking || fs.whole != nil,
		Steps:       steps,
	}, nil
}

func writable(d *disk.Disk) error {
	if d.ReadOnly {
		return fmt.Errorf("%s is read-only", d.Path)
	}
	return nil
}

func fitsInFree(d *disk.Disk, start, size uint64) bool {
	for _, r := range d.Regions() {
		if r.Kind == disk.RegionFree && start >= r.Start && start+size <= r.End() {
			return true
		}
	}
	return false
}

func settle() Step {
	return Step{Desc: "Wait for the device nodes to update", Argv: []string{"udevadm", "settle"}}
}

func sysfsStart(devPath string) string {
	return "/sys/class/block/" + strings.TrimPrefix(devPath, "/dev/") + "/start"
}

func tableName(t TableType) string {
	if t == TableMSDOS {
		return "MBR"
	}
	return "GPT"
}

func fsSuffix(p *disk.Partition) string {
	if p.FSType == "" {
		return ""
	}
	return ", " + p.FSType
}
