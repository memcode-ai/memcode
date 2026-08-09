package sessionlog

// Entity-graph retrieval, HippoRAG-style minus the embeddings: facts records
// carry entity keys, so sessions and entities form a bipartite graph (plus
// entity–entity co-mention edges within a fact). A query seeds its
// stem-matched entities and personalized PageRank flows relevance to the
// sessions that discuss them — surfacing sessions whose wording never
// lexically matches the question ("the dog" finds the session about Biscuit).
// Built in-memory per query from the already-loaded records; nothing is
// persisted beyond the facts records themselves.

import (
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/structure"
)

const (
	pprSeedWeight  = 20.0 // extra teleport mass on a query-matched entity
	pprSessionEdge = 1.0  // session ↔ entity mention
	pprCoMention   = 0.5  // entity ↔ entity within one fact
)

// entityPPRSessions returns session ids ranked by personalized PageRank over
// the facts entity graph, best first. Empty when the corpus has no facts or
// the query seeds no entity — callers treat that as "no signal", never as
// evidence against a session.
func entityPPRSessions(bySession map[string][]Record, qToks []string) []string {
	qSet := map[string]bool{}
	for _, t := range qToks {
		qSet[t] = true
	}
	if len(qSet) == 0 {
		return nil
	}

	sessIdx := map[string]int{}
	entIdx := map[string]int{}
	var nodes int
	var sessIDs []string
	var entNames []string
	refs := map[int]map[int]float64{}
	edge := func(a, b int, w float64) {
		if refs[a] == nil {
			refs[a] = map[int]float64{}
		}
		refs[a][b] += w
	}

	for sid, recs := range bySession {
		sNode := -1
		for _, r := range recs {
			if r.Kind != KindFacts || len(r.Entities) == 0 {
				continue
			}
			if sNode < 0 {
				sNode = nodes
				sessIdx[sid] = sNode
				sessIDs = append(sessIDs, sid)
				nodes++
			}
			factEnts := make([]int, 0, len(r.Entities))
			for _, e := range r.Entities {
				// Entities index under their stemmed form so "dogs" in a
				// question meets "dog" in a fact.
				key := strings.Join(rankTokenize(e), " ")
				if key == "" {
					continue
				}
				eNode, ok := entIdx[key]
				if !ok {
					eNode = nodes
					entIdx[key] = eNode
					entNames = append(entNames, key)
					nodes++
				}
				edge(sNode, eNode, pprSessionEdge)
				edge(eNode, sNode, pprSessionEdge)
				factEnts = append(factEnts, eNode)
			}
			for i := 0; i < len(factEnts); i++ {
				for j := i + 1; j < len(factEnts); j++ {
					edge(factEnts[i], factEnts[j], pprCoMention)
					edge(factEnts[j], factEnts[i], pprCoMention)
				}
			}
		}
	}
	if len(entIdx) == 0 {
		return nil
	}

	personal := map[int]float64{}
	for name, idx := range entIdx {
		for _, tok := range strings.Fields(name) {
			if qSet[tok] {
				personal[idx] = pprSeedWeight
				break
			}
		}
	}
	if len(personal) == 0 {
		return nil
	}

	ranks := structure.PageRank(nodes, refs, personal)
	type sr struct {
		id string
		r  float64
	}
	out := make([]sr, 0, len(sessIDs))
	for _, sid := range sessIDs {
		out = append(out, sr{sid, ranks[sessIdx[sid]]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].r != out[j].r {
			return out[i].r > out[j].r
		}
		return out[i].id < out[j].id
	})
	ids := make([]string, len(out))
	for i, s := range out {
		ids[i] = s.id
	}
	return ids
}
