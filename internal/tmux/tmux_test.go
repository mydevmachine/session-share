package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func testServer(t *testing.T) Tmux {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tm := New("", fmt.Sprintf("ss-test-%d-%d", os.Getpid(), time.Now().UnixNano()))
	t.Cleanup(func() { _ = tm.command("kill-server").Run() })
	return tm
}

func newSession(t *testing.T, tm Tmux, name, program string) {
	t.Helper()
	if out, err := tm.command("new-session", "-d", "-s", name, "-x", "80", "-y", "24", program).CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
}

func capture(t *testing.T, tm Tmux, name string) string {
	t.Helper()
	out, err := tm.run("capture-pane", "-p", "-t", pane(name))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHasSessionMatchesTheExactName(t *testing.T) {
	tm := testServer(t)
	newSession(t, tm, "api-old", "cat")
	if err := tm.HasSession("api"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("\"api\" must not match \"api-old\": got %v", err)
	}
	if err := tm.HasSession("api-old"); err != nil {
		t.Fatal(err)
	}
}

func TestSendBytesReachesTheProgramInThePane(t *testing.T) {
	tm := testServer(t)
	newSession(t, tm, "api", "cat")
	v, err := tm.Watch("api", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	go func() { _, _ = io.Copy(io.Discard, v) }()
	eventually(t, "the read-only viewer to attach", func() bool {
		out, _ := tm.run("list-clients")
		return strings.TrimSpace(string(out)) != ""
	})
	if err := tm.SendBytes("api", []byte("hello\r\x1b[D")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "cat to echo hello", func() bool { return strings.Count(capture(t, tm, "api"), "hello") >= 2 })
}

func TestSendBytesDoesNotRunTmuxCommands(t *testing.T) {
	tm := testServer(t)
	newSession(t, tm, "api", "cat")
	prefixKillWindow, prefixKillServer := "\x02&y", "\x02:kill-server\r"
	if err := tm.SendBytes("api", []byte(prefixKillWindow+prefixKillServer)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := tm.HasSession("api"); err != nil {
		t.Fatalf("the prefix key reached tmux: %v", err)
	}
}

func TestWatchPaintsTheScreenAndEndsWithTheSession(t *testing.T) {
	tm := testServer(t)
	newSession(t, tm, "api", "sh -c 'echo marker-on-screen; cat'")
	owner, err := pty.StartWithSize(tm.command("attach-session", "-t", exact("api")), &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	go func() { _, _ = io.Copy(io.Discard, owner) }()
	eventually(t, "the owner to attach", func() bool {
		out, _ := tm.run("list-clients")
		return strings.TrimSpace(string(out)) != ""
	})
	v, err := tm.Watch("api", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var seen bytes.Buffer
	got := make(chan struct{})
	painted := got
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := v.Read(buf)
			seen.Write(buf[:n])
			if got != nil && strings.Contains(seen.String(), "marker-on-screen") {
				close(got)
				got = nil
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-painted:
	case <-time.After(5 * time.Second):
		t.Fatalf("the viewer never painted the screen; saw %q", seen.String())
	}
	if out, _ := tm.run("list-clients", "-F", "#{client_readonly}"); !strings.Contains(string(out), "1") {
		t.Fatalf("viewer client is not read-only: %q", out)
	}
	if err := v.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if out, _ := tm.run("display-message", "-p", "-t", pane("api"), "#{window_width}"); strings.TrimSpace(string(out)) != "80" {
		t.Fatalf("the viewer resized the owner's window to %q", out)
	}
	if _, err := tm.run("kill-session", "-t", exact("api")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-v.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the viewer did not end with the session")
	}
}

func TestKeepPanesSurvivesTheProgramExiting(t *testing.T) {
	tm := testServer(t)
	newSession(t, tm, "api", "cat")
	v, err := tm.Watch("api", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	go func() { _, _ = io.Copy(io.Discard, v) }()
	eventually(t, "the read-only viewer to attach", func() bool {
		out, _ := tm.run("list-clients")
		return strings.TrimSpace(string(out)) != ""
	})
	if err := tm.Notify("api", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := tm.KeepPanes("api"); err != nil {
		t.Fatal(err)
	}
	if err := tm.SendBytes("api", []byte{0x04}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := tm.HasSession("api"); err != nil {
		t.Fatalf("the session ended with its program: %v", err)
	}
}
