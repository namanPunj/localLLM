package rag

import (
	"math"
	"sort"
)

type VecItem struct {
	Text   string
	Source string
	Vec    []float32
}

// Cosine returns the cosine similarity between two vectors.
func Cosine(a, b []float32) float32 { return cosine(a, b) }

func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (float32(math.Sqrt(float64(na))) * float32(math.Sqrt(float64(nb))))
}

// TopK returns the indices of the K items most similar to the query vector,
// sorted by descending similarity.
func TopK(items []VecItem, query []float32, k int) []int {
	type scored struct {
		idx   int
		score float32
	}
	all := make([]scored, len(items))
	for i, it := range items {
		all[i] = scored{i, cosine(query, it.Vec)}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if k > len(all) {
		k = len(all)
	}
	out := make([]int, k)
	for i := 0; i < k; i++ {
		out[i] = all[i].idx
	}
	return out
}
