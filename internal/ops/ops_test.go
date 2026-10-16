package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	if got[0] != "wipefs --all /dev/sdz" || got[1] != "sfdisk --quiet --wipe always /dev/sdz" || p.Steps[1].Stdin != "label: dos\n" {
		t.Errorf("steps = %q, stdin %q", got, p.Steps[1].Stdin)
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
	if got[0] != strings.Join(tableCheck(d).Argv, " ") {
		t.Errorf("the plan should check the table first: %q", got[0])
	}
	got = got[1:]
	want := []string{
		"sfdisk --quiet --no-reread --no-tell-kernel --wipe-partitions always --append /dev/sdz",
		"partx --add --nr 4 /dev/sdz",
		strings.Join(settle().Argv, " "),
		strings.Join(waitForNode("/dev/sdz4").Argv, " "),
		"grep -qx 411648 /sys/class/block/sdz4/start",
		"mkfs.ext4 -F -L home /dev/sdz4",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("steps:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if in := p.Steps[1].Stdin; in != "start=411648, size=102400, type="+gptLinux+`, name="home"`+"\n" {
		t.Errorf("sfdisk input = %q", in)
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
	if got := argvs(p); got[1] != "sfdisk --quiet --no-reread --no-tell-kernel --delete /dev/sdz 1" || got[2] != "partx --delete --nr 1 /dev/sdz" {
		t.Errorf("steps = %q", got)
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
	gs := argvs(grow)[1:]
	if gs[0] != "sfdisk --quiet --no-reread --no-tell-kernel -N 3 /dev/sdz" || grow.Steps[1].Stdin != ",409600\n" {
		t.Errorf("grow should change the table first: %q stdin %q", gs, grow.Steps[0].Stdin)
	}
	if gs[len(gs)-1] != "resize2fs /dev/sdz3" {
		t.Errorf("grow should end by growing the filesystem: %q", gs)
	}

	shrink, err := Resize(d, p3, 60*disk.MiB)
	if err != nil {
		t.Fatal(err)
	}
	ss := argvs(shrink)[1:]
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

// sfdiskEntry is one partition as `sfdisk --json` reports it.
type sfdiskEntry struct {
	Node  string `json:"node"`
	Start uint64 `json:"start"`
	Size  uint64 `json:"size"`
	Type  string `json:"type"`
	Name  string `json:"name"`
}

// TestPlansOnImage runs the table-writing steps of each plan (sfdisk and
// wipefs, which work on image files as on disks) and reads the table back,
// creating partitions into a gap to check that sfdisk numbers them the way
// NextPartitionNumber predicts.
func TestPlansOnImage(t *testing.T) {
	for _, tool := range []string{"sfdisk", "wipefs"} {
		if !toolExists(tool) {
			t.Skip(tool + " not installed")
		}
	}
	for _, table := range []TableType{TableGPT, TableMSDOS} {
		t.Run(string(table), func(t *testing.T) {
			img := filepath.Join(t.TempDir(), "disk.img")
			if err := os.WriteFile(img, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(img, 64*disk.MiB); err != nil {
				t.Fatal(err)
			}
			d := &disk.Disk{Path: img, Size: 64 * disk.MiB, SectorSize: 512}
			apply := func(p *Plan, err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range p.Steps {
					if s.Argv[0] != "sfdisk" && s.Argv[0] != "wipefs" {
						continue
					}
					cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
					cmd.Stdin = strings.NewReader(s.Stdin)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("%s: %v\n%s", s.command(), err, out)
					}
				}
			}
			// read loads the table into d the way the kernel would show it.
			read := func() map[int]sfdiskEntry {
				t.Helper()
				out, err := exec.Command("sfdisk", "--json", img).Output()
				if err != nil {
					t.Fatal(err)
				}
				var j struct {
					Table struct {
						Label      string        `json:"label"`
						Partitions []sfdiskEntry `json:"partitions"`
					} `json:"partitiontable"`
				}
				if err := json.Unmarshal(out, &j); err != nil {
					t.Fatal(err)
				}
				d.Table = j.Table.Label
				d.Partitions = nil
				got := map[int]sfdiskEntry{}
				for _, e := range j.Table.Partitions {
					n, _ := strconv.Atoi(strings.TrimPrefix(e.Node, img))
					got[n] = e
					d.Partitions = append(d.Partitions, disk.Partition{Number: n, Start: e.Start * 512, Size: e.Size * 512})
				}
				return got
			}

			apply(NewTable(d, table))
			if got := read(); len(got) != 0 || d.Table != sfdiskLabel(table) {
				t.Fatalf("new table: label %q, partitions %+v", d.Table, got)
			}
			for _, start := range []uint64{1, 9, 17} {
				apply(Create(d, CreateRequest{Start: start * disk.MiB, Size: 8 * disk.MiB, Label: "data"}))
				read()
			}
			apply(Delete(d, d.Partition(2)))
			read()
			if n := d.NextPartitionNumber(); n != 2 {
				t.Fatalf("next = %d after deleting 2", n)
			}
			apply(Create(d, CreateRequest{Start: 40 * disk.MiB, Size: 4 * disk.MiB, Label: "late"}))
			got := read()
			p2 := got[2]
			if p2.Start != 40*disk.MiB/512 || p2.Size != 4*disk.MiB/512 {
				t.Errorf("sfdisk did not put the new partition in slot 2: %+v", got)
			}
			if want := partType(d.Table, nil, false); !strings.EqualFold(p2.Type, want) {
				t.Errorf("type = %q, want %q", p2.Type, want)
			}
			if table == TableGPT && p2.Name != "late" {
				t.Errorf("GPT name = %q, want the label", p2.Name)
			}
			apply(Resize(d, d.Partition(3), 12*disk.MiB))
			if got := read(); got[3].Size != 12*disk.MiB/512 || got[3].Start != 17*disk.MiB/512 {
				t.Errorf("resize: %+v", got[3])
			}
		})
	}
}

func TestGPTName(t *testing.T) {
	for label, want := range map[string]bool{
		"home": true, "": false, `say "hi"`: false, `a\b`: false, "tab\there": false,
		strings.Repeat("x", 36): true, strings.Repeat("x", 37): false, strings.Repeat("é", 36): true,
		strings.Repeat("😀", 18): true, strings.Repeat("😀", 19): false,
	} {
		if got := gptName(label); got != want {
			t.Errorf("gptName(%q) = %v, want %v", label, got, want)
		}
	}
}

func TestMissingMessage(t *testing.T) {
	if MissingMessage(nil) != "" {
		t.Error("nothing missing should say nothing")
	}
	msg := MissingMessage([]Tool{requiredTools[0], requiredTools[1], {"raven-terminal", "", ""}, {"sudo", "sudo", ""}})
	want := "sfdisk, partx, raven-terminal and sudo are not installed, so changes can only be simulated. Install with: rvn install util-linux sudo"
	if msg != want {
		t.Errorf("got  %q\nwant %q", msg, want)
	}
}

// The check runs against image files, which partx reads like disks.
func TestTableCheck(t *testing.T) {
	for _, tool := range []string{"partx", "sfdisk"} {
		if !toolExists(tool) {
			t.Skip(tool + " not installed")
		}
	}
	img := filepath.Join(t.TempDir(), "disk.img")
	run := func(stdin string, argv ...string) error {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	if err := run("", "truncate", "-s", "64M", img); err != nil {
		t.Fatal(err)
	}
	// Through Script, so the quoting the plan runs with is tested too.
	check := func(d *disk.Disk) error {
		step := tableCheck(d)
		step.Argv[4] = img
		return run("", "/bin/sh", "-c", (&Plan{Steps: []Step{step}}).Script())
	}
	d := &disk.Disk{Path: "/dev/sdz", Table: "dos", Partitions: []disk.Partition{
		{Number: 1, Start: disk.MiB, Size: 4 * disk.MiB},
		{Number: 2, Start: 6 * disk.MiB, Size: 1024, TypeCode: "0x5"},
		{Number: 5, Start: 7 * disk.MiB, Size: 2 * disk.MiB},
	}}
	// What happened to a USB stick: the table was emptied while its first
	// partition stayed mounted, so the kernel still showed it.
	if err := run("label: dos\n", "sfdisk", "-q", img); err != nil {
		t.Fatal(err)
	}
	if err := check(d); err == nil || !strings.Contains(err.Error(), "not the one the kernel is using") {
		t.Errorf("an empty table should not match three partitions: %v", err)
	}
	if err := run("start=2048,size=8192,type=c\nstart=12288,size=40960,type=5\nstart=14336,size=4096,type=83\n", "sfdisk", "-q", img); err != nil {
		t.Fatal(err)
	}
	if err := check(d); err != nil {
		t.Errorf("the same table should match: %v", err)
	}
	d.Partitions[0].Size = 3 * disk.MiB
	if err := check(d); err == nil {
		t.Error("a partition of another size should not match")
	}
	if err := check(&disk.Disk{Path: "/dev/sdz", Table: "dos"}); err == nil {
		t.Error("a table with partitions should not match an empty one")
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
	if got[1] != "fatresize --force-yes --partition 1 --size 1069547520 /dev/sdz" {
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

// fakeRoot puts a raven-terminal that runs its command in place and a sudo
// with the given body first on PATH, so runInTerminal can run without a
// display or a password.
func fakeRoot(t *testing.T, sudoBody string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: plans run directly")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("raven-terminal", "[ \"$1\" = -e ] && shift\nexec \"$@\"\n")
	write("sudo", sudoBody)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func runLive(t *testing.T, p *Plan) (string, error) {
	var lines []string
	err := (&Runner{}).Run(context.Background(), p, func(l string) { lines = append(lines, l) })
	return strings.Join(lines, "\n"), err
}

func TestRunInTerminal(t *testing.T) {
	fakeRoot(t, "[ \"$1\" = -- ] && shift\nexec \"$@\"\n")
	marker := filepath.Join(t.TempDir(), "ran")
	out, err := runLive(t, &Plan{Title: "t", Steps: []Step{{Desc: "touch", Argv: []string{"touch", marker}}}})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the plan did not run:\n%s", out)
	}
	if !strings.Contains(out, ">> touch") || !strings.Contains(out, ">> Done") {
		t.Errorf("the plan's output should reach the log:\n%s", out)
	}

	out, err = runLive(t, &Plan{Title: "t", Steps: []Step{{Desc: "fails", Argv: []string{"false"}}}})
	if err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("err = %v, want the plan's failure\n%s", err, out)
	}
}

// A plan that stops at its table check reports ErrTableMismatch, so the
// window can offer to re-read the table.
func TestRunReportsTableMismatch(t *testing.T) {
	fakeRoot(t, "[ \"$1\" = -- ] && shift\nexec \"$@\"\n")
	check := Step{Desc: "check", Argv: []string{"sh", "-c", "echo \"$1\" >&2; exit 1", "sh", mismatchMarker + " test"}}
	out, err := runLive(t, &Plan{Title: "t", Steps: []Step{check}})
	if !errors.Is(err, ErrTableMismatch) {
		t.Errorf("err = %v, want ErrTableMismatch\n%s", err, out)
	}
	out, err = runLive(t, &Plan{Title: "t", Steps: []Step{{Desc: "fails", Argv: []string{"false"}}}})
	if errors.Is(err, ErrTableMismatch) {
		t.Errorf("an ordinary failure is not a mismatch\n%s", out)
	}
}

func TestReloadTable(t *testing.T) {
	d := testDisk()
	p, err := ReloadTable(d)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"umount --all-targets /dev/sdz2", "blockdev --rereadpt /dev/sdz", strings.Join(settle().Argv, " ")}
	if got := argvs(p); strings.Join(got, "\n") != strings.Join(want, "\n") || p.Destructive {
		t.Errorf("steps = %q, destructive = %v; want the mounted sdz2 unmounted, then the re-read", got, p.Destructive)
	}
	d.Partitions[0].Holders = []string{"luks-sdz1"}
	if _, err := ReloadTable(d); err == nil {
		t.Error("a partition holding LUKS cannot be let go by unmounting")
	}
}

func TestRunInTerminalSudoRefused(t *testing.T) {
	fakeRoot(t, "echo 'Sorry, try again.' >&2\nexit 1\n")
	marker := filepath.Join(t.TempDir(), "ran")
	out, err := runLive(t, &Plan{Title: "t", Steps: []Step{{Desc: "touch", Argv: []string{"touch", marker}}}})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("err = %v, want sudo's refusal\n%s", err, out)
	}
	if !strings.Contains(out, "Sorry, try again.") {
		t.Errorf("sudo's message should reach the log:\n%s", out)
	}
}

func TestRunInTerminalClosedEarly(t *testing.T) {
	fakeRoot(t, "exit 0\n")
	// A terminal that is closed without running anything.
	dir := filepath.SplitList(os.Getenv("PATH"))[0]
	if err := os.WriteFile(filepath.Join(dir, "raven-terminal"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runLive(t, &Plan{Title: "t", Steps: []Step{{Desc: "x", Argv: []string{"true"}}}})
	if err == nil || !strings.Contains(err.Error(), "closed before") {
		t.Errorf("err = %v, want the terminal-closed error", err)
	}
}
