// Package logging preserves error diagnostics and redacts configured credentials.
package logging

import (
	"log/slog"
	"net/url"
	"slices"
	"strings"
)

// Message is an application-authored diagnostic.
type Message string

func (m Message) Error() string { return string(m) }

func Error(err error) slog.Attr {
	if err == nil {
		return slog.String("error", "")
	}
	return slog.String("error", err.Error())
}

// RedactCredentials replaces active credentials in messages and string/error attributes.
// The standard handlers apply it to groups and inherited attributes as well.
func RedactCredentials(token, appKey string) func([]string, slog.Attr) slog.Attr {
	var secrets []string
	for _, secret := range []string{token, appKey} {
		if secret != "" {
			secrets = append(secrets, secret, url.QueryEscape(secret), url.PathEscape(secret))
		}
	}
	slices.SortFunc(secrets, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	secrets = slices.Compact(secrets)
	replacements := make([]string, 0, 2*len(secrets))
	for _, secret := range secrets {
		replacements = append(replacements, secret, "<redacted>")
	}
	replacer := strings.NewReplacer(replacements...)
	return func(_ []string, attr slog.Attr) slog.Attr {
		switch attr.Value.Kind() {
		case slog.KindString:
			attr.Value = slog.StringValue(replacer.Replace(attr.Value.String()))
		case slog.KindAny:
			if err, ok := attr.Value.Any().(error); ok {
				attr.Value = slog.StringValue(replacer.Replace(err.Error()))
			}
		}
		return attr
	}
}
