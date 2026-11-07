package api

import (
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
	"github.com/daybeam/vortex/tools"
)

// The 2026-11-06 report was "好像收不到通知" while the tasks themselves ran. The
// notifications were produced: they were written into the session's in-memory
// event ring, which only forwardEvents reads, and that only runs during the
// user's own turn. A task that outlived the turn, a decision that arrived after
// its deadline, and anything produced by a hub that lost the getUpdates race
// all disappeared without a log line. These tests pin the other half: a
// notification is now a row, and every way it can fail to become a row names
// itself in the log.

type hub struct {
	t      *testing.T
	h      *ChatHandler
	engine *core.DirectedEngine
	outbox store.IChatOutbox
	logger *obs.Logger
	logDir string
	db     *sql.DB
}

// newHub builds one hub process's worth of wiring against one DB file. Two of
// these on the same file is the multi-hub shape the queue exists for.
func newHub(t *testing.T, dbPath string) *hub {
	t.Helper()

	reg, err := config.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logDir := t.TempDir()
	logger, err := obs.NewLogger(logDir, nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	engineDir := t.TempDir()
	engine := core.NewDirectedEngine(reg, nil, nil, nil, logger, nil, engineDir, engineDir, nil)

	db, err := store.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		logger.Close()
		db.Close()
	})

	outbox := store.NewSQLiteChatOutbox(db)
	return &hub{
		t: t,
		h: &ChatHandler{
			App:    &tools.App{Scheduler: engine, DB: db, Logger: logger},
			Store:  chat.NewChatSessionStore(t.TempDir(), store.NewSQLiteChatBackend(db)),
			Outbox: outbox,
		},
		engine: engine,
		outbox: outbox,
		logger: logger,
		logDir: logDir,
		db:     db,
	}
}

func (hb *hub) register(taskID, sessionID string, status schemas.GraphStatus) {
	hb.t.Helper()
	hb.engine.RegisterGraphForTest(&schemas.TaskGraph{
		TaskID:    taskID,
		SessionID: sessionID,
		Status:    status,
		Steps:     map[string]*schemas.Step{},
	})
}

type outboxRow struct {
	sessionID string
	kind      string
	dedupeKey string
	status    string
	holder    string
	text      string
	meta      string
	attempts  int
	lastErr   string
}

