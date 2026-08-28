package gwconsoleapi

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/strutil"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// The organization's own video jobs.
//
// Every operation here delegates to proxy.VideoJobs. This file decides three
// things and nothing else: whether the caller may act on this organization,
// whether they may see money, and which error shape the answer takes. What a
// cancel means, whether a running job may be deleted, and where the bytes come
// from are the video plane's judgements and are made once, on the other side of
// that value.
//
// The plane is not read-only, which is a deliberate departure from how it was
// first scoped. "Cancel it and you pay nothing" is a commitment made to
// customers, and a commitment reachable only by writing a script against the
// data plane is worth much less than one with a button. Cancelling and deleting
// both only ever reduce what is owed, so neither needs the estimate-and-charge
// machinery a submit would (ADR-0225).

const (
	defaultVideoJobLimit = 20
	maxVideoJobLimit     = 100
)

// videoPlane is the job surface, or the answer for an assembly that has none.
//
// An internal error rather than a 404: the endpoint exists in this contract, and
// "no such thing" would send an operator looking for a routing bug when the real
// fault is a deployment wired without a data plane.
func (s *Server) videoPlane() (*proxy.VideoJobs, error) {
	if s.videoJobs == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonInternal,
			"this deployment has no video plane")
	}
	return s.videoJobs, nil
}

// videoAuthorize resolves the caller's access to the organization and hands
// back its internal id.
//
// The scope runner opens the organization's transaction to answer, and the job
// reads then run outside it. That is not an oversight: every job statement
// carries `org_id` in its own predicate rather than relying on the session
// setting, precisely so that a job id from another organization is
// indistinguishable from one that never existed no matter which connection
// asks.
// It asks for no sensitive dimension. Every member may see that a job exists,
// what was asked for and how it ended -- those are operational facts, the same
// class the request log shows everyone. Whether the *charge* is rendered is
// decided from the access it resolves to, not by refusing the page: requiring
// the financial capability here would have hidden the whole list from a member,
// which answers a question nobody asked.
func (s *Server) videoAuthorize(
	ctx context.Context, orgPublicID OrgID, require orgReadRequirements,
) (pgtype.UUID, orgReadAccess, error) {
	var orgID pgtype.UUID
	var access orgReadAccess
	err := s.scopeRead(ctx, orgPublicID, require,
		func(_ context.Context, _ *gwdb.Queries, org pgtype.UUID, resolved orgReadAccess) error {
			orgID, access = org, resolved
			return nil
		})
	return orgID, access, err
}

// videoAuthorizeWrite is the same for the two operations that change something.
// Access comes back too so that the job returned by a cancel is rendered under
// the same money rule as one that was only read.
func (s *Server) videoAuthorizeWrite(
	ctx context.Context, orgPublicID OrgID,
) (pgtype.UUID, orgReadAccess, error) {
	var orgID pgtype.UUID
	if err := s.scopeWrite(ctx, orgPublicID,
		func(_ context.Context, _ *gwdb.Queries, org pgtype.UUID) error {
			orgID = org
			return nil
		}); err != nil {
		return pgtype.UUID{}, orgReadAccess{}, err
	}
	_, access, err := s.videoAuthorize(ctx, orgPublicID, orgReadRequirements{})
	if err != nil {
		return pgtype.UUID{}, orgReadAccess{}, err
	}
	return orgID, access, nil
}

// videoError turns the video plane's error into this contract's shape.
//
// The code carries across unchanged and the registry supplies the status, so
// `gateway.artifact_gone` is a 410 on both surfaces rather than a 410 on one
// and whatever this file happened to choose on the other.
func videoError(gerr *proxy.Error) error {
	return httpx.ErrCodeDetail(gerr.Code, gerr.Message)
}

