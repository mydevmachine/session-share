package share

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewShareStartsActiveAndEndsAtItsDeadline(t *testing.T) {
	now := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	s, password, err := New(Options{Session: "api", Mode: ModeRead, For: 2 * time.Hour, Web: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	if password == "" || s.PasswordHash == "" {
		t.Fatal("a web share needs a password")
	}
	if !s.CheckPassword(password) {
		t.Fatal("the password it returned does not match")
	}
	if s.CheckPassword(password + "x") {
		t.Fatal("a wrong password matched")
	}
	if !s.Active(now.Add(time.Hour)) {
		t.Fatal("share should be active before its deadline")
	}
	if s.Active(now.Add(2 * time.Hour)) {
		t.Fatal("share should end at its deadline")
	}
}

func TestNewRefusesABadRequest(t *testing.T) {
	now := time.Now()
	cases := map[string]Options{
		"no session":   {Mode: ModeRead, For: time.Hour, Web: true},
		"bad mode":     {Session: "api", Mode: "admin", For: time.Hour, Web: true},
		"too short":    {Session: "api", Mode: ModeRead, For: 10 * time.Second, Web: true},
		"too long":     {Session: "api", Mode: ModeRead, For: 25 * time.Hour, Web: true},
		"no way in":    {Session: "api", Mode: ModeRead, For: time.Hour},
		"bad guest":    {Session: "api", Mode: ModeRead, For: time.Hour, Guests: []Guest{{Name: "Bob Smith", Keys: []string{"k"}}}},
		"guest no key": {Session: "api", Mode: ModeRead, For: time.Hour, Guests: []Guest{{Name: "bob"}}},
	}
	for name, opts := range cases {
		if _, _, err := New(opts, now); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestStoreSavesLoadsAndListsShares(t *testing.T) {
	st := NewStore(t.TempDir())
	now := time.Now()
	a, _, _ := New(Options{Session: "api", Mode: ModeRead, For: time.Hour, Web: true}, now)
	b, _, _ := New(Options{Session: "web", Mode: ModeWrite, For: time.Hour, Web: true}, now)
	for _, s := range []*Share{a, b} {
		if err := st.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Load(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Session != "api" || got.PasswordHash != a.PasswordHash {
		t.Fatalf("loaded %+v", got)
	}
	all, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d shares, want 2", len(all))
	}
	info, err := os.Stat(filepath.Join(st.Dir, "shares", a.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("share file mode %v, want 0600: it holds the password hash", info.Mode().Perm())
	}
}

func TestStoreLoadSaysWhenAShareDoesNotExist(t *testing.T) {
	st := NewStore(t.TempDir())
	if _, err := st.Load("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if _, err := st.Load("../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a path in the id must not be read: got %v", err)
	}
}

func TestEndRecordsWhyAndWhen(t *testing.T) {
	now := time.Now()
	s, _, _ := New(Options{Session: "api", Mode: ModeRead, For: time.Hour, Web: true}, now)
	s.End(ReasonRevoked, now.Add(time.Minute))
	if s.Active(now.Add(2 * time.Minute)) {
		t.Fatal("an ended share is not active")
	}
	if s.EndReason != ReasonRevoked || s.EndedAt == nil {
		t.Fatalf("got reason %q ended %v", s.EndReason, s.EndedAt)
	}
	s.End(ReasonExpired, now.Add(time.Hour))
	if s.EndReason != ReasonRevoked {
		t.Fatal("the first reason must stick")
	}
}

func TestConnectionsWhoseProcessDiedAreNotCounted(t *testing.T) {
	st := NewStore(t.TempDir())
	live := Conn{ID: "c1", ShareID: "s1", Kind: "web", PID: os.Getpid(), Started: time.Now()}
	dead := Conn{ID: "c2", ShareID: "s1", Kind: "ssh", PID: 999999, Started: time.Now()}
	for _, c := range []Conn{live, dead} {
		if err := st.AddConn(c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Conns("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c1" {
		t.Fatalf("got %+v, want only the live one", got)
	}
	if err := st.RemoveConn("s1", "c1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Conns("s1"); len(got) != 0 {
		t.Fatalf("got %+v after removing", got)
	}
}
