package core

import (
	"regexp"
	"strconv"
	"strings"
)

// HeartbeatAction classifies a heartbeat directive's intent.
type HeartbeatAction int

const (
	HeartbeatNone     HeartbeatAction = iota // no directive found
	HeartbeatSleep                            // [[Heartbeat::Sleep::N]] — suspend turn for N seconds
	HeartbeatComplete                         // [[Heartbeat::Complete]] — task done, return report
	HeartbeatFail                             // [[Heartbeat::Fail]] — task failed, return report
)

// HeartbeatDirective is the parsed result of heartbeat protocol markers from
// an agent's response text. When Action is HeartbeatNone, the response is a
// normal turn — proceed with the existing spawner flow.
//
// Protocol tags (VCP-inspired, see docs/plan/VCP_HEARTBEAT_AND_CONTEXT_TIERING_DESIGN.md):
//
//	[[Heartbeat::Sleep::秒数]]                              — sleep N seconds
//	[[Heartbeat::WakePrompt]]自定义唤醒提示词[[/Heartbeat::WakePrompt]] — next-turn prompt
//	[[Heartbeat::Complete]]                                — task complete
//	[[Heartbeat::Fail]]                                    — task failed
type HeartbeatDirective struct {
	Action       HeartbeatAction
	SleepSeconds int    // valid when Action == HeartbeatSleep
	WakePrompt   string // valid when Action == HeartbeatSleep
	Report       string // cleaned response text, valid when Action == Complete or Fail
}

var (
	heartbeatSleepRe      = regexp.MustCompile(`\[\[Heartbeat::Sleep::(\d+)\]\]`)
	heartbeatWakePromptRe = regexp.MustCompile(`(?s)\[\[Heartbeat::WakePrompt\]\](.*?)\[\[/Heartbeat::WakePrompt\]\]`)
	heartbeatCompleteRe   = regexp.MustCompile(`\[\[Heartbeat::Complete\]\]`)
	heartbeatFailRe       = regexp.MustCompile(`\[\[Heartbeat::Fail\]\]`)
	heartbeatAnyRe        = regexp.MustCompile(`\[\[Heartbeat::`)
)

// parseHeartbeat extracts a heartbeat directive from the agent's response text.
// Returns HeartbeatDirective{Action: HeartbeatNone} if no directive is present.
//
// Priority: Complete > Fail > Sleep. This means if the agent emits both
// Complete and Sleep (contradictory), Complete wins — the task is done.
func parseHeartbeat(text string) HeartbeatDirective {
	if !heartbeatAnyRe.MatchString(text) {
		return HeartbeatDirective{Action: HeartbeatNone}
	}

	// Complete — highest priority.
	if heartbeatCompleteRe.MatchString(text) {
		return HeartbeatDirective{
			Action: HeartbeatComplete,
			Report: stripHeartbeatDirectives(text),
		}
	}

	// Fail.
	if heartbeatFailRe.MatchString(text) {
		return HeartbeatDirective{
			Action: HeartbeatFail,
			Report: stripHeartbeatDirectives(text),
		}
	}

	// Sleep.
	if m := heartbeatSleepRe.FindStringSubmatch(text); m != nil {
		secs, _ := strconv.Atoi(m[1])
		if secs <= 0 {
			return HeartbeatDirective{Action: HeartbeatNone}
		}
		wakePrompt := ""
		if wm := heartbeatWakePromptRe.FindStringSubmatch(text); wm != nil {
			wakePrompt = strings.TrimSpace(wm[1])
		}
		return HeartbeatDirective{
			Action:       HeartbeatSleep,
			SleepSeconds: secs,
			WakePrompt:   wakePrompt,
		}
	}

	return HeartbeatDirective{Action: HeartbeatNone}
}

// stripHeartbeatDirectives removes all [[Heartbeat::...]] markers from the
// text, leaving the clean report content for the caller.
func stripHeartbeatDirectives(text string) string {
	text = heartbeatCompleteRe.ReplaceAllString(text, "")
	text = heartbeatFailRe.ReplaceAllString(text, "")
	text = heartbeatSleepRe.ReplaceAllString(text, "")
	text = heartbeatWakePromptRe.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}
