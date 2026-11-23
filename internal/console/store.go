package console

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// The schema creates what is missing and retires the tables of the plan,
// research and old candidate views. The old candidate table showed research's
// pessimistic net as if it were the search result.
const schema = `
DROP TABLE IF EXISTS opportunities;
DROP TABLE IF EXISTS pool_activity;
DROP TABLE IF EXISTS research_counts;
CREATE TABLE IF NOT EXISTS sends(
	instance TEXT NOT NULL,
	hash TEXT NOT NULL,
	intent_id TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT '',
	sent_ms INTEGER NOT NULL DEFAULT 0,
	asset TEXT NOT NULL DEFAULT '',
	input TEXT NOT NULL DEFAULT '',
	expected_net TEXT NOT NULL DEFAULT '',
	quoted_gross TEXT NOT NULL DEFAULT '',
	budget TEXT NOT NULL DEFAULT '',
	tip TEXT NOT NULL DEFAULT '',
	gas_limit INTEGER NOT NULL DEFAULT 0,
	conv_num TEXT NOT NULL DEFAULT '',
	conv_den TEXT NOT NULL DEFAULT '',
	bid TEXT NOT NULL DEFAULT '',
	route TEXT NOT NULL DEFAULT '[]',
	trigger_block INTEGER NOT NULL DEFAULT 0,
	trigger_source TEXT NOT NULL DEFAULT '',
	lane TEXT NOT NULL DEFAULT '',
	received_ns INTEGER NOT NULL DEFAULT 0,
	execute_ns INTEGER NOT NULL DEFAULT 0,
	ack_ns INTEGER,
	error TEXT NOT NULL DEFAULT '',
	paths TEXT NOT NULL DEFAULT '',
	landed_block INTEGER,
	landed_frame INTEGER,
	landed_index INTEGER,
	landed_ns INTEGER,
	send_to_feed_ns INTEGER,
	landed_success INTEGER,
	invalidated INTEGER NOT NULL DEFAULT 0,
	settled_block INTEGER,
	settled_ms INTEGER,
	success INTEGER,
	cancelled INTEGER NOT NULL DEFAULT 0,
	profit TEXT NOT NULL DEFAULT '',
	net TEXT NOT NULL DEFAULT '',
	fee TEXT NOT NULL DEFAULT '',
	l1_fee TEXT NOT NULL DEFAULT '',
	gas_used INTEGER,
	profit_value REAL,
	net_value REAL,
	fee_value REAL,
	profit_native REAL,
	net_native REAL,
	fee_status TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(instance, hash)
);
CREATE INDEX IF NOT EXISTS sends_recent ON sends(instance, sent_ms DESC);
CREATE TABLE IF NOT EXISTS candidates(
	id INTEGER PRIMARY KEY,
	instance TEXT NOT NULL,
	source TEXT NOT NULL,
	block INTEGER NOT NULL,
	recorded_ms INTEGER NOT NULL,
	asset TEXT NOT NULL,
	input TEXT NOT NULL,
	gross TEXT NOT NULL,
	gross_value REAL NOT NULL,
	hops INTEGER NOT NULL,
	route TEXT NOT NULL,
	research_net TEXT NOT NULL,
	research_fee TEXT NOT NULL,
	research_basis TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS candidates_recent ON candidates(instance, recorded_ms DESC);
CREATE TABLE IF NOT EXISTS reactive_skips(
	instance TEXT NOT NULL,
	to_ms INTEGER NOT NULL,
	reason TEXT NOT NULL,
	count INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS reactive_skips_recent ON reactive_skips(instance, to_ms);
CREATE TABLE IF NOT EXISTS reactive_latest(
	instance TEXT NOT NULL,
	type TEXT NOT NULL,
	at_ms INTEGER NOT NULL,
	body TEXT NOT NULL,
	PRIMARY KEY(instance, type)
);
CREATE TABLE IF NOT EXISTS assets(
	instance TEXT NOT NULL,
	token TEXT NOT NULL,
	symbol TEXT NOT NULL,
	decimals INTEGER NOT NULL,
	PRIMARY KEY(instance, token)
);
CREATE TABLE IF NOT EXISTS rivals(
	id INTEGER PRIMARY KEY,
	instance TEXT NOT NULL,
	block INTEGER NOT NULL,
	frame INTEGER NOT NULL,
	tx_index INTEGER NOT NULL,
	hash TEXT NOT NULL,
	sender TEXT NOT NULL,
	tip TEXT NOT NULL,
	tip_value REAL NOT NULL,
	success INTEGER NOT NULL,
	pools TEXT NOT NULL,
	received_ms INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS rivals_identity ON rivals(instance, hash, block);
CREATE INDEX IF NOT EXISTS rivals_block ON rivals(instance, block);
CREATE INDEX IF NOT EXISTS rivals_recent ON rivals(instance, received_ms);
CREATE TABLE IF NOT EXISTS audit(
	id INTEGER PRIMARY KEY,
	at_ms INTEGER NOT NULL,
	actor TEXT NOT NULL,
	instance TEXT NOT NULL,
	action TEXT NOT NULL,
	reason TEXT NOT NULL,
	outcome TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS tail_files(
	path TEXT PRIMARY KEY,
	instance TEXT NOT NULL,
	run_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	offset INTEGER NOT NULL DEFAULT 0,
	done INTEGER NOT NULL DEFAULT 0
);
UPDATE tail_files SET done=1 WHERE kind NOT IN ('candidate', 'reactive', 'competition');`

