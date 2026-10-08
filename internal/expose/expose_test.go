package expose

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const funnelReady = `{"BackendState":"Running","Self":{"DNSName":"main.tail-example.ts.net.","CapMap":{"funnel":null,"https://tailscale.com/cap/funnel-ports?ports=443,8443,10000":null}}}`

type fakeTailscale struct {
	status string
	calls  []string
}

func (f *fakeTailscale) run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if args[0] == "status" {
		if f.status == "" {
			return nil, errors.New("not running")
		}
		return []byte(f.status), nil
	}
	return nil, nil
}

func manager(t *testing.T, ts *fakeTailscale, installed ...string) *Manager {
	return &Manager{
		Path:   filepath.Join(t.TempDir(), "expose.json"),
		Listen: "127.0.0.1:7690",
		Run:    ts.run,
		LookPath: func(bin string) (string, error) {
			for _, b := range installed {
				if b == bin {
					return "/usr/bin/" + bin, nil
				}
			}
			return "", errors.New("not found")
		},
		Stat: func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	}
}

func TestFunnelPublishesOnItsOwnPortAndOffRemovesOnlyThat(t *testing.T) {
	ts := &fakeTailscale{status: funnelReady}
	m := manager(t, ts, "tailscale")
	e, err := m.Funnel()
	if err != nil {
		t.Fatal(err)
	}
	if e.URL != "https://main.tail-example.ts.net:10000" {
		t.Fatalf("url %q", e.URL)
	}
	if err := m.Off(); err != nil {
		t.Fatal(err)
	}
	want := []string{"status --json", "funnel --bg --https=10000 http://127.0.0.1:7690", "funnel --https=10000 off"}
	if strings.Join(ts.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q", ts.calls)
	}
	if cur, _ := m.Current(); cur != nil {
		t.Fatalf("still exposed: %+v", cur)
	}
}

func TestFunnelExplainsWhyItCannotRun(t *testing.T) {
	cases := map[string]struct {
		status    string
		installed []string
		want      string
	}{
		"not installed": {installed: nil, want: "not installed"},
		"stopped":       {installed: []string{"tailscale"}, want: "not running"},
		"no capability": {status: `{"BackendState":"Running","Self":{"DNSName":"a.ts.net.","CapMap":{}}}`, installed: []string{"tailscale"}, want: "Headscale"},
		"port not allowed": {status: `{"BackendState":"Running","Self":{"DNSName":"a.ts.net.","CapMap":{"funnel":null,"https://tailscale.com/cap/funnel-ports?ports=443":null}}}`,
			installed: []string{"tailscale"}, want: "port 10000"},
	}
	for name, c := range cases {
		m := manager(t, &fakeTailscale{status: c.status}, c.installed...)
		if _, err := m.Funnel(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func TestProxyNeedsABareHTTPSAddress(t *testing.T) {
	m := manager(t, &fakeTailscale{})
	if _, err := m.Proxy("https://share.example.com/"); err != nil {
		t.Fatal(err)
	}
	cur, _ := m.Current()
	if cur.Kind != KindProxy || cur.URL != "https://share.example.com" {
		t.Fatalf("got %+v", cur)
	}
	for _, bad := range []string{"share.example.com", "ftp://share.example.com", "https://example.com/share"} {
		if _, err := m.Proxy(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestDetectListsEveryWayOut(t *testing.T) {
	m := manager(t, &fakeTailscale{status: funnelReady}, "tailscale", "cloudflared")
	opts := m.Detect()
	if len(opts) != 3 || !opts[0].Available || !opts[1].Available || opts[2].Kind != "cloudflared" {
		t.Fatalf("got %+v", opts)
	}
	m = manager(t, &fakeTailscale{})
	opts = m.Detect()
	if len(opts) != 2 || opts[1].Available {
		t.Fatalf("got %+v", opts)
	}
}