func (s *Server) ListVideoJobs(ctx context.Context, req ListVideoJobsRequestObject) (ListVideoJobsResponseObject, error) {
	jobs, err := s.videoPlane()
	if err != nil {
		return nil, err
	}
	orgID, access, err := s.videoAuthorize(ctx, req.OrgId, orgReadRequirements{})
	if err != nil {
		return nil, err
	}
	p := req.Params
	limit := defaultVideoJobLimit
	if p.Limit != nil {
		limit = *p.Limit
	}
	filter := proxy.VideoJobFilter{
		Status:    string(strutil.Deref((*string)(p.Status))),
		ModelSlug: strutil.Deref(p.Model),
		After:     strutil.Deref(p.Cursor),
		Limit:     min(limit, maxVideoJobLimit),
	}
	if p.From != nil {
		filter.From = *p.From
	}
	if p.To != nil {
		filter.To = *p.To
	}
	rows, more, gerr := jobs.List(ctx, orgID, filter)
	if gerr != nil {
		return nil, videoError(gerr)
	}
	// One call for the page, not one per row: resolving each job's route
	// separately made a full page a hundred and one queries.
	details := jobs.Details(ctx, rows)
	items := make([]VideoJob, 0, len(rows))
	for i, row := range rows {
		items = append(items, videoJobDTO(row, details[i], access))
	}
	var next *string
	if more && len(rows) > 0 {
		// The cursor is the last job's own id, and the next page resolves it by
		// reading that row back. An encoded timestamp would have to be trusted
		// to describe a row it is no longer attached to; the row can always
		// answer for its own sort key.
		next = strutil.Ptr(proxy.VideoJobID(rows[len(rows)-1]))
	}
	return ListVideoJobs200JSONResponse{Items: items, NextCursor: next}, nil
}

func (s *Server) GetVideoJob(ctx context.Context, req GetVideoJobRequestObject) (GetVideoJobResponseObject, error) {
	jobs, err := s.videoPlane()
	if err != nil {
		return nil, err
	}
	orgID, access, err := s.videoAuthorize(ctx, req.OrgId, orgReadRequirements{})
	if err != nil {
		return nil, err
	}
	row, gerr := jobs.Get(ctx, orgID, req.VideoId)
	if gerr != nil {
		return nil, videoError(gerr)
	}
	return GetVideoJob200JSONResponse(videoJobDTO(row, jobs.Detail(ctx, row), access)), nil
}

func (s *Server) CancelVideoJob(ctx context.Context, req CancelVideoJobRequestObject) (CancelVideoJobResponseObject, error) {
	jobs, err := s.videoPlane()
	if err != nil {
		return nil, err
	}
	orgID, access, err := s.videoAuthorizeWrite(ctx, req.OrgId)
	if err != nil {
		return nil, err
	}
	row, gerr := jobs.Cancel(ctx, orgID, req.VideoId)
	if gerr != nil {
		return nil, videoError(gerr)
	}
	return CancelVideoJob200JSONResponse(videoJobDTO(row, jobs.Detail(ctx, row), access)), nil
}

func (s *Server) DeleteVideoJob(ctx context.Context, req DeleteVideoJobRequestObject) (DeleteVideoJobResponseObject, error) {
	jobs, err := s.videoPlane()
	if err != nil {
		return nil, err
	}
	orgID, _, err := s.videoAuthorizeWrite(ctx, req.OrgId)
	if err != nil {
		return nil, err
	}
	row, gerr := jobs.Delete(ctx, orgID, req.VideoId)
	if gerr != nil {
		return nil, videoError(gerr)
	}
	return DeleteVideoJob200JSONResponse{Id: proxy.VideoJobID(row), Deleted: true}, nil
}

func (s *Server) GetVideoJobContent(ctx context.Context, req GetVideoJobContentRequestObject) (GetVideoJobContentResponseObject, error) {
	jobs, err := s.videoPlane()
	if err != nil {
		return nil, err
	}
	orgID, _, err := s.videoAuthorize(ctx, req.OrgId, orgReadRequirements{})
	if err != nil {
		return nil, err
	}
	body, info, gerr := jobs.OpenContent(ctx, orgID, req.VideoId)
	if gerr != nil {
		// A film past its retention window is a normal ending, and the generated
		// 410 response is how this contract says so. Everything else falls
		// through to the registry's own status.
		return nil, videoError(gerr)
	}
	return GetVideoJobContent200Videomp4Response{Body: body, ContentLength: info.SizeBytes}, nil
}

