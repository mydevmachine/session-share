package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mydevmachine/session-share/internal/app"
	"github.com/mydevmachine/session-share/internal/share"
	"github.com/mydevmachine/session-share/internal/sshkeys"
)

type env struct {
	app    *app.App
	srv    *Server
	http   *httptest.Server
	socket string
	mu     sync.Mutex
	clock  time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir := t.TempDir()
	e := &env{socket: fmt.Sprintf("ss-srv-%d-%d", os.Getpid(), time.Now().UnixNano()), clock: time.Now()}
	e.app = &app.App{
		Store: share.NewStore(filepath.Join(dir, "state")),
		Keys:  sshkeys.File{Path: filepath.Join(dir, "authorized_keys")},
		Exe:   "/usr/local/bin/session-share",
		Now:   e.now,
	}
	e.srv = New(e.app)
	e.srv.CheckEvery = 50 * time.Millisecond
	e.http = httptest.NewServer(e.srv.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(e.srv.CheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.srv.enforce()
			}
		}
	}()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for e.srv.openViewers() > 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		<-done
		e.http.Close()
		_ = exec.Command("tmux", "-L", e.socket, "kill-server").Run()
	})
	return e
}

func (e *env) now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.clock
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = e.clock.Add(d)
}

func (e *env) tmux(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("tmux", append([]string{"-L", e.socket}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return string(out)
}

func (e *env) share(t *testing.T, mode share.Mode) (*share.Share, string) {
	t.Helper()
	e.tmux(t, "new-session", "-d", "-s", "api", "-x", "80", "-y", "24", "sh -c 'echo marker-on-screen; cat'")
	sh, password, err := e.app.Start(context.Background(), app.StartOptions{Options: share.Options{
		Session: "api", Socket: e.socket, Mode: mode, For: time.Hour, Web: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return sh, password
}

func (e *env) client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) login(t *testing.T, c *http.Client, id, password string) *http.Response {
	t.Helper()
	resp, err := c.PostForm(e.http.URL+"/s/"+id+"/login", url.Values{"password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func (e *env) dial(t *testing.T, c *http.Client, id string) *websocket.Conn {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(e.http.URL, "http")+"/s/"+id+"/ws", &websocket.DialOptions{HTTPClient: c})
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(1 << 20)
	return ws
}

func readUntil(t *testing.T, ws *websocket.Conn, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var seen strings.Builder
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v; saw %q", want, err, seen.String())
		}
		if typ == websocket.MessageBinary {
			seen.Write(data)
		}
		if strings.Contains(seen.String(), want) {
			return
		}
	}
}

func closeStatus(t *testing.T, ws *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := ws.Read(ctx)
		if err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func send(t *testing.T, ws *websocket.Conn, msg clientMessage) {
	t.Helper()
	data, _ := json.Marshal(msg)
	if err := ws.Write(context.Background(), websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
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

func TestThePageAsksForThePasswordFirst(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeRead)
	c := e.client(t)
	resp, err := c.Get(e.http.URL + "/s/" + sh.ID + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `name="password"`) || strings.Contains(string(body), "xterm.js") {
		t.Fatalf("expected the login form only:\n%s", body)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"frame-ancestors 'none'", "style-src 'self' 'unsafe-inline'", "default-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "script-src") {
		t.Fatalf("CSP %q must leave scripts to default-src 'self'", csp)
	}
	if r := e.login(t, c, sh.ID, "wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	if r := e.login(t, c, sh.ID, password); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("right password: %d", r.StatusCode)
	}
	resp, _ = c.Get(e.http.URL + "/s/" + sh.ID + "/")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "/assets/xterm.js") || !strings.Contains(string(body), `data-mode="read"`) {
		t.Fatalf("expected the terminal page:\n%s", body)
	}
}

func TestFiveWrongPasswordsLockTheShare(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeRead)
	c := e.client(t)
	for i := 0; i < failLimit; i++ {
		e.login(t, c, sh.ID, "wrong")
	}
	if r := e.login(t, c, sh.ID, password); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the right password after a lock: %d, want 429", r.StatusCode)
	}
	e.advance(lockDuration + time.Second)
	if r := e.login(t, c, sh.ID, password); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("after the lock: %d", r.StatusCode)
	}
}

func TestAViewerWithoutTheCookieIsTurnedAway(t *testing.T) {
	e := newEnv(t)
	sh, _ := e.share(t, share.ModeRead)
	ws := e.dial(t, e.client(t), sh.ID)
	if got := closeStatus(t, ws); got != CloseUnauthorized {
		t.Fatalf("close %d, want %d", got, CloseUnauthorized)
	}
}

func TestAReadOnlyViewerSeesTheScreenButCannotType(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeRead)
	c := e.client(t)
	e.login(t, c, sh.ID, password)
	ws := e.dial(t, c, sh.ID)
	defer ws.CloseNow()
	readUntil(t, ws, "marker-on-screen")
	send(t, ws, clientMessage{Type: "input", Data: "typed-by-guest\r"})
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(e.tmux(t, "capture-pane", "-p", "-t", "=api:"), "typed-by-guest") {
		t.Fatal("a read-only viewer typed into the session")
	}
}

func TestAWriteViewerTypesIntoThePane(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeWrite)
	c := e.client(t)
	e.login(t, c, sh.ID, password)
	ws := e.dial(t, c, sh.ID)
	defer ws.CloseNow()
	readUntil(t, ws, "marker-on-screen")
	send(t, ws, clientMessage{Type: "input", Data: "\x1b[?1;2c\x1b]11;rgb:0f0f/1111/1515\x1b\\"})
	send(t, ws, clientMessage{Type: "input", Data: "typed-by-guest\r"})
	eventually(t, "the input to reach the pane", func() bool {
		return strings.Contains(e.tmux(t, "capture-pane", "-p", "-t", "=api:"), "typed-by-guest")
	})
	if pane := e.tmux(t, "capture-pane", "-p", "-t", "=api:"); strings.Contains(pane, "rgb:") || strings.Contains(pane, "1;2c") {
		t.Fatalf("terminal answers reached the pane:\n%s", pane)
	}
	readUntil(t, ws, "typed-by-guest")
}

