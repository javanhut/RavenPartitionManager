// Raven Partition Manager: view and change disk partitions on Raven Linux.
//
//	raven-partition-manager           open the window
//	raven-partition-manager --probe   print every disk, partition and free region, then exit
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"raven-partition-manager/internal/disk"
	"raven-partition-manager/internal/ui"
)

const appID = "com.ravenpartitionmanager.Raven"

func main() {
	for _, a := range os.Args[1:] {
		switch a {
		case "--probe":
			os.Exit(probe())
		case "-h", "--help":
			fmt.Println("usage: raven-partition-manager [--probe]")
			fmt.Println("  --probe  print every disk, partition and free region, then exit")
			return
		}
	}

	// gotk4 sends GLib's log to slog at INFO, which buries GTK's warnings
	// under Mesa's Vulkan loader chatter. G_MESSAGES_DEBUG still brings it back.
	if os.Getenv("G_MESSAGES_DEBUG") == "" {
		slog.SetLogLoggerLevel(slog.LevelWarn)
	}

	app := gtk.NewApplication(appID, gio.ApplicationDefaultFlags)
	app.ConnectActivate(func() {
		if w := app.ActiveWindow(); w != nil {
			w.Present()
			return
		}
		ui.NewWindow(app).Present()
	})
	os.Exit(app.Run(os.Args[:1]))
}

func probe() int {
	disks, err := disk.Probe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, d := range disks {
		var flags []string
		if d.Table != "" {
			flags = append(flags, "table="+d.Table)
		} else {
			flags = append(flags, "no table")
		}
		if d.Removable {
			flags = append(flags, "removable")
		}
		if d.ReadOnly {
			flags = append(flags, "read-only")
		}
		if d.Busy() {
			flags = append(flags, "in use")
		}
		fmt.Printf("%s  [%s]\n", d.DisplayName(), strings.Join(flags, ", "))
		if d.Table == "" && d.FSType != "" {
			fmt.Printf("    whole-disk %s filesystem %s\n", d.FSType, strings.Join(d.Mountpoints, ", "))
		}
		for _, r := range d.Regions() {
			start := float64(r.Start) / disk.MiB
			if r.Kind == disk.RegionFree {
				fmt.Printf("    %-16s %12.0f MiB  %10s\n", "unallocated", start, disk.HumanSize(r.Size))
				continue
			}
			p := r.Part
			fmt.Printf("    %-16s %12.0f MiB  %10s  %-8s %-14s %s\n",
				p.Name, start, disk.HumanSize(p.Size), p.FSType, p.Label, strings.Join(p.Mountpoints, ", "))
		}
	}
	return 0
}
