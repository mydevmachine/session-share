package attach

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/mydevmachine/session-share/internal/app"
	"github.com/mydevmachine/session-share/internal/eventlog"
	"github.com/mydevmachine/session-share/internal/share"
)

const (
	ctrlC = 0x03
)

type Options struct {
	ShareID    string
	Guest      string
	In         *os.File
	Out        io.Writer
	Remote     string
	CheckEvery time.Duration
	Banner     time.Duration
}

func RemoteFromEnv() string {
	fields := strings.Fields(os.Getenv("SSH_CONNECTION"))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// Run is the forced command of a guest's SSH key. It paints the shared
// session and, for a write share, types what the guest types into the pane.
// It never hands the guest a shell or a tmux client that takes input.
func Run(ctx context.Context, a *app.App, o Options) error {
	if o.CheckEvery == 0 {
		o.CheckEvery = time.Second
	}
	sh, reason, err := a.Check(o.ShareID)
	if err != nil {
		return errors.New("this share does not exist")
	}
	if reason != "" {
		return fmt.Errorf("this share has ended (%s)", reason)
	}
	if _, ok := sh.Guest(o.Guest); !ok {
		return errors.New("you are not a guest of this share")
	}
	fd := int(o.In.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("session-share needs a terminal: connect with ssh -t")
	}
	cols, rows, err := term.GetSize(fd)
	if err != nil || cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}

	fmt.Fprintf(o.Out, "session-share: %s\r\n", banner(sh))
	if o.Banner > 0 {
		time.Sleep(o.Banner)
	}

	tm := a.Tmux(sh)
	viewer, err := tm.Watch(sh.Session, cols, rows)
	if err != nil {
		return err
	}
	defer viewer.Close()

	state, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("preparing the terminal: %w", err)
	}
	restored := false
	restore := func() {
		if !restored {
			_ = term.Restore(fd, state)
			restored = true
		}
	}
	defer restore()

	connID := newConnID()
	started := time.Now()
	log := a.Log(sh.ID)
	_ = a.Store.AddConn(share.Conn{ID: connID, ShareID: sh.ID, Kind: "ssh", Guest: o.Guest, Remote: o.Remote, PID: os.Getpid(), Started: started})
	defer func() { _ = a.Store.RemoveConn(sh.ID, connID) }()
	_ = log.Log("viewer_joined", eventlog.Fields{"conn_id": connID, "kind": "ssh", "guest": o.Guest, "remote": o.Remote, "mode": sh.Mode})
	_ = tm.Notify(sh.Session, fmt.Sprintf("session-share: %s joined over SSH (%s)", o.Guest, sh.Mode))

	var once sync.Once
	ended := make(chan struct{})
	var why string
	end := func(reason string) {
		once.Do(func() {
			why = reason
			close(ended)
		})
	}

	go func() {
		_, _ = io.Copy(o.Out, viewer)
		if tm.HasSession(sh.Session) != nil {
			end("The shared session ended.")
			return
		}
		end("The viewer stopped.")
	}()

	var typed int64
	var typedMu sync.Mutex
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := o.In.Read(buf)
			if err != nil {
				end("connection closed")
				return
			}
			data := buf[:n]
			current, reason, _ := a.Check(sh.ID)
			if reason != "" || current == nil {
				continue
			}
			if current.Mode != share.ModeWrite {
				if leaves(data) {
					end("You left.")
					return
				}
				continue
			}
			if err := tm.SendBytes(sh.Session, data); err != nil {
				_ = log.Log("tmux_error", eventlog.Fields{"conn_id": connID, "op": "paste", "error": err.Error()})
				continue
			}
			typedMu.Lock()
			typed += int64(n)
			typedMu.Unlock()
		}
	}()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(hangup)

	ticker := time.NewTicker(o.CheckEvery)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			end("stopped")
		case <-ended:
			break loop
		case <-hangup:
			end("connection closed")
		case <-winch:
			if c, r, err := term.GetSize(fd); err == nil {
				_ = viewer.Resize(c, r)
			}
		case <-ticker.C:
			current, reason, err := a.Check(sh.ID)
			switch {
			case err != nil:
				end("The owner stopped sharing.")
			case reason != "":
				end(share.EndedMessage(reason))
			case !current.ExpiresAt.Equal(sh.ExpiresAt):
				sh = current
			}
		}
	}

	_ = viewer.Close()
	restore()
	fmt.Fprintf(o.Out, "\r\nsession-share: %s\r\n", why)
	typedMu.Lock()
	n := typed
	typedMu.Unlock()
	_ = log.Log("viewer_left", eventlog.Fields{"conn_id": connID, "kind": "ssh", "guest": o.Guest, "reason": why, "seconds": int(time.Since(started).Seconds()), "input_bytes": n})
	_ = tm.Notify(sh.Session, fmt.Sprintf("session-share: %s left", o.Guest))
	return nil
}

func leaves(data []byte) bool {
	for _, b := range data {
		if b == 'q' || b == 'Q' || b == ctrlC {
			return true
		}
	}
	return false
}

func banner(sh *share.Share) string {
	until := sh.ExpiresAt.Local().Format("15:04")
	if sh.Mode == share.ModeWrite {
		return fmt.Sprintf("you can type in %q until %s. To leave, press Enter, then ~ and .", sh.Session, until)
	}
	return fmt.Sprintf("watching %q (read only) until %s. Press q to leave.", sh.Session, until)
}

func newConnID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
