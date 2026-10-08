package termio

import "regexp"

// A viewer's terminal answers the questions the tmux client asks it (device
// attributes, mode reports, colours) and reports mouse and focus events.
// Those bytes belong to the tmux client that asked, not to the program in the
// shared pane, where they would show up as typed garbage.
var replies = []*regexp.Regexp{
	regexp.MustCompile(`^\x1b\[[?>=][0-9;]*c`),
	regexp.MustCompile(`^\x1b\[\??[0-9;]*\$y`),
	regexp.MustCompile(`^\x1b\][0-9;]*rgb:[0-9a-fA-F/]*(?:\x07|\x1b\\)`),
	regexp.MustCompile(`^\x1bP[^\x1b]*\x1b\\`),
	regexp.MustCompile(`^\x1b\[<[0-9;]+[Mm]`),
	regexp.MustCompile(`^\x1b\[M[\x20-\xff]{3}`),
	regexp.MustCompile(`^\x1b\[[IO]`),
}

// Split separates what the guest typed from what their terminal answered.
func Split(data []byte) (typed, answered []byte) {
	for i := 0; i < len(data); {
		if data[i] == 0x1b {
			if n := replyLength(data[i:]); n > 0 {
				answered = append(answered, data[i:i+n]...)
				i += n
				continue
			}
		}
		typed = append(typed, data[i])
		i++
	}
	return typed, answered
}

func replyLength(data []byte) int {
	for _, re := range replies {
		if loc := re.FindIndex(data); loc != nil {
			return loc[1]
		}
	}
	return 0
}
