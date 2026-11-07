package chat

import (
	"context"
	"testing"
)

type fakeSink struct {
	name    string
	caps    CapSet
	renders []ChatEvent
	flushed bool
}

func (f *fakeSink) Name() string                  { return f.name }
func (f *fakeSink) Caps() CapSet                  { return f.caps }
func (f *fakeSink) Render(_ context.Context, ev ChatEvent) error {
	f.renders = append(f.renders, ev)
	return nil
}
func (f *fakeSink) Flush() error {
	f.flushed = true
	return nil
}

func TestSinkInterface(t *testing.T) {
	var s Sink = &fakeSink{name: "test", caps: CapSet{TextDelta: true}}
	if s.Name() != "test" {
		t.Errorf("Name() = %q, want %q", s.Name(), "test")
	}
	if !s.Caps().TextDelta {
		t.Error("Caps().TextDelta = false, want true")
	}
}

func TestSinkRenderAndFlush(t *testing.T) {
	f := &fakeSink{name: "test"}
	_ = f.Render(context.Background(), ChatEvent{Type: EvDelta, Data: "hello"})
	_ = f.Render(context.Background(), ChatEvent{Type: EvDone})
	if len(f.renders) != 2 {
		t.Fatalf("len(renders) = %d, want 2", len(f.renders))
	}
	if f.renders[0].Data != "hello" {
		t.Errorf("renders[0].Data = %q, want %q", f.renders[0].Data, "hello")
	}
	_ = f.Flush()
	if !f.flushed {
		t.Error("Flush() did not set flushed flag")
	}
}

func TestSinkEnabled_Default(t *testing.T) {
	if SinkEnabled() {
		t.Error("SinkEnabled() = true with no env, want false")
	}
}

func TestSinkEnabled_Sink(t *testing.T) {
	t.Setenv(ChatSinkEnv, "sink")
	if !SinkEnabled() {
		t.Error("SinkEnabled() = false with VORTEX_CHAT_SINK=sink, want true")
	}
}

func TestSinkEnabled_Legacy(t *testing.T) {
	t.Setenv(ChatSinkEnv, "legacy")
	if SinkEnabled() {
		t.Error("SinkEnabled() = true with VORTEX_CHAT_SINK=legacy, want false")
	}
}

func TestSinkEnabled_Garbage(t *testing.T) {
	t.Setenv(ChatSinkEnv, "garbage")
	if SinkEnabled() {
		t.Error("SinkEnabled() = true with VORTEX_CHAT_SINK=garbage, want false")
	}
}
