// Package alerts tells an operator when something in the console failed.
//
// It posts a small JSON document to one webhook URL. The document carries both
// "text" (Slack, Mattermost) and "content" (Discord) so common chat webhooks
// work without an adapter.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/peaceful/cloud-console/internal/models"
)

// Notifier posts failure events to a webhook.
type Notifier struct {
	url    string
	client *http.Client
	log    *slog.Logger
}

// New returns a Notifier for webhookURL, or nil when it is empty.
func New(webhookURL string, log *slog.Logger) (*Notifier, error) {
	if webhookURL == "" {
		return nil, nil
	}
	u, err := url.Parse(webhookURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("alert webhook must be an http(s) URL")
	}
	return &Notifier{
		url:    webhookURL,
		client: &http.Client{Timeout: 10 * time.Second},
		log:    log,
	}, nil
}

// event is the webhook body.
type event struct {
	Event   string    `json:"event"`
	Text    string    `json:"text"`
	Content string    `json:"content"`
	JobID   string    `json:"job_id,omitempty"`
	Kind    string    `json:"kind,omitempty"`
	Target  string    `json:"target,omitempty"`
	Error   string    `json:"error,omitempty"`
	User    string    `json:"user,omitempty"`
	Time    time.Time `json:"time"`
}

// JobFailed reports a failed background job. It returns at once; delivery
// happens in the background so a slow webhook never holds up a worker. The job
// payload is never sent: it can hold passwords.
func (n *Notifier) JobFailed(job *models.Job, err error) {
	if n == nil || job == nil {
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	text := fmt.Sprintf("Console job failed: %s on %s — %s", job.Kind, job.Target, msg)
	go n.post(event{
		Event:   "job.failed",
		Text:    text,
		Content: text,
		JobID:   job.ID,
		Kind:    job.Kind,
		Target:  job.Target,
		Error:   msg,
		User:    job.CreatedBy,
		Time:    time.Now().UTC(),
	})
}

func (n *Notifier) post(e event) {
	body, err := json.Marshal(e)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		// The URL often carries a secret token, so only the cause is logged.
		n.log.Warn("could not deliver alert", "event", e.Event, "err", errors.Unwrap(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		n.log.Warn("alert webhook refused the event", "event", e.Event, "status", resp.StatusCode)
	}
}
