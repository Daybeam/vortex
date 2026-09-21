package providers

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"empty", "", 0},
		{"seconds", "30", 30},
		{"invalid", "abc", 0},
		{"future_date", time.Now().UTC().Add(10 * time.Second).Format(http.TimeFormat), 10},
		{"past_date", time.Now().UTC().Add(-10 * time.Second).Format(http.TimeFormat), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set("Retry-After", tt.header)
			}
			got := parseRetryAfter(h)
			// Dates might be off by a second depending on execution timing
			if tt.name == "future_date" {
				if got < 9 || got > 11 {
					t.Errorf("parseRetryAfter() = %v, want ~10", got)
				}
			} else if got != tt.want {
				t.Errorf("parseRetryAfter() = %v, want %v", got, tt.want)
			}
		})
	}
}
