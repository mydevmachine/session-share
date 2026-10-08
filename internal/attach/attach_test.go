package attach

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/mydevmachine/session-share/internal/app"
	"github.com/mydevmachine/session-share/internal/share"
	"github.com/mydevmachine/session-share/internal/sshkeys"
)

const bobKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJv"

type guestTerminal struct {
	ptm  *os.File
	tty  *os.File
	mu   sync.Mutex
	seen bytes.Buffer
}

func (g *guestTerminal) output() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen.String()
}

type env struct {
	app    *app.App
	socket string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir := t.TempDir()
	e := &env{socket: fmt.Sprintf("ss-att-%d-%d", os.Getpid(), time.Now().UnixNano())}
	e.app = &app.App{
		Store: share.NewStore(filepath.Join(dir, "state")),
		Keys:  sshkeys.File{Path: filepath.Join(dir, "authorized_keys")},
		Exe:   "/usr/local/bin/session-share",
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", e.socket, "kill-server").Run() })
	return e
}

func (e *env) share(t *testing.T, mode share.Mode) *share.Share {
	t.Helper()
	if out, err := exec.Command("tmux", "-L", e.socket, "new-session", "-d", "-s", "api", "-x", "80", "-y", "24", "sh -c 'echo marker-on-screen; cat'").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	sh, _, err := e.app.Start(context.Background(), app.StartOptions{
		Options:   share.Options{Session: "api", Socket: e.socket, Mode: mode, For: time.Hour},
		SSHGuests: []app.GuestRequest{{Name: "bob", Keys: []string{bobKey}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

func (e *env) attach(t *testing.T, sh *share.Share, guest string) (*guestTerminal, chan error) {
	t.Helper()
	ptm, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = pty.Setsize(ptm, &pty.Winsize{Cols: 100, Rows: 30})
	g := &guestTerminal{ptm: ptm, tty: tty}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptm.Read(buf)
			g.mu.Lock()
			g.seen.Write(buf[:n])
			g.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), e.app, Options{ShareID: sh.ID, Guest: guest, In: tty, Out: tty, Remote: "203.0.113.7", CheckEvery: 50 * time.Millisecond})
	}()
	t.Cleanup(func() {
		ptm.Close()
		tty.Close()
	})
	return g, done
}

func (e *env) pane(t *testing.T) string {
	out, _ := exec.Command("tmux", "-L", e.socket, "capture-pane", "-p", "-t", "=api:").CombinedOutput()
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

func finished(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("attach did not return")
	}
	return nil
}

func TestAReadOnlyGuestWatchesAndLeavesWithQ(t *testing.T) {
	e := newEnv(t)
	sh := e.share(t, share.ModeRead)
	g, done := e.attach(t, sh, "bob")
	eventually(t, "the screen", func() bool { return strings.Contains(g.output(), "marker-on-screen") })
	_, _ = io.WriteString(g.ptm, "typed")
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(e.pane(t), "typed") {
		t.Fatal("a read-only guest typed into the session")
	}
	_, _ = io.WriteString(g.ptm, "q")
	if err := finished(t, done); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.output(), "You left.") {
		t.Fatalf("output %q", g.output())
	}
	conns, _ := e.app.Store.Conns(sh.ID)
	if len(conns) != 0 {
		t.Fatalf("connection record left behind: %+v", conns)
	}
}

func TestAWriteGuestTypesIntoThePane(t *testing.T) {
	e := newEnv(t)
	sh := e.share(t, share.ModeWrite)
	g, _ := e.attach(t, sh, "bob")
	eventually(t, "the screen", func() bool { return strings.Contains(g.output(), "marker-on-screen") })
	_, _ = io.WriteString(g.ptm, "typed-over-ssh\r")
	eventually(t, "the input in the pane", func() bool { return strings.Contains(e.pane(t), "typed-over-ssh") })
}

func TestStopDisconnectsTheGuest(t *testing.T) {
	e := newEnv(t)
	sh := e.share(t, share.ModeWrite)
	g, done := e.attach(t, sh, "bob")
	eventually(t, "the screen", func() bool { return strings.Contains(g.output(), "marker-on-screen") })
	if _, err := e.app.Stop(sh.ID); err != nil {
		t.Fatal(err)
	}
	if err := finished(t, done); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the reason", func() bool { return strings.Contains(g.output(), "The owner stopped sharing.") })
	if err := exec.Command("tmux", "-L", e.socket, "has-session", "-t", "=api").Run(); err != nil {
		t.Fatal("the session must outlive the share")
	}
}

func TestAnUnknownGuestIsRefused(t *testing.T) {
	e := newEnv(t)
	sh := e.share(t, share.ModeRead)
	_, done := e.attach(t, sh, "mallory")
	if err := finished(t, done); err == nil || !strings.Contains(err.Error(), "not a guest") {
		t.Fatalf("got %v", err)
	}
}

func TestAGuestWithoutATerminalIsRefused(t *testing.T) {
	e := newEnv(t)
	sh := e.share(t, share.ModeRead)
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	err := Run(context.Background(), e.app, Options{ShareID: sh.ID, Guest: "bob", In: r, Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "ssh -t") {
		t.Fatalf("got %v", err)
	}
}
