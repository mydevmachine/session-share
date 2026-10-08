package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mydevmachine/session-share/internal/eventlog"
	"github.com/mydevmachine/session-share/internal/share"
	"github.com/mydevmachine/session-share/internal/sshkeys"
	"github.com/mydevmachine/session-share/internal/tmux"
)

const HistoryKept = 30 * 24 * time.Hour

type App struct {
	Store   *share.Store
	Keys    sshkeys.File
	TmuxBin string
	Exe     string
	Fetch   sshkeys.Fetcher
	Now     func() time.Time
}

func StateDir() (string, error) {
	if dir := os.Getenv("SESSION_SHARE_HOME"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "session-share"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "session-share"), nil
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) Tmux(s *share.Share) tmux.Tmux {
	return tmux.New(a.TmuxBin, s.Socket)
}

func (a *App) Log(id string) *eventlog.Logger {
	return eventlog.New(a.Store.LogPath(id), id)
}

func (a *App) ServerLog() *eventlog.Logger {
	return eventlog.New(filepath.Join(a.Store.Dir, "logs", "server.jsonl"), "")
}

type GuestRequest struct {
	Name   string
	GitHub string
	Keys   []string
}

type StartOptions struct {
	share.Options
	SSHGuests []GuestRequest
}

func (a *App) Start(ctx context.Context, o StartOptions) (*share.Share, string, error) {
	tm := tmux.New(a.TmuxBin, o.Socket)
	if err := tm.HasSession(o.Session); err != nil {
		return nil, "", err
	}
	guests, err := a.resolveGuests(ctx, o.SSHGuests)
	if err != nil {
		return nil, "", err
	}
	o.Guests = guests
	s, password, err := share.New(o.Options, a.now())
	if err != nil {
		return nil, "", err
	}
	if err := a.Store.Save(s); err != nil {
		return nil, "", err
	}
	if err := a.writeKeys(s); err != nil {
		_ = a.Store.Delete(s.ID)
		return nil, "", err
	}
	log := a.Log(s.ID)
	if s.Mode == share.ModeWrite {
		if err := tm.KeepPanes(s.Session); err != nil {
			_ = log.Log("tmux_error", eventlog.Fields{"op": "remain-on-exit", "error": err.Error()})
		}
	}
	_ = log.Log("share_started", eventlog.Fields{
		"session": s.Session, "name": s.Name, "mode": s.Mode, "expires_at": s.ExpiresAt,
		"web": s.Web, "ssh_guests": guestNames(s.Guests), "max_viewers": s.MaxViewers,
	})
	_ = tm.Notify(s.Session, fmt.Sprintf("session-share: shared (%s) until %s", s.Mode, s.ExpiresAt.Local().Format("15:04")))
	return s, password, nil
}

func (a *App) resolveGuests(ctx context.Context, reqs []GuestRequest) ([]share.Guest, error) {
	var guests []share.Guest
	for _, r := range reqs {
		g := share.Guest{Name: r.Name}
		if r.GitHub != "" {
			if a.Fetch == nil {
				return nil, errors.New("fetching GitHub keys is not available")
			}
			keys, err := a.Fetch(ctx, r.GitHub)
			if err != nil {
				return nil, err
			}
			g.Keys = append(g.Keys, keys...)
		}
		for _, k := range r.Keys {
			parsed, err := sshkeys.Parse(k)
			if err != nil {
				return nil, fmt.Errorf("guest %s: %w", r.Name, err)
			}
			g.Keys = append(g.Keys, parsed)
		}
		guests = append(guests, g)
	}
	return guests, nil
}

