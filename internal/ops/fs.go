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
	ID       string // also the lsblk FSTYPE for most
	Name     string
	Tool     string // mkfs binary; the filesystem is offered only when it is installed
	MBRType  string // partition type byte for sfdisk on an MBR table
	GPTType  string // partition type GUID for sfdisk on a GPT
	MaxLabel int    // label length limit in bytes
	mkfs     func(dev, label string) []string
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

// Partition type GUIDs for GPT.
const (
	gptLinux     = "0FC63DAF-8483-4772-8E79-3D69D8477DE4" // Linux filesystem
	gptSwap      = "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F" // Linux swap
	gptBasicData = "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7" // Microsoft basic data: FAT, exFAT, NTFS
	gptESP       = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B" // EFI system partition
)

// partType is the type sfdisk gives a new partition: the filesystem's, a
// Linux filesystem when it is left unformatted, or the EFI system partition
// type (0xef on MBR, which UEFI firmware looks for there).
func partType(table string, fs *Filesystem, esp bool) string {
	gpt := table == "gpt"
	switch {
	case esp && gpt:
		return gptESP
	case esp:
		return "ef"
	case fs == nil && gpt:
		return gptLinux
	case fs == nil:
		return "83"
	case gpt:
		return fs.GPTType
	}
	return fs.MBRType
}

// Filesystems is every filesystem the window knows how to create, in menu
// order.
var Filesystems = []*Filesystem{
	{ID: "ext4", Name: "ext4", Tool: "mkfs.ext4", MBRType: "83", GPTType: gptLinux, MaxLabel: 16,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.ext4", "-F"}, "-L", label), dev)
		}},
	{ID: "btrfs", Name: "Btrfs", Tool: "mkfs.btrfs", MBRType: "83", GPTType: gptLinux, MaxLabel: 255,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.btrfs", "-f"}, "-L", label), dev)
		}},
	{ID: "xfs", Name: "XFS", Tool: "mkfs.xfs", MBRType: "83", GPTType: gptLinux, MaxLabel: 12,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.xfs", "-f"}, "-L", label), dev)
		}},
	{ID: "fat32", Name: "FAT32", Tool: "mkfs.fat", MBRType: "c", GPTType: gptBasicData, MaxLabel: 11,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.fat", "-F", "32"}, "-n", label), dev)
		}},
	{ID: "exfat", Name: "exFAT", Tool: "mkfs.exfat", MBRType: "7", GPTType: gptBasicData, MaxLabel: 15,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.exfat"}, "-L", label), dev)
		}},
	{ID: "ntfs", Name: "NTFS", Tool: "mkfs.ntfs", MBRType: "7", GPTType: gptBasicData, MaxLabel: 128,
		mkfs: func(dev, label string) []string {
			return append(withLabel([]string{"mkfs.ntfs", "--fast", "--force"}, "-L", label), dev)
		}},
	{ID: "swap", Name: "Linux swap", Tool: "mkswap", MBRType: "82", GPTType: gptSwap, MaxLabel: 15,
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
	// fatresize (built on libparted, not the parted program) resizes the FAT
	// and then sets the partition's new end itself, committing the table only after the filesystem resize
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
