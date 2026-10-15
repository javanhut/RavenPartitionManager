// Package disk reads the machine's block devices from lsblk and turns each
// disk into the ordered list of partitions and free regions the window shows.
package disk

import (
	"fmt"
	"sort"
	"strings"
)

// MiB is the alignment unit for every region and offset this package hands
// out, which is what parted's "optimal" alignment settles on for nearly every
// disk.
const MiB = 1 << 20

// Disk is one whole block device and what lsblk says lives on it.
type Disk struct {
	Name       string
	Path       string
	Model      string
	Serial     string
	Transport  string
	Size       uint64 // bytes
	SectorSize uint64 // logical sector size in bytes
	Table      string // "gpt", "dos", or "" when there is no partition table
	Removable  bool
	ReadOnly   bool

	// A filesystem written straight onto the disk, with no partition table.
	FSType      string
	Label       string
	Mountpoints []string
	Holders     []string // device-mapper, LVM or RAID members built on the disk itself

	Partitions []Partition
}

// Partition is one entry in a disk's partition table.
type Partition struct {
	Name        string
	Path        string
	Number      int
	Start       uint64 // bytes from the start of the disk
	Size        uint64 // bytes
	FSType      string
	Label       string
	UUID        string
	PartLabel   string
	TypeCode    string // GPT type GUID or MBR type byte ("0x83")
	TypeName    string
	Mountpoints []string
	Holders     []string
	ReadOnly    bool
}

// End is the first byte past the partition.
func (p *Partition) End() uint64 { return p.Start + p.Size }

// Busy reports whether the kernel is using the partition: mounted, active
// swap, or the backing device of something else (LUKS, LVM, RAID).
func (p *Partition) Busy() bool { return len(p.Mountpoints) > 0 || len(p.Holders) > 0 }

// Extended reports whether this is an MBR extended partition, the container
// that logical partitions live in.
func (p *Partition) Extended() bool {
	switch strings.ToLower(p.TypeCode) {
	case "0x5", "0x05", "0xf", "0x0f", "0x85":
		return true
	}
	return false
}

// DisplayName is what the window calls the disk: its model and size.
func (d *Disk) DisplayName() string {
	model := d.Model
	if model == "" {
		model = "Disk"
	}
	return fmt.Sprintf("%s — %s (%s)", d.Path, model, HumanSize(d.Size))
}

// Busy reports whether anything on the disk, the disk itself or any of its
// partitions, is in use.
func (d *Disk) Busy() bool {
	if len(d.Mountpoints) > 0 || len(d.Holders) > 0 {
		return true
	}
	for i := range d.Partitions {
		if d.Partitions[i].Busy() {
			return true
		}
	}
	return false
}

// Partition returns the partition with the given number, or nil.
func (d *Disk) Partition(number int) *Partition {
	for i := range d.Partitions {
		if d.Partitions[i].Number == number {
			return &d.Partitions[i]
		}
	}
	return nil
}

// PrimaryCount is the number of MBR primary slots taken, extended included.
func (d *Disk) PrimaryCount() int {
	n := 0
	for _, p := range d.Partitions {
		if p.Number >= 1 && p.Number <= 4 {
			n++
		}
	}
	return n
}

// CanAddPartition reports whether the table has room for another partition,
// and why not when it does not. Only primary MBR partitions are created, so a
// DOS table is full at four.
func (d *Disk) CanAddPartition() (bool, string) {
	switch d.Table {
	case "gpt":
		if len(d.Partitions) >= 128 {
			return false, "the GPT already holds 128 partitions"
		}
		return true, ""
	case "dos":
		if d.PrimaryCount() >= 4 {
			return false, "an MBR table holds at most four primary partitions"
		}
		return true, ""
	case "":
		return false, "the disk has no partition table"
	}
	return false, fmt.Sprintf("%q partition tables are not supported", d.Table)
}

// NextPartitionNumber is the number parted gives the next partition it
// creates: the lowest unused one (only 1-4 on MBR, since only primaries are
// created here).
func (d *Disk) NextPartitionNumber() int {
	used := map[int]bool{}
	for _, p := range d.Partitions {
		used[p.Number] = true
	}
	for n := 1; ; n++ {
		if !used[n] {
			return n
		}
	}
}

