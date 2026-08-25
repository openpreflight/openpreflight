// SPDX-License-Identifier: Apache-2.0

// Package logs owns the per-job log file: a size-capped writer plus reading and
// pruning.
package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Writer appends to /data/logs/<job-id>.log and stops at a byte cap so a
// runaway build cannot fill the volume.
type Writer struct {
	mu       sync.Mutex
	f        *os.File
	max      int64
	written  int64
	overflow bool
}

// Path returns the log file for a job id.
func Path(dir, jobID string) string { return filepath.Join(dir, jobID+".log") }

// Create opens the log file for a job. max <= 0 means unlimited.
func Create(dir, jobID string, max int64) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("logs: mkdir: %w", err)
	}
	f, err := os.OpenFile(Path(dir, jobID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, fmt.Errorf("logs: create: %w", err)
	}
	return &Writer{f: f, max: max}, nil
}

// Write appends, truncating once the cap is hit. It never returns a short write
// error: a full log must not fail the build, only stop growing.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return len(p), nil
	}
	if w.max > 0 && w.written >= w.max {
		return len(p), nil
	}
	chunk := p
	if w.max > 0 && w.written+int64(len(chunk)) > w.max {
		chunk = chunk[:w.max-w.written]
		w.overflow = true
	}
	n, err := w.f.Write(chunk)
	w.written += int64(n)
	if w.overflow {
		w.f.WriteString(fmt.Sprintf("\n\n--- log truncated at %d bytes (max_log_bytes) ---\n", w.max))
		w.f.Sync()
		w.f.Close()
		w.f = nil
	}
	if err != nil {
		return len(p), fmt.Errorf("logs: write: %w", err)
	}
	return len(p), nil
}

// Printf writes a framing line (step headers, timings).
func (w *Writer) Printf(format string, args ...any) {
	fmt.Fprintf(w, format, args...)
}

// Bytes reports how much was written.
func (w *Writer) Bytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// Truncated reports whether the cap was reached.
func (w *Writer) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overflow
}

// Close flushes and closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Read returns a job's log, or "" if there is none yet.
func Read(dir, jobID string) (string, error) {
	b, err := os.ReadFile(Path(dir, jobID))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("logs: read: %w", err)
	}
	return string(b), nil
}

// Tail returns at most n bytes from the end of a log, for the Check Run output.
func Tail(dir, jobID string, n int64) (string, error) {
	f, err := os.Open(Path(dir, jobID))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("logs: open: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("logs: stat: %w", err)
	}
	start := int64(0)
	if info.Size() > n {
		start = info.Size() - n
	}
	buf := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && len(buf) > 0 {
		return "", fmt.Errorf("logs: read tail: %w", err)
	}
	out := string(buf)
	if start > 0 {
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
		out = "… earlier output omitted …\n" + out
	}
	return out, nil
}

// Delete removes a job's log file.
func Delete(dir, jobID string) error {
	err := os.Remove(Path(dir, jobID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("logs: delete: %w", err)
	}
	return nil
}
