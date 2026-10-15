package ops

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Script renders a plan as a POSIX shell script that runs the steps in
// order, echoing each one first, and stops at the first that fails. The
// steps run in a subshell whose failure exits 1. Every step prints a ">> "
// line before it runs, which is how a failure inside the plan is told apart
// from sudo refusing to start it.
func (p *Plan) Script() string {
	var b strings.Builder
	b.WriteString("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n")
	b.WriteString("(\nset -e\n")
	for _, s := range p.Steps {
		fmt.Fprintf(&b, "echo %s\n", Quote(">> "+s.Desc))
		cmd := s.command()
		if len(s.OKExit) > 0 {
			var ok []string
			for _, c := range s.OKExit {
				ok = append(ok, "[ $rc -eq "+strconv.Itoa(c)+" ]")
			}
			fmt.Fprintf(&b, "rc=0; %s || rc=$?; [ $rc -eq 0 ] || %s\n", cmd, strings.Join(ok, " || "))
		} else {
			b.WriteString(cmd + "\n")
		}
	}
	b.WriteString(")\n")
	b.WriteString("[ $? -eq 0 ] || { echo '>> Stopped: the step above failed'; exit 1; }\n")
	b.WriteString("echo '>> Done'\n")
	return b.String()
}

// Commands lists the plan's commands one per line, as they would be typed.
func (p *Plan) Commands() string {
	var b strings.Builder
	for _, s := range p.Steps {
		b.WriteString(s.command() + "\n")
	}
	return b.String()
}

// command is the step as one shell line, its stdin piped in from printf so
// that the line never breaks: ",2048\n" becomes printf '%s\n' ,2048 | ...
func (s Step) command() string {
	cmd := QuoteArgv(s.Argv)
	if s.Stdin == "" {
		return cmd
	}
	format := "%s"
	body := s.Stdin
	if strings.HasSuffix(body, "\n") {
		format, body = `%s\n`, strings.TrimSuffix(body, "\n")
	}
	var args []string
	for _, line := range strings.Split(body, "\n") {
		args = append(args, Quote(line))
	}
	return fmt.Sprintf("printf %s %s | %s", Quote(format), strings.Join(args, " "), cmd)
}

// Quote quotes s for a POSIX shell, leaving plain words alone.
func Quote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,+@%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteArgv quotes each argument and joins them with spaces.
func QuoteArgv(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// Runner executes plans, or in dry-run mode only reports what it would run.
type Runner struct {
	DryRun bool
}

// Run executes the plan as root, sending its output line by line to out.
// When the program already runs as root the plan runs directly. Otherwise it
// runs in a terminal window under sudo, the way other Raven apps get root:
// the window shows the commands, sudo asks for the password there, and the
// user authenticates once per operation. In dry-run mode it writes the
// commands to out and runs nothing.
func (r *Runner) Run(ctx context.Context, p *Plan, out func(string)) error {
	if r.DryRun {
		out("[dry run] " + p.Title + " — nothing was changed. This would run as root:")
		for _, line := range strings.Split(strings.TrimRight(p.Commands(), "\n"), "\n") {
			out("  # " + line)
		}
		return nil
	}
	out("Running: " + p.Title)
	mismatch := false
	watch := func(line string) {
		if strings.HasPrefix(line, mismatchMarker) {
			mismatch = true
		}
		out(line)
	}
	var err error
	if os.Geteuid() == 0 {
		err = runDirect(ctx, p, watch)
	} else {
		err = runInTerminal(ctx, p, watch)
	}
	if err != nil && mismatch {
		return ErrTableMismatch
	}
	return err
}

func runDirect(ctx context.Context, p *Plan, out func(string)) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", p.Script())
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	done := make(chan struct{})
	go func() {
		sendLines(pr, out)
		close(done)
	}()
	err := cmd.Run()
	pw.Close()
	<-done
	if err != nil {
		return fmt.Errorf("%s failed: %w", p.Title, err)
	}
	return nil
}

func sendLines(r io.Reader, out func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		out(sc.Text())
	}
}

// terminals can show a command to the user and give sudo a tty to ask for
// the password on, in order of preference, each with the flag that
// introduces the command.
var terminals = []struct{ name, flag string }{
	{"raven-terminal", "-e"},
	{"foot", ""},
	{"alacritty", "-e"},
	{"kitty", ""},
	{"xterm", "-e"},
}

