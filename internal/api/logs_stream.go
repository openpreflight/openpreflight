// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/openpreflight/openpreflight/internal/logs"
)

const (
	logStreamPoll      = 500 * time.Millisecond
	logStreamHeartbeat = 15 * time.Second
)

// streamJobLogs tails a job log over SSE. Auth matches getJobLogs. Polls the
// file (no fsnotify). First events are the bytes already on disk, then appends.
func (s *Server) streamJobLogs(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobLogAccess(w, r)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flush(w)

	max := int64(0)
	if settings, err := s.store.Settings(); err == nil {
		max = settings.MaxLogBytes
	}

	var offset, sent int64
	send := func() error {
		for {
			select {
			case <-r.Context().Done():
				return r.Context().Err()
			default:
			}
			if max > 0 && sent >= max {
				return nil
			}
			data, next, err := logs.ReadFrom(s.cfg.LogDir(), job.ID, offset)
			if err != nil {
				return err
			}
			if len(data) == 0 {
				offset = next
				return nil
			}
			if max > 0 && sent+int64(len(data)) > max {
				data = data[:max-sent]
				next = offset + int64(len(data))
			}
			writeSSEData(w, data)
			flush(w)
			sent += int64(len(data))
			offset = next
		}
	}

	if err := send(); err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.log.Error("log stream", "job", job.ID, "error", err)
		return
	}
	if !job.InFlight() {
		writeSSEFinished(w)
		flush(w)
		return
	}

	poll := time.NewTicker(logStreamPoll)
	defer poll.Stop()
	heartbeat := time.NewTicker(logStreamHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flush(w)
		case <-poll.C:
			latest, err := s.store.Job(job.ID)
			if err != nil {
				writeSSEFinished(w)
				flush(w)
				return
			}
			job = latest
			if err := send(); err != nil {
				if r.Context().Err() != nil {
					return
				}
				s.log.Error("log stream", "job", job.ID, "error", err)
				return
			}
			if !job.InFlight() {
				writeSSEFinished(w)
				flush(w)
				return
			}
		}
	}
}

func writeSSEData(w http.ResponseWriter, chunk []byte) {
	for _, line := range bytes.Split(chunk, []byte{'\n'}) {
		w.Write([]byte("data: "))
		w.Write(line)
		w.Write([]byte("\n"))
	}
	w.Write([]byte("\n"))
}

func writeSSEFinished(w http.ResponseWriter) {
	fmt.Fprint(w, "event: finished\ndata: \n\n")
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
