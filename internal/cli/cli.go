package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mydevmachine/session-share/internal/app"
	"github.com/mydevmachine/session-share/internal/attach"
	"github.com/mydevmachine/session-share/internal/chat"
	"github.com/mydevmachine/session-share/internal/expose"
	"github.com/mydevmachine/session-share/internal/server"
	"github.com/mydevmachine/session-share/internal/share"
	"github.com/mydevmachine/session-share/internal/sshkeys"
)

const (
	DefaultListen = "127.0.0.1:7690"
	JSONVersion   = 1
)

type CLI struct {
	App     *app.App
	Expose  *expose.Manager
	Listen  string
	Out     io.Writer
	Err     io.Writer
	Version string
}

func New(version string, stdout, stderr io.Writer) (*CLI, error) {
	dir, err := app.StateDir()
	if err != nil {
		return nil, err
	}
	keys, err := sshkeys.DefaultFile()
	if err != nil {
		return nil, err
	}
	listen := listenAddress(dir)
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return &CLI{
		App: &app.App{
			Store:   share.NewStore(dir),
			Keys:    keys,
			TmuxBin: findTmux(),
			Exe:     exe,
			Fetch:   sshkeys.GitHubKeys(&http.Client{Timeout: 15 * time.Second}),
		},
		Expose: &expose.Manager{
			Path:     filepath.Join(dir, "expose.json"),
			Listen:   listen,
			Run:      expose.ExecRunner,
			LookPath: exec.LookPath,
			Stat:     os.Stat,
		},
		Listen:  listen,
		Out:     stdout,
		Err:     stderr,
		Version: version,
	}, nil
}

// listenAddress lets each account on a machine run its own server: the
// address comes from the environment, then from <state>/listen.
func listenAddress(stateDir string) string {
	if v := os.Getenv("SESSION_SHARE_LISTEN"); v != "" {
		return v
	}
	if data, err := os.ReadFile(filepath.Join(stateDir, "listen")); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			return v
		}
	}
	return DefaultListen
}

