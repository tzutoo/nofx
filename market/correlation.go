package market

import (
	"math"
	"sort"
	"time"
)

// minCorrSamples is the minimum number of aligned return samples required to
// report a correlation as usable. Fewer samples -> ok=false so callers can
// fail-open (do not over-block on sparse data).
const minCorrSamples = 10

// Pearson returns the Pearson correlation coefficient of two equal-length
// samples. A degenerate (constant) sample yields NaN, which callers should
// treat as "no meaningful correlation".
func Pearson(a, b []float64) float64 {
	n := len(a)
	if n == 0 || len(b) != n {
		return math.NaN()
	}
	var ma, mb float64
	for i := 0; i < n; i++ {
		ma += a[i]
		mb += b[i]
	}
	ma /= float64(n)
	mb /= float64(n)
	var num, da, db float64
	for i := 0; i < n; i++ {
		dx := a[i] - ma
		dy := b[i] - mb
		num += dx * dy
		da += dx * dx
		db += dy * dy
	}
	if da == 0 || db == 0 {
		return math.NaN()
	}
	return num / (math.Sqrt(da) * math.Sqrt(db))
}

// SymbolPairCorrelation returns the Pearson correlation of the percentage
// returns between two symbols over the lookback window, aligned by candle open
// time. ok is false when fewer than minCorrSamples aligned returns are
// available or either series is degenerate, so callers can fail-open.
func SymbolPairCorrelation(a, b, timeframe string, lookback time.Duration) (corr float64, ok bool, err error) {
	end := time.Now()
	start := end.Add(-lookback)
	ka, err := GetKlinesRange(a, timeframe, start, end)
	if err != nil {
		return 0, false, err
	}
	kb, err := GetKlinesRange(b, timeframe, start, end)
	if err != nil {
		return 0, false, err
	}

	closeA := map[int64]float64{}
	closeB := map[int64]float64{}
	for _, k := range ka {
		if k.Close > 0 {
			closeA[k.OpenTime] = k.Close
		}
	}
	var times []int64
	for _, k := range kb {
		if k.Close > 0 {
			closeB[k.OpenTime] = k.Close
			if _, ok := closeA[k.OpenTime]; ok {
				times = append(times, k.OpenTime)
			}
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	if len(times) < minCorrSamples {
		return 0, false, nil
	}

	var ra, rb []float64
	for i := 1; i < len(times); i++ {
		pa, pax := closeA[times[i]], closeA[times[i-1]]
		pb, pbx := closeB[times[i]], closeB[times[i-1]]
		if pa <= 0 || pax <= 0 || pb <= 0 || pbx <= 0 {
			continue
		}
		ra = append(ra, (pa-pax)/pax)
		rb = append(rb, (pb-pbx)/pbx)
	}
	if len(ra) < minCorrSamples-1 {
		return 0, false, nil
	}
	c := Pearson(ra, rb)
	if math.IsNaN(c) {
		return 0, false, nil
	}
	return c, true, nil
}
