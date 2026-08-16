package market

import (
	"math"
	"testing"
)

func TestPearson(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	b := []float64{2, 4, 6, 8, 10} // perfect positive
	if c := Pearson(a, b); c < 0.999 {
		t.Fatalf("perfect positive corr = %.4f, want ~1", c)
	}
	c := []float64{5, 4, 3, 2, 1} // perfect negative
	if c2 := Pearson(a, c); c2 > -0.999 {
		t.Fatalf("perfect negative corr = %.4f, want ~-1", c2)
	}
	z := []float64{3, 3, 3, 3, 3} // constant -> NaN
	if !math.IsNaN(Pearson(a, z)) {
		t.Fatal("constant series should yield NaN")
	}
	if !math.IsNaN(Pearson(a, []float64{1, 2})) { // length mismatch -> NaN
		t.Fatal("length mismatch should yield NaN")
	}
}
