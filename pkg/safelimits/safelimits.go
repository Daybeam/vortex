// Package safelimits defines size ceilings for untrusted network reads.
//
// All io.ReadAll calls on HTTP response bodies must be wrapped with
// io.LimitReader(reader, safelimits.MaxResponseBody) or
// io.LimitReader(reader, safelimits.MaxErrorBody) to prevent unbounded
// memory allocation from malicious or buggy servers (audit H1).
//
// Rationale:
//   - MaxResponseBody (50 MiB): large enough for any legitimate MCP tool
//     response, LLM provider response, or GitHub raw content fetch (GitHub's
//     own API caps raw content at 100 MiB). Provides 5-50x headroom over
//     typical responses while preventing OOM on a 512 MiB-2 GiB container.
//   - MaxErrorBody (1 MiB): error bodies are read only for logging. HTTP API
//     error JSON rarely exceeds 10-100 KiB; 1 MiB gives 10-100x headroom.
//     If an error response exceeds 1 MiB it is almost certainly garbage or
//     an attack, not a legitimate error message.
package safelimits

const (
	// MaxResponseBody is the maximum number of bytes to read from a trusted
	// HTTP response body (MCP tool results, LLM completions, GitHub content).
	MaxResponseBody = 50 << 20 // 50 MiB

	// MaxErrorBody is the maximum number of bytes to read from an HTTP error
	// response body for logging purposes only.
	MaxErrorBody = 1 << 20 // 1 MiB
)
