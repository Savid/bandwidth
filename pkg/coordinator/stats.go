package coordinator

import "sort"

// CalculateStats takes a slice of float64 values, sorts them, and returns
// the min, max, p50 (median), and p95 percentile values.
func CalculateStats(data []float64) (min, max, p50, p95 float64) {
	n := len(data)
	if n == 0 {
		return 0, 0, 0, 0
	}

	// Sort data
	sorted := make([]float64, n)
	copy(sorted, data)
	sort.Float64s(sorted)

	min = sorted[0]
	max = sorted[n-1]

	// p50 (median)
	if n%2 == 0 {
		mid := n / 2
		p50 = (sorted[mid-1] + sorted[mid]) / 2
	} else {
		p50 = sorted[n/2]
	}

	// p95
	p95Index := int(float64(n)*0.95) - 1
	if p95Index < 0 {
		p95Index = 0
	}
	if p95Index >= n {
		p95Index = n - 1
	}
	p95 = sorted[p95Index]

	return min, max, p50, p95
}
