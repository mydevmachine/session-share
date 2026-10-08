package share

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Mode string

const (
	ModeRead  Mode = "read"
	ModeWrite Mode = "write"
)

const (
	ReasonExpired      = "expired"
	ReasonRevoked      = "revoked"
	ReasonSessionEnded = "session-ended"
)

const (
	MinDuration = time.Minute
	MaxDuration = 24 * time.Hour
)

var ErrNotFound = errors.New("no such share")

var (
	guestName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)
	shareID   = regexp.MustCompile(`^[a-z2-7]{12}$`)
)

type Guest struct {
	Name string   `json:"name"`
	Keys []string `json:"keys"`
}

type Share struct {
	ID           string     `json:"id"`
	Name         string     `json:"name,omitempty"`
	Session      string     `json:"session"`
	Socket       string     `json:"socket,omitempty"`
	Mode         Mode       `json:"mode"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	EndReason    string     `json:"end_reason,omitempty"`
	Web          bool       `json:"web"`
	MaxViewers   int        `json:"max_viewers"`
	Guests       []Guest    `json:"guests,omitempty"`
	PasswordHash string     `json:"password_hash,omitempty"`
	Salt         string     `json:"salt,omitempty"`
	CookieKey    string     `json:"cookie_key,omitempty"`
}

type Options struct {
	Name       string
	Session    string
	Socket     string
	Mode       Mode
	For        time.Duration
	Web        bool
	MaxViewers int
	Guests     []Guest
}

func New(o Options, now time.Time) (*Share, string, error) {
	if err := validate(o); err != nil {
		return nil, "", err
	}
	id, err := randomID()
	if err != nil {
		return nil, "", err
	}
	if o.MaxViewers <= 0 {
		o.MaxViewers = 1
	}
	s := &Share{
		ID:         id,
		Name:       o.Name,
		Session:    o.Session,
		Socket:     o.Socket,
		Mode:       o.Mode,
		CreatedAt:  now.UTC(),
		ExpiresAt:  now.Add(o.For).UTC(),
		Web:        o.Web,
		MaxViewers: o.MaxViewers,
		Guests:     o.Guests,
	}
	if !o.Web {
		return s, "", nil
	}
	password, err := randomPassword()
	if err != nil {
		return nil, "", err
	}
	salt, err := randomHex(16)
	if err != nil {
		return nil, "", err
	}
	cookieKey, err := randomHex(32)
	if err != nil {
		return nil, "", err
	}
	s.Salt = salt
	s.CookieKey = cookieKey
	s.PasswordHash = hashPassword(salt, password)
	return s, password, nil
}

func validate(o Options) error {
	if strings.TrimSpace(o.Session) == "" {
		return errors.New("name the tmux session to share")
	}
	if o.Mode != ModeRead && o.Mode != ModeWrite {
		return fmt.Errorf("mode %q: use read or write", o.Mode)
	}
	if o.For < MinDuration || o.For > MaxDuration {
		return fmt.Errorf("duration %s: use between %s and %s", o.For, MinDuration, MaxDuration)
	}
	if !o.Web && len(o.Guests) == 0 {
		return errors.New("nobody could reach this share: keep the web link or add an SSH guest")
	}
	for _, g := range o.Guests {
		if !guestName.MatchString(g.Name) {
			return fmt.Errorf("guest name %q: use lower case letters, digits, '.', '_' or '-', up to 32", g.Name)
		}
		if len(g.Keys) == 0 {
			return fmt.Errorf("guest %s has no SSH key", g.Name)
		}
	}
	return nil
}

func EndedMessage(reason string) string {
	switch reason {
	case ReasonExpired:
		return "This share expired."
	case ReasonRevoked:
		return "The owner stopped sharing."
	case ReasonSessionEnded:
		return "The shared session ended."
	}
	return "This share has ended."
}

func (s *Share) Active(now time.Time) bool {
	return s.EndedAt == nil && now.Before(s.ExpiresAt)
}

func (s *Share) End(reason string, now time.Time) {
	if s.EndedAt != nil {
		return
	}
	t := now.UTC()
	s.EndedAt = &t
	s.EndReason = reason
}

func (s *Share) CheckPassword(password string) bool {
	if s.PasswordHash == "" {
		return false
	}
	return hmac.Equal([]byte(hashPassword(s.Salt, password)), []byte(s.PasswordHash))
}

func (s *Share) Guest(name string) (Guest, bool) {
	for _, g := range s.Guests {
		if g.Name == name {
			return g, true
		}
	}
	return Guest{}, false
}

// The password is random and long, so a single SHA-256 round is enough: there
// is no dictionary to slow down.
func hashPassword(salt, password string) string {
	sum := sha256.Sum256([]byte(salt + ":" + password))
	return hex.EncodeToString(sum[:])
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading randomness: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:12], nil
}

const passwordAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

func randomPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading randomness: %w", err)
	}
	var out strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			out.WriteByte('-')
		}
		out.WriteByte(passwordAlphabet[int(c)%len(passwordAlphabet)])
	}
	return out.String(), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading randomness: %w", err)
	}
	return hex.EncodeToString(b), nil
}

type Store struct {
	Dir string
}

func NewStore(dir string) *Store {
	return &Store{Dir: dir}
}

func (st *Store) sharePath(id string) string {
	return filepath.Join(st.Dir, "shares", id+".json")
}

func (st *Store) LogPath(id string) string {
	return filepath.Join(st.Dir, "logs", id+".jsonl")
}

func (st *Store) Save(s *Share) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding share %s: %w", s.ID, err)
	}
	return writeFileAtomic(st.sharePath(s.ID), data)
}

func (st *Store) Load(id string) (*Share, error) {
	if !shareID.MatchString(id) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	data, err := os.ReadFile(st.sharePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("reading share %s: %w", id, err)
	}
	var s Share
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("reading share %s: %w", id, err)
	}
	return &s, nil
}

func (st *Store) List() ([]*Share, error) {
	paths, err := filepath.Glob(filepath.Join(st.Dir, "shares", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("listing shares: %w", err)
	}
	var out []*Share
	for _, p := range paths {
		s, err := st.Load(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (st *Store) Delete(id string) error {
	if !shareID.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	for _, p := range []string{st.sharePath(id), st.LogPath(id)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("deleting %s: %w", p, err)
		}
	}
	_ = os.RemoveAll(filepath.Join(st.Dir, "conns", id))
	return nil
}

type Conn struct {
	ID      string    `json:"id"`
	ShareID string    `json:"share_id"`
	Kind    string    `json:"kind"`
	Guest   string    `json:"guest,omitempty"`
	Remote  string    `json:"remote,omitempty"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

func (st *Store) connPath(shareID, connID string) string {
	return filepath.Join(st.Dir, "conns", shareID, connID+".json")
}

func (st *Store) AddConn(c Conn) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding connection: %w", err)
	}
	return writeFileAtomic(st.connPath(c.ShareID, c.ID), data)
}

func (st *Store) RemoveConn(shareID, connID string) error {
	err := os.Remove(st.connPath(shareID, connID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing connection %s: %w", connID, err)
	}
	return nil
}

func (st *Store) Conns(shareID string) ([]Conn, error) {
	paths, err := filepath.Glob(filepath.Join(st.Dir, "conns", shareID, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("listing connections: %w", err)
	}
	var out []Conn
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c Conn
		if json.Unmarshal(data, &c) != nil {
			continue
		}
		if !processAlive(c.PID) {
			_ = os.Remove(p)
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
