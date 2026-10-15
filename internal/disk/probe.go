package disk

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// lsblkColumns are the columns Probe asks for. START is always in 512-byte
// units, whatever the disk's logical sector size.
const lsblkColumns = "NAME,PATH,TYPE,SIZE,FSTYPE,LABEL,UUID,PARTLABEL,PARTTYPE,PARTTYPENAME," +
	"MOUNTPOINTS,MODEL,SERIAL,TRAN,RM,RO,PTTYPE,LOG-SEC,START,PARTN"

type lsblkOutput struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name         string        `json:"name"`
	Path         string        `json:"path"`
	Type         string        `json:"type"`
	Size         uint64        `json:"size"`
	FSType       string        `json:"fstype"`
	Label        string        `json:"label"`
	UUID         string        `json:"uuid"`
	PartLabel    string        `json:"partlabel"`
	PartType     string        `json:"parttype"`
	PartTypeName string        `json:"parttypename"`
	Mountpoints  []*string     `json:"mountpoints"`
	Model        string        `json:"model"`
	Serial       string        `json:"serial"`
	Tran         string        `json:"tran"`
	RM           bool          `json:"rm"`
	RO           bool          `json:"ro"`
	PTType       string        `json:"pttype"`
	LogSec       uint64        `json:"log-sec"`
	Start        uint64        `json:"start"`
	PartN        int           `json:"partn"`
	Children     []lsblkDevice `json:"children"`
}

// Probe runs lsblk and returns every disk on the machine, in the order lsblk
// lists them. Zero-sized devices (empty card readers, unused loop devices)
// are left out.
func Probe() ([]Disk, error) {
	out, err := exec.Command("lsblk", "--json", "--bytes", "--tree", "-o", lsblkColumns).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("lsblk: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	return Parse(out)
}

// Parse turns `lsblk --json --bytes --tree -o <lsblkColumns>` output into
// disks.
func Parse(data []byte) ([]Disk, error) {
	var out lsblkOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parsing lsblk output: %w", err)
	}
	var disks []Disk
	for _, dev := range out.BlockDevices {
		if !isDisk(dev.Type) || dev.Size == 0 {
			continue
		}
		disks = append(disks, toDisk(dev))
	}
	return disks, nil
}

// isDisk reports whether an lsblk TYPE is a whole device that can carry a
// partition table.
func isDisk(t string) bool {
	switch t {
	case "disk", "loop", "mmc":
		return true
	}
	return false
}

func toDisk(dev lsblkDevice) Disk {
	d := Disk{
		Name:        dev.Name,
		Path:        dev.Path,
		Model:       strings.TrimSpace(dev.Model),
		Serial:      strings.TrimSpace(dev.Serial),
		Transport:   dev.Tran,
		Size:        dev.Size,
		SectorSize:  dev.LogSec,
		Table:       dev.PTType,
		Removable:   dev.RM,
		ReadOnly:    dev.RO,
		FSType:      dev.FSType,
		Label:       dev.Label,
		Mountpoints: mountpoints(dev.Mountpoints),
	}
	if d.SectorSize == 0 {
		d.SectorSize = 512
	}
	for _, c := range dev.Children {
		if c.Type == "part" {
			d.Partitions = append(d.Partitions, toPartition(c))
		} else {
			d.Holders = append(d.Holders, c.Name)
		}
	}
	sort.Slice(d.Partitions, func(i, j int) bool { return d.Partitions[i].Number < d.Partitions[j].Number })
	return d
}

func toPartition(dev lsblkDevice) Partition {
	p := Partition{
		Name:        dev.Name,
		Path:        dev.Path,
		Number:      dev.PartN,
		Start:       dev.Start * 512,
		Size:        dev.Size,
		FSType:      dev.FSType,
		Label:       dev.Label,
		UUID:        dev.UUID,
		PartLabel:   dev.PartLabel,
		TypeCode:    dev.PartType,
		TypeName:    dev.PartTypeName,
		Mountpoints: mountpoints(dev.Mountpoints),
		ReadOnly:    dev.RO,
	}
	for _, c := range dev.Children {
		p.Holders = append(p.Holders, c.Name)
	}
	return p
}

func mountpoints(in []*string) []string {
	var out []string
	for _, m := range in {
		if m != nil && *m != "" {
			out = append(out, *m)
		}
	}
	return out
}
