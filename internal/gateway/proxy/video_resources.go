package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Reading, cancelling and fetching a video job.
//
// None of these resolves candidates: a job is reached through the route and
// credential pinned on its own row, because an upstream job id means nothing on
// a different upstream account. That is why the video plane needs only one
// surface rather than a second derived one.

const maxVideoJobPage = 100

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// videoCaller authenticates the request and checks its scope. It is the whole
// of what this surface adds over VideoJobs: who is asking, and in which error
// shape the answer comes back.
func (p *Pipeline) videoCaller(r *http.Request) (Identity, *Error) {
	id, gerr := p.auth.Authenticate(r.Context(), CredentialOf(r))
	if gerr != nil {
		return Identity{}, gerr
	}
	if gerr := RequireScope(id, "inference"); gerr != nil {
		return Identity{}, gerr
	}
	return id, nil
}

func parseVideoJobID(raw string) (pgtype.UUID, bool) {
	var u pgtype.UUID
	trimmed := raw
	if len(raw) > 4 && raw[:4] == "vid_" {
		trimmed = raw[4:]
	}
	if err := u.Scan(trimmed); err != nil {
		return pgtype.UUID{}, false
	}
	return u, true
}

// handleVideoGet is a pure read.
//
// It never polls the upstream. Letting a caller's GET drive the poll would
// leave a hold outstanding whenever a caller walks away, and turn a caller
// polling in a tight loop into an amplifier aimed at the upstream. The
// reconciler owns that, on its own schedule.
func (p *Pipeline) handleVideoGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, gerr := p.videoCaller(r)
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		job, gerr := p.VideoJobs().Get(r.Context(), id.OrgID, chi.URLParam(r, "video_id"))
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		writeJSON(w, http.StatusOK, videoJobPayload(job))
	}
}

// handleVideoList pages an organization's jobs, newest first.
func (p *Pipeline) handleVideoList() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, gerr := p.videoCaller(r)
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		filter := VideoJobFilter{After: r.URL.Query().Get("after")}
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
			filter.Limit = n
		}
		jobs, _, gerr := p.VideoJobs().List(r.Context(), id.OrgID, filter)
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		out := make([]videoJobResponse, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, videoJobPayload(j))
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
	}
}

// handleVideoCancel, handleVideoDelete and handleVideoContent authenticate and
// then hand straight to VideoJobs. Everything they used to decide -- what
// cancel means on this model, whether a running job may be deleted, where the
// bytes come from -- lives there now, because the organization console has to
// reach the same answers and two copies of them would be two answers.
func (p *Pipeline) handleVideoCancel() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, gerr := p.videoCaller(r)
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		job, gerr := p.VideoJobs().Cancel(r.Context(), id.OrgID, chi.URLParam(r, "video_id"))
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		writeJSON(w, http.StatusOK, videoJobPayload(job))
	}
}

func (p *Pipeline) handleVideoDelete() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, gerr := p.videoCaller(r)
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		job, gerr := p.VideoJobs().Delete(r.Context(), id.OrgID, chi.URLParam(r, "video_id"))
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": VideoJobID(job), "object": "video", "deleted": true,
		})
	}
}

func (p *Pipeline) handleVideoContent() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, gerr := p.videoCaller(r)
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		body, info, gerr := p.VideoJobs().OpenContent(r.Context(), id.OrgID, chi.URLParam(r, "video_id"))
		if gerr != nil {
			Write(w, SurfaceOpenAI, gerr)
			return
		}
		defer func() { _ = body.Close() }()
		streamArtifact(w, body, info)
	}
}

func streamArtifact(w http.ResponseWriter, body io.Reader, info ArtifactInfo) {
	ct := info.ContentType
	if ct == "" {
		ct = "video/mp4"
	}
	w.Header().Set("Content-Type", ct)
	if info.SizeBytes > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(info.SizeBytes, 10))
	}
	// Range support is deliberately absent for now: the store's read interface
	// has no range parameter, and widening a port for a feature nobody has
	// asked for is speculative.
	w.Header().Set("Accept-Ranges", "none")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		slog.Warn("dataplane: streaming a video artifact was interrupted", "error", err)
	}
}
