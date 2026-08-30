package autonomy

import "encoding/json"

type StructuredFact struct {
	Key                string
	Value              json.RawMessage
	Source             string
	Evidence           []string
	Confidence         float64
	Confirmed          bool
	Sensitivity, Scope string
}

func (f StructuredFact) UsableForExternalRepresentation(policyAllowsInferred bool) bool {
	return f.Confirmed || policyAllowsInferred
}
