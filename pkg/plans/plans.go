// Package plans is the host-facing contract for managing plans from a
// frontend such as a CLI or TUI. It unifies the two plan systems behind one
// model and one Service: the shared, named plans agents collaborate on
// (pkg/tools/builtin/plan) and the single per-session plan owned by the
// runtime (pkg/tools/builtin/sessionplan).
//
// The package wraps the existing storage rather than duplicating it. Shared
// plans go through a caller-supplied plan.Storage — pass plan.SharedStorage()
// to operate on the same store, and thus the same mutex, as the plan tools of
// agents running in this process. The session plan is read through the
// sessionplan helpers. Session plans have no revisions or optimistic locking
// and belong to their session, so the Service exposes them read/export-only
// and rejects mutations with a typed *UnsupportedError.
package plans

import (
	"context"
	"time"
)

// Scope identifies which plan system a plan belongs to.
type Scope string

const (
	// ScopeShared is the cross-session store of named plans that agents
	// collaborate on. Shared plans are versioned and fully mutable.
	ScopeShared Scope = "shared"
	// ScopeSession is the per-session plan of the "draft, review, execute"
	// workflow. At most one exists per session; it has no versions and is
	// read/export-only through the Service.
	ScopeSession Scope = "session"
)

// Mutable reports whether plans in this scope can be created, updated, and
// deleted through the Service, so a frontend can disable editing up front
// instead of provoking an *UnsupportedError.
func (s Scope) Mutable() bool { return s == ScopeShared }

// Plan is the host-facing view of a plan from either scope.
//
// The JSON tags are a stable, snake_case wire contract for host consumers
// (e.g. the plans CLI --json output). It is deliberately independent of the
// agent tool JSON of pkg/tools/builtin/plan, which keeps its historical
// camelCase fields (updatedAt, bytesWritten) for backward compatibility.
type Plan struct {
	// Scope tells which plan system the plan lives in.
	Scope Scope `json:"scope"`
	// Name is the canonical identity within the scope: the validated plan
	// name for shared plans, the session ID for session plans.
	Name string `json:"name"`
	// Title, Author, and Status are shared-plan metadata, empty for session
	// plans. Status is a free-form lifecycle label with no fixed vocabulary.
	Title  string `json:"title,omitempty"`
	Author string `json:"author,omitempty"`
	Status string `json:"status,omitempty"`
	// Content is the plan body. List returns metadata only, so Content is
	// empty there; Get populates it.
	Content string `json:"content,omitempty"`
	// Version is the optimistic-lock revision of a shared plan. It is nil for
	// session plans, which have no revisions: nil means version-guarded
	// operations are unsupported, not "version zero".
	Version *int `json:"version,omitempty"`
	// UpdatedAt is the time of the last write: the stored timestamp for
	// shared plans, the plan file's modification time for session plans.
	// Zero when unknown (and then omitted from JSON via omitzero, which
	// consults time.Time.IsZero; omitempty would keep the zero struct).
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	// SessionID is the owning session of a session plan, empty for shared
	// plans.
	SessionID string `json:"session_id,omitempty"`
	// Path is the backing file of a session plan. It is empty for shared
	// plans, whose storage backend is pluggable and opaque.
	Path string `json:"path,omitempty"`
}

// Ref addresses a plan in either scope: Name addresses a shared plan,
// SessionID the plan of that session. The field matching Scope must be set.
type Ref struct {
	Scope     Scope
	Name      string
	SessionID string
}

// SharedRef addresses the named shared plan.
func SharedRef(name string) Ref { return Ref{Scope: ScopeShared, Name: name} }

// SessionRef addresses the plan of the given session.
func SessionRef(sessionID string) Ref { return Ref{Scope: ScopeSession, SessionID: sessionID} }

// ListOptions controls List.
type ListOptions struct {
	// SessionID, when non-empty, also includes that session's plan in the
	// listing if one exists. Only the identified session is consulted; plan
	// files left behind by other sessions are never enumerated.
	SessionID string
}

// ListResult is the outcome of List. Warnings carries plans that exist but
// could not be read, so a caller can tell "no plans" apart from "some plans
// failed to load".
type ListResult struct {
	Plans    []Plan   `json:"plans"`
	Warnings []string `json:"warnings,omitempty"`
}

// CreateRequest creates a new shared plan. Create is create-only: the write
// is guarded by expected version 0, so it fails with a *ConflictError instead
// of silently overwriting a plan that already exists.
type CreateRequest struct {
	Ref     Ref
	Content string
	Title   string
	Author  string
	Status  string
}

// UpdateRequest replaces the content of an existing shared plan. Nil metadata
// pointers preserve the previous values; explicit values (including "")
// overwrite them.
//
// ExpectedVersion is the version the caller last read: the write is rejected
// with a *ConflictError when it no longer matches. An explicit nil means
// unconditional replacement and must only be sent when the user deliberately
// chose to force.
type UpdateRequest struct {
	Ref             Ref
	Content         string
	Title           *string
	Author          *string
	Status          *string
	ExpectedVersion *int
}

// SetStatusRequest sets the free-form status of an existing shared plan
// without touching its body. ExpectedVersion follows the same rules as in
// UpdateRequest.
type SetStatusRequest struct {
	Ref             Ref
	Status          string
	ExpectedVersion *int
}

// DeleteRequest removes a shared plan. ExpectedVersion follows the same rules
// as in UpdateRequest: nil deletes unconditionally.
type DeleteRequest struct {
	Ref             Ref
	ExpectedVersion *int
}

// ExportRequest writes a plan's content to a file on disk. Export works for
// both scopes.
type ExportRequest struct {
	Ref  Ref
	Path string
}

// ExportResult reports a completed export. Version is the exported shared
// plan's version, nil for session plans.
type ExportResult struct {
	Scope        Scope  `json:"scope"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	Version      *int   `json:"version,omitempty"`
	BytesWritten int    `json:"bytes_written"`
}

// Service is the host-facing contract for managing plans across both scopes.
// Mutations address shared plans only; a mutation aimed at a session plan
// fails with a typed *UnsupportedError. Failures are reported as the typed
// errors of this package so frontends never classify by error text.
type Service interface {
	// List returns plan metadata (Content is left empty): every shared plan
	// sorted by name and, when opts.SessionID is set, that session's plan
	// first. A missing session plan is simply not included, never an error.
	List(ctx context.Context, opts ListOptions) (ListResult, error)
	// Get returns the full plan, including content. A missing plan is a
	// *NotFoundError in either scope.
	Get(ctx context.Context, ref Ref) (Plan, error)
	// Create adds a new shared plan; a name that already exists fails with a
	// *ConflictError carrying the current version.
	Create(ctx context.Context, req CreateRequest) (Plan, error)
	// Update replaces the content (and optionally metadata) of an existing
	// shared plan, honouring req.ExpectedVersion.
	Update(ctx context.Context, req UpdateRequest) (Plan, error)
	// SetStatus sets the free-form status of an existing shared plan,
	// honouring req.ExpectedVersion.
	SetStatus(ctx context.Context, req SetStatusRequest) (Plan, error)
	// Delete removes a shared plan, honouring req.ExpectedVersion. Deleting a
	// missing plan is a *NotFoundError.
	Delete(ctx context.Context, req DeleteRequest) error
	// Export writes a plan's content to req.Path, creating parent directories
	// as needed and replacing an existing file atomically.
	Export(ctx context.Context, req ExportRequest) (ExportResult, error)
}
