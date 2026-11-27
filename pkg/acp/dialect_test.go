package acp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daybeam/vortex/core/obs"
)

// Register Q3 regression: auto detection had one initialize field signal and
// could not use deployment config or defer an ambiguous request to L3.
func TestDetectDialect_Layers(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		dialect string
		layer   string
		ok      bool
	}{
		{name: "L1 null still counts", params: `{"protocolVersion":null}`, dialect: "standard", layer: "L1", ok: true},
		{name: "L2 client capabilities", params: `{"clientCapabilities":{}}`, dialect: "standard", layer: "L2", ok: true},
		{name: "L2 alias", params: `{"capabilities":{}}`, dialect: "standard", layer: "L2", ok: true},
		{name: "L2 client info", params: `{"clientInfo":{"name":"c"}}`, dialect: "standard", layer: "L2", ok: true},
		{name: "L2 legacy shape", params: `{"client":"dsh"}`, dialect: "legacy", layer: "L2", ok: true},
		{name: "malformed unresolved", params: `{`, ok: false},
		{name: "missing unresolved", params: ``, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := detectDialect(json.RawMessage(tt.params))
			if ok != tt.ok || got.dialect != tt.dialect || got.layer != tt.layer {
				t.Fatalf("got %+v/%v, want %s/%s/%v", got, ok, tt.dialect, tt.layer, tt.ok)
			}
		})
	}
}

func TestDetectDialect_OverrideWins(t *testing.T) {
	tests := []struct{ env, config, mode, source string }{
		{"standard", "legacy", "standard", "env"},
		{"legacy", "standard", "legacy", "env"},
		{"auto", "standard", "auto", "env"},
		{"", "legacy", "legacy", "config"},
		{"invalid", "standard", "auto", "env"},
		{"", "", "auto", "config"},
	}
	for _, tt := range tests {
		mode, source := resolveDialectMode(tt.env, tt.config)
		if mode != tt.mode || source != tt.source {
			t.Errorf("resolve(%q,%q)=%s/%s, want %s/%s", tt.env, tt.config, mode, source, tt.mode, tt.source)
		}
	}
}

func TestDetectDialectMethod(t *testing.T) {
	for _, method := range []string{"session/new", "session/prompt", "session/cancel", "session/update"} {
		if d, ok := detectDialectMethod(method); !ok || d.dialect != "standard" {
			t.Errorf("%s = %+v/%v, want standard", method, d, ok)
		}
	}
	for _, method := range []string{"run_task", "cancel", "custom"} {
		if d, ok := detectDialectMethod(method); !ok || d.dialect != "legacy" {
			t.Errorf("%s = %+v/%v, want legacy", method, d, ok)
		}
	}
	if _, ok := detectDialectMethod("ping"); ok {
		t.Fatal("ping must not lock a dialect")
	}
}

func TestACP_AmbiguousInitializeDefersToL3(t *testing.T) {
	t.Setenv("VORTEX_ACP_DIALECT", "")
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{}}`,
	}, "\n")
	var out bytes.Buffer
	if err := ServeACPWithOptions(&mockACPEngine{}, strings.NewReader(input), &out, ServeACPOptions{}); err != nil {
		t.Fatal(err)
	}
	responses := parseResponses(t, out.String())
	if len(responses) != 3 {
		t.Fatalf("got %d responses: %s", len(responses), out.String())
	}
	if responses[0].Result.(map[string]any)["server"] != "orchestrator" || responses[1].Result.(map[string]any)["pong"] != true {
		t.Fatalf("shared methods did not respond provisionally: %+v", responses)
	}
	if responses[2].Error != nil {
		t.Fatalf("L3 standard routing failed: %+v", responses[2].Error)
	}
}

func TestACP_DialectEventOnce(t *testing.T) {
	t.Setenv("VORTEX_ACP_DIALECT", "")
	events := make(chan obs.AgentEvent, 4)
	unsub := obs.DefaultBus.SubscribeWithCancel(string(obs.EventACPDialectDetected), func(ev obs.AgentEvent) error {
		events <- ev
		return nil
	})
	defer unsub()
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","id":3,"method":"run_task","params":{"prompt":"x"}}`,
	}, "\n")
	var out bytes.Buffer
	if err := ServeACPWithOptions(&mockACPEngine{}, strings.NewReader(input), &out, ServeACPOptions{Dialect: "auto"}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("dialect events = %d, want exactly 1", len(events))
	}
	ev := <-events
	if ev.Payload["dialect"] != "standard" || ev.Payload["layer"] != "L3" {
		t.Fatalf("event payload = %#v", ev.Payload)
	}
}
