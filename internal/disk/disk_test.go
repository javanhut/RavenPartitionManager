package disk

import (
	"os"
	"path/filepath"
	"testing"
)

// A 10 GiB GPT disk: a 512 MiB ESP at 1 MiB, a gap, a mounted 4 GiB root,
// and free space to the end.
const sampleJSON = `{"blockdevices":[
 {"name":"nvme0n1","path":"/dev/nvme0n1","type":"disk","size":10737418240,"fstype":null,"label":null,
  "mountpoints":[null],"model":"Test SSD ","serial":"S1","tran":"nvme","rm":false,"ro":false,
  "pttype":"gpt","log-sec":512,"start":null,"partn":null,
  "children":[
   {"name":"nvme0n1p1","path":"/dev/nvme0n1p1","type":"part","size":536870912,"fstype":"vfat","label":"EFI",
    "uuid":"AAAA-BBBB","partlabel":"EFI system","parttype":"c12a7328-f81f-11d2-ba4b-00a0c93ec93b",
    "parttypename":"EFI System","mountpoints":["/boot"],"rm":false,"ro":false,"pttype":"gpt","log-sec":512,
    "start":2048,"partn":1},
   {"name":"nvme0n1p2","path":"/dev/nvme0n1p2","type":"part","size":4294967296,"fstype":"ext4","label":"root",
    "mountpoints":[],"rm":false,"ro":false,"pttype":"gpt","log-sec":512,"start":2099200,"partn":2}
  ]},
 {"name":"sr0","path":"/dev/sr0","type":"rom","size":1073741312,"mountpoints":[null]},
 {"name":"loop7","path":"/dev/loop7","type":"loop","size":0,"mountpoints":[null]}
]}`