// videoJobDTO renders one job for its owner.
//
// The upstream job id and the upstream download URL are absent because they are
// never rendered anywhere, and so is `settlement_state`: what the money did is
// bookkeeping for whoever runs the deployment, and the answer this organization
// asked for is `charged_nano`.
func videoJobDTO(row gwdb.GatewayAsyncJob, d proxy.VideoJobDetail, access orgReadAccess) VideoJob {
	out := VideoJob{
		Id:        proxy.VideoJobID(row),
		Model:     row.ModelSlug,
		Status:    VideoJobStatus(row.Status),
		Progress:  new(int(row.Progress)),
		Prompt:    strutil.Ptr(d.Params.Prompt),
		CreatedAt: row.CreatedAt.Time.UTC(),
		ExpiresAt: row.ExpiresAt.Time.UTC(),
		Cancel:    new(VideoJobCancel(d.Cancel)),
		// The same predicate VideoJobs.Delete enforces. Stated here so the
		// interface can leave the control out rather than offer one that
		// refuses -- and derived from the row rather than re-derived in the
		// client, because the rule is about money and there should be one copy
		// of it.
		//
		// Not gated on access.Finance, unlike ChargedNano below: deleting a job
		// is not a financial action, and a member who may delete has to be told
		// whether they may. The honest consequence is that on a terminal job a
		// false here does say the charge is unresolved -- that much of the
		// settlement state is derivable. What stays gated is the amount, which
		// is what the finance capability is actually about.
		Deletable: proxy.VideoJobDeletable(row),
	}
	if d.Params.NegativePrompt != "" {
		out.NegativePrompt = strutil.Ptr(d.Params.NegativePrompt)
	}
	if d.Params.DurationSeconds > 0 {
		out.DurationSeconds = new(d.Params.DurationSeconds)
	}
	if d.Params.Resolution != "" {
		out.Resolution = strutil.Ptr(d.Params.Resolution)
	}
	if d.Params.AspectRatio != "" {
		out.AspectRatio = strutil.Ptr(d.Params.AspectRatio)
	}
	if d.Params.Audio != nil {
		out.Audio = new(*d.Params.Audio)
	}
	if d.Params.N > 0 {
		out.N = new(d.Params.N)
	}
	out.BilledUnits = new(d.BilledUnits)
	out.BilledUnit = new(VideoJobBilledUnit(d.BilledUnit))
	// A failed or cancelled job is charged nothing, and its row is kept rather
	// than hidden so that the zero is visible: "it failed and you were not
	// billed" is the answer, and an absent row would leave the customer to
	// wonder.
	if access.Finance {
		out.ChargedNano = new(row.ChargedNano)
		out.ChargedCurrency = strutil.Ptr(row.ChargedCurrency)
	}
	if row.ErrorCode != "" || row.ErrorMessage != "" {
		out.Error = &struct {
			Code    string  `json:"code"`
			Message *string `json:"message,omitempty"`
		}{Code: row.ErrorCode, Message: strutil.Ptr(row.ErrorMessage)}
	}
	if row.Status == "completed" {
		// Available means the bytes can still be produced. A stored artifact
		// past its lifecycle rule and an upstream link past its window both
		// read as unavailable here, and the endpoint answers 410 for either.
		available := row.ArtifactKey != "" || row.UpstreamArtifactRef != ""
		out.Artifact = &struct {
			Available   bool    `json:"available"`
			Bytes       *int64  `json:"bytes,omitempty"`
			ContentType *string `json:"content_type,omitempty"`
		}{
			Available:   available,
			Bytes:       new(row.ArtifactBytes),
			ContentType: strutil.Ptr(row.ArtifactContentType),
		}
	}
	out.SubmittedAt = videoTimePtr(row.SubmittedAt)
	out.TerminalAt = videoTimePtr(row.TerminalAt)
	return out
}

func videoTimePtr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}
