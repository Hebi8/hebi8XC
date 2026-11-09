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
// epoch. A supplied Epoch that differs also resets After and ignores Ack.
// The collector must detect Epoch changes and restart its cursor at 0.
type ExportOptions struct {
	Epoch string
	After int64
	Limit int
	Ack   *int64
	Since *int64
}

// ExportPage is the response for the local control export endpoint. Next is the
// last delivered cursor (After when empty), even on the last page; it is never
// reset to zero at the end. A first request or epoch mismatch starts at zero.
// Items are immutable, ordered by increasing Cursor.
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

// scanExport initializes a durable cutoff, then commits each session separately.
// Source reads and fingerprint/JSON work never hold the store connection.
// Callers hold exportMu; peer reads do not take that lock.
func (s *Store) scanExport(ctx context.Context, self string, source Source, initialize *ExportOptions) error {
	var since sql.NullInt64
	var initialRev int64
	err := s.db.QueryRowContext(ctx, `SELECT since,initial_rev FROM export_state WHERE id=1`).Scan(&since, &initialRev)
	if errors.Is(err, sql.ErrNoRows) {
		if initialize == nil {
			return nil
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO export_state(id,since,initial_rev)
   SELECT 1,?,COALESCE(MAX(rev),0) FROM sessions WHERE node=?`, initialize.Since, self); err != nil {
			return err
		}
		if err := s.db.QueryRowContext(ctx, `SELECT since,initial_rev FROM export_state WHERE id=1`).Scan(&since, &initialRev); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+`, rev, deleted FROM sessions
  WHERE node=? AND rev > COALESCE((SELECT rev FROM export_scan WHERE key=sessions.key),0) ORDER BY rev`, self)
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
	var failures []error
	for _, c := range changes {
		initialBaseline := since.Valid && c.Rev <= initialRev && c.UpdatedAt < since.Int64
		if err := s.scanExportSession(ctx, source, c, initialBaseline); err != nil {
			failures = append(failures, fmt.Errorf("export session %q: %w", c.Key, err))
		}
	}
	return errors.Join(failures...)
}

type exportIdentity struct {
	role       string
	time       int64
	print      string
	structured bool
}

type exportMessage struct {
	message Message
	print   string
}

func (s *Store) scanExportSession(ctx context.Context, source Source, c Change, initialBaseline bool) error {
	var g int64
	var logged bool
	var previousJSON string
	err := s.db.QueryRowContext(ctx, `SELECT g,logged,session FROM export_scan WHERE key=?`, c.Key).Scan(&g, &logged, &previousJSON)
	scanned := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var previous Session
	if scanned {
		if err := json.Unmarshal([]byte(previousJSON), &previous); err != nil {
			return err
		}
	}
	known := make(map[int64]exportIdentity)
	rows, err := s.db.QueryContext(ctx, `SELECT seq,role,ts,print,structured FROM export_messages WHERE key=? AND g=?`, c.Key, g)
	if err != nil {
		return err
	}
	for rows.Next() {
		var seq int64
		var id exportIdentity
		if err := rows.Scan(&seq, &id.role, &id.time, &id.print, &id.structured); err != nil {
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
	// Retain richer history while the source's side tables are being rebuilt.
	// New seqs still enter the log; skipped identities keep their old fingerprint.
	degraded := !c.Deleted && previous.ToolCalls != nil && c.ToolCalls == nil
	var current []exportMessage
	changed, renumbered := c.Deleted, false
	for _, m := range messages {
		old, exists := known[m.Seq]
		if exists && old.structured && m.Kind == "" {
			degraded = true
			continue
		}
		body := m
		body.ID = 0 // source paging ids change on reindexing
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		print := hex.EncodeToString(sum[:])
		changed = changed || !exists || old.print != print
		renumbered = renumbered || (exists && (old.role != m.Role || old.time != m.Time))
		current = append(current, exportMessage{message: m, print: print})
	}
	// A stale parse cannot supply the full new generation. Keep identities
	// stable until structure recovers; new seqs can still be archived now.
	if degraded && renumbered {
		renumbered = false
		kept := current[:0]
		changed = false
		for _, m := range current {
			old, exists := known[m.message.Seq]
			if exists && (old.role != m.message.Role || old.time != m.message.Time) {
				continue
			}
			changed = changed || !exists || old.print != m.print
			kept = append(kept, m)
		}
		current = kept
	}
	if degraded {
		c.ToolCalls, c.ToolErrors = previous.ToolCalls, previous.ToolErrors
		c.Tokens, c.Files = previous.Tokens, previous.Files
	}
	if renumbered {
		g++
	}
	emit := logged || (scanned && changed) || (!scanned && !initialBaseline)
	var items []string
	if emit {
		raw, err := json.Marshal(ExportItem{Type: "session", Key: c.Key, Agent: c.Agent, G: g, Rev: c.Rev, Deleted: c.Deleted, Session: &c.Session})
		if err != nil {
			return err
		}
		items = append(items, string(raw))
	}
	for i := range current {
		m := &current[i]
		old, exists := known[m.message.Seq]
		if emit && (!logged || renumbered || !exists || old.print != m.print) {
			raw, err := json.Marshal(ExportItem{Type: "message", Key: c.Key, Agent: c.Agent, G: g, Seq: m.message.Seq, Rev: c.Rev, Message: &m.message})
			if err != nil {
				return err
			}
			items = append(items, string(raw))
		}
	}
	sessionJSON, err := json.Marshal(c.Session)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, raw := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO export_log(item) VALUES (?)`, raw); err != nil {
			return err
		}
	}
	for _, m := range current {
		if _, err := tx.ExecContext(ctx, `INSERT INTO export_messages(key,g,seq,role,ts,print,structured) VALUES (?,?,?,?,?,?,?)
   ON CONFLICT(key,g,seq) DO UPDATE SET role=excluded.role,ts=excluded.ts,print=excluded.print,structured=excluded.structured`,
			c.Key, g, m.message.Seq, m.message.Role, m.message.Time, m.print, m.message.Kind != ""); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO export_scan(key,rev,g,logged,session) VALUES (?,?,?,?,?)
  ON CONFLICT(key) DO UPDATE SET rev=excluded.rev,g=excluded.g,logged=excluded.logged,session=excluded.session`,
		c.Key, c.Rev, g, logged || emit, string(sessionJSON)); err != nil {
		return err
	}
	return tx.Commit()
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
	if options.Epoch != "" && options.Epoch != out.Epoch {
		options.After, options.Ack, out.Next = 0, nil, 0
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