// findTmux resolves tmux to a full path. An SSH forced command runs with a
// short PATH that leaves out Homebrew and MacPorts.
func findTmux() string {
	if bin := os.Getenv("SESSION_SHARE_TMUX"); bin != "" {
		return bin
	}
	if p, err := exec.LookPath("tmux"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/opt/local/bin/tmux", "/usr/bin/tmux", "/bin/tmux"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "tmux"
}

const usage = `session-share shares a running tmux session with someone else, for a
limited time, in a browser or over SSH.

Usage:
  session-share start <session> [--mode read|write] [--for 1h] [--name label]
                      [--ssh-github user]... [--ssh-key name=KEY]... [--no-web]
                      [--max-viewers 1] [--socket name] [--json]
  session-share list [--all] [--json]
  session-share stop <id> [--json]
  session-share extend <id> --for 30m [--json]
  session-share logs <id> [--follow]
  session-share chat <id> [message] [--follow] [--json]
  session-share expose [status|proxy --url URL|funnel|off] [--json]
  session-share serve [--listen 127.0.0.1:7690]
  session-share attach <id> --guest <name>     (the command an SSH guest's key runs)
  session-share gc
  session-share version
`

func (c *CLI) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(c.Out, usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "start":
		return c.start(ctx, rest)
	case "list", "ls":
		return c.list(rest)
	case "stop":
		return c.stop(rest)
	case "extend":
		return c.extend(rest)
	case "logs":
		return c.logs(ctx, rest)
	case "chat":
		return c.chat(ctx, rest)
	case "expose":
		return c.expose(rest)
	case "serve":
		return c.serve(ctx, rest)
	case "attach":
		return c.attach(ctx, rest)
	case "gc":
		_, err := c.App.Reap()
		return err
	case "version", "--version", "-v":
		fmt.Fprintln(c.Out, c.Version)
		return nil
	case "help", "--help", "-h":
		fmt.Fprint(c.Out, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q: run session-share help", cmd)
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// parse lets flags come before or after the positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func (c *CLI) printJSON(v map[string]any) error {
	v["version"] = JSONVersion
	enc := json.NewEncoder(c.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type viewerView struct {
	ConnID  string    `json:"conn_id"`
	Kind    string    `json:"kind"`
	Guest   string    `json:"guest,omitempty"`
	Remote  string    `json:"remote,omitempty"`
	Started time.Time `json:"started"`
}

type shareView struct {
	ID         string       `json:"id"`
	Name       string       `json:"name,omitempty"`
	Session    string       `json:"session"`
	Mode       share.Mode   `json:"mode"`
	CreatedAt  time.Time    `json:"created_at"`
	ExpiresAt  time.Time    `json:"expires_at"`
	Active     bool         `json:"active"`
	EndedAt    *time.Time   `json:"ended_at,omitempty"`
	EndReason  string       `json:"end_reason,omitempty"`
	URL        string       `json:"url,omitempty"`
	MaxViewers int          `json:"max_viewers"`
	Guests     []string     `json:"ssh_guests"`
	Viewers    []viewerView `json:"viewers"`
}

func (c *CLI) view(s *share.Share) shareView {
	v := shareView{
		ID: s.ID, Name: s.Name, Session: s.Session, Mode: s.Mode, CreatedAt: s.CreatedAt,
		ExpiresAt: s.ExpiresAt, Active: s.Active(time.Now()), EndedAt: s.EndedAt, EndReason: s.EndReason,
		MaxViewers: s.MaxViewers, Guests: []string{}, Viewers: []viewerView{},
	}
	if s.Web {
		v.URL = c.baseURL() + "/s/" + s.ID + "/"
	}
	for _, g := range s.Guests {
		v.Guests = append(v.Guests, g.Name)
	}
	if v.Active {
		conns, _ := c.App.Store.Conns(s.ID)
		for _, cn := range conns {
			v.Viewers = append(v.Viewers, viewerView{ConnID: cn.ID, Kind: cn.Kind, Guest: cn.Guest, Remote: cn.Remote, Started: cn.Started})
		}
	}
	return v
}

func (c *CLI) baseURL() string {
	if cur, err := c.Expose.Current(); err == nil && cur != nil {
		return cur.URL
	}
	return expose.LocalURL(c.Listen)
}

func (c *CLI) start(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	mode := fs.String("mode", "read", "read or write")
	dur := fs.Duration("for", time.Hour, "how long the share lasts")
	name := fs.String("name", "", "a label for the share")
	noWeb := fs.Bool("no-web", false, "SSH guests only, no web link")
	maxViewers := fs.Int("max-viewers", 1, "web viewers at the same time")
	socket := fs.String("socket", "", "tmux socket name (tmux -L)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	var githubUsers, keys stringList
	fs.Var(&githubUsers, "ssh-github", "GitHub user whose public keys may connect over SSH")
	fs.Var(&keys, "ssh-key", "name=KEY: a public key that may connect over SSH")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: session-share start <session> [flags]")
	}
	var guests []app.GuestRequest
	for _, u := range githubUsers {
		guests = append(guests, app.GuestRequest{Name: strings.ToLower(u), GitHub: u})
	}
	for _, k := range keys {
		guestName, key, ok := strings.Cut(k, "=")
		if !ok {
			return fmt.Errorf("--ssh-key %q: use name=KEY", k)
		}
		guests = append(guests, app.GuestRequest{Name: guestName, Keys: []string{key}})
	}
	sh, password, err := c.App.Start(ctx, app.StartOptions{
		Options: share.Options{
			Name: *name, Session: pos[0], Socket: *socket, Mode: share.Mode(*mode),
			For: *dur, Web: !*noWeb, MaxViewers: *maxViewers,
		},
		SSHGuests: guests,
	})
	if err != nil {
		return err
	}
	if sh.Web {
		if err := c.ensureServer(); err != nil {
			_, _ = c.App.Stop(sh.ID)
			return err
		}
	}
	v := c.view(sh)
	sshUser := currentUser()
	exposed, _ := c.Expose.Current()
	if *asJSON {
		out := map[string]any{"share": v, "ssh_user": sshUser, "exposed": exposed != nil}
		if password != "" {
			out["password"] = password
		}
		return c.printJSON(out)
	}
	modeText := "read only"
	if sh.Mode == share.ModeWrite {
		modeText = "can type"
	}
	fmt.Fprintf(c.Out, "Sharing %q (%s) until %s.\n\n", sh.Session, modeText, sh.ExpiresAt.Local().Format("15:04"))
	if sh.Web {
		fmt.Fprintf(c.Out, "  Link:      %s\n", v.URL)
		if exposed == nil {
			fmt.Fprintf(c.Out, "             Only this machine can open it. Publish it with: session-share expose\n")
		}
		fmt.Fprintf(c.Out, "  Password:  %s\n", password)
		fmt.Fprintf(c.Out, "             Send it apart from the link.\n")
	}
	for _, g := range sh.Guests {
		fmt.Fprintf(c.Out, "  SSH:       %s connects with: ssh -t %s@<this machine>\n", g.Name, sshUser)
	}
	fmt.Fprintf(c.Out, "  Stop:      session-share stop %s\n", sh.ID)
	return nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

var errOtherAccount = errors.New("another account's session-share already listens there")

// healthy reports whether this account's server answers. A server of another
// account on the same address would publish its own shares, not ours.
func (c *CLI) healthy() (bool, error) {
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(expose.LocalURL(c.Listen) + "/healthz")
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	if owner := server.HealthOwner(string(body)); owner != server.Owner() {
		return false, fmt.Errorf("%s (%s): give this account its own address with SESSION_SHARE_LISTEN or %s", c.Listen, errOtherAccount, filepath.Join(c.App.Store.Dir, "listen"))
	}
	return true, nil
}

// ensureServer starts `serve` in the background when it is not running. The
// server outlives this command and stops on its own when nothing is shared.
func (c *CLI) ensureServer() error {
	if ok, err := c.healthy(); ok || err != nil {
		return err
	}
	logDir := filepath.Join(c.App.Store.Dir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", logDir, err)
	}
	out, err := os.OpenFile(filepath.Join(logDir, "server.out"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening the server output: %w", err)
	}
	defer out.Close()
	cmd := exec.Command(c.App.Exe, "serve", "--listen", c.Listen)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the web server: %w", err)
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok, err := c.healthy(); ok || err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the web server did not answer on %s: see %s", c.Listen, filepath.Join(logDir, "server.out"))
}

func (c *CLI) list(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	all := fs.Bool("all", false, "include shares that ended")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	_, _ = c.App.Reap()
	shares, err := c.App.Store.List()
	if err != nil {
		return err
	}
	views := []shareView{}
	for _, s := range shares {
		v := c.view(s)
		if v.Active || *all {
			views = append(views, v)
		}
	}
	if *asJSON {
		return c.printJSON(map[string]any{"shares": views})
	}
	if len(views) == 0 {
		fmt.Fprintln(c.Out, "Nothing is shared.")
		return nil
	}
	for _, v := range views {
		state := fmt.Sprintf("until %s", v.ExpiresAt.Local().Format("15:04"))
		if !v.Active {
			state = "ended: " + v.EndReason
		}
		fmt.Fprintf(c.Out, "%s  %-16s %-5s  %s  viewers: %d\n", v.ID, v.Session, v.Mode, state, len(v.Viewers))
	}
	return nil
}

func (c *CLI) stop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: session-share stop <id>")
	}
	sh, err := c.App.Stop(pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(map[string]any{"share": c.view(sh)})
	}
	fmt.Fprintf(c.Out, "Stopped sharing %q. Anyone connected is disconnected within a second.\n", sh.Session)
	return nil
}

func (c *CLI) extend(args []string) error {
	fs := flag.NewFlagSet("extend", flag.ContinueOnError)
	dur := fs.Duration("for", 30*time.Minute, "how much longer")
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: session-share extend <id> --for 30m")
	}
	sh, err := c.App.Extend(pos[0], *dur)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(map[string]any{"share": c.view(sh)})
	}
	fmt.Fprintf(c.Out, "Sharing %q until %s.\n", sh.Session, sh.ExpiresAt.Local().Format("15:04"))
	return nil
}

func (c *CLI) logs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("follow", false, "keep printing new events")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: session-share logs <id> [--follow]")
	}
	if _, err := c.App.Store.Load(pos[0]); err != nil {
		return err
	}
	f, err := os.Open(c.App.Store.LogPath(pos[0]))
	if err != nil {
		return fmt.Errorf("opening the log: %w", err)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" && strings.HasSuffix(line, "\n") {
			fmt.Fprint(c.Out, line)
			continue
		}
		if err == io.EOF && *follow {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(300 * time.Millisecond):
			}
			if line != "" {
				_, _ = f.Seek(-int64(len(line)), io.SeekCurrent)
				r.Reset(f)
			}
			continue
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the log: %w", err)
		}
	}
}

