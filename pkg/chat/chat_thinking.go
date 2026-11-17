package chat

import "strings"

// thinkingExtractor detects \x3cthink\x3e/\x3c/think\x3e tags in a chunk stream and separates
// thinking content from regular content. Handles tags split across chunks.
// Used by ChatHarness to emit "thinking" events separately from "delta" events,
// enabling Telegram/web clients to display reasoning collapsibly.
type thinkingExtractor struct {
	inThinking bool
	pending    string // buffer for partial tag detection
}

const (
	thinkOpenTag  = "\x3cthink\x3e"  // imd
	thinkCloseTag = "\x3c/think\x3e" // }
)

// Process takes a chunk and returns (content, thinking) strings.
// content should be emitted as "delta", thinking as "thinking".
// Both may be empty if the chunk is buffered for tag detection.
func (te *thinkingExtractor) Process(chunk string) (content, thinking string) {
	te.pending += chunk
	for len(te.pending) > 0 {
		if !te.inThinking {
			idx := strings.Index(te.pending, thinkOpenTag)
			if idx == -1 {
				safe := len(te.pending) - (len(thinkOpenTag) - 1)
				if safe <= 0 {
					return content, thinking
				}
				content += te.pending[:safe]
				te.pending = te.pending[safe:]
			} else {
				if idx > 0 {
					content += te.pending[:idx]
				}
				te.pending = te.pending[idx+len(thinkOpenTag):]
				te.inThinking = true
			}
		} else {
			idx := strings.Index(te.pending, thinkCloseTag)
			if idx == -1 {
				safe := len(te.pending) - (len(thinkCloseTag) - 1)
				if safe <= 0 {
					return content, thinking
				}
				thinking += te.pending[:safe]
				te.pending = te.pending[safe:]
			} else {
				if idx > 0 {
					thinking += te.pending[:idx]
				}
				te.pending = te.pending[idx+len(thinkCloseTag):]
				te.inThinking = false
			}
		}
	}
	return content, thinking
}

// Flush returns any remaining buffered content. Call at the end of the stream.
func (te *thinkingExtractor) Flush() (content, thinking string) {
	if te.inThinking {
		thinking = te.pending
	} else {
		content = te.pending
	}
	te.pending = ""
	return content, thinking
}