func (a *App) writeKeys(s *share.Share) error {
	if len(s.Guests) == 0 {
		return nil
	}
	if a.Exe == "" {
		return errors.New("cannot tell where this program lives, so SSH guests have no command to run")
	}
	var entries []sshkeys.Entry
	for _, g := range s.Guests {
		for _, k := range g.Keys {
			entries = append(entries, sshkeys.Entry{
				ShareID: s.ID, Guest: g.Name, Key: k, Expires: s.ExpiresAt,
				Command: fmt.Sprintf("%s attach %s --guest %s", a.Exe, s.ID, g.Name),
			})
		}
	}
	if err := a.Keys.Set(s.ID, entries); err != nil {
		return fmt.Errorf("adding SSH guests: %w", err)
	}
	return nil
}

func (a *App) Stop(id string) (*share.Share, error) {
	s, err := a.Store.Load(id)
	if err != nil {
		return nil, err
	}
	if s.EndedAt != nil {
		return s, nil
	}
	return s, a.end(s, share.ReasonRevoked)
}

func (a *App) Extend(id string, d time.Duration) (*share.Share, error) {
	s, err := a.Store.Load(id)
	if err != nil {
		return nil, err
	}
	now := a.now()
	if !s.Active(now) {
		return nil, fmt.Errorf("share %s has ended (%s): start a new one", id, endReason(s, now))
	}
	if d <= 0 {
		return nil, errors.New("extend by a positive duration")
	}
	expires := s.ExpiresAt.Add(d)
	if expires.Sub(now) > share.MaxDuration {
		return nil, fmt.Errorf("a share lasts at most %s from now", share.MaxDuration)
	}
	s.ExpiresAt = expires.UTC()
	if err := a.Store.Save(s); err != nil {
		return nil, err
	}
	if err := a.writeKeys(s); err != nil {
		return nil, err
	}
	_ = a.Log(s.ID).Log("share_extended", eventlog.Fields{"expires_at": s.ExpiresAt, "by": d.String()})
	return s, nil
}

func endReason(s *share.Share, now time.Time) string {
	if s.EndReason != "" {
		return s.EndReason
	}
	if !now.Before(s.ExpiresAt) {
		return share.ReasonExpired
	}
	return ""
}

// Check reloads a share from disk, so every process sees a stop or an
// extension another one made.
func (a *App) Check(id string) (*share.Share, string, error) {
	s, err := a.Store.Load(id)
	if err != nil {
		return nil, "", err
	}
	if s.Active(a.now()) {
		return s, "", nil
	}
	return s, endReason(s, a.now()), nil
}

func (a *App) end(s *share.Share, reason string) error {
	s.End(reason, a.now())
	if err := a.Store.Save(s); err != nil {
		return err
	}
	log := a.Log(s.ID)
	removed, err := a.Keys.Remove(s.ID)
	if err != nil {
		_ = log.Log("cleanup_failed", eventlog.Fields{"what": "ssh keys", "error": err.Error()})
		return err
	}
	_ = log.Log("share_ended", eventlog.Fields{"reason": reason, "ssh_keys_removed": removed})
	_ = a.Tmux(s).Notify(s.Session, "session-share: sharing ended ("+reason+")")
	return nil
}

// Reap ends what has run out or lost its session, and forgets shares that
// ended long ago.
func (a *App) Reap() ([]*share.Share, error) {
	all, err := a.Store.List()
	if err != nil {
		return nil, err
	}
	now := a.now()
	var ended []*share.Share
	var errs []error
	for _, s := range all {
		switch {
		case s.EndedAt == nil && !now.Before(s.ExpiresAt):
			errs = append(errs, a.end(s, share.ReasonExpired))
			ended = append(ended, s)
		case s.EndedAt == nil && errors.Is(a.Tmux(s).HasSession(s.Session), tmux.ErrNoSession):
			errs = append(errs, a.end(s, share.ReasonSessionEnded))
			ended = append(ended, s)
		case s.EndedAt != nil && now.Sub(*s.EndedAt) > HistoryKept:
			errs = append(errs, a.Store.Delete(s.ID))
		}
	}
	return ended, errors.Join(errs...)
}

func guestNames(gs []share.Guest) []string {
	names := make([]string, 0, len(gs))
	for _, g := range gs {
		names = append(names, g.Name)
	}
	return names
}
