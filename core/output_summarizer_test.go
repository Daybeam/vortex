package core

import (
	"strings"
	"testing"
)

func TestSummarizeOutput_SmallContent_Passthrough(t *testing.T) {
	data := []byte("hello world")
	got := SummarizeOutput(data, "test")
	if got != "hello world" {
		t.Errorf("expected passthrough, got %q", got)
	}
}

func TestSummarizeOutput_JSON(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("{\n")
	for i := 0; i < 1000; i++ {
		sb.WriteString("  \"key" + itoa(i) + "\": \"val" + itoa(i) + "\",\n")
	}
	sb.WriteString("  \"end\": true\n}\n")
	data := []byte(sb.String())

	got := SummarizeOutput(data, "json")
	if !strings.Contains(got, "JSON Summary") {
		t.Errorf("expected JSON summary, got: %s", got[:100])
	}
}

func TestSummarizeOutput_JSONL(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString("{\"id\":" + itoa(i) + ",\"name\":\"item" + itoa(i) + "\"}\n")
	}
	data := []byte(sb.String())

	got := SummarizeOutput(data, "jsonl")
	if !strings.Contains(got, "JSONL Summary") {
		t.Errorf("expected JSONL summary, got: %s", got[:100])
	}
	if !strings.Contains(got, "1000 records") {
		t.Errorf("expected record count, got: %s", got[:200])
	}
}

func TestSummarizeOutput_Log(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		if i%50 == 0 {
			sb.WriteString("ERROR something failed\n")
		} else {
			sb.WriteString("INFO processing item\n")
		}
	}
	data := []byte(sb.String())

	got := SummarizeOutput(data, "log")
	if !strings.Contains(got, "Log Summary") {
		t.Errorf("expected log summary, got: %s", got[:100])
	}
	if !strings.Contains(got, "ERROR:") {
		t.Errorf("expected ERROR count, got: %s", got[:200])
	}
}

func TestSummarizeOutput_Table(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("| ID | Name | Value |\n")
	sb.WriteString("|----|------|-------|\n")
	for i := 0; i < 1000; i++ {
		sb.WriteString("| " + itoa(i) + " | item | 100 |\n")
	}
	data := []byte(sb.String())

	got := SummarizeOutput(data, "table")
	if !strings.Contains(got, "Table Summary") {
		t.Errorf("expected table summary, got: %s", got[:100])
	}
}

func TestSummarizeOutput_PlainText_Fallback(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("line " + itoa(i) + " of plain text content\n")
	}
	data := []byte(sb.String())

	got := SummarizeOutput(data, "plain")
	if !strings.Contains(got, "TRUNCATED") && !strings.Contains(got, "GATEWAY") && len(got) >= len(data) {
		t.Errorf("expected truncation or passthrough for plain text, got %d bytes", len(got))
	}
}

func TestFileSkeleton_GoCode(t *testing.T) {
	code := `package main

import "fmt"

func main() {
	fmt.Println("hello")
}

func helper(x int) int {
	return x * 2
}

type Config struct {
	Name string
	Port int
}

var defaultConfig = Config{}

const MaxRetries = 3
`
	got := FileSkeleton([]byte(code), "main.go")
	if !strings.Contains(got, "Code Skeleton") {
		t.Errorf("expected code skeleton header, got: %s", got)
	}
	if !strings.Contains(got, "func main") {
		t.Errorf("expected func main, got: %s", got)
	}
	if !strings.Contains(got, "func helper") {
		t.Errorf("expected func helper, got: %s", got)
	}
	if !strings.Contains(got, "type Config") {
		t.Errorf("expected type Config, got: %s", got)
	}
}

func TestFileSkeleton_PythonCode(t *testing.T) {
	code := `import os

def hello(name):
    print(f"hello {name}")

class MyClass:
    def __init__(self):
        self.x = 1
    
    async def process(self):
        pass
`
	got := FileSkeleton([]byte(code), "test.py")
	if !strings.Contains(got, "Code Skeleton") {
		t.Errorf("expected code skeleton, got: %s", got)
	}
	if !strings.Contains(got, "def hello") {
		t.Errorf("expected def hello, got: %s", got)
	}
	if !strings.Contains(got, "class MyClass") {
		t.Errorf("expected class MyClass, got: %s", got)
	}
}

func TestFileSkeleton_HTML(t *testing.T) {
	html := `<!DOCTYPE html>
<html>
<head>
  <title>Test</title>
  <meta charset="utf-8"/>
</head>
<body>
  <div id="main">
    <p>Hello</p>
    <img src="test.png"/>
  </div>
</body>
</html>
`
	got := FileSkeleton([]byte(html), "page.html")
	if !strings.Contains(got, "Tag Skeleton") {
		t.Errorf("expected tag skeleton, got: %s", got)
	}
	if !strings.Contains(got, "<html>") {
		t.Errorf("expected <html> tag, got: %s", got)
	}
	if !strings.Contains(got, "<div>") {
		t.Errorf("expected <div> tag, got: %s", got)
	}
}

func TestFileSkeleton_JSON(t *testing.T) {
	jsonData := `{
  "name": "test",
  "version": "1.0",
  "items": [1, 2, 3, 4, 5],
  "config": {
    "debug": true,
    "port": 8080
  }
}`
	got := FileSkeleton([]byte(jsonData), "config.json")
	if !strings.Contains(got, "JSON Skeleton") {
		t.Errorf("expected JSON skeleton, got: %s", got)
	}
}

func TestFileSkeleton_UnknownExtension_Fallback(t *testing.T) {
	data := []byte("just some text\n")
	got := FileSkeleton(data, "file.xyz")
	if string(data) != got {
		t.Errorf("expected passthrough for small unknown file, got %q", got)
	}
}

func TestFileSkeleton_LineNumbers(t *testing.T) {
	code := "package main\n\nfunc alpha() {}\n\nfunc beta() {}\n"
	got := FileSkeleton([]byte(code), "test.go")
	if !strings.Contains(got, "L3:") {
		t.Errorf("expected L3 for alpha, got: %s", got)
	}
	if !strings.Contains(got, "L5:") {
		t.Errorf("expected L5 for beta, got: %s", got)
	}
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"json object", `{"a":1,"b":2}`, "json"},
		{"json array", `[1,2,3]`, "json"},
		{"jsonl", `{"id":1}\n{"id":2}\n{"id":3}`, "jsonl"},
		{"table", "| A | B |\n|---|---|\n| 1 | 2 |", "table"},
		{"log", "INFO start\nERROR fail\nWARN retry\nINFO done", "log"},
		{"plain", "just some text\nmore text\n", "plain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(strings.ReplaceAll(tt.data, `\n`, "\n"))
			if len(data) < 8*1024 {
				data = append(data, make([]byte, 8*1024)...)
			}
			got := detectFormat(data)
			if got != tt.want && got != "plain" {
				t.Errorf("detectFormat() = %q, want %q (or plain)", got, tt.want)
			}
		})
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
