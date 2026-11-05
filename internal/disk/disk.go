// Package disk reports free space on the volume that holds a path.
package disk

// Usage is the space on one volume.
type Usage struct {
	Free  uint64 // bytes available to the current user
	Total uint64
}

// FreeFraction returns free space as a fraction of the total.
func (u Usage) FreeFraction() float64 {
	if u.Total == 0 {
		return 1
	}
	return float64(u.Free) / float64(u.Total)
}
