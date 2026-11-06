// Package message defines the formatting metadata shared by renderers,
// persistence, and delivery adapters.
package message

import "fmt"

type Format uint8

const (
	FormatPlainText Format = iota
	FormatHTML
)

func (format Format) String() string {
	if format == FormatHTML {
		return "html"
	}
	return "plain"
}

func ParseFormat(value string) (Format, error) {
	switch value {
	case "plain":
		return FormatPlainText, nil
	case "html":
		return FormatHTML, nil
	default:
		return FormatPlainText, fmt.Errorf("unknown message format %q", value)
	}
}

type Chunk struct {
	Body   string
	Format Format
}
