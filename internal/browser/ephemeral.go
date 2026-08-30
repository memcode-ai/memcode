package browser

// EphemeralController identifies the existing fresh-profile backend used by
// ordinary sessions. Session remains the concrete implementation while callers
// migrate behind Controller.
type EphemeralController struct{ *Session }
