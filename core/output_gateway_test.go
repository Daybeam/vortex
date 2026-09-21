package core

import (
	"bytes"
	"fmt"
	"testing"
)

func TestAdaptiveOutputResult_Small(t *testing.T) {
	smallData := []byte("hello world\nline 2\n")
	res := AdaptiveOutputResult(smallData, "echo test")
	if !bytes.Equal(res, smallData) {
		t.Fatalf("expected small data to pass through unmodified")
	}
}

func TestAdaptiveOutputResult_MediumTruncate(t *testing.T) {
	var buf bytes.Buffer
	for i := 1; i <= 300; i++ {
		buf.WriteString(string(bytes.Repeat([]byte("a"), 50)) + fmtLine(i) + "\n")
	}
	buf.WriteString("CRITICAL error: something went wrong in middle\n")
	for i := 302; i <= 400; i++ {
		buf.WriteString(string(bytes.Repeat([]byte("b"), 50)) + fmtLine(i) + "\n")
	}

	data := buf.Bytes()
	if len(data) < 8*1024 {
		data = append(bytes.Repeat([]byte("x"), 9*1024), data...)
	}

	res := AdaptiveOutputResult(data, "long_cmd")
	if !bytes.Contains(res, []byte("OUTPUT TRUNCATED")) && !bytes.Contains(res, []byte("OUTPUT GATEWAY")) {
		t.Fatalf("expected output to be truncated or gatewayed")
	}
}

func fmtLine(i int) string {
	return fmt.Sprintf(" line number %d", i)
}
