package logging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/logging"
)

func TestErrorPreservesDescription(t *testing.T) {
	for _, err := range []error{
		nil, errors.New("ordinary failure"), fmt.Errorf("wrapped: %w", errors.New("cause")),
		errors.Join(errors.New("first"), errors.New("second")),
		&url.Error{Op: "Get", URL: "https://api.example/test", Err: errors.New("connection reset")},
		&net.DNSError{Name: "api.example", Server: "resolver.example", Err: "lookup refused"},
		&os.PathError{Op: "open", Path: "/var/lib/edi/edi.db", Err: syscall.EACCES},
	} {
		want := ""
		if err != nil {
			want = err.Error()
		}
		if got := logging.Error(err).Value.String(); got != want {
			t.Errorf("description = %q, want %q", got, want)
		}
	}
}

func TestCredentialRedaction(t *testing.T) {
	token, key := "123:token/a+b ?", "key/+ value"
	variants := []string{token, url.QueryEscape(token), url.PathEscape(token), key, url.QueryEscape(key), url.PathEscape(key)}
	original := strings.Join(append(variants, variants...), " | ")
	want := strings.TrimSuffix(strings.Repeat("<redacted> | ", len(variants)*2), " | ")
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			opts := &slog.HandlerOptions{ReplaceAttr: logging.RedactCredentials(token, key)}
			var handler slog.Handler = slog.NewTextHandler(&out, opts)
			if format == "json" {
				handler = slog.NewJSONHandler(&out, opts)
			}
			logger := slog.New(handler).With("inherited", original).WithGroup("request")
			logger.Error(original, "string", original, "error", errors.New(original), slog.Group("nested", "value", original))
			if format == "json" {
				var record map[string]any
				if err := json.Unmarshal(out.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				request := record["request"].(map[string]any)
				for name, value := range map[string]any{"msg": record["msg"], "inherited": record["inherited"], "string": request["string"], "error": request["error"], "nested": request["nested"].(map[string]any)["value"]} {
					if value != want {
						t.Errorf("%s = %v, want %q", name, value, want)
					}
				}
			} else if strings.Count(out.String(), "<redacted>") != len(variants)*2*5 {
				t.Errorf("redaction missing: %s", out.String())
			}
			for _, secret := range variants {
				if strings.Contains(out.String(), secret) {
					t.Errorf("credential leaked: %q", secret)
				}
			}
		})
	}
}

func TestCredentialRedactionLongestFirstAndEmpty(t *testing.T) {
	for _, test := range []struct{ token, key, input, want string }{
		{"abc", "abcdef", "abcdef abc abcdef", "<redacted> <redacted> <redacted>"},
		{"same", "same", "same same", "<redacted> <redacted>"},
		{"", "", "unchanged", "unchanged"},
		{"redacted", "secret", "secret", "<redacted>"},
	} {
		replace := logging.RedactCredentials(test.token, test.key)
		if got := replace(nil, slog.String("key", test.input)); got.Key != "key" || got.Value.String() != test.want {
			t.Errorf("got %v, want %q", got, test.want)
		}
	}
}

func TestCredentialRedactionPreservesTypesAndKeys(t *testing.T) {
	replace := logging.RedactCredentials("secret", "")
	for _, attr := range []slog.Attr{slog.Int("secret", 42), slog.Bool("secret", true), slog.Time("secret", time.Now()), slog.Duration("secret", time.Second), slog.Float64("secret", 1.5), slog.Uint64("secret", 3), slog.Any("secret", struct{ Value string }{"secret"})} {
		if got := replace(nil, attr); !reflect.DeepEqual(got, attr) {
			t.Errorf("changed attribute type or key: %v => %v", attr, got)
		}
	}
}
