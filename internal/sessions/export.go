package sessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ExportOptions selects snapshots for one local collector. After is an exclusive
// cursor. Limit defaults to 100, and is capped at MaxPage; the JSON page budget
// also applies, but always allows one whole item without cutting its contents.
// Ack prunes snapshots at or below that cursor, including across restarts. Only
// acknowledge what the destination has durably accepted. There is one shared
// acknowledgement stream, not independent retention for multiple collectors.
//
// Since is a Unix millisecond watermark, established only by the first Export.
// Existing sessions with UpdatedAt < Since get a baseline but no log items.
// Later changes (including changes to those sessions) and later arrivals are
// exported regardless of their timestamps. Nil on later calls inherits the
// persisted watermark; later calls keep that first watermark. On the first
// call in a new store, After and Ack are ignored, since they may name a lost
// epoch. The collector must detect Epoch changes and restart its cursor at 0.
type ExportOptions struct {
	After int64
	Limit int
	Ack   *int64
	Since *int64
}

// ExportPage is the response for the local control export endpoint. Next is the
// last delivered cursor (After when empty), even on the last page; it is never
// reset to zero at the end. Items are immutable, ordered by increasing Cursor.
type ExportPage struct {
	Epoch string       `json:"epoch"`
	Items []ExportItem `json:"items"`
	Next  int64        `json:"next"`
	More  bool         `json:"more"`
}

// ExportItem has Type "session" or "message" and exactly one nested snapshot.
// Key, Agent, G and Rev belong to both kinds. Seq is meaningful only for a
// message (and equals Message.Seq); it is 0 on a session item. Deleted is only
// used on session items. A session's span and totals are Session.CreatedAt,
// UpdatedAt and Messages. Message contains the entire Source message, without
// the peer wire clamps; truncation already done by a source cannot be undone.
//
// A message is keyed by Key, G, Seq. G starts at zero and advances once whenever
// an observed Seq has a different role or time. The whole current session is
// then emitted under the new G, preceded by its session item. Earlier snapshots
// are retained until Ack; earlier generation identities remain in the baseline.
type ExportItem struct {
	Cursor  int64    `json:"cursor"`
	Type    string   `json:"type"`
	Key     string   `json:"key"`
	Agent   string   `json:"agent"`
	G       int64    `json:"g"`
	Seq     int64    `json:"seq"`
	Rev     int64    `json:"rev"`
	Deleted bool     `json:"deleted,omitempty"`
	Session *Session `json:"session,omitempty"`
	Message *Message `json:"message,omitempty"`
}

