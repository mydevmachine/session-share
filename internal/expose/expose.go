package expose

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	KindProxy  = "proxy"
	KindFunnel = "funnel"
	FunnelPort = 10000
)

type Exposure struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

type Option struct {
	Kind      string `json:"kind"`
	Available bool   `json:"available"`
	Detail    string `json:"detail"`
}

type Runner func(name string, args ...string) ([]byte, error)

func ExecRunner(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %s", filepath.Base(name), strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return out, nil
}

type Manager struct {
	Path     string
	Listen   string
	Run      Runner
	LookPath func(string) (string, error)
	Stat     func(string) (os.FileInfo, error)
}

func (m *Manager) Current() (*Exposure, error) {
	data, err := os.ReadFile(m.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", m.Path, err)
	}
	var e Exposure
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("reading %s: %w", m.Path, err)
	}
	return &e, nil
}

func (m *Manager) save(e *Exposure) error {
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(m.Path), err)
	}
	data, _ := json.MarshalIndent(e, "", "  ")
	if err := os.WriteFile(m.Path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", m.Path, err)
	}
	return nil
}

// Proxy records a public address that something else (Caddy, nginx, a
// Cloudflare named tunnel) already forwards to the local server.
func (m *Manager) Proxy(raw string) (*Exposure, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http(s) address like https://share.example.com", raw)
	}
	if u.Path != "" || u.RawQuery != "" {
		return nil, fmt.Errorf("%q has a path: point the whole host at %s, the share links carry their own path", raw, m.Listen)
	}
	if err := m.Off(); err != nil {
		return nil, err
	}
	e := &Exposure{Kind: KindProxy, URL: u.String()}
	return e, m.save(e)
}

func (m *Manager) tailscale() (string, bool) {
	if p, err := m.LookPath("tailscale"); err == nil {
		return p, true
	}
	const macApp = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
	if _, err := m.Stat(macApp); err == nil {
		return macApp, true
	}
	return "", false
}

type tailscaleStatus struct {
	BackendState string `json:"BackendState"`
	Self         struct {
		DNSName string                     `json:"DNSName"`
		CapMap  map[string]json.RawMessage `json:"CapMap"`
	} `json:"Self"`
}

// funnelCheck tells whether this node may publish on Funnel port 10000. A
// tailnet run by Headscale has no Funnel, so it never carries the capability.
func (m *Manager) funnelCheck() (string, string, error) {
	bin, ok := m.tailscale()
	if !ok {
		return "", "", errors.New("tailscale is not installed")
	}
	out, err := m.Run(bin, "status", "--json")
	if err != nil {
		return "", "", fmt.Errorf("tailscale is not running: %w", err)
	}
	var st tailscaleStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return "", "", fmt.Errorf("reading tailscale status: %w", err)
	}
	if st.BackendState != "Running" {
		return "", "", fmt.Errorf("tailscale is %s, not connected", strings.ToLower(st.BackendState))
	}
	if _, ok := st.Self.CapMap["funnel"]; !ok {
		return "", "", errors.New("this machine may not use Funnel: allow it in the tailnet policy (Headscale has no Funnel)")
	}
	for cap := range st.Self.CapMap {
		if strings.HasPrefix(cap, "https://tailscale.com/cap/funnel-ports?ports=") && !strings.Contains(cap, fmt.Sprint(FunnelPort)) {
			return "", "", fmt.Errorf("Funnel port %d is not allowed for this machine (%s)", FunnelPort, strings.TrimPrefix(cap, "https://tailscale.com/cap/funnel-ports?"))
		}
	}
	host := strings.TrimSuffix(st.Self.DNSName, ".")
	if host == "" {
		return "", "", errors.New("tailscale did not report this machine's name")
	}
	return bin, host, nil
}

// Funnel publishes the local server on port 10000 of the machine's ts.net
// name. It uses its own port so it never touches what the machine already
// serves on 443.
func (m *Manager) Funnel() (*Exposure, error) {
	bin, host, err := m.funnelCheck()
	if err != nil {
		return nil, err
	}
	if err := m.Off(); err != nil {
		return nil, err
	}
	if _, err := m.Run(bin, "funnel", "--bg", fmt.Sprintf("--https=%d", FunnelPort), "http://"+m.Listen); err != nil {
		return nil, fmt.Errorf("turning on Funnel: %w", err)
	}
	e := &Exposure{Kind: KindFunnel, URL: fmt.Sprintf("https://%s:%d", host, FunnelPort)}
	return e, m.save(e)
}

// Off removes only what this program published; other Funnel or serve
// routes on the machine stay as they are.
func (m *Manager) Off() error {
	cur, err := m.Current()
	if err != nil || cur == nil {
		return err
	}
	if cur.Kind == KindFunnel {
		if bin, ok := m.tailscale(); ok {
			if _, err := m.Run(bin, "funnel", fmt.Sprintf("--https=%d", FunnelPort), "off"); err != nil {
				return fmt.Errorf("turning off Funnel: %w", err)
			}
		}
	}
	if err := os.Remove(m.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", m.Path, err)
	}
	return nil
}

func (m *Manager) Detect() []Option {
	opts := []Option{{Kind: KindProxy, Available: true, Detail: fmt.Sprintf("point your own proxy (Caddy, nginx, a Cloudflare named tunnel) at http://%s, then: session-share expose proxy --url https://share.example.com", m.Listen)}}
	if _, host, err := m.funnelCheck(); err != nil {
		opts = append(opts, Option{Kind: KindFunnel, Detail: err.Error()})
	} else {
		opts = append(opts, Option{Kind: KindFunnel, Available: true, Detail: fmt.Sprintf("https://%s:%d (no custom domain: Funnel only serves ts.net names)", host, FunnelPort)})
	}
	if _, err := m.LookPath("cloudflared"); err == nil {
		opts = append(opts, Option{Kind: "cloudflared", Available: true, Detail: fmt.Sprintf("route a named tunnel's hostname to http://%s, then use expose proxy with that hostname", m.Listen)})
	}
	return opts
}

func LocalURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	return "http://" + net.JoinHostPort(host, port)
}