func TestParse(t *testing.T) {
	disks, err := Parse([]byte(sampleJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 {
		t.Fatalf("got %d disks, want 1 (rom and empty loop dropped)", len(disks))
	}
	d := disks[0]
	if d.Model != "Test SSD" || d.Table != "gpt" || d.SectorSize != 512 {
		t.Errorf("disk fields: %+v", d)
	}
	if len(d.Mountpoints) != 0 {
		t.Errorf("null mountpoint should be dropped, got %q", d.Mountpoints)
	}
	if len(d.Partitions) != 2 {
		t.Fatalf("got %d partitions", len(d.Partitions))
	}
	p1 := d.Partition(1)
	if p1.Start != MiB || !p1.Busy() || p1.Label != "EFI" {
		t.Errorf("p1: %+v", p1)
	}
	if d.Partition(2).Busy() {
		t.Error("p2 has no mountpoints and should not be busy")
	}
	if !d.Busy() {
		t.Error("disk with a mounted partition should be busy")
	}
}

func TestRegions(t *testing.T) {
	disks, _ := Parse([]byte(sampleJSON))
	d := disks[0]
	rs := d.Regions()
	// p1 [1, 513) MiB, free [513, 1025) MiB, p2 [1025, 5121) MiB, free to 10 GiB - 1 MiB.
	want := []struct {
		kind       RegionKind
		start, end uint64
	}{
		{RegionPartition, 1 * MiB, 513 * MiB},
		{RegionFree, 513 * MiB, 1025 * MiB},
		{RegionPartition, 1025 * MiB, 5121 * MiB},
		{RegionFree, 5121 * MiB, 10239 * MiB},
	}
	if len(rs) != len(want) {
		t.Fatalf("got %d regions: %+v", len(rs), rs)
	}
	for i, w := range want {
		if rs[i].Kind != w.kind || rs[i].Start != w.start || rs[i].End() != w.end {
			t.Errorf("region %d = {%v %d..%d}, want {%v %d..%d}", i,
				rs[i].Kind, rs[i].Start/MiB, rs[i].End()/MiB, w.kind, w.start/MiB, w.end/MiB)
		}
	}
	if got := d.FreeAfter(d.Partition(1)); got != 512*MiB {
		t.Errorf("FreeAfter(p1) = %d MiB", got/MiB)
	}
	if got := d.FreeAfter(d.Partition(2)); got != (10239-5121)*MiB {
		t.Errorf("FreeAfter(p2) = %d MiB", got/MiB)
	}
}

func TestRegionsEmptyTable(t *testing.T) {
	d := Disk{Path: "/dev/sdz", Size: 100 * MiB, Table: "dos", SectorSize: 512}
	rs := d.Regions()
	if len(rs) != 1 || rs[0].Kind != RegionFree || rs[0].Start != MiB || rs[0].End() != 100*MiB {
		t.Fatalf("regions = %+v", rs)
	}
	d.Table = ""
	if len(d.Regions()) != 0 {
		t.Error("a disk without a table has no regions")
	}
}

func TestRegionsExtended(t *testing.T) {
	// MBR: p1 [1, 101) MiB, extended p2 at 101 MiB (reported as 1 KiB),
	// logicals p5 [102, 202) and p6 [203, 303). Nothing inside the extended
	// partition is free; space after 303 MiB is.
	d := Disk{Path: "/dev/sdz", Size: 1000 * MiB, Table: "dos", SectorSize: 512, Partitions: []Partition{
		{Number: 1, Start: 1 * MiB, Size: 100 * MiB},
		{Number: 2, Start: 101 * MiB, Size: 1024, TypeCode: "0x5"},
		{Number: 5, Start: 102 * MiB, Size: 100 * MiB},
		{Number: 6, Start: 203 * MiB, Size: 100 * MiB},
	}}
	for _, r := range d.Regions() {
		if r.Kind == RegionFree && r.Start < 303*MiB {
			t.Errorf("free region inside extended partition: %d..%d MiB", r.Start/MiB, r.End()/MiB)
		}
	}
	rs := d.Regions()
	last := rs[len(rs)-1]
	if last.Kind != RegionFree || last.Start != 303*MiB || last.End() != 1000*MiB {
		t.Errorf("last region = %+v", last)
	}
}

func TestPartitionPath(t *testing.T) {
	cases := map[string]string{
		"/dev/sda":     "/dev/sda3",
		"/dev/nvme0n1": "/dev/nvme0n1p3",
		"/dev/mmcblk0": "/dev/mmcblk0p3",
		"/dev/loop0":   "/dev/loop0p3",
	}
	for disk, want := range cases {
		if got := (&Disk{Path: disk}).PartitionPath(3); got != want {
			t.Errorf("PartitionPath(%s) = %s, want %s", disk, got, want)
		}
	}
}

func TestNextPartitionNumberAndLimits(t *testing.T) {
	d := Disk{Table: "dos", Partitions: []Partition{{Number: 1}, {Number: 3}}}
	if n := d.NextPartitionNumber(); n != 2 {
		t.Errorf("next = %d, want 2", n)
	}
	d.Partitions = append(d.Partitions, Partition{Number: 2}, Partition{Number: 4})
	if ok, _ := d.CanAddPartition(); ok {
		t.Error("MBR with four primaries should be full")
	}
	d.Table = ""
	if ok, _ := d.CanAddPartition(); ok {
		t.Error("no table means no partitions")
	}
}

// lsblk leaves PARTN null when udev did not record the partition entry; the
// number then comes from sysfs, and a number that cannot be read there stops
// NextPartitionNumber from guessing.
func TestPartitionNumberFromSysfs(t *testing.T) {
	old := sysfsBlock
	sysfsBlock = t.TempDir()
	defer func() { sysfsBlock = old }()
	if err := os.MkdirAll(filepath.Join(sysfsBlock, "sdb1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysfsBlock, "sdb1", "partition"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const js = `{"blockdevices":[
	 {"name":"sdb","path":"/dev/sdb","type":"disk","size":256355860480,"pttype":"dos","log-sec":512,"partn":null,
	  "children":[
	   {"name":"sdb1","path":"/dev/sdb1","type":"part","size":2147483648,"fstype":"vfat","pttype":"dos",
	    "start":8192,"partn":null,"mountpoints":["/media/usb"]},
	   {"name":"sdb2","path":"/dev/sdb2","type":"part","size":1048576,"pttype":"dos","start":4202496,"partn":null}
	  ]}]}`
	disks, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	d := disks[0]
	if d.Partition(1) == nil || d.Partition(1).Name != "sdb1" {
		t.Fatalf("sdb1 should be partition 1: %+v", d.Partitions)
	}
	if n := d.NextPartitionNumber(); n != 0 {
		t.Errorf("next = %d with sdb2's number unknown, want 0", n)
	}
	d.Partitions = []Partition{*d.Partition(1)}
	if n := d.NextPartitionNumber(); n != 2 {
		t.Errorf("next = %d, want 2", n)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[uint64]string{
		512:           "512 B",
		1536:          "1.5 KiB",
		MiB:           "1.0 MiB",
		5000981077504: "4.5 TiB",
	}
	for in, want := range cases {
		if got := HumanSize(in); got != want {
			t.Errorf("HumanSize(%d) = %s, want %s", in, got, want)
		}
	}
}
