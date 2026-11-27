package core

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func captureStandardLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// Regression for Q1: enabling MemoryBank without usable profile data must
// degrade without injection, but must say why instead of silently disappearing.
func TestPromptAssembler_MemoryBankSkipIsObservable(t *testing.T) {
	cases := []struct {
		name string
		reg  *config.Registry
		mb   *store.MemoryBankStore
		want string
	}{
		{"nil_store", &config.Registry{Roles: map[string]*config.Role{}}, nil, "store_unavailable"},
		{"empty_profile", &config.Registry{Roles: map[string]*config.Role{}}, store.NewMemoryBankStore(t.TempDir(), nil), "empty_profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureStandardLog(t)
			enabled := true
			tc.reg.System.MemoryBankEnabled = &enabled
			pa := NewPromptAssembler(nil, nil, tc.reg, nil)
			pa.SetMemoryBankStore(tc.mb)
			if _, err := pa.Build(context.Background(), nil, nil, nil, "coding", &config.ProviderConfig{}, nil, nil, nil, false, nil, "task", "", "q1", ""); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), "memorybank_injection_skipped") || !strings.Contains(logs.String(), tc.want) {
				t.Fatalf("missing %q skip observation: %s", tc.want, logs.String())
			}
		})
	}
}

type reflectionErrorTaskStore struct {
	store.ITaskStore
	err error
}

func (s reflectionErrorTaskStore) GetBatch(context.Context, string, []string) (map[string]*store.StepResult, error) {
	return nil, s.err
}

// Regression for Q2: reflection's formerly ignored batch error is observable,
// and missing task-store/registry dependencies remain a graceful no-op.
func TestReflection_EmptyAndErrorPathsAreObservable(t *testing.T) {
	reg := &config.Registry{Roles: map[string]*config.Role{}}
	t.Run("missing_dependencies", func(t *testing.T) {
		logs := captureStandardLog(t)
		re := NewReflectionEngine(nil, nil, nil, nil, nil)
		re.ReflectOnTask(context.Background(), &schemas.TaskGraph{TaskID: "q2", Steps: map[string]*schemas.Step{"s1": {ID: "s1"}}})
		if !strings.Contains(logs.String(), "reflection_skipped") || !strings.Contains(logs.String(), "dependencies_unavailable") {
			t.Fatalf("missing dependency observation: %s", logs.String())
		}
	})
	t.Run("empty_graph", func(t *testing.T) {
		logs := captureStandardLog(t)
		re := NewReflectionEngine(nil, nil, reg, nil, nil)
		re.ReflectOnTask(context.Background(), &schemas.TaskGraph{TaskID: "q2"})
		if !strings.Contains(logs.String(), "empty_graph") {
			t.Fatalf("missing empty graph observation: %s", logs.String())
		}
	})
	t.Run("batch_error", func(t *testing.T) {
		logs := captureStandardLog(t)
		ts := reflectionErrorTaskStore{err: errors.New("batch unavailable")}
		es := mustNewExperienceStore(t, t.TempDir(), ts, &config.SystemSettings{}, nil, nil)
		re := NewReflectionEngine(es, ts, reg, nil, nil)
		graph := &schemas.TaskGraph{TaskID: "q2_batch", Status: schemas.GraphCompleted, Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "worker", Status: schemas.StepSkipped},
		}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		re.ReflectOnTask(ctx, graph)
		re.Wait()
		es.WaitAsyncSaves()
		if !strings.Contains(logs.String(), "reflection_batch_failed") || !strings.Contains(logs.String(), "batch unavailable") {
			t.Fatalf("missing batch error observation: %s", logs.String())
		}
	})
}