func (s *Store) exportActive(ctx context.Context) (bool, error) {
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM export_state)`).Scan(&active)
	return active, err
}

// scanExport does nothing before the first Export (initialize != nil). The
// append, message identities and per-session scan revision commit together.
// Callers hold exportMu, so a refresh cannot replace the feed while scanning.
func (s *Store) scanExport(ctx context.Context, self string, source Source, initialize *ExportOptions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var since sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT since FROM export_state WHERE id=1`).Scan(&since)
	first := errors.Is(err, sql.ErrNoRows)
	if err != nil && !first {
		return err
	}
	if first {
		if initialize == nil {
			return nil
		}
		if initialize.Since != nil {
			since = sql.NullInt64{Int64: *initialize.Since, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO export_state(id,since) VALUES (1,?)`, since); err != nil {
			return err
		}
	}
	// Load only metadata here and close its rows before issuing writes. Message
	// bodies are held for one changed session at a time, read in Source pages.
	rows, err := tx.QueryContext(ctx, `SELECT `+sessionColumns+`, rev, deleted FROM sessions
		WHERE node=? AND rev > COALESCE((SELECT rev FROM export_scan WHERE key=sessions.key),0)
		ORDER BY rev`, self)
	if err != nil {
		return err
	}
	var changes []Change
	for rows.Next() {
		var c Change
		if c.Session, err = scanSession(rows.Scan, &c.Rev, &c.Deleted); err != nil {
			rows.Close()
			return err
		}
		changes = append(changes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range changes {
		emit := !(first && since.Valid && c.UpdatedAt < since.Int64)
		if err := scanExportSession(ctx, tx, source, c, emit); err != nil {
			return fmt.Errorf("export session %q: %w", c.Key, err)
		}
	}
	return tx.Commit()
}

type exportIdentity struct {
	role  string
	time  int64
	print string
}

func scanExportSession(ctx context.Context, tx *sql.Tx, source Source, c Change, emit bool) error {
	var g int64
	var logged bool
	err := tx.QueryRowContext(ctx, `SELECT g,logged FROM export_scan WHERE key=?`, c.Key).Scan(&g, &logged)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	known := make(map[int64]exportIdentity)
	rows, err := tx.QueryContext(ctx, `SELECT seq,role,ts,print FROM export_messages WHERE key=? AND g=?`, c.Key, g)
	if err != nil {
		return err
	}
	for rows.Next() {
		var seq int64
		var id exportIdentity
		if err := rows.Scan(&seq, &id.role, &id.time, &id.print); err != nil {
			rows.Close()
			return err
		}
		known[seq] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var messages []Message
	if !c.Deleted {
		messages, err = exportMessages(ctx, source, c.Key)
		if err != nil {
			return err
		}
	}
	renumbered := false
	for _, m := range messages {
		if old, ok := known[m.Seq]; ok && (old.role != m.Role || old.time != m.Time) {
			renumbered = true
			g++
			break
		}
	}
	if emit {
		if err := appendExport(ctx, tx, ExportItem{Type: "session", Key: c.Key, Agent: c.Agent,
			G: g, Rev: c.Rev, Deleted: c.Deleted, Session: &c.Session}); err != nil {
			return err
		}
	}
	for _, m := range messages {
		// ID is only a paging cursor; reparsing may replace it without changing
		// the message. The snapshot still contains the actual ID read this time.
		body := m
		body.ID = 0
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		print := hex.EncodeToString(sum[:])
		old, exists := known[m.Seq]
		if emit && (!logged || renumbered || !exists || old.print != print) {
			if err := appendExport(ctx, tx, ExportItem{Type: "message", Key: c.Key, Agent: c.Agent,
				G: g, Seq: m.Seq, Rev: c.Rev, Message: &m}); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO export_messages(key,g,seq,role,ts,print) VALUES (?,?,?,?,?,?)
			ON CONFLICT(key,g,seq) DO UPDATE SET role=excluded.role,ts=excluded.ts,print=excluded.print`,
			c.Key, g, m.Seq, m.Role, m.Time, print); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO export_scan(key,rev,g,logged) VALUES (?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET rev=excluded.rev,g=excluded.g,logged=excluded.logged`,
		c.Key, c.Rev, g, logged || emit)
	return err
}

func exportMessages(ctx context.Context, source Source, key string) ([]Message, error) {
	var out []Message
	var after int64
	seen := make(map[int64]bool)
	for {
		messages, more, err := source.Messages(ctx, key, after, MaxPage, PageBudget)
		if err != nil {
			return nil, err
		}
		for _, m := range messages {
			if m.ID <= after || seen[m.Seq] {
				return nil, errors.New("source message IDs must advance and seq must be unique")
			}
			after = m.ID
			seen[m.Seq] = true
			out = append(out, m)
		}
		if !more {
			return out, nil
		}
		if len(messages) == 0 {
			return nil, errors.New("source returned an empty page with more messages")
		}
	}
}

func appendExport(ctx context.Context, tx *sql.Tx, item ExportItem) error {
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO export_log(item) VALUES (?)`, string(raw))
	return err
}

func (s *Store) exportPage(ctx context.Context, options ExportOptions) (ExportPage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExportPage{}, err
	}
	defer tx.Rollback()
	out := ExportPage{Next: options.After, Items: []ExportItem{}}
	if err := tx.QueryRowContext(ctx, `SELECT v FROM meta WHERE k='epoch'`).Scan(&out.Epoch); err != nil {
		return ExportPage{}, err
	}
	if options.Ack != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM export_log WHERE cursor<=?`, *options.Ack); err != nil {
			return ExportPage{}, err
		}
	}
	limit := options.Limit
	if limit < 1 {
		limit = 100
	}
	limit = min(limit, MaxPage)
	rows, err := tx.QueryContext(ctx, `SELECT cursor,item FROM export_log WHERE cursor>? ORDER BY cursor LIMIT ?`, options.After, limit+1)
	if err != nil {
		return ExportPage{}, err
	}
	size := 0
	for rows.Next() {
		var item ExportItem
		var cursor int64
		var raw string
		if err := rows.Scan(&cursor, &raw); err != nil {
			rows.Close()
			return ExportPage{}, err
		}
		size += len(raw) + 1
		if len(out.Items) == limit || (size > PageBudget && len(out.Items) > 0) {
			out.More = true
			break
		}
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			rows.Close()
			return ExportPage{}, err
		}
		item.Cursor = cursor
		out.Items = append(out.Items, item)
		out.Next = cursor
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ExportPage{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExportPage{}, err
	}
	return out, nil
}
