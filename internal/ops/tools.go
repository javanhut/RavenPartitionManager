package ops

import (
	"fmt"
	"os"
	"strings"
)

// Tool is a program the plans need, and the package that provides it.
type Tool struct {
	Name    string
	Package string // rvn package, or "" when it does not come from one
	Purpose string
}

// requiredTools are what every change to a disk is made with. All of them
// are in util-linux, the same tools raven-install partitions disks with.
// mkfs and resize tools are optional: a filesystem is offered only when its
// tools are installed.
var requiredTools = []Tool{
	{"sfdisk", "util-linux", "writes partition tables"},
	{"partx", "util-linux", "tells the kernel about changed partitions"},
	{"blockdev", "util-linux", "has the kernel re-read a partition table"},
	{"wipefs", "util-linux", "erases old signatures"},
	{"umount", "util-linux", "unmounts partitions"},
	{"swapoff", "util-linux", "stops swap partitions"},
}

// MissingTools lists the required tools that are not installed. Unless the
// app already runs as root, getting root also needs sudo and a terminal for
// it to ask for the password in.
func MissingTools() []Tool {
	var missing []Tool
	for _, t := range requiredTools {
		if !toolExists(t.Name) {
			missing = append(missing, t)
		}
	}
	if os.Geteuid() != 0 {
		if !toolExists("sudo") {
			missing = append(missing, Tool{"sudo", "sudo", "runs the changes as root"})
		}
		if _, err := terminalCommand(); err != nil {
			missing = append(missing, Tool{"raven-terminal", "", "asks for the sudo password"})
		}
	}
	return missing
}

// MissingMessage says which tools are missing and how to install them, or ""
// when nothing is.
func MissingMessage(missing []Tool) string {
	if len(missing) == 0 {
		return ""
	}
	var names, pkgs []string
	seen := map[string]bool{}
	for _, t := range missing {
		names = append(names, t.Name)
		if t.Package != "" && !seen[t.Package] {
			seen[t.Package] = true
			pkgs = append(pkgs, t.Package)
		}
	}
	msg := fmt.Sprintf("%s %s not installed, so changes can only be simulated.", joinWords(names), isAre(len(names)))
	if len(pkgs) > 0 {
		msg += " Install with: rvn install " + strings.Join(pkgs, " ")
	}
	return msg
}

func joinWords(w []string) string {
	if len(w) <= 1 {
		return strings.Join(w, "")
	}
	return strings.Join(w[:len(w)-1], ", ") + " and " + w[len(w)-1]
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
