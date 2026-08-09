package structure

// Personalized PageRank over the symbol reference graph. Standard power
// iteration with a personalization (teleport) vector: rank flows along
// references, teleports land on the seed symbols — chat-focused paths and
// dirty files — so "important near what you're working on" outranks
// "important in the abstract" when seeds exist. Dangling mass (symbols with
// no outgoing refs) redistributes via the same teleport vector.

const (
	prDamping = 0.85
	prIters   = 40
	prEpsilon = 1e-9
)

// PageRank returns one score per symbol. personal maps symbol index → EXTRA
// teleport weight, blended on top of a uniform base (p[i] ∝ 1 + personal[i]):
// seeds zoom the map without erasing the global structure — an exclusive
// teleport vector would rank a dirty scratch file above the repo's real hubs.
// Empty/nil personal = plain uniform PageRank.
func PageRank(n int, refs map[int]map[int]float64, personal map[int]float64) []float64 {
	if n == 0 {
		return nil
	}
	p := make([]float64, n)
	pTotal := float64(n)
	for i := range p {
		p[i] = 1
	}
	for i, w := range personal {
		if i >= 0 && i < n && w > 0 {
			p[i] += w
			pTotal += w
		}
	}
	for i := range p {
		p[i] /= pTotal
	}

	// Out-weight totals for normalization.
	outSum := make([]float64, n)
	for from, tos := range refs {
		for _, w := range tos {
			outSum[from] += w
		}
	}

	rank := make([]float64, n)
	copy(rank, p)
	next := make([]float64, n)
	for iter := 0; iter < prIters; iter++ {
		var dangling float64
		for i := 0; i < n; i++ {
			if outSum[i] <= 0 {
				dangling += rank[i]
			}
			next[i] = 0
		}
		for from, tos := range refs {
			if outSum[from] <= 0 {
				continue
			}
			share := rank[from] / outSum[from]
			for to, w := range tos {
				next[to] += prDamping * share * w
			}
		}
		var delta float64
		for i := 0; i < n; i++ {
			next[i] += (1-prDamping)*p[i] + prDamping*dangling*p[i]
			d := next[i] - rank[i]
			if d < 0 {
				d = -d
			}
			delta += d
		}
		rank, next = next, rank
		if delta < prEpsilon {
			break
		}
	}
	return rank
}
