package ops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"raven-partition-manager/internal/disk"
)

func testDisk() *disk.Disk {
	return &disk.Disk{
		Path: "/dev/sdz", Size: 1024 * disk.MiB, SectorSize: 512, Table: "gpt",
		Partitions: []disk.Partition{
			{Name: "sdz1", Path: "/dev/sdz1", Number: 1, Start: disk.MiB, Size: 100 * disk.MiB, FSType: "ext4"},
			{Name: "sdz2", Path: "/dev/sdz2", Number: 2, Start: 101 * disk.MiB, Size: 100 * disk.MiB, FSType: "btrfs", Mountpoints: []string{"/data"}},
			{Name: "sdz3", Path: "/dev/sdz3", Number: 3, Start: 301 * disk.MiB, Size: 100 * disk.MiB, FSType: "ext4"},
		},
	}
}

func argvs(p *Plan) []string {
	var out []string
	for _, s := range p.Steps {
		out = append(out, strings.Join(s.Argv, " "))
	}
	return out
}

func TestNewTableRefusesBusyDisk(t *testing.T) {
	if _, err := NewTable(testDisk(), TableGPT); err == nil {
		t.Fatal("expected an error for a disk with a mounted partition")
	}
	d := testDisk()
	d.Partitions[1].Mountpoints = nil
	p, err := NewTable(d, TableMSDOS)
	if err != nil {
		t.Fatal(err)
	}
	got := argvs(p)
	if got[0] != "wipefs --all /dev/sdz" || got[1] != "parted --script /dev/sdz mklabel msdos" {
		t.Errorf("steps = %q", got)
	}
	if !p.Destructive {
		t.Error("a new table erases the disk")
	}
}

