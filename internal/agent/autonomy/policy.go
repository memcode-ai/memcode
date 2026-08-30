package autonomy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

type ConsequenceClass string

const (
	Observe                ConsequenceClass = "observe"
	LocalMutation          ConsequenceClass = "local_mutation"
	ExternalEffect         ConsequenceClass = "external_effect"
	ExternalRepresentation ConsequenceClass = "external_representation"
	Financial              ConsequenceClass = "financial"
	LegalAttestation       ConsequenceClass = "legal_attestation"
	Destructive            ConsequenceClass = "destructive"
)

type DelegationPolicy struct {
	ObjectiveScope      string             `json:"objective_scope"`
	AllowedTools        []string           `json:"allowed_tools,omitempty"`
	FilesystemRoots     map[string]string  `json:"filesystem_roots,omitempty"`
	BrowserOrigins      []string           `json:"browser_origins,omitempty"`
	MCPTools            []string           `json:"mcp_tools,omitempty"`
	ConsequenceClasses  []ConsequenceClass `json:"consequence_classes,omitempty"`
	MaxActionsPerPeriod int                `json:"max_actions_per_period,omitempty"`
	MaxConcurrency      int                `json:"max_concurrency,omitempty"`
	MaxDelegationDepth  int                `json:"max_delegation_depth,omitempty"`
	MaxTokens           int                `json:"max_tokens,omitempty"`
	MaxSeconds          int                `json:"max_seconds,omitempty"`
	GeneratedCode       bool               `json:"generated_code,omitempty"`
	QuietHours          string             `json:"quiet_hours,omitempty"`
	ExpiresAt           *time.Time         `json:"expires_at,omitempty"`
	Revoked             bool               `json:"revoked,omitempty"`
}

func CanonicalPolicy(p DelegationPolicy) ([]byte, string, error) {
	sort.Strings(p.AllowedTools)
	sort.Strings(p.BrowserOrigins)
	sort.Strings(p.MCPTools)
	sort.Slice(p.ConsequenceClasses, func(i, j int) bool { return p.ConsequenceClasses[i] < p.ConsequenceClasses[j] })
	b, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, b); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(compact.Bytes())
	return compact.Bytes(), hex.EncodeToString(sum[:]), nil
}

func (p DelegationPolicy) AllowsConsequence(c ConsequenceClass, now time.Time) bool {
	if p.Revoked || (p.ExpiresAt != nil && !now.Before(*p.ExpiresAt)) {
		return false
	}
	for _, allowed := range p.ConsequenceClasses {
		if allowed == c {
			return true
		}
	}
	return false
}

func IsRestriction(parent, next DelegationPolicy) bool {
	return subset(next.AllowedTools, parent.AllowedTools) &&
		classSubset(next.ConsequenceClasses, parent.ConsequenceClasses) &&
		subset(next.BrowserOrigins, parent.BrowserOrigins) &&
		subset(next.MCPTools, parent.MCPTools) &&
		filesystemRootsSubset(next.FilesystemRoots, parent.FilesystemRoots) &&
		next.MaxConcurrency <= parent.MaxConcurrency &&
		next.MaxDelegationDepth <= parent.MaxDelegationDepth &&
		boundedBy(next.MaxActionsPerPeriod, parent.MaxActionsPerPeriod) &&
		boundedBy(next.MaxTokens, parent.MaxTokens) &&
		boundedBy(next.MaxSeconds, parent.MaxSeconds) &&
		(!next.GeneratedCode || parent.GeneratedCode) &&
		(parent.QuietHours == "" || next.QuietHours == parent.QuietHours)
}

// boundedBy compares budget fields where 0 means "unset — defer to the
// runtime's own default" rather than literally zero (see nonzero() in
// runner_exec.go). A child leaving one unset is never a widening of
// authority; a child that sets an explicit value must not exceed a parent
// value that is itself explicit.
func boundedBy(next, parent int) bool {
	return next == 0 || parent == 0 || next <= parent
}
func NarrowPolicy(parent, child DelegationPolicy) (DelegationPolicy, error) {
	if !IsRestriction(parent, child) {
		return DelegationPolicy{}, fmt.Errorf("delegated policy expands parent authority")
	}
	return child, nil
}
func subset(a, b []string) bool {
	set := map[string]bool{}
	for _, v := range b {
		set[v] = true
	}
	for _, v := range a {
		if !set[v] {
			return false
		}
	}
	return true
}

// filesystemRootsSubset reports whether every root the child grants is also
// granted by the parent, under the same access mode — the child cannot claim
// a path outside the parent's roots, nor upgrade access on one it shares.
func filesystemRootsSubset(child, parent map[string]string) bool {
	for path, mode := range child {
		if parent[path] != mode {
			return false
		}
	}
	return true
}
func classSubset(a, b []ConsequenceClass) bool {
	set := map[ConsequenceClass]bool{}
	for _, v := range b {
		set[v] = true
	}
	for _, v := range a {
		if !set[v] {
			return false
		}
	}
	return true
}
