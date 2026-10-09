package chat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanKeepsTextAndDropsWhatCouldSteerATerminal(t *testing.T) {
	cases := map[string]string{
		"hello":                    "hello",
		"  two\nlines\r\n  here  ": "two lines here",
		"\x1b[31mred\x1b[0m":       "[31mred[0m",
		"bell\x07 and nul\x00":     "bell and nul",
		"zero​width‮evil":          "zerowidthevil",
		"olá, ação ✓ 日本":           "olá, ação ✓ 日本",
		"bad \xff utf8":            "bad utf8",
		"tab\tseparated para ":     "tab separated para",
	}
	for in, want := range cases {
		got, err := Clean(in, MaxText)
		if err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, empty := range []string{"", "   ", "\x1b\x07​"} {
		if _, err := Clean(empty, MaxText); !errors.Is(err, ErrEmpty) {
			t.Errorf("Clean(%q): want ErrEmpty, got %v", empty, err)
		}
	}
	long, _ := Clean(strings.Repeat("é", MaxText+50), MaxText)
	if n := len([]rune(long)); n != MaxText {
		t.Fatalf("long message kept %d runes, want %d", n, MaxText)
	}
}

func TestLogAppendsAndReadsOnlyWhatIsNew(t *testing.T) {
	l := &Log{Path: filepath.Join(t.TempDir(), "chats", "abc.jsonl")}
	now := time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC)
	if msgs, offset, err := l.Since(0); err != nil || len(msgs) != 0 || offset != 0 {
		t.Fatalf("empty log: %v %d %v", msgs, offset, err)
	}
	if _, err := l.Append("bob\x1b", RoleGuest, "hi there", now); err != nil {
		t.Fatal(err)
	}
	first, offset, err := l.Since(0)
	if err != nil || len(first) != 1 || first[0].From != "bob" || first[0].Role != RoleGuest || first[0].Text != "hi there" {
		t.Fatalf("got %+v, %v", first, err)
	}
	if _, err := l.Append("", RoleOwner, "welcome", now); err != nil {
		t.Fatal(err)
	}
	next, _, _ := l.Since(offset)
	if len(next) != 1 || next[0].From != RoleOwner || next[0].Text != "welcome" {
		t.Fatalf("got %+v", next)
	}
	if _, err := l.Append("bob", RoleGuest, "​", now); !errors.Is(err, ErrEmpty) {
		t.Fatalf("an empty message must be refused: %v", err)
	}
	info, _ := os.Stat(l.Path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}

func TestSinceWaitsForAHalfWrittenLine(t *testing.T) {
	l := &Log{Path: filepath.Join(t.TempDir(), "c.jsonl")}
	if _, err := l.Append("bob", RoleGuest, "one", time.Now()); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(l.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"id":"x","text":"half`)
	f.Close()
	msgs, offset, _ := l.Since(0)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	again, offset2, _ := l.Since(offset)
	if len(again) != 0 || offset2 != offset {
		t.Fatalf("a half line was read: %+v", again)
	}
}

func TestLimiterAllowsABurstThenWaits(t *testing.T) {
	var l Limiter
	now := time.Now()
	for i := 0; i < burst; i++ {
		if !l.Allow(now) {
			t.Fatalf("message %d refused", i)
		}
	}
	if l.Allow(now.Add(time.Second)) {
		t.Fatal("the sixth message in five seconds must wait")
	}
	if !l.Allow(now.Add(burstEvery + time.Millisecond)) {
		t.Fatal("after the window a message goes through")
	}
}

func TestTmuxTextEscapesFormats(t *testing.T) {
	if got := TmuxText("#{pane_pid} #[fg=red]"); got != "##{pane_pid} ##[fg=red]" {
		t.Fatalf("got %q", got)
	}
}
