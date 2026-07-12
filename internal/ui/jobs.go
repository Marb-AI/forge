package ui

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// A job is a long-running operation whose output is the point: registering a
// server (minutes of package installs, and it prints the SSH key you must put on
// GitHub) or checkpointing a session (Claude writes a handoff, then restarts).
// Both would be a lying spinner if we just fired and forgot, so both run as jobs:
// the work streams its output to followers over SSE and ends with a definite
// success/failure — exactly what you'd watch in a terminal.
//
// The state change is a POST (Origin-checked); the stream is a GET keyed by the
// job id it returns, because EventSource can only do GETs.

// jobTTL is how long a finished job's output is kept: long enough for a browser
// to reconnect and read the tail, short enough that a long-lived daemon doesn't
// accumulate them.
const jobTTL = 10 * time.Minute

// job collects an operation's output and lets SSE readers follow along. It is an
// io.Writer, so it plugs straight into anything that reports progress that way.
type job struct {
	mu     sync.Mutex
	chunks []string
	done   bool
	err    error
	ch     chan struct{} // closed (and replaced) on every update; the broadcast
}

func newJob() *job { return &job{ch: make(chan struct{})} }

// Write appends output and wakes every follower.
func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	j.chunks = append(j.chunks, string(p))
	old := j.ch
	j.ch = make(chan struct{})
	j.mu.Unlock()
	close(old)
	return len(p), nil
}

func (j *job) finish(err error) {
	j.mu.Lock()
	j.done, j.err = true, err
	old := j.ch
	j.ch = make(chan struct{})
	j.mu.Unlock()
	close(old)
}

// since returns output from index i onwards, the terminal state, and a channel
// that closes on the next update.
func (j *job) since(i int) (chunks []string, next int, done bool, wait chan struct{}, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if i < len(j.chunks) {
		chunks = append(chunks, j.chunks[i:]...)
	}
	return chunks, len(j.chunks), j.done, j.ch, j.err
}

// startJob runs fn in the background, streaming whatever it writes to out, and
// returns the id a browser follows it by. The job is evicted jobTTL after it
// finishes so the registry can't grow without bound.
func (s *server) startJob(fn func(out io.Writer) error) (string, error) {
	id, err := newJobID()
	if err != nil {
		return "", err
	}
	j := newJob()

	s.jobMu.Lock()
	s.jobs[id] = j
	s.jobMu.Unlock()

	go func() {
		j.finish(fn(j))
		time.AfterFunc(jobTTL, func() {
			s.jobMu.Lock()
			delete(s.jobs, id)
			s.jobMu.Unlock()
		})
	}()
	return id, nil
}

func (s *server) getJob(id string) *job {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	return s.jobs[id]
}

// handleJobStream streams a job's output as SSE. Output chunks are base64 (they
// contain newlines, which SSE frames on), and a final "done" event carries the
// outcome — so the browser always learns whether it worked, never just stops.
func (s *server) handleJobStream(w http.ResponseWriter, r *http.Request) {
	j := s.getJob(r.PathValue("id"))
	if j == nil {
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	i := 0
	for {
		chunks, next, done, wait, jobErr := j.since(i)
		i = next
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", base64.StdEncoding.EncodeToString([]byte(c)))
		}
		if len(chunks) > 0 {
			flusher.Flush()
		}
		if done {
			msg := ""
			if jobErr != nil {
				msg = jobErr.Error()
			}
			payload, _ := json.Marshal(map[string]string{"error": msg})
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", payload)
			flusher.Flush()
			return
		}
		select {
		case <-ctx.Done():
			return // browser went away; the job keeps running
		case <-wait:
		}
	}
}

func newJobID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
