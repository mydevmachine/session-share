package eventlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Fields map[string]any

type Logger struct {
	path    string
	shareID string
	now     func() time.Time
}

func New(path, shareID string) *Logger {
	return &Logger{path: path, shareID: shareID, now: time.Now}
}

// Log appends one JSON line. Each line is a single write on an O_APPEND file,
// so the server and the SSH attach processes can log to the same file.
func (l *Logger) Log(event string, fields Fields) error {
	if l == nil {
		return nil
	}
	line := make(map[string]any, len(fields)+3)
	for k, v := range fields {
		line[k] = v
	}
	line["ts"] = l.now().UTC().Format(time.RFC3339Nano)
	line["event"] = event
	if l.shareID != "" {
		line["share_id"] = l.shareID
	}
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("encoding log event %s: %w", event, err)
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening log %s: %w", l.path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing log %s: %w", l.path, err)
	}
	return nil
}
