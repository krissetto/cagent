package plans

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessionplan"
)

// sessionMutationReason makes the unsupported-operation error actionable:
// it names the constraint and what to do instead.
const sessionMutationReason = "session plans have no versions and belong to their session; change the plan from within its session, or use a shared plan for cross-session collaboration"

type service struct {
	storage    plan.Storage
	sessionDir string
}

var _ Service = (*service)(nil)

// Option configures a Service built by NewService.
type Option func(*service)

// WithSessionDir overrides the directory the session plan is read from,
// defaulting to sessionplan.DefaultDir().
func WithSessionDir(dir string) Option {
	return func(s *service) { s.sessionDir = dir }
}

// NewService returns a Service over the given shared-plan storage. Pass
// plan.SharedStorage() to operate on the same store — and serialize on the
// same mutex — as the plan tools of agents running in this process; any other
// plan.Storage yields an isolated service. The storage must not be nil.
func NewService(storage plan.Storage, opts ...Option) Service {
	if storage == nil {
		panic("plans: storage must not be nil")
	}
	s := &service{storage: storage, sessionDir: sessionplan.DefaultDir()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *service) List(ctx context.Context, opts ListOptions) (ListResult, error) {
	// Always a non-nil slice so an empty listing serializes as [] not null.
	result := ListResult{Plans: []Plan{}}

	if opts.SessionID != "" {
		p, ok, warning, err := s.statSessionPlan(opts.SessionID)
		if err != nil {
			return ListResult{}, err
		}
		if warning != "" {
			result.Warnings = append(result.Warnings, warning)
		}
		if ok {
			result.Plans = append(result.Plans, p)
		}
	}

	summaries, warnings, err := s.storage.List(ctx)
	if err != nil {
		return ListResult{}, &StorageError{Scope: ScopeShared, Op: "list", Err: err}
	}
	result.Warnings = append(result.Warnings, warnings...)
	for _, sum := range summaries {
		result.Plans = append(result.Plans, Plan{
			Scope:     ScopeShared,
			Name:      sum.Name,
			Title:     sum.Title,
			Author:    sum.Author,
			Status:    sum.Status,
			Version:   new(sum.Revision),
			UpdatedAt: parseUpdatedAt(sum.UpdatedAt),
		})
	}
	return result, nil
}

func (s *service) Get(ctx context.Context, ref Ref) (Plan, error) {
	switch ref.Scope {
	case ScopeShared:
		return s.getShared(ctx, ref.Name)
	case ScopeSession:
		return s.getSession(ref.SessionID)
	default:
		return Plan{}, invalidScopeError(ref.Scope)
	}
}

func (s *service) Create(ctx context.Context, req CreateRequest) (Plan, error) {
	if err := checkSharedMutation("create", req.Ref); err != nil {
		return Plan{}, err
	}
	if req.Content == "" {
		return Plan{}, &ValidationError{Message: "content must not be empty"}
	}
	// Expected revision 0 makes the write create-only: it conflicts instead
	// of overwriting when the plan already exists.
	p, err := s.storage.Upsert(ctx, plan.UpsertRequest{
		Name:             req.Ref.Name,
		Content:          &req.Content,
		Title:            &req.Title,
		Author:           &req.Author,
		Status:           &req.Status,
		ExpectedRevision: new(0),
	})
	if err != nil {
		return Plan{}, sharedError("create", req.Ref.Name, err)
	}
	return sharedPlan(p), nil
}

func (s *service) Update(ctx context.Context, req UpdateRequest) (Plan, error) {
	if err := checkSharedMutation("update", req.Ref); err != nil {
		return Plan{}, err
	}
	if req.Content == "" {
		return Plan{}, &ValidationError{Message: "content must not be empty"}
	}
	p, err := s.storage.Upsert(ctx, plan.UpsertRequest{
		Name:             req.Ref.Name,
		Content:          &req.Content,
		Title:            req.Title,
		Author:           req.Author,
		Status:           req.Status,
		ExpectedRevision: req.ExpectedVersion,
		MustExist:        true,
	})
	if err != nil {
		return Plan{}, sharedError("update", req.Ref.Name, err)
	}
	return sharedPlan(p), nil
}

func (s *service) SetStatus(ctx context.Context, req SetStatusRequest) (Plan, error) {
	if err := checkSharedMutation("set_status", req.Ref); err != nil {
		return Plan{}, err
	}
	if req.Status == "" {
		return Plan{}, &ValidationError{Message: "status must not be empty"}
	}
	p, err := s.storage.Upsert(ctx, plan.UpsertRequest{
		Name:             req.Ref.Name,
		Status:           &req.Status,
		ExpectedRevision: req.ExpectedVersion,
		MustExist:        true,
	})
	if err != nil {
		return Plan{}, sharedError("set_status", req.Ref.Name, err)
	}
	return sharedPlan(p), nil
}

func (s *service) Delete(ctx context.Context, req DeleteRequest) error {
	if err := checkSharedMutation("delete", req.Ref); err != nil {
		return err
	}
	deleted, err := s.storage.Delete(ctx, req.Ref.Name, req.ExpectedVersion)
	if err != nil {
		return sharedError("delete", req.Ref.Name, err)
	}
	if !deleted {
		return &NotFoundError{Scope: ScopeShared, Name: req.Ref.Name}
	}
	return nil
}

func (s *service) Export(ctx context.Context, req ExportRequest) (ExportResult, error) {
	if req.Path == "" {
		return ExportResult{}, &ValidationError{Message: "path must not be empty"}
	}
	p, err := s.Get(ctx, req.Ref)
	if err != nil {
		return ExportResult{}, err
	}
	if err := writeExportFile(p.Scope, req.Path, p.Content); err != nil {
		return ExportResult{}, err
	}
	return ExportResult{
		Scope:        p.Scope,
		Name:         p.Name,
		Path:         req.Path,
		Version:      p.Version,
		BytesWritten: len(p.Content),
	}, nil
}

func (s *service) getShared(ctx context.Context, name string) (Plan, error) {
	if err := plan.ValidateName(name); err != nil {
		return Plan{}, &ValidationError{Message: err.Error()}
	}
	p, ok, err := s.storage.Get(ctx, name)
	if err != nil {
		return Plan{}, sharedError("get", name, err)
	}
	if !ok {
		return Plan{}, &NotFoundError{Scope: ScopeShared, Name: name}
	}
	return sharedPlan(p), nil
}

func (s *service) getSession(sessionID string) (Plan, error) {
	content, path, err := sessionplan.ReadContent(s.sessionDir, sessionID)
	if err != nil {
		return Plan{}, sessionError("get", sessionID, err)
	}
	p := sessionPlan(sessionID, path, time.Time{})
	p.Content = content
	// The timestamp comes from file metadata; it is best-effort so a racing
	// delete cannot fail a read that already succeeded.
	if info, err := os.Stat(path); err == nil {
		p.UpdatedAt = info.ModTime().UTC()
	}
	return p, nil
}

// statSessionPlan returns list metadata for the session's plan without
// reading its content. A missing plan is (_, false, "", nil); an existing but
// unreadable one is surfaced as a warning, mirroring how List reports
// unreadable shared plans.
func (s *service) statSessionPlan(sessionID string) (p Plan, ok bool, warning string, err error) {
	path, err := sessionplan.Path(s.sessionDir, sessionID)
	if err != nil {
		return Plan{}, false, "", sessionError("list", sessionID, err)
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Plan{}, false, "", nil
	case err != nil:
		return Plan{}, false, fmt.Sprintf("skipped session plan %q: %v", sessionID, err), nil
	case info.IsDir():
		return Plan{}, false, fmt.Sprintf("skipped session plan %q: %s is a directory", sessionID, path), nil
	}
	return sessionPlan(sessionID, path, info.ModTime().UTC()), true, "", nil
}

// checkSharedMutation gates every mutation: session plans are refused with a
// typed, actionable error, unknown scopes are invalid input, and shared names
// are validated with the storage's canonical rule before touching it.
func checkSharedMutation(op string, ref Ref) error {
	switch ref.Scope {
	case ScopeShared:
		if err := plan.ValidateName(ref.Name); err != nil {
			return &ValidationError{Message: err.Error()}
		}
		return nil
	case ScopeSession:
		return &UnsupportedError{Scope: ScopeSession, Op: op, Reason: sessionMutationReason}
	default:
		return invalidScopeError(ref.Scope)
	}
}

// sharedError maps a plan.Storage failure to this package's typed errors by
// the storage contract's own types, never by matching error text.
func sharedError(op, name string, err error) error {
	var conflict *plan.VersionConflictError
	if errors.As(err, &conflict) {
		return &ConflictError{Name: conflict.Name, Expected: conflict.Expected, Current: conflict.Current}
	}
	var corrupt *plan.CorruptPlanError
	if errors.As(err, &corrupt) {
		return &CorruptError{Scope: ScopeShared, Name: name, Err: err}
	}
	if errors.Is(err, plan.ErrPlanNotFound) {
		return &NotFoundError{Scope: ScopeShared, Name: name}
	}
	return &StorageError{Scope: ScopeShared, Op: op, Err: err}
}

func sessionError(op, sessionID string, err error) error {
	switch {
	case errors.Is(err, sessionplan.ErrPlanNotFound):
		return &NotFoundError{Scope: ScopeSession, Name: sessionID}
	case errors.Is(err, sessionplan.ErrInvalidSessionID):
		return &ValidationError{Message: err.Error()}
	default:
		return &StorageError{Scope: ScopeSession, Op: op, Err: err}
	}
}

func invalidScopeError(scope Scope) error {
	return &ValidationError{Message: fmt.Sprintf("invalid plan scope %q: use %q or %q", scope, ScopeShared, ScopeSession)}
}

func sharedPlan(p plan.Plan) Plan {
	return Plan{
		Scope:     ScopeShared,
		Name:      p.Name,
		Title:     p.Title,
		Author:    p.Author,
		Status:    p.Status,
		Content:   p.Content,
		Version:   new(p.Revision),
		UpdatedAt: parseUpdatedAt(p.UpdatedAt),
	}
}

func sessionPlan(sessionID, path string, modTime time.Time) Plan {
	return Plan{
		Scope:     ScopeSession,
		Name:      sessionID,
		SessionID: sessionID,
		UpdatedAt: modTime,
		Path:      path,
	}
}

// parseUpdatedAt tolerates a missing or malformed stored timestamp: it is
// display metadata, so it degrades to the zero time instead of failing a read.
func parseUpdatedAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// writeExportFile mirrors the plan toolset's export behaviour: parent
// directories are created and the write is atomic (temp + rename), so a
// reader never observes a partial export.
func writeExportFile(scope Scope, path, content string) error {
	clean := filepath.Clean(path)
	if info, err := os.Stat(clean); err == nil && info.IsDir() {
		return &ValidationError{Message: fmt.Sprintf("path %q is a directory, not a file", path)}
	}
	if err := os.MkdirAll(filepath.Dir(clean), 0o700); err != nil {
		return &StorageError{Scope: scope, Op: "export", Err: err}
	}
	if err := atomicfile.Write(clean, strings.NewReader(content), 0o600); err != nil {
		return &StorageError{Scope: scope, Op: "export", Err: err}
	}
	return nil
}