func TestCreate(t *testing.T) {
	if !FilesystemByID("ext4").Available() {
		t.Skip("mkfs.ext4 not installed")
	}
	d := testDisk()
	// Free space after sdz2 is [201, 301) MiB; sdz4 is the next number.
	p, err := Create(d, CreateRequest{Start: 201 * disk.MiB, Size: 50 * disk.MiB, FS: "ext4", Label: "home"})
	if err != nil {
		t.Fatal(err)
	}
	got := argvs(p)
	want := []string{
		"parted --script --align optimal /dev/sdz unit B mkpart home ext4 210763776B 263192575B",
		"udevadm settle",
		"grep -qx 411648 /sys/class/block/sdz4/start",
		"mkfs.ext4 -F -L home /dev/sdz4",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("steps:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if _, err := Create(d, CreateRequest{Start: 250 * disk.MiB, Size: 100 * disk.MiB}); err == nil {
		t.Error("expected an error for a partition overlapping sdz3")
	}
	if _, err := Create(d, CreateRequest{Start: 201 * disk.MiB, Size: 10 * disk.MiB, FS: "ext4", Label: strings.Repeat("x", 17)}); err == nil {
		t.Error("expected an error for a 17-byte ext4 label")
	}
}

func TestDeleteAndFormatRefuseMounted(t *testing.T) {
	d := testDisk()
	if _, err := Delete(d, &d.Partitions[1]); err == nil {
		t.Error("deleting a mounted partition should fail")
	}
	if _, err := Format(d, &d.Partitions[1], "ext4", ""); err == nil {
		t.Error("formatting a mounted partition should fail")
	}
	p, err := Delete(d, &d.Partitions[0])
	if err != nil {
		t.Fatal(err)
	}
	if argvs(p)[0] != "parted --script /dev/sdz rm 1" {
		t.Errorf("steps = %q", argvs(p))
	}
}

func TestResize(t *testing.T) {
	if _, err := resizer("ext4"); err != nil {
		t.Skip("resize2fs not installed")
	}
	d := testDisk()
	p1 := &d.Partitions[0]
	l, err := Limits(d, p1)
	if err != nil {
		t.Fatal(err)
	}
	// sdz1 ends at 101 MiB, right where mounted sdz2 starts: no room to grow.
	if l.CanGrow || !l.CanShrink {
		t.Errorf("limits = %+v", l)
	}

	// sdz3 can grow into [401, 1023) MiB.
	p3 := &d.Partitions[2]
	grow, err := Resize(d, p3, 200*disk.MiB)
	if err != nil {
		t.Fatal(err)
	}
	gs := argvs(grow)
	if gs[0] != "sfdisk --no-reread -N 3 /dev/sdz" || grow.Steps[0].Stdin != ",409600\n" {
		t.Errorf("grow should change the table first: %q stdin %q", gs, grow.Steps[0].Stdin)
	}
	if gs[len(gs)-1] != "resize2fs /dev/sdz3" {
		t.Errorf("grow should end by growing the filesystem: %q", gs)
	}

	shrink, err := Resize(d, p3, 60*disk.MiB)
	if err != nil {
		t.Fatal(err)
	}
	ss := argvs(shrink)
	if ss[0] != "e2fsck -f -y /dev/sdz3" || ss[1] != "resize2fs /dev/sdz3 61440K" || !strings.HasPrefix(ss[2], "sfdisk") {
		t.Errorf("shrink should resize the filesystem before the table: %q", ss)
	}
	if _, err := Resize(d, p3, 2000*disk.MiB); err == nil {
		t.Error("growing past the free space should fail")
	}

	d.Partitions[2].FSType = "btrfs"
	if _, err := Limits(d, p3); err == nil {
		t.Error("btrfs should not be resizable offline")
	}
}

func TestScriptRunsAndStops(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	p := &Plan{Steps: []Step{
		{Desc: "first", Argv: []string{"echo", "it's ok"}},
		{Desc: "tolerated", Argv: []string{"sh", "-c", "exit 1"}, OKExit: []int{1}},
		{Desc: "stdin", Argv: []string{"cat"}, Stdin: ",2048\n"},
		{Desc: "fails", Argv: []string{"false"}},
		{Desc: "never", Argv: []string{"echo", "unreachable"}},
	}}
	out, err := exec.Command("sh", "-c", p.Script()).CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("want exit 1, got %v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{"it's ok", ">> tolerated", ",2048", ">> Stopped"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "unreachable") || strings.Contains(s, ">> Done") {
		t.Errorf("script kept going after a failure:\n%s", s)
	}
}

func TestDryRunRunsNothing(t *testing.T) {
	p := &Plan{Title: "t", Steps: []Step{{Desc: "x", Argv: []string{"touch", t.TempDir() + "/should-not-exist"}}}}
	var lines []string
	if err := (&Runner{DryRun: true}).Run(context.Background(), p, func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 || !strings.Contains(lines[1], "touch") {
		t.Errorf("dry run output = %q", lines)
	}
}

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"/dev/sda1": "/dev/sda1",
		"my disk":   "'my disk'",
		"it's":      `'it'\''s'`,
		"":          "''",
		"$(reboot)": "'$(reboot)'",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

// usbStick is the shape of the disk this was written for: a FAT partition,
// a Linux partition after it, then free space.
func usbStick() *disk.Disk {
	return &disk.Disk{
		Path: "/dev/sdz", Size: 1024 * disk.MiB, SectorSize: 512, Table: "dos",
		Partitions: []disk.Partition{
			{Name: "sdz1", Path: "/dev/sdz1", Number: 1, Start: 4 * disk.MiB, Size: 100 * disk.MiB, FSType: "vfat"},
			{Name: "sdz2", Path: "/dev/sdz2", Number: 2, Start: 104 * disk.MiB, Size: 200 * disk.MiB},
		},
	}
}

func TestFATResizeNeedsFatresize(t *testing.T) {
	if toolExists("fatresize") {
		t.Skip("fatresize is installed")
	}
	d := usbStick()
	_, err := Limits(d, &d.Partitions[0])
	if err == nil || !strings.Contains(err.Error(), "rvn install fatresize") {
		t.Errorf("err = %v, want a hint to install fatresize", err)
	}
}

func TestFATResizeWithFatresize(t *testing.T) {
	if !toolExists("fatresize") {
		// Put a stand-in on PATH; only its presence is checked.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "fatresize"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	d := usbStick()
	// After sdz2 is deleted, sdz1 can grow over it and the free space after.
	d.Partitions = d.Partitions[:1]
	p := &d.Partitions[0]
	l, err := Limits(d, p)
	if err != nil {
		t.Fatal(err)
	}
	if !l.CanGrow || !l.CanShrink || l.Max != 1020*disk.MiB {
		t.Errorf("limits = %+v (max %d MiB)", l, l.Max/disk.MiB)
	}
	plan, err := Resize(d, p, l.Max)
	if err != nil {
		t.Fatal(err)
	}
	got := argvs(plan)
	if got[0] != "fatresize --force-yes --partition 1 --size 1069547520 /dev/sdz" {
		t.Errorf("steps = %q", got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "sfdisk") {
			t.Errorf("fatresize sets the partition end itself; sfdisk must not run too: %q", got)
		}
	}
	if !plan.Destructive {
		t.Error("a FAT resize rewrites the FAT and should be confirmed as destructive")
	}
}

func TestUnmount(t *testing.T) {
	p := &disk.Partition{Path: "/dev/sdz1", Mountpoints: []string{"/media/a", "/mnt/b"}}
	plan, err := Unmount(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := argvs(plan); len(got) != 1 || got[0] != "umount --all-targets /dev/sdz1" {
		t.Errorf("steps = %q", got)
	}
	swap := &disk.Partition{Path: "/dev/sdz2", Mountpoints: []string{"[SWAP]"}}
	plan, _ = Unmount(swap)
	if got := argvs(plan); len(got) != 1 || got[0] != "swapoff /dev/sdz2" {
		t.Errorf("swap steps = %q", got)
	}
	if _, err := Unmount(&disk.Partition{Path: "/dev/sdz3", Holders: []string{"luks-1"}}); err == nil {
		t.Error("a LUKS member is not mounted; Unmount should say so")
	}
}
