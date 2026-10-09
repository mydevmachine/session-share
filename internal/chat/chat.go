package chat

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxText    = 500
	MaxName    = 32
	RoleOwner  = "owner"
	RoleGuest  = "guest"
	burst      = 5
	burstEvery = 5 * time.Second
)

var ErrEmpty = errors.New("say something first")

type Message struct {
	ID   string    `json:"id"`
	TS   time.Time `json:"ts"`
	From string    `json:"from"`
	Role string    `json:"role"`
	Text string    `json:"text"`
}

// Clean keeps what a person types and drops what could steer a terminal or a
// page: control characters, escape sequences, and invisible format runes
// (zero-width, bidirectional overrides). New lines become spaces.
func Clean(text string, max int) (string, error) {
	var b strings.Builder
	count := 0
	space := false
	for _, r := range strings.ToValidUTF8(text, "") {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r), r == utf8.RuneError:
			continue
		}
		if space {
			if count >= max {
				break
			}
			b.WriteByte(' ')
			count++
			space = false
		}
		if count >= max {
			break
		}
		b.WriteRune(r)
		count++
	}
	if b.Len() == 0 {
		return "", ErrEmpty
	}
	return b.String(), nil
}

type Log struct {
	Path string
	mu   sync.Mutex
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (l *Log) Append(from, role, text string, now time.Time) (Message, error) {
	name, err := Clean(from, MaxName)
	if err != nil {
		name = role
	}
	body, err := Clean(text, MaxText)
	if err != nil {
		return Message{}, err
	}
	m := Message{ID: newID(), TS: now.UTC(), From: name, Role: role, Text: body}
	data, err := json.Marshal(m)
	if err != nil {
		return Message{}, fmt.Errorf("encoding chat message: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return Message{}, fmt.Errorf("creating chat directory: %w", err)
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Message{}, fmt.Errorf("opening chat %s: %w", l.Path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return Message{}, fmt.Errorf("writing chat %s: %w", l.Path, err)
	}
	return m, nil
}

// Since reads the complete lines written after offset and the offset to
// read from next time. A line still being written waits for the next call.
func (l *Log) Since(offset int64) ([]Message, int64, error) {
	f, err := os.Open(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, offset, fmt.Errorf("opening chat %s: %w", l.Path, err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("reading chat %s: %w", l.Path, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, fmt.Errorf("reading chat %s: %w", l.Path, err)
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, offset, nil
	}
	var out []Message
	sc := bufio.NewScanner(bytes.NewReader(data[:end+1]))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out, offset + int64(end+1), nil
}

func (l *Log) Recent(n int) ([]Message, int64, error) {
	all, offset, err := l.Since(0)
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, offset, err
}

// Limiter lets a burst of messages through, then one per burstEvery/burst.
type Limiter struct {
	mu   sync.Mutex
	sent []time.Time
}

func (l *Limiter) Allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	var recent []time.Time
	for _, t := range l.sent {
		if now.Sub(t) < burstEvery {
			recent = append(recent, t)
		}
	}
	l.sent = recent
	if len(l.sent) >= burst {
		return false
	}
	l.sent = append(l.sent, now)
	return true
}

// TmuxText makes a message safe for tmux display-message, which reads #
// as the start of a format.
func TmuxText(s string) string {
	return strings.ReplaceAll(s, "#", "##")
}
