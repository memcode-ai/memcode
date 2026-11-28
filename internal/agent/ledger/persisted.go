package ledger

import (
	"encoding/json"
	"sort"

	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/store"
)

// PersistedCacheSummary aggregates token and cache utilization across
// persisted agent_session_finished events from state.db.
type PersistedCacheSummary struct {
	Sessions   int
	Total      llm.Stat
	ByProvider map[string]llm.BackendStat
}

// ProviderRows returns ByProvider sorted by total input descending, then name.
func (p PersistedCacheSummary) ProviderRows() []BackendSpend {
	out := make([]BackendSpend, 0, len(p.ByProvider))
	for k, st := range p.ByProvider {
		out = append(out, BackendSpend{
			Backend:    k,
			Calls:      st.Calls,
			In:         st.In,
			Out:        st.Out,
			CacheRead:  st.CacheRead,
			CacheWrite: st.CacheWrite,
			USD:        st.USD,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].TotalInput(), out[j].TotalInput()
		if ti != tj {
			return ti > tj
		}
		return out[i].Backend < out[j].Backend
	})
	return out
}

type persistedBackend struct {
	Calls      int     `json:"calls"`
	In         int     `json:"in"`
	Out        int     `json:"out"`
	CacheRead  int     `json:"cache_read"`
	CacheWrite int     `json:"cache_write"`
	USD        float64 `json:"usd"`
}

type persistedFinishPayload struct {
	InputTokens      int                         `json:"input_tokens"`
	OutputTokens     int                         `json:"output_tokens"`
	CacheReadTokens  int                         `json:"cache_read_tokens"`
	CacheWriteTokens int                         `json:"cache_write_tokens"`
	EstCostUSD       float64                     `json:"est_cost_usd"`
	ByBackend        map[string]persistedBackend `json:"by_backend"`
}

// SummarizePersistedCache computes cumulative and per-provider cache utilization
// from persisted agent_session_finished events (state.db). Legacy events without
// by_backend attribute their totals to "unknown" (or modelVendor if provided in
// a preceding agent_session_started event for the same session_id).
func SummarizePersistedCache(evs []store.Event) PersistedCacheSummary {
	out := PersistedCacheSummary{ByProvider: map[string]llm.BackendStat{}}
	sessionModel := map[string]string{}
	for _, ev := range evs {
		if ev.Kind == "agent_session_started" {
			var p struct {
				SessionID string `json:"session_id"`
				Model     string `json:"model"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil && p.SessionID != "" && p.Model != "" {
				sessionModel[p.SessionID] = p.Model
			}
			continue
		}
		if ev.Kind != "agent_session_finished" {
			continue
		}
		var fp persistedFinishPayload
		if json.Unmarshal(ev.Payload, &fp) != nil {
			continue
		}
		if fp.InputTokens == 0 && fp.OutputTokens == 0 && fp.CacheReadTokens == 0 && fp.CacheWriteTokens == 0 && len(fp.ByBackend) == 0 {
			continue
		}
		out.Sessions++
		out.Total.Calls++
		out.Total.In += fp.InputTokens
		out.Total.Out += fp.OutputTokens
		out.Total.CacheRead += fp.CacheReadTokens
		out.Total.CacheWrite += fp.CacheWriteTokens
		out.Total.USD += fp.EstCostUSD

		if len(fp.ByBackend) > 0 {
			for name, b := range fp.ByBackend {
				st := out.ByProvider[name]
				st.Calls += b.Calls
				st.In += b.In
				st.Out += b.Out
				st.CacheRead += b.CacheRead
				st.CacheWrite += b.CacheWrite
				st.USD += b.USD
				out.ByProvider[name] = st
			}
			continue
		}
		var sid struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(ev.Payload, &sid)
		prov := inferProviderFromModel(sessionModel[sid.SessionID])
		st := out.ByProvider[prov]
		st.Calls++
		st.In += fp.InputTokens
		st.Out += fp.OutputTokens
		st.CacheRead += fp.CacheReadTokens
		st.CacheWrite += fp.CacheWriteTokens
		st.USD += fp.EstCostUSD
		out.ByProvider[prov] = st
	}
	return out
}

func inferProviderFromModel(model string) string {
	switch {
	case len(model) >= 6 && model[:6] == "claude":
		return "anthropic"
	case len(model) >= 6 && model[:6] == "gemini":
		return "gemini"
	case len(model) >= 3 && model[:3] == "gpt":
		return "openai"
	case len(model) >= 4 && model[:4] == "grok":
		return "grok"
	default:
		return "unknown"
	}
}
