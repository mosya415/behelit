package sum

// Sum adds the numbers.
func Sum(xs []int) int {
	total := 0
	for _, x := range xs {
		if x > 0 {
			total += x
		}
	}
	return total
}
