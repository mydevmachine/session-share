package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
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
