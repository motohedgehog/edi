package monitor

import (
	"errors"
	"strings"
	"time"

	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/tfl"
)

// RenderAutomaticAlert reserves every severity label and the observation time,
// then shares the remaining message budget among descriptions.
func RenderAutomaticAlert(line AlertLine, observed time.Time, timezone string) (message.Chunk, error) {
	name := strings.TrimSpace(line.DisplayName)
	if name == "" {
		name = line.LineID
	}
	statuses := line.Statuses
	if len(statuses) == 0 {
		statuses = []tfl.NormalizedStatus{{Description: "Unknown status"}}
	}
	blocks := make([]statusBlock, len(statuses))
	for i, status := range statuses {
		indicator := ClassifyStatus(status)
		if line.Kind == "recovery" {
			indicator = StatusIndicator{Icon: "✅", Label: "Good Service restored"}
		}
		blocks[i] = statusBlock{lineID: line.LineID, lineName: name, indicator: indicator}
	}
	timestamp := observed.In(locationFor(timezone)).Format("02 Jan · 15:04 MST")
	const omitted = "[description omitted]"
	// Reserve omission markers before spending any space on descriptions.
	for i, status := range statuses {
		if strings.TrimSpace(status.Reason) != "" {
			blocks[i].reason = omitted
		}
	}
	if messageLength(blocks, timestamp, "") > MaxTelegramMessage {
		return message.Chunk{}, errors.New("automatic alert labels exceed message limit")
	}
	for i, status := range statuses {
		reason := []rune(safeText(strings.TrimSpace(status.Reason)))
		if len(reason) == 0 {
			continue
		}
		blocks[i].reason = string(reason)
		if messageLength(blocks, timestamp, "") <= MaxTelegramMessage {
			continue
		}
		low, high := 0, len(reason)
		for low < high {
			mid := low + (high-low+1)/2
			blocks[i].reason = string(reason[:mid]) + "… " + omitted
			if messageLength(blocks, timestamp, "") <= MaxTelegramMessage {
				low = mid
			} else {
				high = mid - 1
			}
		}
		blocks[i].reason = omitted
		if low > 0 {
			blocks[i].reason = string(reason[:low]) + "… " + omitted
		}
	}
	return message.Chunk{Body: renderHTML(blocks, timestamp, ""), Format: message.FormatHTML}, nil
}
