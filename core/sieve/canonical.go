package sieve

import "strings"

// controlArgPrefix marks engine-level control arguments that steer the model or
// tag the trace but are stripped before the tool payload is sent (`_reason`,
// `_decision_id`). They must never influence a loop-detection hash: otherwise
// the model can mint a fresh key for an otherwise identical call just by
// rephrasing its `_reason`, which silently defeats A1, #11 and every Sieve
// check at once. Guidance is not supposed to change the judgement it is
// attached to.
const controlArgPrefix = "_"

// CanonicalToolArgs returns args with control arguments removed. It never
// mutates the input: the reflection gate still needs to read `_reason` after
// the key has been computed. The original map is returned untouched when it
// holds no control arguments, so the common path allocates nothing.
func CanonicalToolArgs(args map[string]any) map[string]any {
	hasControl := false
	for k := range args {
		if strings.HasPrefix(k, controlArgPrefix) {
			hasControl = true
			break
		}
	}
	if !hasControl {
		return args
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if strings.HasPrefix(k, controlArgPrefix) {
			continue
		}
		out[k] = v
	}
	return out
}