// chat sends a message as the owner, or prints the conversation. The share
// server tails the same file, so a guest sees an owner message within a
// moment, whichever process wrote it.
func (c *CLI) chat(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	follow := fs.Bool("follow", false, "keep printing new messages")
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return errors.New("usage: session-share chat <id> [message] [--follow] [--json]")
	}
	if _, err := c.App.Store.Load(pos[0]); err != nil {
		return err
	}
	log := c.App.Chat(pos[0])
	if len(pos) > 1 {
		m, err := log.Append(currentUser(), chat.RoleOwner, strings.Join(pos[1:], " "), time.Now())
		if err != nil {
			return err
		}
		if *asJSON {
			return c.printJSON(map[string]any{"message": m})
		}
		return nil
	}
	msgs, offset, err := log.Recent(500)
	if err != nil {
		return err
	}
	if *asJSON {
		if msgs == nil {
			msgs = []chat.Message{}
		}
		return c.printJSON(map[string]any{"messages": msgs})
	}
	print := func(ms []chat.Message) {
		for _, m := range ms {
			fmt.Fprintf(c.Out, "%s  %s: %s\n", m.TS.Local().Format("15:04"), m.From, m.Text)
		}
	}
	print(msgs)
	for *follow {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		next, n, err := log.Since(offset)
		if err != nil {
			return err
		}
		offset = n
		print(next)
	}
	return nil
}

