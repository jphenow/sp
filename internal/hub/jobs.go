package hub

import (
	"fmt"
	"sync"
	"time"
)

// JobState is the lifecycle of a background action.
type JobState string

const (
	// JobRunning means the action is still in flight.
	JobRunning JobState = "running"
	// JobDone means the action completed successfully.
	JobDone JobState = "done"
	// JobFailed means the action returned an error.
	JobFailed JobState = "failed"
)

// Job is one background ladder operation.
type Job struct {
	ID       string    `json:"id"`
	Key      string    `json:"key"`
	State    JobState  `json:"state"`
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}

// jobQueue runs actions in the background and remembers their outcome so a
// phone can poll for it after the tab comes back to the foreground.
type jobQueue struct {
	mu sync.Mutex
	// byID holds finished and running jobs for polling.
	byID map[string]*Job
	// active maps a sprite+action key to the running job, which is what makes
	// repeat taps idempotent.
	active map[string]string
	seq    int
}

// jobRetention is how long a finished job stays pollable. Long enough for a
// phone to come back from the lock screen and read the result.
const jobRetention = 10 * time.Minute

// newJobQueue builds an empty queue.
func newJobQueue() *jobQueue {
	return &jobQueue{
		byID:   map[string]*Job{},
		active: map[string]string{},
	}
}

// start runs fn in the background under a key, returning the job id and
// whether a new job was actually started. When a job for the same key is
// already running, its id comes back with started=false and fn is not run.
func (q *jobQueue) start(key string, fn func() error) (string, bool) {
	q.mu.Lock()
	if id, ok := q.active[key]; ok {
		q.mu.Unlock()
		return id, false
	}
	q.seq++
	id := fmt.Sprintf("j%d", q.seq)
	job := &Job{ID: id, Key: key, State: JobRunning, Started: time.Now()}
	q.byID[id] = job
	q.active[key] = id
	q.sweepLocked()
	q.mu.Unlock()

	go func() {
		err := fn()
		q.mu.Lock()
		defer q.mu.Unlock()
		job.Finished = time.Now()
		if err != nil {
			job.State, job.Error = JobFailed, err.Error()
		} else {
			job.State = JobDone
		}
		delete(q.active, key)
	}()

	return id, true
}

// get returns a job by id.
func (q *jobQueue) get(id string) (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return Job{}, false
	}
	return *job, true
}

// sweepLocked drops finished jobs past the retention window. The caller holds
// the lock.
func (q *jobQueue) sweepLocked() {
	cutoff := time.Now().Add(-jobRetention)
	for id, job := range q.byID {
		if job.State != JobRunning && job.Finished.Before(cutoff) {
			delete(q.byID, id)
		}
	}
}
