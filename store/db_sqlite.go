package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// InitDB initializes a pure-Go SQLite database with WAL mode enabled.
func InitDB(dbPath string) (*sql.DB, error) {
	if dbPath == "" {
		dbPath = "orchestrator_runtime.db"
	}

	// Ensure parent directory exists
	dir := filepath.Dir(dbPath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create db directory: %w", err)
		}
	}

	// Open connection with WAL mode and normal synchronous flags for high concurrency
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite db: %w", err)
	}

	// WAL mode supports concurrent readers and a single writer. Allow a small
	// pool of connections so concurrent step goroutines don't serialize on DB
	// reads. The busy_timeout pragma (5000ms) handles writer contention safely.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping sqlite db: %w", err)
	}

	// Run schema migrations
	if err := runMigrations(db); err != nil {
		db.Close() // audit M8: prevent connection pool leak on migration failure
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return db, nil
}

func runMigrations(db *sql.DB) error {
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS roles_meta (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		base_capability TEXT NOT NULL,
		provider TEXT,
		model TEXT,
		raw_json TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS mcp_configs (
		id TEXT PRIMARY KEY,
		url TEXT,
		command TEXT,
		trusted BOOLEAN,
		raw_json TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS task_steps (
		task_id    TEXT NOT NULL,
		step_id    TEXT NOT NULL,
		status     TEXT DEFAULT 'pending', -- pending | running | done | failed
		result_json BLOB,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (task_id, step_id)
	);
	CREATE INDEX IF NOT EXISTS idx_task_steps_task ON task_steps(task_id);

	-- Task-level state (graph lifecycle). task_steps stores per-step results only;
	-- without this table GetStatus() has nothing to fall back to when the graph is
	-- not in the in-process map (e.g. Master/Proxy split, or after a restart).
	CREATE TABLE IF NOT EXISTS tasks (
		task_id    TEXT PRIMARY KEY,
		status     TEXT NOT NULL DEFAULT 'running',
		graph_json BLOB,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);

	-- P1 ExperienceStore Tables
	CREATE TABLE IF NOT EXISTS task_patterns (
		id TEXT PRIMARY KEY,
		task_type TEXT,
		sequence_key TEXT,
		sample_count INTEGER DEFAULT 1,
		avg_confidence REAL DEFAULT 0,
		last_seen DATETIME,
		is_seed BOOLEAN DEFAULT FALSE,
		source_text TEXT,
		embedding BLOB,
		embedding_model TEXT,
		raw_json TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS role_profiles (
		role_id TEXT PRIMARY KEY,
		total_runs INTEGER DEFAULT 0,
		success_count INTEGER DEFAULT 0,
		partial_count INTEGER DEFAULT 0,
		failure_count INTEGER DEFAULT 0,
		avg_confidence REAL DEFAULT 0,
		raw_json TEXT NOT NULL,
		last_updated DATETIME
	);

	CREATE TABLE IF NOT EXISTS route_weights (
		role_id TEXT NOT NULL,
		model_id TEXT NOT NULL,
		capability TEXT NOT NULL,
		skill_id TEXT NOT NULL,
		total_runs INTEGER DEFAULT 0,
		success_count INTEGER DEFAULT 0,
		avg_score REAL DEFAULT 0,
		weight REAL DEFAULT 0.5,
		last_updated DATETIME,
		PRIMARY KEY (role_id, model_id, capability, skill_id)
	);

	CREATE TABLE IF NOT EXISTS generated_skills (
		id TEXT PRIMARY KEY,
		capability TEXT NOT NULL,
		description TEXT,
		usage_count INTEGER DEFAULT 0,
		success_rate REAL DEFAULT 0,
		is_verified BOOLEAN DEFAULT FALSE,
		criticality REAL DEFAULT 0,
		parent_id TEXT,
		os TEXT,
		arch TEXT,
		shell TEXT,
		failure_signal TEXT,
		source_text TEXT,
		embedding BLOB,
		embedding_model TEXT,
		last_used DATETIME,
		token_cost_total INTEGER DEFAULT 0,
		avg_latency_ms REAL DEFAULT 0,
		metabolic_roi REAL DEFAULT 1.0,
		skill_status TEXT DEFAULT 'active'
	);

	CREATE TABLE IF NOT EXISTS decision_outcomes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		decision_type TEXT,
		role_id TEXT,
		capability TEXT,
		choice TEXT,
		resolved BOOLEAN DEFAULT FALSE,
		final_confidence REAL,
		timestamp DATETIME
	);

	CREATE TABLE IF NOT EXISTS skill_affinities (
		base_capability TEXT NOT NULL,
		added_skill TEXT NOT NULL,
		confidence_delta REAL DEFAULT 0,
		sample_count INTEGER DEFAULT 0,
		last_updated DATETIME,
		is_seed BOOLEAN DEFAULT FALSE,
		PRIMARY KEY (base_capability, added_skill)
	);

	CREATE TABLE IF NOT EXISTS environment_issues (
		command TEXT PRIMARY KEY,
		message TEXT,
		count INTEGER DEFAULT 1,
		last_seen DATETIME,
		is_resolved BOOLEAN DEFAULT FALSE
	);

	CREATE TABLE IF NOT EXISTS decision_precedents (
		id TEXT PRIMARY KEY,
		raw_json TEXT NOT NULL,
		embedding BLOB,
		embedding_model TEXT,
		created_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS role_affinities (
		role_id TEXT NOT NULL,
		task_type TEXT NOT NULL,
		score REAL DEFAULT 0,
		PRIMARY KEY (role_id, task_type)
	);

	-- P2 Schedules and MemoryBank Tables
	CREATE TABLE IF NOT EXISTS schedules (
		id TEXT PRIMARY KEY,
		name TEXT,
		cron TEXT,
		task_request_json TEXT,
		last_run DATETIME,
		next_run DATETIME,
		is_enabled BOOLEAN DEFAULT TRUE,
		created_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS memory_bank (
		category TEXT,
		key TEXT,
		content TEXT,
		metadata_json TEXT,
		embedding BLOB,
		embedding_model TEXT,
		last_updated DATETIME,
		PRIMARY KEY (category, key)
	);

	CREATE TABLE IF NOT EXISTS anti_patterns (
		id TEXT PRIMARY KEY,
		anti_pattern TEXT,
		correct_pattern TEXT,
		trigger_condition TEXT,
		symptom TEXT,
		category TEXT,
		confidence REAL,
		tags_json TEXT,
		source_text TEXT,
		embedding BLOB,
		embedding_model TEXT,
		last_seen DATETIME,
		created_at DATETIME
	);

	-- P3 State Potential Table (ADDED 2026-09-08)
	CREATE TABLE IF NOT EXISTS state_potentials (
		state_hash TEXT PRIMARY KEY,
		tool_id TEXT,
		total_runs INTEGER DEFAULT 0,
		success_count INTEGER DEFAULT 0,
		potential REAL DEFAULT 0,
		last_updated DATETIME
	);

	-- P4 Tool Co-occurrence Table (ADDED 2026-09-13)
	-- Tracks consecutive tool/capability pairs within the same task for
	-- Compound Skill auto-crystallization. See docs/COMPOUND_SKILLS_DESIGN.md
	CREATE TABLE IF NOT EXISTS tool_cooccurrence (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tool_a TEXT NOT NULL,
		tool_b TEXT NOT NULL,
		co_count INTEGER DEFAULT 1,
		success_count INTEGER DEFAULT 1,
		avg_latency_ms REAL DEFAULT 0,
		last_updated DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(tool_a, tool_b)
	);

	-- P5 Experience Graph Tables (ADDED 2026-09-15 for A28 DB collapse)
	-- Stores execution trace nodes and their directed relationships.
	-- Replaces experience_nodes.json / experience_edges.json file persistence.
	CREATE TABLE IF NOT EXISTS experience_nodes (
		node_id       TEXT PRIMARY KEY,
		task_id       TEXT NOT NULL,
		step_id       TEXT,
		role_id       TEXT,
		capability    TEXT,
		strategy      TEXT,
		action        TEXT,
		outcome       TEXT,
		error_signal  TEXT,
		model_id      TEXT,
		failure_mode  TEXT,
		critique      TEXT,
		confidence    REAL DEFAULT 0,
		source_text   TEXT,
		embedding     BLOB,
		embedding_model TEXT,
		timestamp     DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_exp_nodes_task ON experience_nodes(task_id);
	CREATE INDEX IF NOT EXISTS idx_exp_nodes_capability ON experience_nodes(capability);
	CREATE INDEX IF NOT EXISTS idx_exp_nodes_outcome ON experience_nodes(outcome);

	CREATE TABLE IF NOT EXISTS experience_edges (
		source_id TEXT NOT NULL,
		target_id TEXT NOT NULL,
		relation  TEXT NOT NULL,
		PRIMARY KEY (source_id, target_id, relation)
	);
	CREATE INDEX IF NOT EXISTS idx_exp_edges_source ON experience_edges(source_id);
	CREATE INDEX IF NOT EXISTS idx_exp_edges_target ON experience_edges(target_id);

	-- P6 Chat Session Tables (ADDED 2026-09-15 for A20 DB collapse)
	-- Replaces file-per-session JSON persistence for chat history.
	CREATE TABLE IF NOT EXISTS chat_sessions (
		id             TEXT PRIMARY KEY,
		root_id        TEXT,
		active_leaf_id TEXT,
		created_at     DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS chat_messages (
		id         TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		parent_id  TEXT,
		role       TEXT NOT NULL,
		content    TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages(session_id);
	CREATE INDEX IF NOT EXISTS idx_chat_messages_parent ON chat_messages(parent_id);

	-- P7 Promotion Audit Log (ADDED 2026-09-15 for A31 DB collapse)
	CREATE TABLE IF NOT EXISTS promotion_audit (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		candidate_id TEXT NOT NULL,
		action       TEXT NOT NULL,
		auditor      TEXT,
		reason       TEXT,
		timestamp    DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_promo_audit_candidate ON promotion_audit(candidate_id);

	-- P8 Replay Mutation Records (ADDED 2026-09-18 for offline replay self-optimization)
	-- See docs/architecture/OFFLINE_REPLAY_SELF_OPTIMIZATION_DESIGN.md
	CREATE TABLE IF NOT EXISTS replay_mutation_records (
		id               TEXT PRIMARY KEY,
		source_task_id   TEXT NOT NULL,
		source_step_id   TEXT,
		mutation_type    TEXT NOT NULL,
		mutation_payload TEXT,
		verifier_result  TEXT,
		verifier_reason  TEXT,
		candidate_id     TEXT,
		timestamp        DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_replay_mut_source ON replay_mutation_records(source_task_id);
	CREATE INDEX IF NOT EXISTS idx_replay_mut_candidate ON replay_mutation_records(candidate_id);

	-- P9 Model Capability Profiles (ADDED 2026-09-19 for intelligent routing)
	-- See docs/architecture/MODEL_CAPABILITY_AND_INTELLIGENT_ROUTING_ROADMAP.md
	CREATE TABLE IF NOT EXISTS model_capability_profiles (
		model_id       TEXT NOT NULL,
		capability     TEXT NOT NULL,
		total_runs     INTEGER DEFAULT 0,
		success_count  INTEGER DEFAULT 0,
		avg_turns_used REAL DEFAULT 0,
		avg_latency_ms REAL DEFAULT 0,
		avg_token_cost REAL DEFAULT 0,
		theta          REAL DEFAULT 0,
		last_updated   DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (model_id, capability)
	);
	`)
	return err
}
