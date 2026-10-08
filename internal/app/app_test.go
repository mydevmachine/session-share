package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mydevmachine/session-share/internal/share"
	"github.com/mydevmachine/session-share/internal/sshkeys"
)

const bobKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJv"

type fixture struct {
	app    *App
	socket string
	clock  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir := t.TempDir()
	f := &fixture{socket: fmt.Sprintf("ss-app-%d-%d", os.Getpid(), time.Now().UnixNano()), clock: time.Now()}
	f.app = &App{
		Store: share.NewStore(filepath.Join(dir, "state")),
		Keys:  sshkeys.File{Path: filepath.Join(dir, "authorized_keys")},
		Exe:   "/usr/local/bin/session-share",
		Fetch: func(ctx context.Context, user string) ([]string, error) { return []string{bobKey}, nil },
		Now:   func() time.Time { return f.clock },
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", f.socket, "kill-server").Run() })
	return f
}

func (f *fixture) session(t *testing.T, name string) {
	t.Helper()
	if out, err := exec.Command("tmux", "-L", f.socket, "new-session", "-d", "-s", name, "cat").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

func (f *fixture) start(t *testing.T, o StartOptions) *share.Share {
	t.Helper()
	o.Socket = f.socket
	s, _, err := f.app.Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fixture) keys(t *testing.T) string {
	data, _ := os.ReadFile(f.app.Keys.Path)
	return string(data)
}

func TestStartRefusesASessionThatDoesNotExist(t *testing.T) {
	f := newFixture(t)
	f.session(t, "api-old")
	_, _, err := f.app.Start(context.Background(), StartOptions{Options: share.Options{Session: "api", Socket: f.socket, Mode: share.ModeRead, For: time.Hour, Web: true}})
	if err == nil || !strings.Contains(err.Error(), "no such tmux session") {
		t.Fatalf("got %v", err)
	}
}

func TestStartWithAGuestWritesARestrictedKeyAndStopRemovesIt(t *testing.T) {
	f := newFixture(t)
	f.session(t, "api")
	s := f.start(t, StartOptions{
		Options:   share.Options{Session: "api", Mode: share.ModeRead, For: time.Hour},
		SSHGuests: []GuestRequest{{Name: "bob", GitHub: "bob"}},
	})
	keys := f.keys(t)
	if !strings.Contains(keys, `command="/usr/local/bin/session-share attach `+s.ID+` --guest bob"`) || !strings.HasPrefix(keys, "restrict,pty,expiry-time=") {
		t.Fatalf("authorized_keys:\n%s", keys)
	}
	if _, err := f.app.Stop(s.ID); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.keys(t), s.ID) {
		t.Fatalf("stop left the key behind:\n%s", f.keys(t))
	}
	_, reason, _ := f.app.Check(s.ID)
	if reason != share.ReasonRevoked {
		t.Fatalf("reason %q, want revoked", reason)
	}
}

func TestReapEndsExpiredSharesAndSharesWhoseSessionIsGone(t *testing.T) {
	f := newFixture(t)
	f.session(t, "api")
	f.session(t, "web")
	expiring := f.start(t, StartOptions{Options: share.Options{Session: "api", Mode: share.ModeRead, For: time.Hour}, SSHGuests: []GuestRequest{{Name: "bob", Keys: []string{bobKey}}}})
	orphan := f.start(t, StartOptions{Options: share.Options{Session: "web", Mode: share.ModeRead, For: 3 * time.Hour, Web: true}})
	if out, err := exec.Command("tmux", "-L", f.socket, "kill-session", "-t", "=web").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	f.clock = f.clock.Add(2 * time.Hour)
	ended, err := f.app.Reap()
	if err != nil {
		t.Fatal(err)
	}
	if len(ended) != 2 {
		t.Fatalf("ended %d shares, want 2", len(ended))
	}
	if _, reason, _ := f.app.Check(expiring.ID); reason != share.ReasonExpired {
		t.Fatalf("expiring: %q", reason)
	}
	if _, reason, _ := f.app.Check(orphan.ID); reason != share.ReasonSessionEnded {
		t.Fatalf("orphan: %q", reason)
	}
	if strings.Contains(f.keys(t), expiring.ID) {
		t.Fatal("reap left an expired key behind")
	}
	f.clock = f.clock.Add(HistoryKept + time.Hour)
	if _, err := f.app.Reap(); err != nil {
		t.Fatal(err)
	}
	if all, _ := f.app.Store.List(); len(all) != 0 {
		t.Fatalf("old history kept: %d", len(all))
	}
}

func TestExtendMovesTheDeadlineAndTheKeyExpiry(t *testing.T) {
	f := newFixture(t)
	f.session(t, "api")
	s := f.start(t, StartOptions{Options: share.Options{Session: "api", Mode: share.ModeRead, For: time.Hour}, SSHGuests: []GuestRequest{{Name: "bob", Keys: []string{bobKey}}}})
	extended, err := f.app.Extend(s.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !extended.ExpiresAt.Equal(s.ExpiresAt.Add(30 * time.Minute)) {
		t.Fatalf("expires %v", extended.ExpiresAt)
	}
	want := `expiry-time="` + extended.ExpiresAt.Local().Format("20060102150405") + `"`
	if !strings.Contains(f.keys(t), want) || strings.Count(f.keys(t), "session-share:"+s.ID) != 1 {
		t.Fatalf("authorized_keys:\n%s", f.keys(t))
	}
	if _, err := f.app.Extend(s.ID, 24*time.Hour); err == nil {
		t.Fatal("extending past 24h from now must be refused")
	}
	if _, err := f.app.Stop(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Extend(s.ID, time.Minute); err == nil {
		t.Fatal("an ended share cannot be extended")
	}
}

func TestWriteShareKeepsPanesWhenTheProgramExits(t *testing.T) {
	f := newFixture(t)
	f.session(t, "api")
	f.start(t, StartOptions{Options: share.Options{Session: "api", Mode: share.ModeWrite, For: time.Hour, Web: true}})
	out, err := exec.Command("tmux", "-L", f.socket, "show-options", "-w", "-t", "=api:", "-v", "remain-on-exit").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "on" {
		t.Fatalf("remain-on-exit = %q, %v", out, err)
	}
}
