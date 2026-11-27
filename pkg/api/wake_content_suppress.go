// VORTEX_WAKE_CONTENT_SUPPRESS selects the evidence a queued wake must overcome
// before it is dropped. All four values keep the same precondition (the turn was
// shown the task's terminal state); they differ in what else has to be true:
//
//	off     — nothing. Any turn that saw a terminal status drops the wake.
//	observe — same behaviour as off, plus one stderr line per suppression
//	          saying what the `on` rule would have decided.
//	on      — the answer already quotes 4-significant-digit numbers from the
//	          declared artifacts. Blind on tasks whose artifacts carry no such
//	          numbers (011/030/028/009/002: measured 0/181 firings could ever
//	          be suppressed), so it degenerates to "always deliver" there.
//	read    — the turn read a declared artifact (read_file). This is the fact
//	          the marker was supposed to stand for. Default: measured over 283
//	          archived firings, the terminal-status marker is wrong in 137-169
//	          of them (48-60%), while read evidence is observable exactly.
//
// `observe` previews `on`, not `read`: it was written for the content rule.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	wakeContentSuppressEnv = "VORTEX_WAKE_CONTENT_SUPPRESS"
	defaultMinSigDigits    = 4
)

type wakeContentSuppressMode int

const (
	wakeContentSuppressOff wakeContentSuppressMode = iota
	wakeContentSuppressObserve
	wakeContentSuppressOn
	wakeContentSuppressRead
)

func (m wakeContentSuppressMode) String() string {
	switch m {
	case wakeContentSuppressObserve:
		return "observe"
	case wakeContentSuppressOn:
		return "on"
	case wakeContentSuppressRead:
		return "read"
	default:
		return "off"
	}
}

func parseWakeContentSuppressMode(raw string) wakeContentSuppressMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "observe", "log":
		return wakeContentSuppressObserve
	case "on", "1", "true", "yes":
		return wakeContentSuppressOn
	case "off", "0", "false", "no":
		return wakeContentSuppressOff
	default:
		// Unset and `read` land here on purpose: the default is the evidence
		// rule. A typo therefore enables it rather than silently reverting to
		// the marker rule that is wrong in half its firings — and the decision
		// is logged per suppression, so a surprising arm is visible in the log
		// rather than inferred from the score.
		return wakeContentSuppressRead
	}
}

func wakeContentSuppressSetting() wakeContentSuppressMode {
	return parseWakeContentSuppressMode(os.Getenv(wakeContentSuppressEnv))
}

func headlineFigures(root string, declared []string, minSig int) []string {
	rootClean, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	var figures []string
	seen := make(map[string]struct{})
	for _, name := range declared {
		path, ok := resolveArtifactPath(rootClean, name)
		if !ok {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if strings.ToLower(filepath.Ext(name)) != ".json" {
			continue
		}
		data, _, err := readArtifactPrefix(path)
		if err != nil || data == nil {
			continue
		}
		figs := extractJSONFigures(data, minSig)
		for _, f := range figs {
			if _, dup := seen[f]; !dup {
				seen[f] = struct{}{}
				figures = append(figures, f)
			}
		}
	}
	return figures
}

func extractJSONFigures(data []byte, minSig int) []string {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil
	}
	var figures []string
	collectFigures(obj, 0, minSig, &figures)
	return figures
}

func collectFigures(obj map[string]any, dictDepth int, minSig int, figures *[]string) {
	if dictDepth > 1 {
		return
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := obj[k].(type) {
		case json.Number:
			s := string(v)
			if sigDigits(s) >= minSig {
				f, _ := v.Float64()
				*figures = append(*figures, scalarText(f))
			}
		case []any:
			for _, elem := range v {
				switch e := elem.(type) {
				case map[string]any:
					collectFigures(e, dictDepth+1, minSig, figures)
				case json.Number:
					s := string(e)
					if sigDigits(s) >= minSig {
						f, _ := e.Float64()
						*figures = append(*figures, scalarText(f))
					}
				}
			}
		case map[string]any:
			collectFigures(v, dictDepth+1, minSig, figures)
		}
	}
}

