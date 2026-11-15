package api

import (
	"reflect"
	"sort"
	"testing"

	"github.com/daybeam/vortex/pkg/chat"
)

// The tests below guard cloneHarnessForRequest, which cannot copy the struct
// wholesale because ChatHarness carries a sync.Mutex (SteerLock) and vet rejects
// the copy. The fields are therefore listed by hand, and that list has silently
// dropped fields repeatedly (ReadinessGateSoft, MaxContextWindow, Segments, and
// most recently DelegateStatus — the tool table said "you can check task status"
// while the receipt said "there is no such wiring"). Both sides compile, so
// review cannot catch it; only a run can.
//
//	TestCloneHarnessForRequest_ClassifiesEveryField — a NEW field must be
//	  classified (carried, or excluded with a reason). Catches "nobody thought
//	  about it".
//	TestCloneHarnessForRequest_CarriesEveryField — a classified field must
//	  actually survive the clone. Catches "thought about it, forgot the line".
//
// Interface-typed fields are skipped by the second test: reflection cannot
// manufacture a non-nil value of an unknown interface type, so Provider, Embed
// and Memory stay unverified there. That is a real hole, stated rather than
// papered over.

var harnessCloneExcluded = map[string]string{
	"Delegate": "replaced per request with a session-scoped closure",
	"OnTerminalTaskSeen": "replaced per request: a shared value would attribute " +
		"one session's consumption to another",
	"HelpProvider": "replaced per request with a closure bound to the user's tier",
	"declaredArtifacts": "per-request state, set fresh at the start of Run (not by the API layer)",
	"declaredFields":    "per-request state, set fresh at the start of Run (not by the API layer)",
	"contract":          "per-request state, set fresh at the start of Run (not by the API layer)",
	// Set on the same line of Run as the two fields above (chat_harness.go:515),
	// and consumed only during that Run by delegatedPromptWithSpec, so carrying
	// it would leak the *previous* request's spec into a follow-up's clone.
	"taskPrompt": "per-request state, set fresh at the start of Run (not by the API layer)",
	"PendingSteer": "per-request drift correction, must not leak between requests",
	"SteerLock":    "the mutex this whole function exists to avoid copying",
	"fileReadCalls":     "per-turn delegation drift counter, zero-initialized per request",
	"delegationSteered": "per-turn delegation drift flag, zero-initialized per request",
	// Post-delegation lockout (pkg/chat/post_deleg_lockout.go). Resolved from
	// the env on first use and accumulated inside Run. Carrying it would make a
	// follow-up request inherit the previous request's delegation and lock its
	// tools before it has done anything.
	"pd": "per-run state (arm, task id, block count), resolved and accumulated inside Run",
}

var harnessCloneCarried = []string{
	"Provider", "Model", "Role", "Archive", "MemoryBank", "Embed", "OutputBase",
	"MaxTurns", "MaxOutputTokens", "SessionRoot", "Sieve", "Memory", "Attachments",
	"ReadinessGateSoft", "Segments", "DelegateStatus", "MaxContextWindow",
	"SkillQuickPath",
	// Unexported — reflection cannot set a sentinel on it, so
	// TestCloneHarnessForRequest_CarriesEveryField skips it by way of IsZero.
	// The copy is made through SetProjectResolver(src.ProjectOf); this entry
	// is what stops the field from being silently forgotten, which is exactly
	// what happened when Project Space Phase 3 added it (the copy fell back to
	// "default" and cross-project recall quietly narrowed).
	"projectResolver",
}

func TestCloneHarnessForRequest_ClassifiesEveryField(t *testing.T) {
	carried := map[string]bool{}
	for _, n := range harnessCloneCarried {
		carried[n] = true
	}

	typ := reflect.TypeOf(chat.ChatHarness{})
	var unclassified []string
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if _, ok := harnessCloneExcluded[name]; ok {
			continue
		}
		if !carried[name] {
			unclassified = append(unclassified, name)
		}
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Fatalf("ChatHarness field(s) %v are neither carried by cloneHarnessForRequest nor "+
			"excluded with a reason. A field the base harness sets but the per-request copy "+
			"drops is invisible: both sides compile and only runtime behaviour differs.",
			unclassified)
	}

	for _, name := range harnessCloneCarried {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("carried list names %q, which ChatHarness no longer has", name)
		}
	}
}

func TestCloneHarnessForRequest_CarriesEveryField(t *testing.T) {
	src := &chat.ChatHarness{}
	sv := reflect.ValueOf(src).Elem()
	st := sv.Type()

	// Populate every settable non-interface field with a distinguishable value.
	for i := 0; i < st.NumField(); i++ {
		f := sv.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("sentinel")
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			f.SetInt(7)
		case reflect.Ptr:
			f.Set(reflect.New(f.Type().Elem()))
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		case reflect.Map:
			f.Set(reflect.MakeMap(f.Type()))
		case reflect.Func:
			ft := f.Type()
			f.Set(reflect.MakeFunc(ft, func([]reflect.Value) []reflect.Value {
				out := make([]reflect.Value, ft.NumOut())
				for k := range out {
					out[k] = reflect.Zero(ft.Out(k))
				}
				return out
			}))
		}
		// Interface kinds are left nil on purpose — see the file comment.
	}

	dst := cloneHarnessForRequest(src)
	// dst is a *ChatHarness (the copy is returned by pointer so that returning
	// it cannot copy the mutex); reflect over the struct it points at.
	dv := reflect.ValueOf(dst).Elem()
	carried := map[string]bool{}
	for _, n := range harnessCloneCarried {
		carried[n] = true
	}

	var dropped []string
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		if !carried[name] {
			continue
		}
		in := sv.Field(i)
		if in.Kind() == reflect.Interface || in.IsZero() {
			continue
		}
		if dv.Field(i).IsZero() {
			dropped = append(dropped, name)
		}
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		t.Fatalf("cloneHarnessForRequest dropped field(s) %v: the source had a value, the "+
			"per-request copy has zero. The request path would then run with the feature "+
			"apparently unwired while the tool table still advertises it.", dropped)
	}
}

// TestCloneHarnessForRequest_DelegateIsPerRequest pins the one field the clone
// must NOT carry: Delegate is replaced with a session-scoped closure, so copying
// the base one would submit tasks against the wrong session.
func TestCloneHarnessForRequest_DelegateIsPerRequest(t *testing.T) {
	src := &chat.ChatHarness{Delegate: func(string) (string, error) { return "task_base", nil }}
	if dst := cloneHarnessForRequest(src); dst.Delegate != nil {
		t.Error("cloneHarnessForRequest must not copy Delegate; SendHandler installs its own")
	}
}

// TestCloneHarnessForRequest_TerminalSeenIsPerRequest pins the same rule for the
// consumption callback. Copying it would let a turn in session A record that
// session B consumed a result, suppressing B's follow-up for a reason that never
// applied to it — and the suppression is silent, so nothing downstream notices.
func TestCloneHarnessForRequest_TerminalSeenIsPerRequest(t *testing.T) {
	src := &chat.ChatHarness{OnTerminalTaskSeen: func(string) {}}
	if dst := cloneHarnessForRequest(src); dst.OnTerminalTaskSeen != nil {
		t.Error("cloneHarnessForRequest must not copy OnTerminalTaskSeen; " +
			"each request installs a closure bound to its own session")
	}
}
