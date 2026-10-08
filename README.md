# session-share

Share a running tmux session with someone else, for a limited time, in a
browser or over SSH. They watch it live, or type into it with you. When the
time is up, they are disconnected and your session keeps running.

```sh
session-share start api --mode write --for 2h
```

```
Sharing "api" (can type) until 16:40.

  Link:      https://share.example.com/s/k3j2abcdwxyz/
  Password:  v3bd-28s5-nave-dvcg
             Send it apart from the link.
  Stop:      session-share stop k3j2abcdwxyz
```

It works with anything that runs in tmux: a shell, Claude Code, vim, htop.
The person you share with joins the session as it is, on the screen you see.

## What a guest can and cannot do

| | read | write |
|---|---|---|
| See the shared session, live | yes | yes |
| See your other sessions or windows you do not show | no | no |
| Run a tmux command (kill the session, open a window, detach you) | no | no |
| Shrink your screen to their size | no | no |
| Type into the program in the pane | no | yes |

A guest never gets a tmux client that takes input. The screen comes from a
read-only tmux client (`attach -f read-only,ignore-size`); what a write guest
types is pasted into the pane, so it reaches the program and never tmux's key
bindings. A write share also turns on `remain-on-exit`, so a guest who types
`exit` leaves a dead pane behind instead of ending your session.

**Write means trust.** A guest who can type into a shell, or approve a tool in
Claude Code, runs commands as you. Share write access only with someone you
would hand your keyboard to, and keep it short.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/mydevmachine/session-share/main/install.sh | bash
```

Linux and macOS, `amd64` and `arm64`. It needs tmux 3.2 or later. One static
binary: the web page and the terminal (xterm.js) are inside it.

## How a guest gets in

### In a browser

`start` runs a small web server on `127.0.0.1:7690`, one for every share. It
only listens on this machine, so something has to publish it:

```sh
session-share expose            # what is available here
session-share expose funnel     # https://<machine>.ts.net:10000, no domain needed
session-share expose proxy --url https://share.example.com
session-share expose off
```

- **`funnel`** uses Tailscale Funnel on port 10000, so it never touches what
  the machine already serves on 443. Funnel only serves `ts.net` names: no
  custom domain. The guest needs no Tailscale account. A tailnet run by
  Headscale has no Funnel.
- **`proxy`** is for your own domain. Point a host at `http://127.0.0.1:7690`
  in Caddy, nginx or a Cloudflare named tunnel, then record its address.
  With Caddy:

  ```
  share.example.com {
      reverse_proxy 127.0.0.1:7690
  }
  ```

The route is set once: every share lives under `/s/<id>/` on the same host.

The guest opens the link and types the password. Send the password through a
different channel than the link.

### Over SSH

```sh
session-share start api --ssh-github bob --for 1h
session-share start api --ssh-key 'bob=ssh-ed25519 AAAA...' --no-web
```

This adds a line to your `~/.ssh/authorized_keys`:

```
restrict,pty,expiry-time="20261008164000",command="/usr/local/bin/session-share attach k3j2abcdwxyz --guest bob" ssh-ed25519 AAAA... session-share:k3j2abcdwxyz:bob
```

Bob connects with `ssh -t you@your-machine` and lands in the shared session.
`restrict` turns off port, agent and X11 forwarding; the forced command means
he cannot run anything else; `expiry-time` makes sshd refuse the key after the
deadline. A read-only guest leaves with `q`; a write guest with Enter, `~`, `.`.

## The deadline

A share lasts between one minute and 24 hours. When it ends, by time or by
`stop`, every connection closes within a second, the SSH lines are removed and
your tmux session carries on. `extend` moves the deadline:

```sh
session-share extend k3j2abcdwxyz --for 30m
```

A share also ends when its tmux session does.

## Commands

| Command | What it does |
|---|---|
| `start <session>` | Share a session. `--mode read\|write` (read), `--for` (1h), `--name`, `--ssh-github`, `--ssh-key name=KEY`, `--no-web`, `--max-viewers` (1), `--socket` (tmux `-L`) |
| `list` | Active shares and who is connected. `--all` includes ended ones. |
| `stop <id>` | End a share now. |
| `extend <id> --for 30m` | Move the deadline. |
| `logs <id> [--follow]` | The share's event log. |
| `expose [status\|funnel\|proxy --url URL\|off]` | Publish the web server. |
| `serve` | The web server. `start` runs it for you; it stops when nothing is shared. |
| `attach <id> --guest <name>` | What an SSH guest's key runs. |
| `gc` | End what ran out and forget shares older than 30 days. |

`start`, `list`, `stop`, `extend` and `expose` take `--json`. Every JSON
document carries `"version": 1`; a change that breaks a field raises it.

## Logs

Each share writes one JSON line per event to
`~/.local/state/session-share/logs/<id>.jsonl`: when it started and how,
every sign-in (right or wrong, with the address), every connection and why it
closed, how many bytes a guest typed, tmux errors, and how it ended. The web
server writes its own to `logs/server.jsonl`. Passwords, tokens, the screen
and what a guest typed never reach a log.

In the browser, **Copy diagnostics** gives the guest a report with the same
connection id the log uses, so the two sides can be matched.

### Why a connection closed

| Code | Meaning |
|---|---|
| 4001 | The share reached its deadline. |
| 4002 | The owner ran `stop`. |
| 4003 | The tmux session ended. |
| 4004 | No valid sign-in: the cookie is missing or belongs to another share. Reload and sign in. |
| 4005 | The share allows a set number of browser viewers (`--max-viewers`, 1 by default), and that many are connected. |
| 1001 | The browser left, or the server shut down. The page reconnects. |
| 1006 | The network dropped. The page reconnects with growing waits. |

## Security notes

- The server listens on loopback only. It trusts `X-Forwarded-For` only from
  loopback, where your proxy runs.
- A password is 16 random characters; it is stored as a salted hash. Five wrong
  attempts in ten minutes lock sign-in for that share for ten minutes.
- The sign-in cookie is an HMAC under a key only that share has. It is
  `HttpOnly`, `SameSite=Strict`, scoped to `/s/<id>/`, and dies with the share.
- The WebSocket only accepts its own origin. Pages send a strict CSP, no
  referrer, and cannot be framed.
- State and logs are `0600` under `~/.local/state/session-share`
  (`$SESSION_SHARE_HOME` or `$XDG_STATE_HOME` move it).

## Environment

| Variable | Default |
|---|---|
| `SESSION_SHARE_HOME` | `$XDG_STATE_HOME/session-share` or `~/.local/state/session-share` |
| `SESSION_SHARE_LISTEN` | `127.0.0.1:7690` |
| `SESSION_SHARE_TMUX` | `tmux` on `PATH`, then the usual Homebrew, MacPorts and system paths |

## Development

```sh
go test -race ./...      # needs tmux; tests run their own tmux server
go build -o session-share ./cmd/session-share
```

## License

MIT. xterm.js is MIT, see `web/static/xterm-LICENSE`.
