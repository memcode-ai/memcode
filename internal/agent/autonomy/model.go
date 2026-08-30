// Package personal implements the domain-general durable state and runtime
// primitives for autonomous agents.
package autonomy

import (
	"encoding/json"
	"time"
)

type Objective struct {
	ID, Description, SuccessCriteria, Status string
	Priority                                 int
	CreatedAt, UpdatedAt                     time.Time
	ReviewAt                                 *time.Time
}

type Subgoal struct {
	ID, ObjectiveID, ParentID, Description, Status, Rationale string
	Priority                                                  int
	Dependencies                                              json.RawMessage
	CreatedAt, UpdatedAt                                      time.Time
}

type Run struct {
	ID, ObjectiveID, SubgoalID, ParentRunID, SessionID string
	Envelope, Outcome, Evidence                        json.RawMessage
	Status                                             string
	CreatedAt, UpdatedAt                               time.Time
}

type Trigger struct {
	ID, ObjectiveID, Kind, Spec, MissedRunPolicy, Status string
	NextDueAt, LastFiredAt                               *time.Time
	CreatedAt, UpdatedAt                                 time.Time
}

type Policy struct {
	ID, ObjectiveID, Hash, Status string
	Version                       int
	Document                      json.RawMessage
	ApprovedAt                    *time.Time
	CreatedAt                     time.Time
}

type Resource struct {
	ID, ObjectiveID, Type, Locator, AccessMode, AuthorizationSource, PolicyHash, Status string
	Constraints                                                                         json.RawMessage
	ExpiresAt                                                                           *time.Time
	CreatedAt, UpdatedAt                                                                time.Time
}

type Action struct {
	ID, ObjectiveID, SubgoalID, RunID, Kind, Target, ConsequenceClass, PolicyHash, Status, IdempotencyKey string
	// JobID links a delegate action to the detached job it spawned, so a later
	// wake can find the action to close out (see Store.ActionForJob).
	JobID                     string
	Request, Result, Evidence json.RawMessage
	CreatedAt, UpdatedAt      time.Time
}

type GeneratedItem struct {
	ID, ObjectiveID, Path, Hash, Purpose, SourceRunID, ParentRevision, ActiveRevision string
	Invocation, Evaluations                                                           json.RawMessage
	LastUsedAt                                                                        *time.Time
	CreatedAt, UpdatedAt                                                              time.Time
}

type Notification struct {
	ID, ObjectiveID, Kind, Status string
	Payload                       json.RawMessage
	CreatedAt, UpdatedAt          time.Time
}
