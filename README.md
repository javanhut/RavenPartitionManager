# Raven Partition Manager

View and change disk partitions on Raven Linux. The window shows each disk as
a map of its partitions and free space. From there you can write a new
partition table, and create, delete, format and resize partitions.

GTK 4 in Go, through [gotk4](https://github.com/diamondburned/gotk4).

Built with [imlazy](lazy.toml):

```
imlazy build      # build to bin/raven-partition-manager (installs gtk4 with rvn if missing)
imlazy run        # run it
imlazy probe      # print every disk, partition and free region the app sees
imlazy test       # unit tests (no GTK needed)
imlazy check      # gofmt, vet and all tests
imlazy install    # install to /usr/local; only the copying asks for root
imlazy uninstall
```

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
- **The table on the disk must be the one shown.** The window shows the
  kernel's view of a disk. If the table on the disk was changed while a
  partition on it was in use, the two differ. New partition, Delete and
  Resize first compare the table on the disk (`partx --show`) with what the
  window shows, and stop before writing anything when they differ. The window
  then offers Re-read Table (`blockdev --rereadpt`, after unmounting what is
  mounted on the disk).
- **New partitions are checked before they are formatted.** sfdisk gives a
  new partition the lowest free number. Before running mkfs, the plan checks
  in `/sys/class/block/<dev>/start` that the device with that number starts
  where the new partition was asked to.
- **Missing tools are named at startup.** If a required tool is missing, a
  banner names it with the `rvn install` command, and dry run stays on until
  it is installed.

## How it works

| Piece | What it does |
|---|---|
| `internal/disk` | Reads `lsblk --json --bytes`, with partition numbers from `/sys/class/block` when lsblk has none (Raven's udev does not record them). Builds each disk's partitions and the free gaps between them, aligned to 1 MiB, with 1 MiB kept at each end of a GPT disk. |
| `internal/ops` | Turns each operation into a `Plan` of commands. Renders the plan as a `set -e` shell script and runs it as root: directly when the app already runs as root, otherwise in a Raven Terminal window under `sudo`, like other Raven apps. The window shows the commands, asks for the password once per operation, and stays open so you can read the result; the output also goes to the app's log. |
| `internal/ui` | The window: disk picker, disk map (cairo), partition list, action bar and log. |

| Operation | Commands |
|---|---|
| New partition table | `wipefs --all`, `sfdisk` with `label: gpt\|dos` |
| New partition | table check, `sfdisk --append` (1 MiB aligned, old signatures in the new partition wiped), `partx --add`, wait for the device node, start check, `mkfs.*` |
| Delete | table check, `sfdisk --delete`, `partx --delete` |
| Format | `wipefs --all`, `mkfs.*` |
| Resize | ext2/3/4: `e2fsck -f -y` + `resize2fs`. NTFS: `ntfsresize`. Unformatted: table only. A table check comes first. The partition's end moves with `sfdisk -N`, then `partx --update`. FAT: `fatresize`, which resizes the filesystem and moves the partition's end itself. |
| Unmount | `umount --all-targets`, or `swapoff` |

sfdisk writes the table without telling the kernel (`--no-reread
--no-tell-kernel`), and `partx` then tells it about the one partition that
changed. That works while other partitions on the disk are mounted. After a
change the plan runs `udevadm settle` if udev is installed. udev is optional
on Raven, and without it the kernel's devtmpfs makes the device nodes.

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

GTK ≥ 4.12, util-linux (`lsblk`, `sfdisk`, `partx`, `blockdev`, `wipefs`,
`umount`, `swapoff`), and `sudo` and `raven-terminal` (or foot, alacritty,
kitty or xterm) unless the app runs as root. These are the same tools
raven-install partitions disks with. Optional: `udevadm`, and the mkfs and
resize tools for each filesystem you use (e2fsprogs, btrfs-progs, dosfstools,
ntfs-3g, …).
