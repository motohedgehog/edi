package tfl

import (
	"fmt"
	"slices"
)

// SeverityKey compares severity sets and availability independently of descriptions.
func SeverityKey(line LineStatus) string {
	codes := make([]int, 0, len(line.Statuses))
	for _, status := range line.Statuses {
		codes = append(codes, status.Severity)
	}
	slices.Sort(codes)
	return fmt.Sprintf("%t:%t:%v", line.Good, line.Unknown, slices.Compact(codes))
}
