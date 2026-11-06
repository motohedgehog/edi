package monitor

import (
	"fmt"
	"html"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/tfl"
)

const MaxTelegramMessage = 4000

type AlertLine struct {
	LineID      string
	DisplayName string
	Kind        string
	Statuses    []tfl.NormalizedStatus
}

type StatusIndicator struct {
	Icon  string
	Label string
}

type statusBlock struct {
	lineID    string
	lineName  string
	indicator StatusIndicator
	reason    string
}

func ClassifyStatus(status tfl.NormalizedStatus) StatusIndicator {
	label := strings.TrimSpace(status.Description)
	category := normalizeStatusCategory(label)
	if category == "" {
		return StatusIndicator{Icon: "❓", Label: "Unknown status"}
	}
	if category == "unknown" || category == "unknown status" {
		return StatusIndicator{Icon: "❓", Label: label}
	}
	switch category {
	case "good service":
		if !status.Known {
			return StatusIndicator{Icon: "❓", Label: "Unknown status"}
		}
		return StatusIndicator{Icon: "✅", Label: label}
	case "severe delays", "suspended", "service suspended", "part suspended", "part-suspended", "closed", "service closed", "part closure":
		return StatusIndicator{Icon: "❌", Label: label}
	default:
		return StatusIndicator{Icon: "⚠️", Label: label}
	}
}

func normalizeStatusCategory(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func RenderStatusCheck(lines []AlertLine, observedAt time.Time, timezone string) []message.Chunk {
	return renderStatus(lines, observedAt, timezone)
}

func RenderAvailability(restored bool, failedChecks int, observedAt time.Time, timezone string) []message.Chunk {
	location := locationFor(timezone)
	timestamp := observedAt.In(location).Format("02 Jan · 15:04 MST")
	var body strings.Builder
	if restored {
		body.WriteString("✅ <b>TfL updates restored</b>")
	} else {
		body.WriteString("⚠️ <b>TfL updates unavailable</b>")
		if failedChecks > 0 {
			_, _ = fmt.Fprintf(&body, "\nTfL updates did not respond after %d failed checks.", failedChecks)
		}
	}
	body.WriteString("\n\n<i>Observed ")
	body.WriteString(escapeHTML(timestamp))
	body.WriteString("</i>")
	return []message.Chunk{{Body: body.String(), Format: message.FormatHTML}}
}

func renderStatus(lines []AlertLine, observedAt time.Time, timezone string) []message.Chunk {
	if len(lines) == 0 {
		return nil
	}
	timestamp := observedAt.In(locationFor(timezone)).Format("02 Jan · 15:04 MST")
	blocks := make([]statusBlock, 0, len(lines))
	for _, line := range lines {
		lineName := strings.TrimSpace(line.DisplayName)
		if lineName == "" {
			lineName = strings.TrimSpace(line.LineID)
		}
		if lineName == "" {
			lineName = "Unknown line"
		}
		statuses := line.Statuses
		if len(statuses) == 0 {
			statuses = []tfl.NormalizedStatus{{Description: "Unknown status"}}
		}
		for _, status := range statuses {
			indicator := ClassifyStatus(status)
			if line.Kind == "recovery" {
				indicator = StatusIndicator{Icon: "✅", Label: "Good Service restored"}
			}
			blocks = append(blocks, statusBlock{lineID: line.LineID, lineName: lineName,
				indicator: indicator, reason: strings.TrimSpace(status.Reason)})
		}
	}
	return renderBlocks(blocks, timestamp)
}

func renderBlocks(blocks []statusBlock, timestamp string) []message.Chunk {
	blocks = splitOversizedReasons(blocks, timestamp)
	groups := make([][]statusBlock, 0, 1)
	current := make([]statusBlock, 0, len(blocks))
	const longestPartLabel = "Part 999/999"
	for _, block := range blocks {
		candidate := append(append([]statusBlock(nil), current...), block)
		if len(current) > 0 && messageLength(candidate, timestamp, longestPartLabel) > MaxTelegramMessage {
			groups = append(groups, current)
			current = []statusBlock{block}
			continue
		}
		current = candidate
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}

	chunks := make([]message.Chunk, 0, len(groups))
	for i, group := range groups {
		partLabel := ""
		if len(groups) > 1 {
			partLabel = fmt.Sprintf("Part %d/%d", i+1, len(groups))
		}
		body := renderHTML(group, timestamp, partLabel)
		chunks = append(chunks, message.Chunk{Body: body, Format: message.FormatHTML})
	}
	return chunks
}

func splitOversizedReasons(blocks []statusBlock, timestamp string) []statusBlock {
	const longestPartLabel = "Part 999/999"
	result := make([]statusBlock, 0, len(blocks))
	for _, block := range blocks {
		if messageLength([]statusBlock{block}, timestamp, longestPartLabel) <= MaxTelegramMessage {
			result = append(result, block)
			continue
		}
		runes := []rune(block.reason)
		if len(runes) == 0 {
			result = append(result, block)
			continue
		}
		for start := 0; start < len(runes); {
			low, high := start+1, len(runes)
			best := start
			for low <= high {
				middle := low + (high-low)/2
				piece := block
				piece.reason = string(runes[start:middle])
				if messageLength([]statusBlock{piece}, timestamp, longestPartLabel) <= MaxTelegramMessage {
					best = middle
					low = middle + 1
				} else {
					high = middle - 1
				}
			}
			if best == start {
				// TfL line names and categories are short; this fallback only applies
				// to an unusually long reason whose fixed labels already fill a chunk.
				best = min(start+1, len(runes))
			}
			piece := block
			piece.reason = string(runes[start:best])
			result = append(result, piece)
			start = best
		}
	}
	return result
}

func renderHTML(blocks []statusBlock, timestamp, partLabel string) string {
	var body strings.Builder
	if partLabel != "" {
		body.WriteString(partLabel)
		body.WriteString("\n\n")
	}
	previousLine := ""
	for i, block := range blocks {
		if i > 0 {
			if block.lineID != previousLine {
				body.WriteString("\n\n")
			} else {
				body.WriteString("\n")
			}
		}
		if i == 0 || block.lineID != previousLine {
			body.WriteString("<b>")
			body.WriteString(escapeHTML(block.lineName))
			body.WriteString("</b>\n")
		}
		body.WriteString(block.indicator.Icon)
		body.WriteString(" <b>")
		body.WriteString(escapeHTML(block.indicator.Label))
		body.WriteString("</b>")
		if block.reason != "" {
			body.WriteString("\n")
			body.WriteString(escapeHTML(block.reason))
		}
		previousLine = block.lineID
	}
	body.WriteString("\n\n<i>Observed ")
	body.WriteString(escapeHTML(timestamp))
	body.WriteString("</i>")
	return body.String()
}

func messageLength(blocks []statusBlock, timestamp, partLabel string) int {
	return len(utf16.Encode([]rune(renderHTML(blocks, timestamp, partLabel))))
}

func escapeHTML(value string) string {
	return html.EscapeString(safeText(value))
}

func safeText(value string) string {
	if utf8.ValidString(value) {
		return value
	}
	return strings.ToValidUTF8(value, "�")
}
