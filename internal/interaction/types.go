package interaction

import (
	"encoding/json"
	"time"
)

type Kind string

const (
	Question           Kind = "question"
	Approval           Kind = "approval"
	EnvironmentHandoff Kind = "environment_handoff"
	Challenge          Kind = "challenge"
	MissingInformation Kind = "missing_information"
	PolicyException    Kind = "policy_exception"
)

type Status string

const (
	Pending   Status = "pending"
	Answered  Status = "answered"
	Cancelled Status = "cancelled"
	Expired   Status = "expired"
)

type Interaction struct {
	ID, RunID, JobID, SessionID, Channel, Conversation, ToolUseID string
	Kind                                                          Kind
	Request, Response, Continuation                               json.RawMessage
	Status                                                        Status
	PolicyVersion                                                 int
	CreatedAt                                                     time.Time
	ExpiresAt, AnsweredAt, CancelledAt                            *time.Time
}
