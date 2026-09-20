package core

// EventJITCompositionSuggested records that one or more historically-repeated
// task patterns were surfaced to the planning LLM as candidates for
// compilation into a reusable JIT tool (see
// store.ExperienceStore.QueryJITCandidates / store.FormatJITCompositionHints).
// Like EventSOPCandidatesOffered (core/sop_matcher.go), this intentionally
// does NOT record whether the LLM actually acted on the suggestion -- that
// can only be inferred later (or not at all) from whether a
// jit.create call follows. This event exists purely so low real-world uptake
// is visible in the logs rather than silently invisible.
// ADDED (2026-07-28).
const EventJITCompositionSuggested EventType = "jit_composition_suggested"
