package core

import "bytes"

// extractSSEBody handles the case where a remote MCP server, upon seeing
// an Accept header that includes text/event-stream, responds with a
// Server-Sent Events stream instead of a bare JSON body. If the body
// already looks like JSON (starts with { or [), it is returned unchanged.
// Otherwise this scans for SSE "data:" lines and returns the concatenated
// payload, which is expected to itself be JSON.
// FIX (2026-07-13): exa/tushare started responding via SSE framing once
// the Accept header was corrected to also allow text/event-stream.
func extractSSEBody(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] == byte(123) || trimmed[0] == byte(91) {
		return body
	}
	var out bytes.Buffer
	for _, line := range bytes.Split(body, []byte("\n")) {
		l := bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(l, []byte("data:")) {
			out.Write(bytes.TrimSpace(l[len("data:"):]))
		}
	}
	if out.Len() == 0 {
		return body
	}
	return out.Bytes()
}
