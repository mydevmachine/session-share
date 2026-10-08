package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/creack/pty"
)

var ErrNoSession = errors.New("no such tmux session")

var bufferSeq atomic.Uint64

type Tmux struct {
	Bin    string
	Socket string
}

func New(bin, socket string) Tmux {
	if bin == "" {
		bin = "tmux"
	}
	return Tmux{Bin: bin, Socket: socket}
}

func (t Tmux) command(args ...string) *exec.Cmd {
	if t.Socket != "" {
		args = append([]string{"-L", t.Socket}, args...)
	}
	cmd := exec.Command(t.Bin, args...)
	cmd.Env = cleanEnv()
	return cmd
}

// cleanEnv drops TMUX: tmux refuses to attach from inside another tmux client,
// and the server may have been started from one.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") || strings.HasPrefix(kv, "TERM=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TERM=xterm-256color")
}

func (t Tmux) run(args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := t.command(args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("tmux %s: %s", args[0], msg)
	}
	return out, nil
}

// exact makes tmux match the session name exactly: a bare "api" also matches
// "api-old" when no "api" exists.
func exact(session string) string {
	return "=" + session
}

func pane(session string) string {
	return exact(session) + ":"
}

func (t Tmux) HasSession(session string) error {
	if _, err := t.run("has-session", "-t", exact(session)); err != nil {
		return fmt.Errorf("%w %q", ErrNoSession, session)
	}
	return nil
}

func (t Tmux) Version() (string, error) {
	out, err := t.run("-V")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// SendBytes types raw bytes into the session's active pane. It loads them
// into a buffer and pastes it, so the bytes reach the program in the pane and
// never the tmux key tables: a guest cannot run a tmux command. send-keys is
// not an option: tmux refuses it while a read-only client is the current one.
func (t Tmux) SendBytes(session string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	buffer := fmt.Sprintf("session-share-%d-%d", os.Getpid(), bufferSeq.Add(1))
	load := t.command("load-buffer", "-b", buffer, "-")
	load.Stdin = bytes.NewReader(data)
	if out, err := load.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux load-buffer: %s", strings.TrimSpace(string(out)))
	}
	_, err := t.run("paste-buffer", "-d", "-r", "-b", buffer, "-t", pane(session))
	return err
}

func (t Tmux) Notify(session, message string) error {
	_, err := t.run("display-message", "-t", pane(session), message)
	return err
}

// KeepPanes turns on remain-on-exit in every window of the session, so a guest
// who types `exit` leaves a dead pane behind instead of ending the session.
func (t Tmux) KeepPanes(session string) error {
	out, err := t.run("list-windows", "-t", exact(session), "-F", "#{window_id}")
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(string(out)) {
		if _, err := t.run("set-option", "-w", "-t", id, "remain-on-exit", "on"); err != nil {
			return err
		}
	}
	return nil
}

type Viewer struct {
	cmd  *exec.Cmd
	pty  *os.File
	once sync.Once
	done chan struct{}
	err  error
}

// Watch attaches a read-only tmux client inside a new pty. The client never
// receives input from the guest; it only paints the screen.
func (t Tmux) Watch(session string, cols, rows int) (*Viewer, error) {
	cmd := t.command("attach-session", "-f", "read-only,ignore-size", "-t", exact(session))
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("attaching to %q: %w", session, err)
	}
	v := &Viewer{cmd: cmd, pty: f, done: make(chan struct{})}
	go func() {
		v.err = cmd.Wait()
		close(v.done)
	}()
	return v, nil
}

func (v *Viewer) Read(p []byte) (int, error) {
	n, err := v.pty.Read(p)
	if err != nil {
		return n, io.EOF
	}
	return n, nil
}

func (v *Viewer) Resize(cols, rows int) error {
	return pty.Setsize(v.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (v *Viewer) Done() <-chan struct{} {
	return v.done
}

func (v *Viewer) Close() error {
	v.once.Do(func() {
		if v.cmd.Process != nil {
			_ = v.cmd.Process.Kill()
		}
		<-v.done
		_ = v.pty.Close()
	})
	return nil
}
