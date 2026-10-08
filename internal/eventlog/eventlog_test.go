package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogWritesOneJSONLinePerEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "abc.jsonl")
	l := New(path, "abc")
	l.now = func() time.Time { return time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC) }
	if err := l.Log("viewer_joined", Fields{"conn_id": "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Log("viewer_left", Fields{"conn_id": "c1", "reason": "client closed"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e map[string]any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %q is not JSON: %v", sc.Text(), err)
		}
		events = append(events, e)
	}
	if len(events) != 2 {
		t.Fatalf("got %d lines, want 2", len(events))
	}
	if events[1]["event"] != "viewer_left" || events[1]["share_id"] != "abc" || events[1]["reason"] != "client closed" {
		t.Fatalf("second line %v", events[1])
	}
	if events[0]["ts"] != "2026-10-08T14:00:00Z" {
		t.Fatalf("ts %v", events[0]["ts"])
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode %v, want 0600", info.Mode().Perm())
	}
}

func TestANilLoggerDoesNothing(t *testing.T) {
	var l *Logger
	if err := l.Log("x", nil); err != nil {
		t.Fatal(err)
	}
}
