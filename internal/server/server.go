package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/mydevmachine/session-share/internal/app"
	"github.com/mydevmachine/session-share/internal/chat"
	"github.com/mydevmachine/session-share/internal/eventlog"
	"github.com/mydevmachine/session-share/internal/share"
	"github.com/mydevmachine/session-share/internal/termio"
	"github.com/mydevmachine/session-share/web"
)

const (
	CloseExpired       websocket.StatusCode = 4001
	CloseRevoked       websocket.StatusCode = 4002
	CloseSessionEnded  websocket.StatusCode = 4003
	CloseUnauthorized  websocket.StatusCode = 4004
	CloseTooManyViewer websocket.StatusCode = 4005
)

const (
	maxMessage   = 64 * 1024
	failLimit    = 5
	failWindow   = 10 * time.Minute
	lockDuration = 10 * time.Minute
)

type Server struct {
	App          *app.App
	PingEvery    time.Duration
	PingTimeout  time.Duration
	CheckEvery   time.Duration
	IdleShutdown time.Duration

	mu       sync.Mutex
	conns    map[string]map[string]*viewerConn
	failures map[string]*failures
	lastBusy time.Time
}

type failures struct {
	at          []time.Time
	lockedUntil time.Time
}

func New(a *app.App) *Server {
	return &Server{
		App:          a,
		PingEvery:    15 * time.Second,
		PingTimeout:  10 * time.Second,
		CheckEvery:   time.Second,
		IdleShutdown: time.Minute,
		conns:        map[string]map[string]*viewerConn{},
		failures:     map[string]*failures{},
	}
}

func (s *Server) now() time.Time {
	if s.App.Now != nil {
		return s.App.Now()
	}
	return time.Now()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "ok %s\n", Owner())
	})
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(web.Static())))
	mux.HandleFunc("GET /s/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/s/"+r.PathValue("id")+"/", http.StatusFound)
	})
	mux.HandleFunc("GET /s/{id}/{$}", s.page)
	mux.HandleFunc("POST /s/{id}/login", s.login)
	mux.HandleFunc("GET /s/{id}/ws", s.socket)
	return securityHeaders(mux)
}

func Owner() string {
	return fmt.Sprintf("uid=%d", os.Getuid())
}

