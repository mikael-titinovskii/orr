package app

import (
	"encoding/json"
	"os"
	"sync"
)

// verdictLogMaxBytes rotates the on-disk audit trail before it grows without
// bound. The current file becomes "<path>.1"; the previous backup is replaced.
var verdictLogMaxBytes int64 = 5 << 20

// verdictLog appends guardrail verdicts as JSON lines. It records the same
// fields as the in-memory log, including the text the check judged, and is safe
// for concurrent requests. The sample is what makes a false positive
// reproducible, so it stays on the line rather than in a separate file that
// would have to be correlated by time.
type verdictLog struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

func openVerdictLog(path string) (*verdictLog, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	log := &verdictLog{path: path, file: file}
	if info, statErr := file.Stat(); statErr == nil {
		log.size = info.Size()
	}
	return log, nil
}

func (l *verdictLog) write(event guardrailEvent) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	if l.size >= verdictLogMaxBytes {
		l.rotateLocked()
		if l.file == nil {
			return nil
		}
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	written, err := l.file.Write(encoded)
	l.size += int64(written)
	return err
}

func (l *verdictLog) rotateLocked() {
	_ = l.file.Close()
	l.file = nil
	_ = os.Remove(l.path + ".1")
	_ = os.Rename(l.path, l.path+".1")
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	l.file = file
	l.size = 0
}

func (l *verdictLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}
