// Package jobs runs the console's long operations in the background.
//
// Creating an instance, taking a backup, uploading to S3 or rebuilding an
// instance can take minutes. The HTTP handler enqueues a job and returns
// immediately; a small in-process worker pool does the work and the UI follows
// progress by polling the job. No external queue is required.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/models"
)

// Handler performs the work for one job kind.
type Handler func(ctx context.Context, job *models.Job) error

// Manager owns the worker pool and the handler registry.
type Manager struct {
	db      *database.DB
	log     *slog.Logger
	workers int

	mu       sync.RWMutex
	handlers map[string]Handler

	queue  chan string
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a job manager with the requested worker count.
func New(db *database.DB, log *slog.Logger, workers int) *Manager {
	if workers < 1 {
		workers = 2
	}
	return &Manager{
		db:       db,
		log:      log,
		workers:  workers,
		handlers: map[string]Handler{},
		queue:    make(chan string, 256),
	}
}

// Register installs the handler for a job kind.
//
// Services register their handlers from main, which keeps this package free of
// dependencies on the service layer.
func (m *Manager) Register(kind string, h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[kind] = h
}

func (m *Manager) handler(kind string) (Handler, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.handlers[kind]
	return h, ok
}

// Start launches the worker pool and requeues anything left over from a
// previous run.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)

	// Jobs interrupted by a restart cannot be resumed, so mark them failed.
	if n, err := m.db.FailStaleJobs(); err != nil {
		m.log.Warn("could not reset stale jobs", "err", err)
	} else if n > 0 {
		m.log.Info("marked interrupted jobs as failed", "count", n)
	}

	for i := 0; i < m.workers; i++ {
		m.wg.Add(1)
		go m.worker(i)
	}

	// Requeue jobs that were still queued.
	pending, err := m.db.ListActiveJobs()
	if err != nil {
		return err
	}
	for _, job := range pending {
		if job.Status == "queued" {
			m.push(job.ID)
		}
	}

	return nil
}

// Stop waits for in-flight jobs to finish.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	close(m.queue)
	m.wg.Wait()
}

// Enqueue records a new job and schedules it.
func (m *Manager) Enqueue(username, kind, target, payload string) (*models.Job, error) {
	if _, ok := m.handler(kind); !ok {
		return nil, errors.New("no handler registered for job kind " + kind)
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}

	job := &models.Job{
		ID:        id,
		Kind:      kind,
		Target:    target,
		Payload:   payload,
		Status:    "queued",
		Progress:  0,
		Message:   "Queued",
		CreatedBy: username,
		CreatedAt: time.Now(),
	}

	if err := m.db.CreateJob(job); err != nil {
		return nil, err
	}

	if !m.push(id) {
		return nil, errors.New("job queue is full")
	}

	return job, nil
}

// Progress updates a job's progress bar. Safe to call from handlers.
func (m *Manager) Progress(jobID string, pct int, message string) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	if err := m.db.UpdateJobProgress(jobID, pct, message); err != nil {
		m.log.Warn("could not update job progress", "job", jobID, "err", err)
	}
}

// WipePayload blanks a job's stored payload, used once secrets in it have
// been consumed so passwords do not linger in the jobs table.
func (m *Manager) WipePayload(jobID string) {
	if err := m.db.WipeJobPayload(jobID); err != nil {
		m.log.Warn("could not wipe job payload", "job", jobID, "err", err)
	}
}

func (m *Manager) push(id string) bool {
	select {
	case m.queue <- id:
		return true
	default:
		m.log.Error("job queue is full, dropping job", "job", id)
		return false
	}
}

func (m *Manager) worker(n int) {
	defer m.wg.Done()

	for id := range m.queue {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		m.run(id)
	}
}

// run executes a single job, always leaving a terminal status behind.
func (m *Manager) run(id string) {
	job, err := m.db.GetJob(id)
	if err != nil {
		m.log.Error("could not load job", "job", id, "err", err)
		return
	}

	// Reload the payload, which is not part of the list projection.
	payload, err := m.db.JobPayload(id)
	if err == nil {
		job.Payload = payload
	}

	h, ok := m.handler(job.Kind)
	if !ok {
		_ = m.db.FinishJob(id, "failed", "Failed", "no handler for "+job.Kind)
		return
	}

	if err := m.db.StartJob(id); err != nil {
		m.log.Error("could not start job", "job", id, "err", err)
		return
	}

	started := time.Now()
	m.log.Info("job started", "job", id, "kind", job.Kind, "target", job.Target)

	// A single job must not run forever.
	ctx, cancel := context.WithTimeout(m.ctx, 60*time.Minute)
	defer cancel()

	if err := h(ctx, job); err != nil {
		m.log.Error("job failed", "job", id, "kind", job.Kind, "err", err, "took", time.Since(started))
		_ = m.db.FinishJob(id, "failed", "Failed", err.Error())
		return
	}

	m.log.Info("job finished", "job", id, "kind", job.Kind, "took", time.Since(started))
	_ = m.db.FinishJob(id, "done", "Completed", "")
}

func newID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