func TestASecondViewerIsRefusedByDefault(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeRead)
	c := e.client(t)
	e.login(t, c, sh.ID, password)
	first := e.dial(t, c, sh.ID)
	defer first.CloseNow()
	readUntil(t, first, "marker-on-screen")
	second := e.dial(t, c, sh.ID)
	if got := closeStatus(t, second); got != CloseTooManyViewer {
		t.Fatalf("close %d, want %d", got, CloseTooManyViewer)
	}
}

func TestAViewerIsAlwaysTheSizeOfTheSharedWindow(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeRead)
	c := e.client(t)
	e.login(t, c, sh.ID, password)
	ws := e.dial(t, c, sh.ID)
	defer ws.CloseNow()
	readUntil(t, ws, "marker-on-screen")
	send(t, ws, clientMessage{Type: "resize", Cols: 200, Rows: 60})
	time.Sleep(300 * time.Millisecond)
	if got := strings.TrimSpace(e.tmux(t, "list-clients", "-F", "#{client_width}x#{client_height}")); got != "80x25" {
		t.Fatalf("viewer client is %s, want 80x25, the window and its status line: a bigger one gets tmux's dotted filler", got)
	}
	e.tmux(t, "resize-window", "-t", "=api:", "-x", "100", "-y", "30")
	eventually(t, "the viewer to follow the window", func() bool {
		return strings.TrimSpace(e.tmux(t, "list-clients", "-F", "#{client_width}x#{client_height}")) == "100x31"
	})
}

func TestTheViewerIsDisconnectedWhenTheShareExpires(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeRead)
	c := e.client(t)
	e.login(t, c, sh.ID, password)
	ws := e.dial(t, c, sh.ID)
	readUntil(t, ws, "marker-on-screen")
	e.advance(time.Hour)
	if got := closeStatus(t, ws); got != CloseExpired {
		t.Fatalf("close %d, want %d", got, CloseExpired)
	}
	if err := exec.Command("tmux", "-L", e.socket, "has-session", "-t", "=api").Run(); err != nil {
		t.Fatal("the session must outlive the share")
	}
	resp, _ := c.Get(e.http.URL + "/s/" + sh.ID + "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("page after expiry: %d, want 410", resp.StatusCode)
	}
}

func TestStopDisconnectsTheViewer(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeWrite)
	c := e.client(t)
	e.login(t, c, sh.ID, password)
	ws := e.dial(t, c, sh.ID)
	readUntil(t, ws, "marker-on-screen")
	if _, err := e.app.Stop(sh.ID); err != nil {
		t.Fatal(err)
	}
	if got := closeStatus(t, ws); got != CloseRevoked {
		t.Fatalf("close %d, want %d", got, CloseRevoked)
	}
}

