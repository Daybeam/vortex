package schemas

import (
	"strings"
	"testing"
)

func BenchmarkParseOutput_DirectJSON(b *testing.B) {
	raw := `{"status":"ok","result":{"answer":"42"},"confidence":0.95}`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseOutput(raw, "research")
	}
}

func BenchmarkParseOutput_MarkdownFence(b *testing.B) {
	raw := "Here is the result:\n```json\n{\"status\":\"ok\",\"result\":{\"answer\":\"42\"},\"confidence\":0.95}\n```\nDone."
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseOutput(raw, "research")
	}
}

func BenchmarkParseOutput_AggressiveExtraction(b *testing.B) {
	raw := "I'll analyze this.\nSome preamble text.\n{\"status\":\"ok\",\"result\":{\"answer\":\"42\"},\"confidence\":0.95}\nSome trailing text."
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseOutput(raw, "research")
	}
}

func BenchmarkParseOutput_FallbackRaw(b *testing.B) {
	raw := "This is just plain text with no JSON at all, just a conversational response from the model."
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseOutput(raw, "research")
	}
}

func BenchmarkParseOutput_LargeToolCallResult(b *testing.B) {
	var sb strings.Builder
	sb.WriteString(`{"status":"ok","result":{`)
	for i := 0; i < 50; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`"field_`)
		sb.WriteString(string(rune('a' + i%26)))
		sb.WriteString(`":"value_with_some_length_`)
		sb.WriteString(string(rune('a' + i%26)))
		sb.WriteString(`"`)
	}
	sb.WriteString(`},"tool_calls":[{"name":"read_file","args":{"path":"/some/path"}}],"confidence":0.8}`)
	raw := sb.String()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseOutput(raw, "research")
	}
}
