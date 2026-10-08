package sshkeys

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var keyTypes = map[string]bool{
	"ssh-ed25519":                        true,
	"ssh-rsa":                            true,
	"ecdsa-sha2-nistp256":                true,
	"ecdsa-sha2-nistp384":                true,
	"ecdsa-sha2-nistp521":                true,
	"sk-ssh-ed25519@openssh.com":         true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
}

var githubUser = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// Parse keeps the type and the key of a public key line and drops its
// comment and any options, so nothing from the guest reaches authorized_keys
// except the key itself.
func Parse(line string) (string, error) {
	fields := strings.Fields(line)
	for i, f := range fields {
		if !keyTypes[f] || i+1 >= len(fields) {
			continue
		}
		blob := fields[i+1]
		if _, err := base64.StdEncoding.DecodeString(blob); err != nil {
			return "", fmt.Errorf("the key after %s is not valid base64", f)
		}
		return f + " " + blob, nil
	}
	return "", errors.New("no SSH public key found: expected something like \"ssh-ed25519 AAAA…\"")
}

type Entry struct {
	ShareID string
	Guest   string
	Key     string
	Expires time.Time
	Command string
}

func marker(shareID string) string {
	return "session-share:" + shareID + ":"
}

// Line builds a restricted authorized_keys line: no forwarding of any kind, a
// pty, a forced command, and an expiry sshd checks at login.
func (e Entry) Line() (string, error) {
	if strings.ContainsAny(e.Command, "\"\n\\") {
		return "", fmt.Errorf("the attach command %q cannot hold quotes, backslashes or new lines", e.Command)
	}
	key, err := Parse(e.Key)
	if err != nil {
		return "", err
	}
	expiry := e.Expires.Local().Format("20060102150405")
	return fmt.Sprintf(`restrict,pty,expiry-time="%s",command="%s" %s %s%s`,
		expiry, e.Command, key, marker(e.ShareID), e.Guest), nil
}

type File struct {
	Path string
}

func DefaultFile() (File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return File{}, fmt.Errorf("finding the home directory: %w", err)
	}
	return File{Path: filepath.Join(home, ".ssh", "authorized_keys")}, nil
}

func (f File) read() ([]string, os.FileMode, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", f.Path, err)
	}
	info, err := os.Stat(f.Path)
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", f.Path, err)
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, info.Mode().Perm(), sc.Err()
}

func (f File) write(lines []string, mode os.FileMode) error {
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".authorized_keys-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", f.Path, err)
	}
	defer os.Remove(tmp.Name())
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", f.Path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", f.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", f.Path, err)
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return fmt.Errorf("writing %s: %w", f.Path, err)
	}
	return nil
}

// Set replaces every line of a share with the given entries.
func (f File) Set(shareID string, entries []Entry) error {
	lines, mode, err := f.read()
	if err != nil {
		return err
	}
	kept := withoutShare(lines, shareID)
	for _, e := range entries {
		line, err := e.Line()
		if err != nil {
			return fmt.Errorf("guest %s: %w", e.Guest, err)
		}
		kept = append(kept, line)
	}
	return f.write(kept, mode)
}

func (f File) Remove(shareID string) (int, error) {
	lines, mode, err := f.read()
	if err != nil {
		return 0, err
	}
	kept := withoutShare(lines, shareID)
	removed := len(lines) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	return removed, f.write(kept, mode)
}

func withoutShare(lines []string, shareID string) []string {
	m := marker(shareID)
	var kept []string
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) > 0 && strings.HasPrefix(fields[len(fields)-1], m) {
			continue
		}
		kept = append(kept, l)
	}
	return kept
}

type Fetcher func(ctx context.Context, user string) ([]string, error)

func GitHubKeys(client *http.Client) Fetcher {
	return func(ctx context.Context, user string) ([]string, error) {
		if !githubUser.MatchString(user) {
			return nil, fmt.Errorf("%q is not a GitHub user name", user)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/"+user+".keys", nil)
		if err != nil {
			return nil, fmt.Errorf("asking GitHub for %s's keys: %w", user, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("asking GitHub for %s's keys: %w", user, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("GitHub has no user %s", user)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("asking GitHub for %s's keys: %s", user, resp.Status)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		if err != nil {
			return nil, fmt.Errorf("reading %s's keys: %w", user, err)
		}
		var keys []string
		for _, l := range strings.Split(string(body), "\n") {
			if k, err := Parse(l); err == nil {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("%s has no public SSH key on GitHub", user)
		}
		return keys, nil
	}
}