func (c *CLI) expose(args []string) error {
	fs := flag.NewFlagSet("expose", flag.ContinueOnError)
	rawURL := fs.String("url", "", "public address of your proxy")
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	action := "status"
	if len(pos) > 0 {
		action = pos[0]
	}
	var e *expose.Exposure
	switch action {
	case "status":
		cur, err := c.Expose.Current()
		if err != nil {
			return err
		}
		opts := c.Expose.Detect()
		if *asJSON {
			return c.printJSON(map[string]any{"current": cur, "options": opts})
		}
		if cur != nil {
			fmt.Fprintf(c.Out, "Published as %s through %s.\n\n", cur.URL, cur.Kind)
		} else {
			fmt.Fprintf(c.Out, "Not published: only this machine can open the links (%s).\n\n", expose.LocalURL(c.Listen))
		}
		for _, o := range opts {
			mark := "no "
			if o.Available {
				mark = "yes"
			}
			fmt.Fprintf(c.Out, "  %-12s %s  %s\n", o.Kind, mark, o.Detail)
		}
		return nil
	case "proxy":
		if *rawURL == "" {
			return errors.New("usage: session-share expose proxy --url https://share.example.com")
		}
		e, err = c.Expose.Proxy(*rawURL)
	case "funnel":
		e, err = c.Expose.Funnel()
	case "off":
		err = c.Expose.Off()
	default:
		return fmt.Errorf("unknown expose action %q: use status, proxy, funnel or off", action)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(map[string]any{"current": e})
	}
	if e == nil {
		fmt.Fprintln(c.Out, "Not published anymore.")
		return nil
	}
	fmt.Fprintf(c.Out, "Share links now start with %s.\n", e.URL)
	return nil
}

func (c *CLI) serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", c.Listen, "address to listen on")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		c.Listen = *listen
		if ok, _ := c.healthy(); ok {
			return nil
		}
		return fmt.Errorf("listening on %s: %w", *listen, err)
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return server.New(c.App).Serve(ctx, ln)
}

func (c *CLI) attach(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	guest := fs.String("guest", "", "guest name")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *guest == "" {
		return errors.New("usage: session-share attach <id> --guest <name>")
	}
	return attach.Run(ctx, c.App, attach.Options{
		ShareID: pos[0], Guest: *guest, In: os.Stdin, Out: os.Stdout,
		Remote: attach.RemoteFromEnv(), Banner: 1500 * time.Millisecond,
	})
}
