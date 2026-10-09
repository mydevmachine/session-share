package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mydevmachine/session-share/internal/app"
	"github.com/mydevmachine/session-share/internal/expose"
	"github.com/mydevmachine/session-share/internal/share"
	"github.com/mydevmachine/session-share/internal/sshkeys"
)

func TestParseTakesFlagsOnEitherSide(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	mode := fs.String("mode", "read", "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, []string{"--json", "api", "--mode", "write"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 1 || pos[0] != "api" || *mode != "write" || !*asJSON {
		t.Fatalf("pos %v mode %q json %v", pos, *mode, *asJSON)
	}
}

func newCLI(t *testing.T) (*CLI, *bytes.Buffer, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir := t.TempDir()
	socket := fmt.Sprintf("ss-cli-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	if out, err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "api", "cat").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	ln.Close()
	var out bytes.Buffer
	c := &CLI{
		App: &app.App{
			Store: share.NewStore(filepath.Join(dir, "state")),
			Keys:  sshkeys.File{Path: filepath.Join(dir, "authorized_keys")},
			Exe:   "/usr/local/bin/session-share",
		},
		Expose: &expose.Manager{
			Path: filepath.Join(dir, "state", "expose.json"), Listen: listen,
			Run:      func(string, ...string) ([]byte, error) { return nil, errors.New("no tailscale") },
			LookPath: func(string) (string, error) { return "", errors.New("not found") },
			Stat:     os.Stat,
		},
		Listen: listen,
		Out:    &out,
		Err:    &out,
	}
	return c, &out, socket
}

func TestStartAndListSpeakVersionedJSON(t *testing.T) {
	c, out, socket := newCLI(t)
	key := "bob=ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJv"
	if err := c.Run(context.Background(), []string{"start", "api", "--socket", socket, "--no-web", "--ssh-key", key, "--json"}); err != nil {
		t.Fatal(err)
	}
	var started struct {
		Version  int       `json:"version"`
		Share    shareView `json:"share"`
		Password string    `json:"password"`
		SSHUser  string    `json:"ssh_user"`
	}
	if err := json.Unmarshal(out.Bytes(), &started); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if started.Version != JSONVersion || started.Share.Session != "api" || started.Share.Mode != share.ModeRead ||
		started.Password != "" || started.Share.URL != "" || strings.Join(started.Share.Guests, ",") != "bob" {
		t.Fatalf("got %+v", started)
	}
	out.Reset()
	if err := c.Run(context.Background(), []string{"list", "--json"}); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Version int         `json:"version"`
		Shares  []shareView `json:"shares"`
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Version != JSONVersion || len(listed.Shares) != 1 || !listed.Shares[0].Active {
		t.Fatalf("got %+v", listed)
	}
	out.Reset()
	if err := c.Run(context.Background(), []string{"stop", started.Share.ID}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := c.Run(context.Background(), []string{"list", "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil || len(listed.Shares) != 0 {
		t.Fatalf("after stop: %+v %v", listed, err)
	}
}

func TestStartRefusesAnUnknownCommandAndBadInput(t *testing.T) {
	c, _, socket := newCLI(t)
	cases := [][]string{
		{"nope"},
		{"start"},
		{"start", "api", "--socket", socket, "--mode", "admin"},
		{"start", "api", "--socket", socket, "--ssh-key", "no-equals-sign"},
		{"start", "missing", "--socket", socket},
	}
	for _, args := range cases {
		if err := c.Run(context.Background(), args); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}

func TestListenAddressComesFromTheEnvironmentThenTheStateFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SESSION_SHARE_LISTEN", "")
	if got := listenAddress(dir); got != DefaultListen {
		t.Fatalf("got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "listen"), []byte("127.0.0.1:7701\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := listenAddress(dir); got != "127.0.0.1:7701" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("SESSION_SHARE_LISTEN", "127.0.0.1:7702")
	if got := listenAddress(dir); got != "127.0.0.1:7702" {
		t.Fatalf("got %q", got)
	}
}

func TestStartRefusesAServerOfAnotherAccount(t *testing.T) {
	c, _, socket := newCLI(t)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok uid=0\n"))
	}))
	defer other.Close()
	c.Listen = strings.TrimPrefix(other.URL, "http://")
	err := c.Run(context.Background(), []string{"start", "api", "--socket", socket})
	if err == nil || !strings.Contains(err.Error(), "another account") {
		t.Fatalf("got %v", err)
	}
	shares, _ := c.App.Store.List()
	if len(shares) != 1 || shares[0].EndReason != share.ReasonRevoked {
		t.Fatalf("the share must not stay active: %+v", shares)
	}
}

func TestChatSendsAsTheOwnerAndPrintsTheConversation(t *testing.T) {
	c, out, socket := newCLI(t)
	key := "bob=ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJv"
	if err := c.Run(context.Background(), []string{"start", "api", "--socket", socket, "--no-web", "--ssh-key", key, "--json"}); err != nil {
		t.Fatal(err)
	}
	var started struct {
		Share shareView `json:"share"`
	}
	if err := json.Unmarshal(out.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	id := started.Share.ID
	if _, err := c.App.Chat(id).Append("bob", "guest", "can you see me?", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(context.Background(), []string{"chat", id, "yes,", "loud", "and", "clear"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := c.Run(context.Background(), []string{"chat", id, "--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version  int `json:"version"`
		Messages []struct {
			From, Role, Text string
		} `json:"messages"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got.Version != JSONVersion || len(got.Messages) != 2 || got.Messages[1].Role != "owner" || got.Messages[1].Text != "yes, loud and clear" {
		t.Fatalf("got %+v", got)
	}
	if err := c.Run(context.Background(), []string{"chat", "zzzzzzzzzzzz", "hi"}); err == nil {
		t.Fatal("chat on a share that does not exist must fail")
	}
}
