package ops

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"unicode/utf8"
)

// Filesystem is one filesystem the window can create.
type Filesystem struct {
	ID         string // also the lsblk FSTYPE for most
	Name       string
	Tool       string // mkfs binary; the filesystem is offered only when it is installed
	PartedType string // fs-type hint for parted mkpart, which sets the partition type
	MaxLabel   int    // label length limit in bytes
	mkfs       func(dev, label string) []string
}

// Mkfs is the command that formats dev with this filesystem.
func (f *Filesystem) Mkfs(dev, label string) []string { return f.mkfs(dev, label) }

// Available reports whether the filesystem's mkfs tool is installed.
func (f *Filesystem) Available() bool { return toolExists(f.Tool) }

// CheckLabel rejects labels the filesystem cannot store.
func (f *Filesystem) CheckLabel(label string) error {
	if len(label) > f.MaxLabel {
		return fmt.Errorf("%s labels are at most %d bytes", f.Name, f.MaxLabel)
	}
	if !utf8.ValidString(label) {
		return fmt.Errorf("the label is not valid UTF-8")
	}
	return nil
}

func withLabel(argv []string, flag, label string) []string {
	if label != "" {
		argv = append(argv, flag, label)
	}
	return argv
}

// Filesystems is every filesystem the window knows how to create, in menu
// order.
var Filesystems = []*Filesystem{
	{ID: "ext4", Name: "ext4", Tool: "mkfs.ext4", PartedType: "ext4", MaxLabel: 16,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.ext4", "-F"}, "-L", label), dev)
		}},
	{ID: "btrfs", Name: "Btrfs", Tool: "mkfs.btrfs", PartedType: "btrfs", MaxLabel: 255,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.btrfs", "-f"}, "-L", label), dev)
		}},
	{ID: "xfs", Name: "XFS", Tool: "mkfs.xfs", PartedType: "xfs", MaxLabel: 12,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.xfs", "-f"}, "-L", label), dev)
		}},
	{ID: "fat32", Name: "FAT32", Tool: "mkfs.fat", PartedType: "fat32", MaxLabel: 11,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.fat", "-F", "32"}, "-n", label), dev)
		}},
	{ID: "exfat", Name: "exFAT", Tool: "mkfs.exfat", PartedType: "", MaxLabel: 15,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.exfat"}, "-L", label), dev)
		}},
	{ID: "ntfs", Name: "NTFS", Tool: "mkfs.ntfs", PartedType: "ntfs", MaxLabel: 128,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.ntfs", "--fast", "--force"}, "-L", label), dev)
		}},
	{ID: "swap", Name: "Linux swap", Tool: "mkswap", PartedType: "linux-swap", MaxLabel: 15,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkswap"}, "-L", label), dev)
		}},
}

// FilesystemByID looks a filesystem up by ID.
func FilesystemByID(id string) *Filesystem {
	for _, f := range Filesystems {
		if f.ID == id {
			return f
		}
	}
	return nil
}

// AvailableFilesystems are the filesystems whose tools are installed.
func AvailableFilesystems() []*Filesystem {
	var out []*Filesystem
	for _, f := range Filesystems {
		if f.Available() {
			out = append(out, f)
		}
	}
	return out
}

// fsResizer knows how to grow and, if shrink is set, shrink one filesystem
// to fill (or fit) its partition. A resizer with whole set instead drives a
// tool that resizes the filesystem and moves the partition's end in one go.
type fsResizer struct {
	tool   string // must be installed for the resizer to be offered
	grow   func(dev string) []Step
	shrink func(dev string, size uint64) []Step
	whole  func(diskPath string, number int, size uint64) []Step
}

func e2fsck(dev string) Step {
	return Step{Desc: "Check the filesystem", Argv: []string{"e2fsck", "-f", "-y", dev}, OKExit: []int{1}}
}

var extResizer = &fsResizer{
	tool: "resize2fs",
	grow: func(dev string) []Step {
		return []Step{e2fsck(dev), {Desc: "Grow the filesystem to fill the partition", Argv: []string{"resize2fs", dev}}}
	},
	shrink: func(dev string, size uint64) []Step {
		return []Step{e2fsck(dev), {
			Desc: "Shrink the filesystem",
			Argv: []string{"resize2fs", dev, strconv.FormatUint(size/1024, 10) + "K"},
		}}
	},
}

var resizers = map[string]*fsResizer{
	"ext2": extResizer,
	"ext3": extResizer,
	"ext4": extResizer,
	"ntfs": {
		tool: "ntfsresize",
		grow: func(dev string) []Step {
			return []Step{{Desc: "Grow the filesystem to fill the partition", Argv: []string{"ntfsresize", "--force", dev}, Stdin: "y\n"}}
		},
		shrink: func(dev string, size uint64) []Step {
			return []Step{{
				Desc:  "Shrink the filesystem",
				Argv:  []string{"ntfsresize", "--force", "--size", strconv.FormatUint(size, 10), dev},
				Stdin: "y\n",
			}}
		},
	},
	// fatresize (libparted) resizes the FAT and then sets the partition's new
	// end itself, committing the table only after the filesystem resize
	// succeeded. Given the whole disk and -n it edits that table entry; a
	// plain number is a size in bytes; -f answers libparted's questions.
	"vfat": {
		tool: "fatresize",
		whole: func(diskPath string, number int, size uint64) []Step {
			return []Step{{
				Desc: "Resize the FAT filesystem and its partition",
				Argv: []string{"fatresize", "--force-yes", "--partition", strconv.Itoa(number), "--size", strconv.FormatUint(size, 10), diskPath},
			}}
		},
	},
	// No filesystem: only the table entry changes.
	"": {
		grow:   func(string) []Step { return nil },
		shrink: func(string, uint64) []Step { return nil },
	},
}

// resizer returns how to resize fsType, or why it cannot be resized offline
// with the tools this package drives.
func resizer(fsType string) (*fsResizer, error) {
	r := resizers[fsType]
	if r == nil {
		switch fsType {
		case "btrfs", "xfs":
			return nil, fmt.Errorf("%s can only be resized while mounted, which this app does not do", fsType)
		case "swap":
			return nil, errors.New("swap cannot be resized; delete it and create a new one")
		}
		return nil, fmt.Errorf("%s filesystems cannot be resized here", fsType)
	}
	if r.tool != "" && !toolExists(r.tool) {
		return nil, fmt.Errorf("resizing %s needs %s, which is not installed (rvn install %s)", fsType, r.tool, toolPackage[r.tool])
	}
	return r, nil
}

// toolPackage is the package that provides each resize tool.
var toolPackage = map[string]string{
	"resize2fs":  "e2fsprogs",
	"ntfsresize": "ntfs-3g",
	"fatresize":  "fatresize",
}

// toolExists reports whether lookTool finds the tool.
func toolExists(name string) bool {
	_, err := lookTool(name)
	return err == nil
}

// lookTool looks for a tool on PATH and then in the sbin directories, which
// an ordinary user's PATH may leave out but root's includes.
func lookTool(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/local/sbin"} {
		if path, err := exec.LookPath(dir + "/" + name); err == nil {
			return path, nil
		}
	}
	return "", exec.ErrNotFound
}
