package sum

import "testing"

func TestSum(t *testing.T) {
	if got := Sum([]int{1, -2, 3}); got != 2 {
		t.Fatalf("Sum = %d, want 2", got)
	}
}