func (hb *hub) rows() []outboxRow {
	hb.t.Helper()
	rs, err := hb.db.Query(`SELECT session_id, kind, dedupe_key, status, claim_holder, text, meta_json, attempts, last_error FROM chat_outbox ORDER BY id`)
	if err != nil {
		hb.t.Fatalf("reading chat_outbox: %v", err)
	}
	defer rs.Close()

	var out []outboxRow
	for rs.Next() {
		var r outboxRow
		if err := rs.Scan(&r.sessionID, &r.kind, &r.dedupeKey, &r.status, &r.holder, &r.text, &r.meta, &r.attempts, &r.lastErr); err != nil {
			hb.t.Fatalf("scanning chat_outbox row: %v", err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		hb.t.Fatalf("iterating chat_outbox: %v", err)
	}
	return out
}

// readSinkLog returns what the obs logger wrote for one sink file. The logger
// buckets by UTC date, which is not always today's local date, so the tree is
// walked rather than guessed.
func readSinkLog(t *testing.T, dir, name string) string {
	t.Helper()
	var logged strings.Builder
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != name {
			return err
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		logged.Write(b)
		return nil
	}); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return logged.String()
}

// TestNotifyTaskTerminal_QueuesWithNoLiveListener is the core regression: the
// notification must exist as a row even though nothing in this process is
// watching the session. Before the outbox it existed only in the ring, so the
// user saw nothing and the log said nothing.
func TestNotifyTaskTerminal_QueuesWithNoLiveListener(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	hubA := newHub(t, dbPath)

	session := hubA.h.Store.GetOrCreate("telegram_4242")
	hubA.register("task_q1", "telegram_4242", schemas.GraphCompleted)

	if err := hubA.h.notifyTaskTerminal(obs.NewAgentEvent("task_q1", "", string(obs.EventTaskCompleted), nil)); err != nil {
		t.Fatalf("notifyTaskTerminal: %v", err)
	}

	rows := hubA.rows()
	if len(rows) != 1 {
		t.Fatalf("want one queued notification, got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.kind != kindTaskCallback || r.status != "pending" {
		t.Errorf("queued row = %s/%s, want %s/pending", r.kind, r.status, kindTaskCallback)
	}
	if r.sessionID != "telegram_4242" {
		t.Errorf("row must carry the session it belongs to, got %q", r.sessionID)
	}
	if r.dedupeKey != "task_callback|task_q1" {
		t.Errorf("dedupe key = %q", r.dedupeKey)
	}
	if !strings.Contains(r.text, "task_q1") || r.text == "" {
		t.Errorf("row text should be the notification the user never got, got %q", r.text)
	}

	// Additive, not a replacement: the live ring still gets its event, so a
	// client that IS watching notices nothing has changed.
	var ringEvent bool
	for _, ev := range session.EventsAfter(-1) {
		if ev.Type == kindTaskCallback {
			ringEvent = true
		}
	}
	if !ringEvent {
		t.Error("task_callback no longer reaches the session event ring")
	}
}

// TestNotifyTaskTerminal_SecondTerminalEventIsNotASecondNotification: a task
// can publish two terminal events; the user must not be told twice.
func TestNotifyTaskTerminal_SecondTerminalEventQueuesOnce(t *testing.T) {
	hubA := newHub(t, filepath.Join(t.TempDir(), "hub.db"))
	hubA.h.Store.GetOrCreate("telegram_7")
	hubA.register("task_q2", "telegram_7", schemas.GraphCompleted)

	if err := hubA.h.notifyTaskTerminal(obs.NewAgentEvent("task_q2", "", string(obs.EventTaskCompleted), nil)); err != nil {
		t.Fatalf("first notify: %v", err)
	}
	if err := hubA.h.notifyTaskTerminal(obs.NewAgentEvent("task_q2", "", string(obs.EventTaskFailed), nil)); err != nil {
		t.Fatalf("second notify: %v", err)
	}

	rows := hubA.rows()
	if len(rows) != 1 {
		t.Fatalf("two terminal events queued %d notifications, want 1: %+v", len(rows), rows)
	}
}

// TestNotifyTaskTerminal_WebSessionStaysHistoryOnly: only a session a transport
// can push to later is queued. A web session replays from its history, so
// queueing it would deliver a second copy of what the client already has.
func TestNotifyTaskTerminal_WebSessionStaysHistoryOnly(t *testing.T) {
	hubA := newHub(t, filepath.Join(t.TempDir(), "hub.db"))
	hubA.h.Store.GetOrCreate("s_web1")
	hubA.register("task_q3", "s_web1", schemas.GraphCompleted)

	if err := hubA.h.notifyTaskTerminal(obs.NewAgentEvent("task_q3", "", string(obs.EventTaskCompleted), nil)); err != nil {
		t.Fatalf("notifyTaskTerminal: %v", err)
	}
	if rows := hubA.rows(); len(rows) != 0 {
		t.Errorf("a non-Telegram session queued %d rows, want none: %+v", len(rows), rows)
	}
}

// TestNotifyDecisionRequired_QueuesWithNoSessionAndLogsWhy. A blocked task
// cannot resolve itself, so it needs a human even when no conversation can be
// found for it -- and where the notification went missing used to be an
// unlogged return. Measured 2026-11-06: 9 of the 12 most recent delegated tasks
// carried no session id at all.
func TestNotifyDecisionRequired_QueuesWithNoSessionAndLogsWhy(t *testing.T) {
	hubA := newHub(t, filepath.Join(t.TempDir(), "hub.db"))
	hubA.register("task_d1", "", schemas.GraphRunning)

	payload := map[string]any{
		"decision_id": "dec_1",
		"type":        "clarification",
		"options":     []string{"approve", "reject"},
	}
	if err := hubA.h.notifyDecisionRequired(obs.NewAgentEvent("task_d1", "s1", string(obs.EventDecisionRequired), payload)); err != nil {
		t.Fatalf("notifyDecisionRequired: %v", err)
	}

	rows := hubA.rows()
	if len(rows) != 1 {
		t.Fatalf("a blocked task with no session queued %d rows, want 1 (a human is still needed): %+v", len(rows), rows)
	}
	if rows[0].kind != kindDecisionNeeded || rows[0].sessionID != "" {
		t.Errorf("row = %s/session %q, want %s with no session", rows[0].kind, rows[0].sessionID, kindDecisionNeeded)
	}
	if rows[0].dedupeKey != "decision_needed|task_d1|dec_1" {
		t.Errorf("dedupe key = %q", rows[0].dedupeKey)
	}

	hubA.logger.Close()
	logged := readSinkLog(t, hubA.logDir, chatOutboxSink+".jsonl")
	if !strings.Contains(logged, eventNotificationDropped) {
		t.Fatalf("the drop left no trace in %s.jsonl:\n%s", chatOutboxSink, logged)
	}
	if !strings.Contains(logged, dropNoSessionBound) || !strings.Contains(logged, "task_d1") {
		t.Errorf("drop line should name the task and why, got:\n%s", logged)
	}
}

// TestResolveSessionForTask_NamesEveryDropReason: the three ways a task has no
// session are now distinguishable, which is what makes "no notification"
// answerable instead of mysterious.
func TestResolveSessionForTask_NamesEveryDropReason(t *testing.T) {
	hubA := newHub(t, filepath.Join(t.TempDir(), "hub.db"))
	hubA.h.Store.GetOrCreate("telegram_1")
	hubA.register("task_ok", "telegram_1", schemas.GraphCompleted)
	hubA.register("task_nosess", "", schemas.GraphCompleted)
	hubA.register("task_gone", "telegram_404", schemas.GraphCompleted)

	cases := []struct {
		name        string
		taskID      string
		wantReason  string
		wantSess    string
		wantSession bool
	}{
		{"unknown task", "task_never_submitted", dropUnknownTask, "", false},
		{"no session bound", "task_nosess", dropNoSessionBound, "", false},
		{"session evicted", "task_gone", dropSessionEvicted, "telegram_404", false},
		{"resolved", "task_ok", "", "telegram_1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := hubA.h.resolveSessionForTask(tc.taskID)
			if ref.dropReason != tc.wantReason {
				t.Errorf("drop reason = %q, want %q", ref.dropReason, tc.wantReason)
			}
			if ref.sessionID != tc.wantSess {
				t.Errorf("session id = %q, want %q -- the queue row routes on it even when the session is gone", ref.sessionID, tc.wantSess)
			}
			if (ref.session != nil) != tc.wantSession {
				t.Errorf("session present = %v, want %v", ref.session != nil, tc.wantSession)
			}
		})
	}

	if ref := hubA.h.resolveSessionForTask(""); ref.dropReason != dropUnknownTask {
		t.Errorf("empty task id = %q, want %q", ref.dropReason, dropUnknownTask)
	}
}

// TestDecisionDedupeKey_CoversBothPayloadShapes: the readiness gate sends a
// decision_id, the max-turns path sends none. Both must dedupe on something
// stable, or the same blocked step notifies once per hub that sees it.
func TestDecisionDedupeKey_CoversBothPayloadShapes(t *testing.T) {
	withID := decisionDedupeKey("task_x", "dec_9", obs.NewAgentEvent("task_x", "s1", "", nil))
	if withID != "decision_needed|task_x|dec_9" {
		t.Errorf("with a decision id, key = %q", withID)
	}
	withoutID := decisionDedupeKey("task_x", "", obs.NewAgentEvent("task_x", "s2", "", nil))
	if withoutID != "decision_needed|task_x|step_s2" {
		t.Errorf("without one the step is the only stable identifier, got %q", withoutID)
	}
}

// TestQueueNotification_MetaSurvivesJSON: the pump rebuilds the live event from
// meta_json, so what the producer put in meta has to arrive.
func TestQueueNotification_MetaSurvivesJSON(t *testing.T) {
	hubA := newHub(t, filepath.Join(t.TempDir(), "hub.db"))
	meta := map[string]any{"task_id": "task_m", "options": []string{"approve", "reject"}}

	hubA.h.queueNotification("task_m", "telegram_5", kindDecisionNeeded, "decision_needed|task_m|dec_1", "text", meta)

	rows := hubA.rows()
	if len(rows) != 1 {
		t.Fatalf("queued %d rows, want 1", len(rows))
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(rows[0].meta), &back); err != nil {
		t.Fatalf("meta_json is not JSON: %v", err)
	}
	if back["task_id"] != "task_m" {
		t.Errorf("meta lost task_id: %v", back)
	}
	// JSON has no []string, and decisionKeyboardOptions has to cope with that;
	// pinned in the pump tests.
	opts, ok := back["options"].([]any)
	if !ok || len(opts) != 2 {
		t.Errorf("options should round-trip as a 2-element list, got %#v", back["options"])
	}
}