func terminalCommand(argv ...string) ([]string, error) {
	for _, t := range terminals {
		if path, err := exec.LookPath(t.name); err == nil {
			cmd := []string{path}
			if t.flag != "" {
				cmd = append(cmd, t.flag)
			}
			return append(cmd, argv...), nil
		}
	}
	return nil, errors.New("root is needed and no terminal is installed to ask for the sudo password in; install raven-terminal or run the app as root")
}

// terminalPoll is how often runInTerminal looks for the plan's exit status.
var terminalPoll = 200 * time.Millisecond

// runInTerminal runs the plan with sudo in a terminal window. The plan, its
// output and its exit status live in a private temporary directory: the
// window's script tees the output to a file and writes the status when sudo
// returns, and this waits for that status rather than for the window, which
// stays open until the user has read it.
func runInTerminal(ctx context.Context, p *Plan, out func(string)) error {
	sudo, err := lookTool("sudo")
	if err != nil {
		return errors.New("root is needed and sudo is not installed; run the app as root instead")
	}
	dir, err := os.MkdirTemp("", "raven-partition-manager-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	planPath := filepath.Join(dir, "plan.sh")
	outPath := filepath.Join(dir, "output")
	statusPath := filepath.Join(dir, "status")
	if err := os.WriteFile(planPath, []byte(p.Script()), 0o600); err != nil {
		return err
	}
	wrapper := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(wrapper, []byte(terminalScript(p, sudo, planPath, outPath, statusPath)), 0o600); err != nil {
		return err
	}
	argv, err := terminalCommand("/bin/sh", wrapper)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not open a terminal: %w", err)
	}
	closed := make(chan struct{})
	go func() {
		cmd.Wait()
		close(closed)
	}()
	out("Enter your password in the terminal window to continue.")

	tick := time.NewTicker(terminalPoll)
	defer tick.Stop()
	finished := func() bool { _, err := os.Stat(statusPath); return err == nil }
	for !finished() {
		select {
		case <-ctx.Done():
			cmd.Process.Kill()
			return ctx.Err()
		case <-closed:
			if !finished() {
				return terminalClosed(outPath, out)
			}
		case <-tick.C:
		}
	}

	output, _ := os.ReadFile(outPath)
	sendLines(bytes.NewReader(output), out)
	status, _ := os.ReadFile(statusPath)
	code := strings.TrimSpace(string(status))
	switch {
	case code == "0":
		return nil
	case !planStarted(output):
		return errors.New("sudo did not run the commands (wrong password, cancelled, or not allowed); nothing was changed")
	default:
		return fmt.Errorf("%s failed (exit status %s)", p.Title, code)
	}
}

// planStarted reports whether the plan got as far as its first step, which
// prints a ">> " line before it runs.
func planStarted(output []byte) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, ">> ") {
			return true
		}
	}
	return false
}

func terminalClosed(outPath string, out func(string)) error {
	output, _ := os.ReadFile(outPath)
	sendLines(bytes.NewReader(output), out)
	if planStarted(output) {
		return errors.New("the terminal was closed while the commands were running, so they may have stopped part way; check the disk before going on")
	}
	return errors.New("the terminal was closed before the commands ran; nothing was changed")
}

// terminalScript is what the terminal window runs: it shows what is about to
// happen, runs the plan under sudo with its output copied to outPath, records
// the exit status in statusPath, and waits for Enter so the result can be
// read.
func terminalScript(p *Plan, sudo, planPath, outPath, statusPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "printf '%%s\\n\\n' %s\n", Quote("Raven Partition Manager: "+p.Title))
	b.WriteString("echo 'These commands will run as root:'\n")
	for _, line := range strings.Split(strings.TrimRight(p.Commands(), "\n"), "\n") {
		fmt.Fprintf(&b, "printf '  %%s\\n' %s\n", Quote(line))
	}
	b.WriteString("echo\n")
	tmp := statusPath + ".tmp"
	fmt.Fprintf(&b, "{ %s -- /bin/sh %s; echo $? > %s; mv %s %s; } 2>&1 | tee %s\n",
		Quote(sudo), Quote(planPath), Quote(tmp), Quote(tmp), Quote(statusPath), Quote(outPath))
	b.WriteString("echo\n")
	fmt.Fprintf(&b, "if [ \"$(cat %s 2>/dev/null)\" = 0 ]; then echo 'Finished. The partition manager has updated.'; else echo 'Not finished; nothing after the failure above was run.'; fi\n", Quote(statusPath))
	b.WriteString("printf 'Press Enter to close this window… '\nread -r _\n")
	return b.String()
}
