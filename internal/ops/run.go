package ops

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Script renders a plan as a POSIX shell script that runs the steps in
// order, echoing each one first, and stops at the first that fails. The
// steps run in a subshell whose failure exits 1, so the script never exits
// 126 or 127 itself and those codes can only mean pkexec refused.
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
// When the program is not already root the whole plan goes through one
// pkexec call, so the user authenticates once per operation. In dry-run mode
// it writes the script to out and runs nothing.
func (r *Runner) Run(ctx context.Context, p *Plan, out func(string)) error {
	if r.DryRun {
		out("[dry run] " + p.Title + " — nothing was changed. This would run as root:")
		for _, line := range strings.Split(strings.TrimRight(p.Commands(), "\n"), "\n") {
			out("  # " + line)
		}
		return nil
	}

	script := p.Script()
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", script)
	} else {
		pkexec, err := exec.LookPath("pkexec")
		if err != nil {
			return fmt.Errorf("root is needed and pkexec is not installed; run the app as root instead")
		}
		cmd = exec.CommandContext(ctx, pkexec, "/bin/sh", "-c", script)
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			out(sc.Text())
		}
		close(done)
	}()
	out("Running: " + p.Title)
	err := cmd.Run()
	pw.Close()
	<-done
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && os.Geteuid() != 0 {
			// pkexec's own exit codes for a refused or dismissed prompt.
			switch ee.ExitCode() {
			case 126:
				return fmt.Errorf("authentication was cancelled; nothing was changed")
			case 127:
				return fmt.Errorf("not authorized to run as root; nothing was changed")
			}
		}
		return fmt.Errorf("%s failed: %w", p.Title, err)
	}
	return nil
}
