package personal

import "encoding/json"

type Observation struct {
	ResourceID, Kind string
	Value            json.RawMessage
	Evidence         []string
	Sensitive        bool
}
type Environment struct {
	Resources    []ResourceGrantModel
	Facts        []StructuredFact
	Observations []Observation
}