// Store is the console database. WAL keeps page reads off the ingest writer.
type Store struct {
	db *sql.DB
}

func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("open console database: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create console schema: %w", err)
	}
	if err := addFeeStatus(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate console schema: %w", err)
	}
	return &Store{db: db}, nil
}

// addFeeStatus adds the column a database from before feed settlement lacks.
func addFeeStatus(ctx context.Context, db *sql.DB) error {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('sends') WHERE name='fee_status'`).Scan(&present); err != nil || present > 0 {
		return err
	}
	_, err := db.ExecContext(ctx, `ALTER TABLE sends ADD COLUMN fee_status TEXT NOT NULL DEFAULT ''`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// tailFile is one append-only output of one run.
type tailFile struct {
	Path, Instance, RunID, Kind string
	Offset                      int64
}

// trackFile adopts a path for the given run. The bot appends to the same files
// across restarts, so a newer run reopens a finished path at its old offset.
func (s *Store) trackFile(ctx context.Context, file tailFile) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tail_files(path, instance, run_id, kind) VALUES(?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET run_id=excluded.run_id, done=0 WHERE tail_files.run_id <> excluded.run_id`, file.Path, file.Instance, file.RunID, file.Kind)
	return err
}

func (s *Store) openFiles(ctx context.Context, instance string) ([]tailFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, instance, run_id, kind, offset FROM tail_files WHERE instance=? AND done=0`, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []tailFile
	for rows.Next() {
		var file tailFile
		if err := rows.Scan(&file.Path, &file.Instance, &file.RunID, &file.Kind, &file.Offset); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func (s *Store) finishFile(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tail_files SET done=1 WHERE path=?`, path)
	return err
}

// commitBatch stores records together with the new offset, so a crash between
// the two can neither lose nor repeat a line.
func (s *Store) commitBatch(ctx context.Context, file tailFile, offset int64, records *batch) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if records != nil {
		if err := writeBatch(ctx, tx, file.Instance, records); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tail_files SET offset=? WHERE path=?`, offset, file.Path); err != nil {
		return err
	}
	return tx.Commit()
}

// prune bounds the high-volume tables. Sends are rare and stay.
func (s *Store) prune(ctx context.Context, keepRows int, skipsSinceMS int64) error {
	for _, table := range []string{"rivals", "candidates"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE id < (SELECT id FROM `+table+` ORDER BY id DESC LIMIT 1 OFFSET ?)`, keepRows-1); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM reactive_skips WHERE to_ms < ?`, skipsSinceMS)
	return err
}

// AuditEntry records one operator action, including refused or failed ones.
type AuditEntry struct {
	ID       int64  `json:"id"`
	AtMS     int64  `json:"at_ms"`
	Actor    string `json:"actor"`
	Instance string `json:"instance"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
	Outcome  string `json:"outcome"`
	Error    string `json:"error"`
}

const (
	outcomePending = "pending"
	outcomeApplied = "applied"
	outcomeFailed  = "failed"
)

func (s *Store) beginAudit(ctx context.Context, entry AuditEntry) (int64, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO audit(at_ms, actor, instance, action, reason, outcome) VALUES(?,?,?,?,?,?)`,
		entry.AtMS, entry.Actor, entry.Instance, entry.Action, entry.Reason, outcomePending)
	if err != nil {
		return 0, fmt.Errorf("write audit entry: %w", err)
	}
	return result.LastInsertId()
}

func (s *Store) finishAudit(ctx context.Context, id int64, outcome, failure string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE audit SET outcome=?, error=? WHERE id=?`, outcome, failure, id)
	return err
}

func (s *Store) Audit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at_ms, actor, instance, action, reason, outcome, error FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.AtMS, &e.Actor, &e.Instance, &e.Action, &e.Reason, &e.Outcome, &e.Error); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
