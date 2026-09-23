//go:build integration

package integration

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tbrockman/rig/internal/incus"
)

// The guest-side verbs, driven through the built binary rather than the
// packages behind it, because the logic under test — what `rig agent start`
// writes, what `rig push` refuses, what `rig forward` tears down — lives in
// the commands and is only honest end to end. None of it needs the card, so
// the VM is made --no-gpu and the card stays where it was.

// rigBin is the binary `make` left in the repo root. The Makefile builds it
// before this suite runs; a stale one would test yesterday's rig.
func rigBin(t *testing.T) string {
	t.Helper()
	bin, err := filepath.Abs("../rig")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no ./rig binary; run make first")
	}
	return bin
}

func rig(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command(rigBin(t), args...).CombinedOutput()
	return string(out), err
}

func mustRig(t *testing.T, args ...string) string {
	t.Helper()
	out, err := rig(t, args...)
	if err != nil {
		t.Fatalf("rig %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// fakeClaude stands in for the agent binary. It records how it was called,
// creates the transcript file the runner looks for when deciding whether to
// resume, speaks one line of stream-json, and declares the brief finished on
// its second turn — so an --until-done run has to survive exactly one turn
// boundary to complete.
const fakeClaude = `#!/bin/sh
D=/var/lib/rig-agent/fake
mkdir -p "$D"
n=$(( $(cat "$D/count" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$D/count"
sid=""; prompt=""; rec=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session-id|--resume) sid="$2"; rec="$rec $1 $2"; shift ;;
    -p) prompt="$2"; rec="$rec -p <prompt>"; shift ;;
    *) rec="$rec $1" ;;
  esac
  shift
done
echo "$rec" >> "$D/argv"
printf '%s' "$prompt" > "$D/prompt.$n"
mkdir -p /var/lib/rig-agent/projects/fake
touch "/var/lib/rig-agent/projects/fake/$sid.jsonl"
printf '{"type":"assistant","message":{"content":[{"type":"text","text":"turn %s"}]}}\n' "$n"
[ "$n" -ge 2 ] && touch /var/lib/rig-agent/DONE
printf '{"type":"result","subtype":"success","is_error":false,"result":"ok"}\n'
exit 0
`

func TestGuestVerbs(t *testing.T) {
	preflight(t)
	bin := rigBin(t)
	const name = "gputest-guest"

	envFile := filepath.Join(t.TempDir(), "creds.env")
	if err := os.WriteFile(envFile, []byte("ANTHROPIC_API_KEY=not-a-real-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	teardown(t, name)
	t.Cleanup(func() { teardown(t, name) })
	mustRig(t, "new", name, "--no-gpu", "--cpus", "2", "--memory", "4GiB", "--env", envFile)
	mustRig(t, "start", name)

	t.Run("agent start refuses a guest with no agent binary", func(t *testing.T) {
		brief := filepath.Join(t.TempDir(), "brief.md")
		os.WriteFile(brief, []byte("Do the thing.\n"), 0o644)
		out, err := rig(t, "agent", "start", name, "--prompt-file", brief)
		if err == nil {
			t.Fatal("agent start succeeded with no claude in the guest; the unit would restart-loop on exit 127")
		}
		for _, want := range []string{"no `claude`", "rig agent install " + name} {
			if !strings.Contains(out, want) {
				t.Errorf("refusal should say %q, got:\n%s", want, out)
			}
		}
	})

	t.Run("agent lifecycle: start, resume across a turn boundary, deliver a message, finish", func(t *testing.T) {
		// /usr/bin is on the unit's PATH and is the one writable directory
		// there; see agent.UnitPATH.
		if err := c.WriteFile(name, "/usr/bin/claude", []byte(fakeClaude), 0o755); err != nil {
			t.Fatal(err)
		}
		brief := filepath.Join(t.TempDir(), "brief.md")
		os.WriteFile(brief, []byte("Do the thing.\n"), 0o644)

		out := mustRig(t, "agent", "start", name, "--prompt-file", brief, "--until-done")
		if !strings.Contains(out, "new session") {
			t.Errorf("a first start must mint a session, got:\n%s", out)
		}
		mustRig(t, "agent", "send", name, "hello from the operator")

		// Turn 1 exits at once; systemd waits RestartSec=30s; turn 2 finishes.
		deadline := time.Now().Add(4 * time.Minute)
		var status string
		for time.Now().Before(deadline) {
			status = mustRig(t, "agent", "status", name)
			if strings.Contains(status, "COMPLETE") {
				break
			}
			time.Sleep(5 * time.Second)
		}
		if !strings.Contains(status, "COMPLETE") {
			t.Fatalf("the mission never completed; last status:\n%s", status)
		}
		if !strings.Contains(status, "last exit      0") {
			t.Errorf("a finished mission reports exit 0, got:\n%s", status)
		}

		argv := pull(t, name, "/var/lib/rig-agent/fake/argv")
		lines := strings.Split(strings.TrimSpace(argv), "\n")
		if len(lines) < 2 {
			t.Fatalf("expected two turns, argv was:\n%s", argv)
		}
		if !strings.Contains(lines[0], "--session-id") || strings.Contains(lines[0], "--resume") {
			t.Errorf("turn 1 must start a session, got: %s", lines[0])
		}
		if !strings.Contains(lines[1], "--resume") {
			t.Errorf("turn 2 must resume, decided from the transcript on disk, got: %s", lines[1])
		}
		for _, l := range lines {
			if !strings.Contains(l, "--dangerously-skip-permissions") || !strings.Contains(l, "--output-format stream-json") {
				t.Errorf("every turn runs unattended with stream-json output, got: %s", l)
			}
		}

		second := pull(t, name, "/var/lib/rig-agent/fake/prompt.2")
		if !strings.Contains(second, "previous turn ended") {
			t.Errorf("turn 2 must be told it was resumed after a turn boundary, got:\n%s", second)
		}
		if strings.Contains(second, "restarted after a failure") {
			t.Error("a turn boundary must not be reported to the agent as a crash")
		}
		delivered := pull(t, name, "/var/lib/rig-agent/fake/prompt.1") + second
		if !strings.Contains(delivered, "hello from the operator") {
			t.Error("the queued message was never delivered in any prompt")
		}

		log := mustRig(t, "agent", "log", name)
		if !strings.Contains(log, "turn 1") || !strings.Contains(log, "turn 2") {
			t.Errorf("agent log must show what the agent said, got:\n%s", log)
		}
		if strings.Contains(log, "tool_use") || strings.Contains(log, `"type"`) {
			t.Error("agent log must be the words, not the JSON")
		}
	})

	t.Run("push refuses to clobber a guest-side edit", func(t *testing.T) {
		src := t.TempDir()
		os.WriteFile(filepath.Join(src, "a.txt"), []byte("from the host\n"), 0o644)
		mustRig(t, "push", name, src, "--dest", "/work/pg")

		if err := c.WriteFile(name, "/work/pg/a.txt", []byte("edited in the guest\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := rig(t, "push", name, src, "--dest", "/work/pg")
		if err == nil {
			t.Fatal("a second push overwrote a file the guest had changed")
		}
		if !strings.Contains(out, "/work/pg/a.txt") || !strings.Contains(out, "--force") {
			t.Errorf("the refusal must name the file and the override, got:\n%s", out)
		}
		if got := pull(t, name, "/work/pg/a.txt"); got != "edited in the guest\n" {
			t.Errorf("the refused push still changed the file: %q", got)
		}

		mustRig(t, "push", name, src, "--dest", "/work/pg", "--force")
		if got := pull(t, name, "/work/pg/a.txt"); got != "from the host\n" {
			t.Errorf("--force did not overwrite: %q", got)
		}
		// Re-pushing what rig itself wrote is the normal workflow and needs
		// no --force.
		mustRig(t, "push", name, src, "--dest", "/work/pg")
	})

	t.Run("forward carries a port and tears its guest half down", func(t *testing.T) {
		if _, err := c.Exec(name, "command -v socat", incus.ExecOpts{Timeout: 15 * time.Second}); err != nil {
			t.Skip("no socat in this guest image; rebuild it with `rig image build`")
		}
		const port = 18099
		server := fmt.Sprintf("setsid socat TCP-LISTEN:%d,reuseaddr,fork SYSTEM:'echo pong' >/tmp/srv.log 2>&1 & disown; true", port)
		if _, err := c.Exec(name, server, incus.ExecOpts{Timeout: 15 * time.Second}); err != nil {
			t.Fatal(err)
		}
		defer c.Exec(name, fmt.Sprintf("pkill -f TCP-LISTEN:%d; true", port), incus.ExecOpts{Timeout: 15 * time.Second})
		// The server starts in the background; do not race it.
		listening := fmt.Sprintf("for i in $(seq 50); do ss -ltnH | grep -q ':%d ' && exit 0; sleep 0.2; done; cat /tmp/srv.log; exit 1", port)
		if out, err := c.Exec(name, listening, incus.ExecOpts{Timeout: 30 * time.Second}); err != nil {
			t.Fatalf("the test server never listened in the guest: %v\n%s", err, out)
		}

		fwd := exec.Command(bin, "forward", name, fmt.Sprint(port))
		stdout, err := fwd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		fwd.Stderr = os.Stderr
		if err := fwd.Start(); err != nil {
			t.Fatal(err)
		}
		// Wait for rig to say the tunnel is up, then prove it with a round trip.
		ready := make(chan bool, 1)
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				if strings.Contains(sc.Text(), "is now") {
					ready <- true
				}
			}
		}()
		select {
		case <-ready:
		case <-time.After(30 * time.Second):
			fwd.Process.Kill()
			t.Fatal("rig forward never reported the tunnel up")
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
		if err != nil {
			fwd.Process.Kill()
			t.Fatalf("dialing the forwarded port: %v", err)
		}
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		if err != nil || strings.TrimSpace(line) != "pong" {
			fwd.Process.Kill()
			t.Fatalf("round trip through the tunnel: %q, %v", line, err)
		}

		// Ctrl-C is the documented way to stop it; the guest half must go too.
		fwd.Process.Signal(os.Interrupt)
		if err := fwd.Wait(); err != nil {
			t.Errorf("rig forward exited non-zero on Ctrl-C: %v", err)
		}
		time.Sleep(2 * time.Second)
		// Match by process name first: a pattern alone also matches the shell
		// carrying it, and reports the probe as the leftover.
		left, _ := c.Exec(name, fmt.Sprintf("pgrep -a -x socat | grep VSOCK-LISTEN:%d || echo none", port), incus.ExecOpts{Timeout: 15 * time.Second})
		if strings.TrimSpace(left) != "none" {
			t.Errorf("the guest half survived teardown:\n%s", left)
		}
		if _, err := c.Pull(name, fmt.Sprintf("/run/rig/forward.%d.pid", port)); err == nil {
			t.Error("the pidfile was left behind")
		}
	})
}

func pull(t *testing.T, name, path string) string {
	t.Helper()
	b, err := c.Pull(name, path)
	if err != nil {
		t.Fatalf("pulling %s: %v", path, err)
	}
	return string(b)
}
