# Raven Partition Manager

View and change disk partitions on Raven Linux. The window shows each disk as
a map of its partitions and free space. From there you can write a new
partition table, and create, delete, format and resize partitions.

GTK 4 in Go, through [gotk4](https://github.com/diamondburned/gotk4).

```
make            # build to bin/raven-partition-manager
make run        # run it
make probe      # print every disk, partition and free region the app sees
make test       # unit tests (no GTK needed)
sudo make install
```

or with imlazy: `imlazy build`, `imlazy run`, `imlazy probe`, `imlazy install`.

The first build compiles the GTK bindings through cgo. That takes several
minutes; later builds are fast.

## Safety

- **Dry run is on when the window opens.** In dry run, an operation writes the
  commands it would run to the log and changes nothing. Turn the header
  switch off to make real changes; the banner turns red while it is off.
- **Every operation shows its plan first:** what will happen in words, and the
  exact commands. Nothing runs until you press Apply (or Simulate).
- **Anything in use is left alone.** Delete, Format and Resize are greyed out
  for partitions that are mounted, active swap, or used by LUKS, LVM or RAID.
  New Partition Table is greyed out while anything on the disk is in use, and
  read-only disks can't be changed. A line under the buttons says why, and
  Unmount (`umount --all-targets`, or `swapoff` for swap) frees a mounted
  partition.
- **Steps stop at the first failure.** When a partition shrinks, the
  filesystem shrinks first. If it doesn't fit, the partition table is never
  touched.
- **New partitions are checked before they are formatted.** parted gives a
  new partition the lowest free number. Before running mkfs, the plan checks
  in `/sys/class/block/<dev>/start` that the device with that number starts
  where the new partition was asked to.

## How it works

| Piece | What it does |
|---|---|
| `internal/disk` | Reads `lsblk --json --bytes`. Builds each disk's partitions and the free gaps between them, aligned to 1 MiB, with 1 MiB kept at each end of a GPT disk. |
| `internal/ops` | Turns each operation into a `Plan` of commands. Renders the plan as a `set -e` shell script and runs it as root: through one `pkexec` prompt per operation, or directly when the app already runs as root. |
| `internal/ui` | The window: disk picker, disk map (cairo), partition list, action bar and log. |

| Operation | Commands |
|---|---|
| New partition table | `wipefs --all`, `parted mklabel gpt\|msdos` |
| New partition | `parted mkpart` (byte-exact, 1 MiB aligned), optional `set esp on`, `udevadm settle`, start check, `mkfs.*` |
| Delete | `parted rm` |
| Format | `wipefs --all`, `mkfs.*` |
| Resize | ext2/3/4: `e2fsck -f -y` + `resize2fs`. NTFS: `ntfsresize`. Unformatted: table only. The partition's end moves with `sfdisk -N`, then `partx --update`. FAT: `fatresize`, which resizes the filesystem and moves the partition's end itself. |
| Unmount | `umount --all-targets`, or `swapoff` |

Filesystems are only offered when their mkfs tool is installed: ext4, Btrfs,
XFS, FAT32, exFAT, NTFS and swap.

### Limits of this first version

- A partition keeps its start when resized, so it can only grow into free
  space directly after it.
- Btrfs and XFS can only be resized while mounted, so they can't be resized
  here. FAT can, once `fatresize` is installed (`rvn install fatresize`).
- On MBR disks, only primary partitions are created (at most four). Existing
  extended and logical partitions are shown and can be deleted or formatted.
- Operations run one at a time. There is no queue of pending changes to
  review and apply together.

## Run-time dependencies

GTK ≥ 4.12, util-linux (`lsblk`, `sfdisk`, `wipefs`, `partx`), `parted`,
`udevadm`, polkit's `pkexec`, plus the mkfs and resize tools for each
filesystem you use (e2fsprogs, btrfs-progs, dosfstools, ntfs-3g, …).