// PartitionPath is the device node the kernel gives partition number n of
// this disk: a "p" separates the number when the disk name already ends in a
// digit (nvme0n1p1, mmcblk0p1, loop0p1), and is left out otherwise (sda1).
func (d *Disk) PartitionPath(n int) string {
	if d.Path == "" {
		return ""
	}
	last := d.Path[len(d.Path)-1]
	if last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", d.Path, n)
	}
	return fmt.Sprintf("%s%d", d.Path, n)
}

// RegionKind tells a partition from unallocated space.
type RegionKind int

const (
	RegionPartition RegionKind = iota
	RegionFree
)

// Region is one stretch of the disk as the window lays it out, in order.
type Region struct {
	Kind  RegionKind
	Start uint64
	Size  uint64
	Part  *Partition // set for RegionPartition
}

// End is the first byte past the region.
func (r Region) End() uint64 { return r.Start + r.Size }

// usableRange is where partitions may go: from the first MiB (which holds the
// MBR or the primary GPT and keeps the first partition aligned) to the end of
// the disk, less a MiB on GPT for the backup header and entries.
func (d *Disk) usableRange() (first, last uint64) {
	first = MiB
	last = alignDown(d.Size, MiB)
	if d.Table == "gpt" && last >= MiB {
		last -= MiB
	}
	if last < first {
		last = first
	}
	return first, last
}

// Regions lists the disk's partitions in on-disk order with the unallocated
// gaps between them. Gaps are aligned inward to whole MiB and gaps smaller
// than a MiB are dropped, so every free region can be used as is.
//
// Logical partitions are shown on their own and the extended partition that
// holds them is folded into one occupied span (lsblk reports an extended
// partition as a couple of sectors), so free space is never offered inside it.
// A disk with no partition table has no regions.
func (d *Disk) Regions() []Region {
	if d.Table == "" {
		return nil
	}
	var parts []*Partition
	var occupied [][2]uint64
	var ext *Partition
	extEnd := uint64(0)
	for i := range d.Partitions {
		p := &d.Partitions[i]
		parts = append(parts, p)
		if p.Extended() {
			ext = p
		}
	}
	for _, p := range parts {
		end := p.End()
		if ext != nil && p.Number > 4 && end > extEnd {
			extEnd = end
		}
		occupied = append(occupied, [2]uint64{p.Start, end})
	}
	if ext != nil && extEnd > ext.End() {
		occupied = append(occupied, [2]uint64{ext.Start, extEnd})
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].Start != parts[j].Start {
			return parts[i].Start < parts[j].Start
		}
		return parts[i].Number < parts[j].Number
	})
	sort.Slice(occupied, func(i, j int) bool { return occupied[i][0] < occupied[j][0] })

	first, last := d.usableRange()
	var free []Region
	cursor := first
	for _, o := range occupied {
		if o[0] > cursor {
			free = appendFree(free, cursor, min(o[0], last))
		}
		if o[1] > cursor {
			cursor = o[1]
		}
	}
	if last > cursor {
		free = appendFree(free, cursor, last)
	}

	regions := make([]Region, 0, len(parts)+len(free))
	for _, p := range parts {
		regions = append(regions, Region{Kind: RegionPartition, Start: p.Start, Size: p.Size, Part: p})
	}
	regions = append(regions, free...)
	sort.SliceStable(regions, func(i, j int) bool { return regions[i].Start < regions[j].Start })
	return regions
}

func appendFree(free []Region, start, end uint64) []Region {
	start = alignUp(start, MiB)
	end = alignDown(end, MiB)
	if end <= start || end-start < MiB {
		return free
	}
	return append(free, Region{Kind: RegionFree, Start: start, Size: end - start})
}

// FreeAfter is the unallocated space directly after the partition, which is
// how far it can grow without moving anything.
func (d *Disk) FreeAfter(p *Partition) uint64 {
	end := p.End()
	for _, r := range d.Regions() {
		if r.Kind == RegionFree && r.Start >= end && r.Start-end < MiB {
			return r.End() - end
		}
	}
	return 0
}

func alignUp(v, a uint64) uint64   { return (v + a - 1) / a * a }
func alignDown(v, a uint64) uint64 { return v / a * a }

// HumanSize renders a byte count in binary units, the way partition tools do.
func HumanSize(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
