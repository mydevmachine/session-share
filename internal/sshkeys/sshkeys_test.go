package sshkeys

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const bobKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJvYmJv"

func TestParseKeepsOnlyTheTypeAndTheKey(t *testing.T) {
	got, err := Parse(`command="rm -rf /",no-pty ` + bobKey + ` bob@laptop`)
	if err != nil {
		t.Fatal(err)
	}
	if got != bobKey {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{"", "hello", "ssh-ed25519 not*base64", "ssh-dss AAAA"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestLineIsRestrictedAndExpires(t *testing.T) {
	e := Entry{ShareID: "abcdefghijkl", Guest: "bob", Key: bobKey,
		Expires: time.Date(2026, 10, 8, 16, 40, 0, 0, time.Local), Command: "/usr/local/bin/session-share attach abcdefghijkl --guest bob"}
	line, err := e.Line()
	if err != nil {
		t.Fatal(err)
	}
	want := `restrict,pty,expiry-time="20261008164000",command="/usr/local/bin/session-share attach abcdefghijkl --guest bob" ` + bobKey + ` session-share:abcdefghijkl:bob`
	if line != want {
		t.Fatalf("got\n%s\nwant\n%s", line, want)
	}
	e.Command = `/bin/x" ,permitopen="*`
	if _, err := e.Line(); err == nil {
		t.Fatal("a quote in the command must be refused")
	}
}

func TestSetAndRemoveTouchOnlyTheShareLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	own := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG93bm93bm93bm93bm93bm93bm93bm93bm93bm93bm93 alice@laptop"
	if err := os.WriteFile(path, []byte(own+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := File{Path: path}
	entry := func(share, guest string) Entry {
		return Entry{ShareID: share, Guest: guest, Key: bobKey, Expires: time.Now().Add(time.Hour), Command: "/bin/session-share attach " + share}
	}
	if err := f.Set("aaaaaaaaaaaa", []Entry{entry("aaaaaaaaaaaa", "bob")}); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("bbbbbbbbbbbb", []Entry{entry("bbbbbbbbbbbb", "carol")}); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("aaaaaaaaaaaa", []Entry{entry("aaaaaaaaaaaa", "bob")}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), "session-share:aaaaaaaaaaaa:"); n != 1 {
		t.Fatalf("Set twice left %d lines for the share:\n%s", n, data)
	}
	n, err := f.Remove("aaaaaaaaaaaa")
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), own) || !strings.Contains(string(data), "session-share:bbbbbbbbbbbb:carol") || strings.Contains(string(data), "aaaaaaaaaaaa") {
		t.Fatalf("unexpected file:\n%s", data)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want the original 0600", info.Mode().Perm())
	}
}

func TestRemoveWithoutAFileIsNotAnError(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "authorized_keys")}
	if n, err := f.Remove("aaaaaaaaaaaa"); err != nil || n != 0 {
		t.Fatalf("got %d, %v", n, err)
	}
}

func TestGitHubKeysReadsTheKeysPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bob.keys":
			_, _ = w.Write([]byte(bobKey + "\nnot a key\n"))
		case "/empty.keys":
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteTo(srv.URL)
	fetch := GitHubKeys(client)
	keys, err := fetch(context.Background(), "bob")
	if err != nil || len(keys) != 1 || keys[0] != bobKey {
		t.Fatalf("got %v, %v", keys, err)
	}
	if _, err := fetch(context.Background(), "empty"); err == nil || !strings.Contains(err.Error(), "no public SSH key") {
		t.Fatalf("got %v", err)
	}
	if _, err := fetch(context.Background(), "nobody"); err == nil || !strings.Contains(err.Error(), "no user") {
		t.Fatalf("got %v", err)
	}
	if _, err := fetch(context.Background(), "../etc"); err == nil {
		t.Fatal("a path must not be a user name")
	}
}

type rewriteTo string

func (base rewriteTo) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	target, _ := http.NewRequest(r.Method, string(base)+u.Path, nil)
	return http.DefaultTransport.RoundTrip(target.WithContext(r.Context()))
}
