// Package webhook receives GitLab webhook deliveries (Merge Request Hook,
// Note Hook and Pipeline Hook): it checks the secret, decodes the payloads
// into internal/events values and enqueues the jobs the events.Router
// returns.
package webhook

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// MaxBodyBytes caps the accepted webhook payload size.
const MaxBodyBytes = 5 << 20

// Options configures the handler.
type Options struct {
	// Secret is compared with the X-Gitlab-Token header.
	Secret string
	// Router classifies the decoded events (internal/events; the poller
	// shares it). Nil means events.NewRouter(events.Options{}): no mention,
	// so notes produce no jobs.
	Router *events.Router
	// Enqueue receives accepted jobs. It must not block for long.
	Enqueue func(core.Job)
	Logger  *slog.Logger
}

type handler struct {
	opts   Options
	router *events.Router
	log    *slog.Logger
}

// NewHandler returns the webhook HTTP handler.
func NewHandler(o Options) http.Handler {
	h := &handler{opts: o, router: o.Router, log: o.Logger}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.router == nil {
		h.router = events.NewRouter(events.Options{})
	}
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.Header.Get("X-Gitlab-Token")
	if h.opts.Secret == "" || subtle.ConstantTimeCompare([]byte(token), []byte(h.opts.Secret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	event := r.Header.Get("X-Gitlab-Event")
	job, why, err := h.classify(event, body)
	if err != nil {
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}
	sp := tracing.SpanFromContext(r.Context()) // the request span (tracing on)
	if job != nil {
		// The job's own trace links to this request.
		job.TraceParent = tracing.TraceParent(r.Context())
		sp.SetAttr(tracing.String("reviewbot.webhook.result", "accepted"), tracing.String("job.kind", string(job.Kind)),
			tracing.Int64("gitlab.project.id", job.ProjectID), tracing.Int64("gitlab.mr.iid", job.MRIID))
		h.log.Info("webhook: job accepted", "event", event, "kind", job.Kind, "mr", job.MRKey(), "user", job.Username)
		if h.opts.Enqueue != nil {
			h.opts.Enqueue(*job)
		}
		writeOK(w, "accepted")
		return
	}
	sp.SetAttr(tracing.String("reviewbot.webhook.result", "ignored"))
	h.log.Debug("webhook: event ignored", "event", event, "reason", why)
	writeOK(w, "ignored: "+why)
}

// classify decodes the payload of a GitLab event and returns its job (routed
// and filtered by the Router), or nil and the reason it is ignored. err is a
// malformed payload.
func (h *handler) classify(event string, body []byte) (*core.Job, string, error) {
	switch event {
	case "Merge Request Hook":
		var p mrPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, "", err
		}
		job, why := h.mrJob(&p)
		return job, why, nil
	case "Note Hook":
		var p notePayload
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, "", err
		}
		job, why := h.noteJob(&p)
		return job, why, nil
	case "Pipeline Hook":
		var p pipelinePayload
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, "", err
		}
		job, why := h.router.Pipeline(p.event())
		return job, why, nil
	default:
		return nil, "unsupported event", nil
	}
}

func writeOK(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, msg+"\n")
}

// ---------------------------------------------------------------------------
// Payloads (only the fields we use)
// ---------------------------------------------------------------------------

type project struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
}

type user struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type boolChange struct {
	Previous *bool `json:"previous"`
	Current  *bool `json:"current"`
}

type stringChange struct {
	Previous *string `json:"previous"`
	Current  *string `json:"current"`
}

type mrPayload struct {
	ObjectKind       string  `json:"object_kind"`
	User             user    `json:"user"`
	Project          project `json:"project"`
	ObjectAttributes struct {
		IID            int64  `json:"iid"`
		State          string `json:"state"`
		Action         string `json:"action"`
		Draft          bool   `json:"draft"`
		WorkInProgress bool   `json:"work_in_progress"`
		OldRev         string `json:"oldrev"`
		LastCommit     struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
	Changes struct {
		Draft          *boolChange   `json:"draft"`
		WorkInProgress *boolChange   `json:"work_in_progress"`
		Title          *stringChange `json:"title"`
	} `json:"changes"`
}

type notePayload struct {
	ObjectKind       string  `json:"object_kind"`
	User             user    `json:"user"`
	ProjectID        int64   `json:"project_id"`
	Project          project `json:"project"`
	ObjectAttributes struct {
		ID           int64  `json:"id"`
		Note         string `json:"note"`
		NoteableType string `json:"noteable_type"`
		System       bool   `json:"system"`
		DiscussionID string `json:"discussion_id"`
		Type         string `json:"type"` // DiscussionNote | DiffNote | "" (see thread.go)
	} `json:"object_attributes"`
	MergeRequest *struct {
		IID int64 `json:"iid"`
	} `json:"merge_request"`
}

// projectID is the project of the note (project.id, else the older
// top-level project_id).
func (p *notePayload) projectID() int64 {
	if p.Project.ID != 0 {
		return p.Project.ID
	}
	return p.ProjectID
}

// ---------------------------------------------------------------------------
// Merge Request Hook
// ---------------------------------------------------------------------------

// mrJob routes a Merge Request Hook.
func (h *handler) mrJob(p *mrPayload) (*core.Job, string) {
	a := &p.ObjectAttributes
	e := events.MREvent{ProjectID: p.Project.ID, ProjectPath: p.Project.PathWithNamespace, IID: a.IID, State: a.State,
		Draft: a.Draft || a.WorkInProgress, Action: a.Action, HeadSHA: a.LastCommit.ID, OldRev: a.OldRev,
		BecameReady: becameReady(p.Changes.Draft) || becameReady(p.Changes.WorkInProgress)}
	if c := p.Changes.Title; titleChanged(c) {
		e.OldTitle, e.NewTitle = *c.Previous, *c.Current
	}
	return h.router.MR(e)
}

func titleChanged(c *stringChange) bool {
	return c != nil && c.Previous != nil && c.Current != nil && *c.Previous != *c.Current
}

func becameReady(c *boolChange) bool {
	return c != nil && c.Previous != nil && c.Current != nil && *c.Previous && !*c.Current
}

// ---------------------------------------------------------------------------
// Note Hook
// ---------------------------------------------------------------------------

// noteJob routes a Note Hook; only notes on merge requests are events.
func (h *handler) noteJob(p *notePayload) (*core.Job, string) {
	a := &p.ObjectAttributes
	if a.NoteableType != "MergeRequest" || p.MergeRequest == nil {
		return nil, "not a merge request note"
	}
	return h.router.Note(events.NoteEvent{ProjectID: p.projectID(), ProjectPath: p.Project.PathWithNamespace,
		IID: p.MergeRequest.IID, NoteID: a.ID, DiscussionID: a.DiscussionID, Body: a.Note, System: a.System,
		Threaded: isThreadedNote(p), UserID: p.User.ID, Username: p.User.Username})
}

// isThreadedNote reports whether the note belongs to a threaded discussion:
// object_attributes.type is "DiscussionNote" (a threaded discussion) or
// "DiffNote" (a diff thread), versus null for a plain, non-threaded comment
// (see internal/events/thread.go).
func isThreadedNote(p *notePayload) bool {
	return p.ObjectAttributes.Type == "DiscussionNote" || p.ObjectAttributes.Type == "DiffNote"
}
