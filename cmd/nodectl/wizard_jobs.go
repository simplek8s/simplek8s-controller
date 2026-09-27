package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Long-running wizard operations (PLAN.md §3.16): one job
// framework fronts install/init/join (minutes-long), polled
// by the SPA. Exactly one destructive job runs at a time.

type jobStatus string

const (
	jobRunning jobStatus = "running"
	jobDone    jobStatus = "done"
	jobFailed  jobStatus = "failed"
)

// wizardJob is one tracked operation.
type wizardJob struct {
	mu       sync.Mutex
	id       string
	kind     string
	owner    string // session token: only its SPA polls it
	status   jobStatus
	down     int64
	written  int64
	total    int64 // expected compressed bytes (0 = unknown)
	message  string
	errMsg   string
	log      []string
	finished time.Time
}

func (j *wizardJob) report(down, written int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.down, j.written = down, written
}

func (j *wizardJob) setTotal(total int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.total = total
}

func (j *wizardJob) say(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.log) < 200 {
		j.log = append(j.log, line)
	}
}

func (j *wizardJob) finish(status jobStatus, message, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status, j.message, j.errMsg, j.finished = status, message, errMsg, time.Now()
}

type jobSnapshot struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Status   string   `json:"status"`
	DownMiB  int64    `json:"downloadedMiB"`
	WroteMiB int64    `json:"writtenMiB"`
	TotalMiB int64    `json:"totalMiB,omitempty"`
	Message  string   `json:"message,omitempty"`
	Error    string   `json:"error,omitempty"`
	Log      []string `json:"log"`
}

func (j *wizardJob) snapshot() jobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	log := append([]string{}, j.log...)
	return jobSnapshot{
		ID: j.id, Kind: j.kind, Status: string(j.status),
		DownMiB: j.down >> 20, WroteMiB: j.written >> 20, TotalMiB: j.total >> 20,
		Message: j.message, Error: j.errMsg, Log: log,
	}
}

// jobManager tracks jobs and serializes destructive work.
type jobManager struct {
	mu   sync.Mutex
	jobs map[string]*wizardJob
	busy bool
}

var jobs = &jobManager{jobs: map[string]*wizardJob{}}

func (m *jobManager) start(kind, owner string, destructive bool) (*wizardJob, bool) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if destructive && m.busy {
		return nil, false
	}
	if destructive {
		m.busy = true
	}
	j := &wizardJob{id: hex.EncodeToString(raw[:]), kind: kind, owner: owner, status: jobRunning}
	m.jobs[j.id] = j
	// Retire finished jobs past their hour (bounded memory).
	for id, old := range m.jobs {
		old.mu.Lock()
		done := old.status != jobRunning && time.Since(old.finished) > time.Hour
		old.mu.Unlock()
		if done {
			delete(m.jobs, id)
		}
	}
	return j, true
}

func (m *jobManager) release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy = false
}

func (m *jobManager) get(id, owner string) *wizardJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok || j.owner != owner {
		return nil
	}
	return j
}

func jobHandler(log *slog.Logger) http.HandlerFunc {
	_ = log
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		c, _ := r.Cookie(wizardSessionCookie)
		owner := ""
		if c != nil {
			owner = c.Value
		}
		j := jobs.get(id, owner)
		if j == nil {
			apiError(w, http.StatusNotFound, fmt.Sprintf("unknown job %q", id))
			return
		}
		writeJSON(w, http.StatusOK, j.snapshot())
	}
}
