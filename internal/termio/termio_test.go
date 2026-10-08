package termio

import "testing"

func TestSplitSendsTerminalAnswersBackAndKeepsTyping(t *testing.T) {
	cases := []struct {
		name, in, typed, answered string
	}{
		{"plain typing", "ls -la\r", "ls -la\r", ""},
		{"arrow and escape keys", "\x1b[A\x1b[D\x1b", "\x1b[A\x1b[D\x1b", ""},
		{"xterm.js on attach", "\x1b[?1;2c\x1b[>0;276;0c\x1b[?2026;0$y\x1b]10;rgb:ffff/ffff/ffff\x1b\\\x1b]11;rgb:0f0f/1111/1515\x1b\\",
			"", "\x1b[?1;2c\x1b[>0;276;0c\x1b[?2026;0$y\x1b]10;rgb:ffff/ffff/ffff\x1b\\\x1b]11;rgb:0f0f/1111/1515\x1b\\"},
		{"version report", "\x1bP>|xterm.js(5.5.0)\x1b\\", "", "\x1bP>|xterm.js(5.5.0)\x1b\\"},
		{"mouse and focus", "\x1b[<0;10;5M\x1b[<0;10;5m\x1b[I\x1b[O", "", "\x1b[<0;10;5M\x1b[<0;10;5m\x1b[I\x1b[O"},
		{"mixed", "a\x1b[?1;2cb", "ab", "\x1b[?1;2c"},
	}
	for _, c := range cases {
		typed, answered := Split([]byte(c.in))
		if string(typed) != c.typed || string(answered) != c.answered {
			t.Errorf("%s: typed %q answered %q, want %q and %q", c.name, typed, answered, c.typed, c.answered)
		}
	}
}