func TestKillingTheSessionClosesTheViewer(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeRead)
	c := e.client(t)
	e.login(t, c, sh.ID, password)
	ws := e.dial(t, c, sh.ID)
	readUntil(t, ws, "marker-on-screen")
	e.tmux(t, "kill-session", "-t", "=api")
	if got := closeStatus(t, ws); got != CloseSessionEnded {
		t.Fatalf("close %d, want %d", got, CloseSessionEnded)
	}
}

func TestTheLogTellsTheStoryOfAConnection(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeWrite)
	c := e.client(t)
	e.login(t, c, sh.ID, "wrong")
	e.login(t, c, sh.ID, password)
	ws := e.dial(t, c, sh.ID)
	readUntil(t, ws, "marker-on-screen")
	send(t, ws, clientMessage{Type: "input", Data: "abc"})
	time.Sleep(200 * time.Millisecond)
	ws.Close(websocket.StatusNormalClosure, "")
	var events []string
	eventually(t, "viewer_left in the log", func() bool {
		data, _ := os.ReadFile(e.app.Store.LogPath(sh.ID))
		events = nil
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) == nil {
				events = append(events, fmt.Sprint(ev["event"]))
				if ev["event"] == "viewer_left" && ev["input_bytes"] != float64(3) {
					t.Fatalf("input_bytes %v, want 3", ev["input_bytes"])
				}
				if strings.Contains(line, password) {
					t.Fatal("the password reached the log")
				}
			}
		}
		return len(events) > 0 && events[len(events)-1] == "viewer_left"
	})
	want := []string{"share_started", "auth_failed", "auth_ok", "viewer_joined", "viewer_left"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v", events, want)
	}
}

func TestServeStopsWhenNothingIsShared(t *testing.T) {
	e := newEnv(t)
	e.srv.IdleShutdown = 0
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.srv.Serve(context.Background(), ln) }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve kept running with nothing to share")
	}
}

func readText(t *testing.T, ws *websocket.Conn, kind string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", kind, err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) == nil && msg["type"] == kind {
			return msg
		}
	}
}

func TestChatReachesTheGuestAndTheOwnersRepliesComeBack(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeRead)
	if _, err := e.app.Chat(sh.ID).Append("alice", "owner", "before you came", time.Now()); err != nil {
		t.Fatal(err)
	}
	c := e.client(t)
	e.login(t, c, sh.ID, password)
	ws := e.dial(t, c, sh.ID)
	defer ws.CloseNow()
	history := readText(t, ws, "chat-history")
	if msgs := history["messages"].([]any); len(msgs) != 1 || msgs[0].(map[string]any)["text"] != "before you came" {
		t.Fatalf("history %v", history)
	}

	send(t, ws, clientMessage{Type: "chat", Name: "bob", Data: "hi \x1b[31mthere‮"})
	got := readText(t, ws, "chat")["message"].(map[string]any)
	if got["from"] != "bob" || got["role"] != "guest" || got["text"] != "hi [31mthere" {
		t.Fatalf("guest message %v", got)
	}
	if _, err := e.app.Chat(sh.ID).Append("alice", "owner", "welcome", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, ws, "chat")["message"].(map[string]any); got["text"] != "welcome" || got["role"] != "owner" {
		t.Fatalf("owner message %v", got)
	}
	if strings.Contains(e.tmux(t, "capture-pane", "-p", "-t", "=api:"), "hi") {
		t.Fatal("a chat message reached the shared pane")
	}
}

func TestChatRefusesAFlood(t *testing.T) {
	e := newEnv(t)
	sh, password := e.share(t, share.ModeRead)
	c := e.client(t)
	e.login(t, c, sh.ID, password)
	ws := e.dial(t, c, sh.ID)
	defer ws.CloseNow()
	readText(t, ws, "chat-history")
	for i := 0; i < 6; i++ {
		send(t, ws, clientMessage{Type: "chat", Name: "bob", Data: fmt.Sprintf("message %d", i)})
	}
	refused := readText(t, ws, "chat-refused")
	if !strings.Contains(fmt.Sprint(refused["reason"]), "wait") {
		t.Fatalf("refusal %v", refused)
	}
	msgs, _, _ := e.app.Chat(sh.ID).Since(0)
	if len(msgs) != 5 {
		t.Fatalf("stored %d messages, want 5", len(msgs))
	}
}