func HealthOwner(body string) string {
	fields := strings.Fields(body)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// xterm.js paints colours from <style> elements it writes at run time;
		// without 'unsafe-inline' for styles every program shows in one colour.
		// Scripts stay limited to this server's own files.
		h.Set("Content-Security-Policy", fmt.Sprintf("default-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws://%[1]s wss://%[1]s; frame-ancestors 'none'; base-uri 'none'; form-action 'self'", r.Host))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type pageData struct {
	Title      string
	ID         string
	Mode       share.Mode
	Expires    string
	ExpiresISO string
	Error      string
	Message    string
}

func title(sh *share.Share) string {
	if sh.Name != "" {
		return sh.Name
	}
	return sh.Session
}

func render(w http.ResponseWriter, status int, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = web.Templates.ExecuteTemplate(w, name, data)
}

func (s *Server) load(w http.ResponseWriter, r *http.Request) (*share.Share, bool) {
	sh, reason, err := s.App.Check(r.PathValue("id"))
	if err != nil {
		render(w, http.StatusNotFound, "ended.html", pageData{Title: "Not found", Message: "There is no share at this address."})
		return nil, false
	}
	if !sh.Web {
		render(w, http.StatusNotFound, "ended.html", pageData{Title: "Not found", Message: "There is no share at this address."})
		return nil, false
	}
	if reason != "" {
		render(w, http.StatusGone, "ended.html", pageData{Title: title(sh), Message: share.EndedMessage(reason)})
		return nil, false
	}
	return sh, true
}

func (s *Server) data(sh *share.Share) pageData {
	return pageData{
		Title:      title(sh),
		ID:         sh.ID,
		Mode:       sh.Mode,
		Expires:    sh.ExpiresAt.Local().Format("15:04"),
		ExpiresISO: sh.ExpiresAt.Format(time.RFC3339),
	}
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.load(w, r)
	if !ok {
		return
	}
	if !authorized(r, sh) {
		render(w, http.StatusOK, "login.html", s.data(sh))
		return
	}
	render(w, http.StatusOK, "terminal.html", s.data(sh))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.load(w, r)
	if !ok {
		return
	}
	log := s.App.Log(sh.ID)
	remote := remoteAddr(r)
	if until, locked := s.locked(sh.ID); locked {
		d := s.data(sh)
		d.Error = "Too many wrong passwords. Try again after " + until.Local().Format("15:04") + "."
		render(w, http.StatusTooManyRequests, "login.html", d)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	password := strings.TrimSpace(r.PostFormValue("password"))
	if !sh.CheckPassword(password) {
		locked := s.fail(sh.ID)
		_ = log.Log("auth_failed", eventlog.Fields{"remote": remote, "locked": locked})
		d := s.data(sh)
		d.Error = "Wrong password."
		if locked {
			d.Error = "Too many wrong passwords. Try again in 10 minutes."
		}
		render(w, http.StatusUnauthorized, "login.html", d)
		return
	}
	_ = log.Log("auth_ok", eventlog.Fields{"remote": remote, "user_agent": r.UserAgent()})
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName(sh.ID),
		Value:    cookieValue(sh),
		Path:     "/s/" + sh.ID + "/",
		Expires:  sh.ExpiresAt.Add(share.MaxDuration),
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/s/"+sh.ID+"/", http.StatusSeeOther)
}

func (s *Server) locked(id string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.failures[id]
	if f == nil {
		return time.Time{}, false
	}
	return f.lockedUntil, s.now().Before(f.lockedUntil)
}

func (s *Server) fail(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	f := s.failures[id]
	if f == nil {
		f = &failures{}
		s.failures[id] = f
	}
	var recent []time.Time
	for _, t := range f.at {
		if now.Sub(t) < failWindow {
			recent = append(recent, t)
		}
	}
	f.at = append(recent, now)
	if len(f.at) >= failLimit {
		f.lockedUntil = now.Add(lockDuration)
		f.at = nil
		return true
	}
	return false
}

func cookieName(id string) string {
	return "session_share_" + id
}

// The cookie is a MAC of the share id under a key only this share has, so it
// stops working the moment the share ends and cannot be forged for another.
func cookieValue(sh *share.Share) string {
	key, _ := hex.DecodeString(sh.CookieKey)
	m := hmac.New(sha256.New, key)
	m.Write([]byte("viewer:" + sh.ID))
	return hex.EncodeToString(m.Sum(nil))
}

func authorized(r *http.Request, sh *share.Share) bool {
	c, err := r.Cookie(cookieName(sh.ID))
	if err != nil || sh.CookieKey == "" {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(cookieValue(sh)))
}

// remoteAddr trusts X-Forwarded-For only from a proxy on this machine: the
// server listens on loopback, and a proxy in front of it adds the header.
func remoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	return host
}

func newConnID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func closeCode(reason string) websocket.StatusCode {
	switch reason {
	case share.ReasonExpired:
		return CloseExpired
	case share.ReasonRevoked:
		return CloseRevoked
	case share.ReasonSessionEnded:
		return CloseSessionEnded
	}
	return websocket.StatusNormalClosure
}

type viewerConn struct {
	id      string
	shareID string
	ws      *websocket.Conn
	once    sync.Once
	ended   chan struct{}
	code    websocket.StatusCode
	reason  string
}

func (v *viewerConn) end(code websocket.StatusCode, reason string) {
	v.once.Do(func() {
		v.code = code
		v.reason = reason
		close(v.ended)
	})
}

func (s *Server) reserve(v *viewerConn, max int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns[v.shareID]) >= max {
		return false
	}
	if s.conns[v.shareID] == nil {
		s.conns[v.shareID] = map[string]*viewerConn{}
	}
	s.conns[v.shareID][v.id] = v
	s.lastBusy = s.now()
	return true
}

func viewerLimitMessage(max int) string {
	if max == 1 {
		return "Someone is already watching, and this share allows one person at a time."
	}
	return fmt.Sprintf("This share allows %d people at a time, and %d are here already.", max, max)
}

func (s *Server) openViewers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, byID := range s.conns {
		n += len(byID)
	}
	return n
}

func (s *Server) remove(v *viewerConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns[v.shareID], v.id)
	if len(s.conns[v.shareID]) == 0 {
		delete(s.conns, v.shareID)
	}
	s.lastBusy = s.now()
}

type clientMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Name string `json:"name"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

const chatHistory = 100

func (s *Server) socket(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(maxMessage)
	id := r.PathValue("id")
	sh, reason, err := s.App.Check(id)
	if err != nil || !sh.Web {
		_ = ws.Close(CloseUnauthorized, "There is no share at this address.")
		return
	}
	if reason != "" {
		_ = ws.Close(closeCode(reason), share.EndedMessage(reason))
		return
	}
	if !authorized(r, sh) {
		_ = ws.Close(CloseUnauthorized, "You are not signed in to this share. Reload the page.")
		return
	}
	v := &viewerConn{id: newConnID(), shareID: sh.ID, ws: ws, ended: make(chan struct{})}
	if !s.reserve(v, sh.MaxViewers) {
		_ = s.App.Log(sh.ID).Log("viewer_refused", eventlog.Fields{"remote": remoteAddr(r), "reason": "viewer limit", "max_viewers": sh.MaxViewers})
		_ = ws.Close(CloseTooManyViewer, viewerLimitMessage(sh.MaxViewers))
		return
	}
	s.serveViewer(r, v, sh)
}

func (s *Server) serveViewer(r *http.Request, v *viewerConn, sh *share.Share) {
	ws := v.ws
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	log := s.App.Log(sh.ID)
	tm := s.App.Tmux(sh)
	remote := remoteAddr(r)
	started := s.now()
	record := share.Conn{ID: v.id, ShareID: sh.ID, Kind: "web", Guest: sh.Name, Remote: remote, PID: os.Getpid(), Started: started}
	_ = s.App.Store.AddConn(record)
	defer func() {
		_ = s.App.Store.RemoveConn(sh.ID, v.id)
		s.remove(v)
	}()

	cols, rows, err := tm.ClientSize(sh.Session)
	if err != nil {
		cols, rows = 80, 24
	}
	viewer, err := tm.Watch(sh.Session, cols, rows)
	if err != nil {
		_ = log.Log("tmux_error", eventlog.Fields{"conn_id": v.id, "op": "attach", "error": err.Error()})
		_ = ws.Close(websocket.StatusInternalError, "Could not attach to the session.")
		return
	}
	defer viewer.Close()

	_ = log.Log("viewer_joined", eventlog.Fields{"conn_id": v.id, "kind": "web", "remote": remote, "user_agent": r.UserAgent(), "mode": sh.Mode})
	_ = tm.Notify(sh.Session, fmt.Sprintf("session-share: a web viewer joined (%s, %s)", sh.Mode, remote))

	hello, _ := json.Marshal(map[string]any{
		"type": "hello", "conn_id": v.id, "mode": sh.Mode, "expires_at": sh.ExpiresAt, "session": sh.Session,
		"cols": cols, "rows": rows,
	})
	if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
		v.end(websocket.StatusGoingAway, "client gone")
	}

	chatLog := s.App.Chat(sh.ID)
	history, chatOffset, _ := chatLog.Recent(chatHistory)
	if history == nil {
		history = []chat.Message{}
	}
	if past, err := json.Marshal(map[string]any{"type": "chat-history", "messages": history}); err == nil {
		_ = ws.Write(ctx, websocket.MessageText, past)
	}
	var limiter chat.Limiter

	go func() {
		offset := chatOffset
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-v.ended:
				return
			case <-t.C:
				msgs, next, err := chatLog.Since(offset)
				if err != nil {
					continue
				}
				offset = next
				for _, m := range msgs {
					data, _ := json.Marshal(map[string]any{"type": "chat", "message": m})
					if ws.Write(ctx, websocket.MessageText, data) != nil {
						v.end(websocket.StatusGoingAway, "client gone")
						return
					}
				}
			}
		}
	}()

	go func() {
		t := time.NewTicker(s.CheckEvery)
		defer t.Stop()
		for {
			select {
			case <-v.ended:
				return
			case <-t.C:
				c, r, err := tm.ClientSize(sh.Session)
				if err != nil || (c == cols && r == rows) {
					continue
				}
				cols, rows = c, r
				_ = viewer.Resize(cols, rows)
				size, _ := json.Marshal(map[string]any{"type": "size", "cols": cols, "rows": rows})
				if ws.Write(ctx, websocket.MessageText, size) != nil {
					v.end(websocket.StatusGoingAway, "client gone")
					return
				}
			}
		}
	}()

	var inputBytes int64
	var inputMu sync.Mutex

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := viewer.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					v.end(websocket.StatusGoingAway, "client gone")
					return
				}
			}
			if err != nil {
				if tm.HasSession(sh.Session) != nil {
					v.end(CloseSessionEnded, share.EndedMessage(share.ReasonSessionEnded))
				} else {
					v.end(websocket.StatusInternalError, "The viewer stopped.")
				}
				return
			}
		}
	}()

	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				v.end(websocket.StatusGoingAway, "client gone")
				return
			}
			var msg clientMessage
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			switch msg.Type {
			case "chat":
				if !limiter.Allow(s.now()) {
					refused, _ := json.Marshal(map[string]any{"type": "chat-refused", "reason": "Too fast: wait a few seconds."})
					_ = ws.Write(ctx, websocket.MessageText, refused)
					continue
				}
				m, err := chatLog.Append(msg.Name, chat.RoleGuest, msg.Data, s.now())
				if err != nil {
					continue
				}
				_ = log.Log("chat_message", eventlog.Fields{"conn_id": v.id, "from": m.From, "chars": len([]rune(m.Text))})
				_ = tm.Notify(sh.Session, chat.TmuxText(fmt.Sprintf("session-share chat — %s: %s", m.From, m.Text)))
			case "input":
				typed, answered := termio.Split([]byte(msg.Data))
				if len(answered) > 0 {
					_, _ = viewer.Write(answered)
				}
				current, reason, _ := s.App.Check(sh.ID)
				if len(typed) == 0 || reason != "" || current == nil || current.Mode != share.ModeWrite {
					continue
				}
				if err := tm.SendBytes(sh.Session, typed); err != nil {
					_ = log.Log("tmux_error", eventlog.Fields{"conn_id": v.id, "op": "paste", "error": err.Error()})
					continue
				}
				inputMu.Lock()
				inputBytes += int64(len(typed))
				inputMu.Unlock()
			}
		}
	}()

	go func() {
		t := time.NewTicker(s.PingEvery)
		defer t.Stop()
		for {
			select {
			case <-v.ended:
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, s.PingTimeout)
				err := ws.Ping(pctx)
				pcancel()
				if err != nil {
					_ = log.Log("ping_timeout", eventlog.Fields{"conn_id": v.id, "error": err.Error()})
					v.end(websocket.StatusGoingAway, "ping timeout")
					return
				}
			}
		}
	}()

	<-v.ended
	_ = ws.Close(v.code, v.reason)
	inputMu.Lock()
	typed := inputBytes
	inputMu.Unlock()
	_ = log.Log("viewer_left", eventlog.Fields{
		"conn_id": v.id, "close_code": int(v.code), "reason": v.reason,
		"seconds": int(s.now().Sub(started).Seconds()), "input_bytes": typed,
	})
	_ = tm.Notify(sh.Session, "session-share: a web viewer left")
}

// enforce closes every connection whose share has ended. It reloads shares
// from disk, so a stop from another process takes effect within CheckEvery.
func (s *Server) enforce() {
	if _, err := s.App.Reap(); err != nil {
		_ = s.App.ServerLog().Log("reap_failed", eventlog.Fields{"error": err.Error()})
	}
	s.mu.Lock()
	var open []*viewerConn
	for _, byID := range s.conns {
		for _, v := range byID {
			open = append(open, v)
		}
	}
	s.mu.Unlock()
	for _, v := range open {
		_, reason, err := s.App.Check(v.shareID)
		if err != nil {
			v.end(CloseRevoked, share.EndedMessage(share.ReasonRevoked))
			continue
		}
		if reason != "" {
			v.end(closeCode(reason), share.EndedMessage(reason))
		}
	}
}

func (s *Server) idle() bool {
	s.mu.Lock()
	busy := len(s.conns) > 0
	since := s.now().Sub(s.lastBusy)
	s.mu.Unlock()
	if busy || since < s.IdleShutdown {
		return false
	}
	all, err := s.App.Store.List()
	if err != nil {
		return false
	}
	for _, sh := range all {
		if sh.Web && sh.Active(s.now()) {
			return false
		}
	}
	return true
}

// Serve runs until ctx ends, or until no web share is active and nobody has
// been connected for IdleShutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.mu.Lock()
	s.lastBusy = s.now()
	s.mu.Unlock()
	log := s.App.ServerLog()
	_ = log.Log("server_started", eventlog.Fields{"listen": ln.Addr().String(), "pid": os.Getpid()})
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	t := time.NewTicker(s.CheckEvery)
	defer t.Stop()
	reason := "stopped"
	for {
		select {
		case <-ctx.Done():
		case err := <-errc:
			if !errors.Is(err, http.ErrServerClosed) {
				_ = log.Log("server_failed", eventlog.Fields{"error": err.Error()})
				return fmt.Errorf("serving: %w", err)
			}
			return nil
		case <-t.C:
			s.enforce()
			if !s.idle() {
				continue
			}
			reason = "idle"
		}
		break
	}
	s.mu.Lock()
	for _, byID := range s.conns {
		for _, v := range byID {
			v.end(websocket.StatusGoingAway, "The server is shutting down.")
		}
	}
	s.mu.Unlock()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	_ = log.Log("server_stopped", eventlog.Fields{"reason": reason})
	return nil
}