func sigDigits(s string) int {
	s = strings.TrimPrefix(s, "-")
	s = strings.TrimPrefix(s, "+")
	hasDecimal := strings.Contains(s, ".")
	s = strings.Replace(s, ".", "", 1)
	s = strings.TrimLeft(s, "0")
	if !hasDecimal {
		s = strings.TrimRight(s, "0")
	}
	return len(s)
}

var figureNumberRe = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)

func figureHits(answer string, figures []string) []string {
	if len(figures) == 0 {
		return nil
	}
	cleaned := strings.ReplaceAll(answer, "$", "")
	cleaned = strings.ReplaceAll(cleaned, ",", "")
	cleaned = strings.ReplaceAll(cleaned, "%", "")
	tokens := figureNumberRe.FindAllString(cleaned, -1)
	if len(tokens) == 0 {
		return nil
	}
	answerNums := make([]float64, 0, len(tokens))
	for _, t := range tokens {
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			continue
		}
		answerNums = append(answerNums, f)
	}
	if len(answerNums) == 0 {
		return nil
	}
	var hits []string
	for _, fig := range figures {
		figVal, err := strconv.ParseFloat(fig, 64)
		if err != nil {
			continue
		}
		tol := math.Abs(figVal) * 1e-3
		if tol < 1e-9 {
			tol = 1e-9
		}
		for _, ansVal := range answerNums {
			if math.Abs(figVal-ansVal) <= tol {
				hits = append(hits, fig)
				break
			}
		}
	}
	return hits
}

func (h *ChatHandler) lastAssistantText(sessionID string) string {
	session := h.Store.Get(sessionID)
	if session == nil {
		return ""
	}
	msgs := session.SnapshotMessages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return msgs[i].Content
		}
	}
	return ""
}

func formatFigureList(figures []string, limit int) string {
	if len(figures) <= limit {
		return strings.Join(figures, ", ")
	}
	return strings.Join(figures[:limit], ", ") + fmt.Sprintf(", …(%d more)", len(figures)-limit)
}

// wakeDropDecision answers, for an entry whose terminal-state marker has already
// been taken, whether the wake should be dropped and why — the reason is what the
// log line prints.
//
// The four arms differ only in the second form of evidence they demand (see this
// file's header). off/observe keep the original rule verbatim so archived runs
// stay comparable, and observe additionally previews `on` exactly as before.
func (h *ChatHandler) wakeDropDecision(sessionID string, e pendingWakeEntry, mode wakeContentSuppressMode) (bool, string) {
	const markerReason = "the turn that just ended already saw this task's terminal state"
	switch mode {
	case wakeContentSuppressRead:
		read := h.readDeclared(sessionID, e.declared)
		if len(read) == 0 {
			return false, ""
		}
		return true, fmt.Sprintf("read %d/%d declared artifact(s) this turn (%s)",
			len(read), len(e.declared), formatFigureList(read, 3))
	case wakeContentSuppressOn:
		hits := figureHits(h.lastAssistantText(sessionID), e.figures)
		if len(hits) == 0 {
			return false, ""
		}
		return true, fmt.Sprintf("answer carries %d/%d headline figures (%s)",
			len(hits), len(e.figures), formatFigureList(hits, 5))
	case wakeContentSuppressObserve:
		hits := figureHits(h.lastAssistantText(sessionID), e.figures)
		fmt.Fprintf(os.Stderr, "[chat-wake] session=%s task=%s would suppress: "+
			"answer carries %d/%d headline figures (%s)\n",
			sessionID, e.taskID, len(hits), len(e.figures), formatFigureList(hits, 5))
		return true, markerReason
	default:
		return true, markerReason
	}
}
