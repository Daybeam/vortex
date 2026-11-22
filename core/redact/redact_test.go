package redact

import (
	"strings"
	"testing"
)

// The bot-token rule exists because a token reaches the logs inside a URL path:
// net/http's url.Error quotes the whole request URL in every transport error,
// and the Bot API URL is "https://api.telegram.org/bot<token>/<method>". The
// key=value rule above cannot see that -- there is no "token=" in the string --
// so without its own pattern a live credential sat next to the failure text in
// logs/<date>/telegram.jsonl.

func scrubTests() []struct {
	name, in, mustNotContain, mustContain string
} {
	return []struct{ name, in, mustNotContain, mustContain string }{
		{
			name:           "telegram_url_path",
			in:             `Post "https://api.telegram.org/bot7333311722:AAGYq9K3n7oLmBvxSampleToken99abcdefghij/getUpdates": Client.Timeout exceeded while awaiting headers`,
			mustNotContain: "AAGYq9K3n7oLmBvxSampleToken99abcdefghij",
			mustContain:    "Client.Timeout exceeded while awaiting headers",
		},
		{
			name:           "openai_key",
			in:             "Authorization: Bearer sk-projABCDEFGHIJKLMNOPQRSTUV1234567890",
			mustNotContain: "sk-projABCDEFGHIJKLMNOPQRSTUV1234567890",
			mustContain:    "Bearer",
		},
		{
			name:           "google_key",
			in:             "https://example.invalid/?key=AIzaSyABCDEFGHIJKLMNOPQRSTUVWXY1234567890",
			mustNotContain: "AIzaSyABCDEFGHIJKLMNOPQRSTUVWXY1234567890",
			mustContain:    "https://example.invalid/?key=",
		},
		{
			name:           "key_value",
			in:             `config line: api_key = "abcdefgh12345678"`,
			mustNotContain: "abcdefgh12345678",
			mustContain:    "config line: ",
		},
	}
}

// Every case pairs a leak it must close with the surrounding text it must keep:
// a scrubber that blanks the whole line destroys the diagnosis it was added
// for. The key=value rule does blank the key name along with the value -- that
// is its existing contract, so the case only pins the text outside the match.
func TestScrubSecrets_CoversEachCredentialShape(t *testing.T) {
	for _, tc := range scrubTests() {
		t.Run(tc.name, func(t *testing.T) {
			got := ScrubSecrets(tc.in)
			if strings.Contains(got, tc.mustNotContain) {
				t.Errorf("secret survived scrubbing:\n in: %s\nout: %s", tc.in, got)
			}
			if !strings.Contains(got, tc.mustContain) {
				t.Errorf("scrubbing destroyed the context around the secret, got: %s", got)
			}
		})
	}
}

// The rule is "8-10 digits, colon, 30+ secret characters". Anything shorter has
// to be left alone, or ordinary log text -- durations, ratios, ports, ids --
// starts turning into [REDACTED] and the log stops being readable.
func TestScrubSecrets_LeavesOrdinaryColonValuesAlone(t *testing.T) {
	for _, in := range []string{
		`telegram getUpdates failed 3 time(s), down for 1m30s: HTTP 500`,
		`ratio 12345678:abcdefghijklmnopqrstuvwxyz`,    // 26-char tail
		`duration 123456:1h2m3s4h5m6h7m8h9h0h1h2h3h4h`, // 6-digit id
		`port mapping 8080:9090 1234567890:5`,
		`epoch 1730000000:5 and a 42:success answer`,
	} {
		if got := ScrubSecrets(in); got != in {
			t.Errorf("scrubbed ordinary text:\n in: %s\nout: %s", in, got)
		}
	}
}
