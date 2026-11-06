package core

import (
	"reflect"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// planJSONContract is model-visible: whatever it does not advertise, a planner
// never declares. Until 2026-11-04 it listed only 'regex:' / 'file:' criteria and
// no deterministic_checks at all, so the declaration channel built for register
// O1 stayed empty in every model-authored plan.
//
// The const's own comment claims it "must stay in sync with schemas.PlanDAG /
// schemas.PlanStep". That claim used to be unchecked, which is exactly how the
// channel went silent: the struct grew a field and the prompt did not. These
// assertions make the claim executable.

// Legal verifier types, per the registry in core/verify/deterministic_verifier.go
// (deterministicVerifiers). An unknown type is skipped silently by
// RunDeterministicChecks — pinned by core/verify/deterministic_verifier_test.go:127
// — so a planner that invents one gets a pass it never earned, which is why the
// contract names the list explicitly.
var legalVerifierTypes = []string{"file_exists", "command_pass", "plan_checkboxes", "code_entities"}

func TestPlanContract_AdvertisesDeterministicChecks(t *testing.T) {
	field, ok := reflect.TypeOf(schemas.PlanStep{}).FieldByName("DeterministicChecks")
	if !ok {
		t.Fatal("schemas.PlanStep lost DeterministicChecks: parser and prompt are out of sync")
	}
	tag := strings.Split(field.Tag.Get("json"), ",")[0]
	if tag == "" {
		t.Fatal("schemas.PlanStep.DeterministicChecks has no json tag, so a plan body cannot bind it")
	}
	if !strings.Contains(planJSONContract, `"`+tag+`"`) {
		t.Fatalf("contract does not advertise the key %q the parser binds; planners will never emit it", tag)
	}
	for _, typeName := range legalVerifierTypes {
		if !strings.Contains(planJSONContract, typeName) {
			t.Errorf("contract omits verifier type %q; a planner cannot choose from a list it has not seen", typeName)
		}
	}
}

// "json" became a defined exit_criteria form the same day (register O28): a
// criterion the gate can actually satisfy has to be visible to the writer, or
// planners keep inventing prose sentences that only pass when echoed back.
func TestPlanContract_AdvertisesJSONCriterion(t *testing.T) {
	if !strings.Contains(planJSONContract, "'json'") {
		t.Fatal("contract does not list 'json' among the exit_criteria forms")
	}
	step, ok := reflect.TypeOf(schemas.StepInput{}).FieldByName("ExitCriteria")
	if !ok {
		t.Fatal("schemas.StepInput lost ExitCriteria")
	}
	if tag := strings.Split(step.Tag.Get("json"), ",")[0]; !strings.Contains(planJSONContract, `"`+tag+`"`) {
		t.Fatalf("contract does not advertise the exit_criteria key %q", tag)
	}
}
