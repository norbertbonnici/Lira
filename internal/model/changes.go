package model

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ---------- MCP write support: proposals + audit log ----------
//
// Writes from an LLM client go through propose→apply. A proposal is a stored,
// user-owned, short-lived plan; applying it claims the proposal and performs
// the changes in ONE DB transaction, logging each changed row to audit_log.
// Every change is conditional on the row still looking the way it did when it
// was proposed, so a stale plan never overwrites a human's later edit.

// ErrProposalUnavailable means the proposal does not exist, belongs to someone
// else, was already applied, or has expired.
var ErrProposalUnavailable = errors.New("proposal not found, expired, or already applied")

// Proposal actions.
const ActionRecategorize = "recategorize"

// CreateProposal stores a plan for userID and returns its id and expiry.
// Expired proposals that were never applied are pruned on the way.
func (s *Store) CreateProposal(userID int64, action string, payload []byte, ttl time.Duration) (id, expiresAt string, err error) {
	if id, err = randToken(18); err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	expiresAt = now.Add(ttl).Format(time.RFC3339)
	_, _ = s.db.Exec(`DELETE FROM mcp_proposals WHERE applied_at='' AND expires_at<?`, now.Format(time.RFC3339))
	_, err = s.db.Exec(
		`INSERT INTO mcp_proposals(id, user_id, action, payload, created_at, expires_at) VALUES(?,?,?,?,?,?)`,
		id, userID, action, string(payload), now.Format(time.RFC3339), expiresAt)
	return id, expiresAt, err
}

// PendingProposal returns the payload of a proposal userID may still apply.
// It does not claim it — ApplyRecategorization does that atomically.
func (s *Store) PendingProposal(userID int64, id, action string) ([]byte, error) {
	var payload string
	err := s.db.QueryRow(
		`SELECT payload FROM mcp_proposals WHERE id=? AND user_id=? AND action=? AND applied_at='' AND expires_at>?`,
		id, userID, action, nowRFC()).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProposalUnavailable
	}
	return []byte(payload), err
}

// RecatChange is one proposed category change. From is the category the row
// had when proposed (nil = uncategorized); To is the new one (nil = clear).
type RecatChange struct {
	TxID int64  `json:"tx"`
	From *int64 `json:"from"`
	To   *int64 `json:"to"`
}

// RecatSkip is a change that was not applied, with why.
type RecatSkip struct {
	TxID   int64
	Reason string
}

// AuditEntry is one row of audit_log.
type AuditEntry struct {
	ID       int64
	UserID   sql.NullInt64
	Username string
	Source   string
	Action   string
	Entity   string
	EntityID int64
	OldValue string
	NewValue string
	Ref      string
	At       string
}

// ApplyRecategorization claims proposal id for userID and applies its changes
// in one transaction. A change applies only if the transaction still exists, is
// not a transfer leg, still has its proposed-from category, and the target
// category still exists; the rest are returned as skipped. Returns
// ErrProposalUnavailable if the proposal can't be claimed (nothing changes).
func (s *Store) ApplyRecategorization(userID int64, id, source string, changes []RecatChange) (applied []RecatChange, skipped []RecatSkip, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	now := nowRFC()
	res, err := tx.Exec(
		`UPDATE mcp_proposals SET applied_at=? WHERE id=? AND user_id=? AND action=? AND applied_at='' AND expires_at>?`,
		now, id, userID, ActionRecategorize, now)
	if err != nil {
		return nil, nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, nil, ErrProposalUnavailable
	}
	for _, ch := range changes {
		var from, to any
		if ch.From != nil {
			from = *ch.From
		}
		if ch.To != nil {
			to = *ch.To
		}
		res, err := tx.Exec(
			`UPDATE transactions SET category_id=?
			 WHERE id=? AND transfer_id IS NULL AND category_id IS ?
			   AND (? IS NULL OR EXISTS(SELECT 1 FROM categories WHERE id=?))`,
			to, ch.TxID, from, to, to)
		if err != nil {
			return nil, nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			skipped = append(skipped, RecatSkip{ch.TxID, "changed since it was proposed (category edited, transaction deleted, or target category removed)"})
			continue
		}
		oldV, err := categoryJSON(tx, ch.From)
		if err != nil {
			return nil, nil, err
		}
		newV, err := categoryJSON(tx, ch.To)
		if err != nil {
			return nil, nil, err
		}
		if _, err := tx.Exec(
			`INSERT INTO audit_log(user_id, source, action, entity, entity_id, old_value, new_value, ref, created_at)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			userID, source, ActionRecategorize, "transaction", ch.TxID, oldV, newV, id, now); err != nil {
			return nil, nil, err
		}
		applied = append(applied, ch)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return applied, skipped, nil
}

// categoryJSON renders a category reference for the audit log, keeping the
// name as it was at change time (categories can be renamed or deleted later).
func categoryJSON(tx *sql.Tx, id *int64) (string, error) {
	v := map[string]any{"categoryId": nil, "category": ""}
	if id != nil {
		var name string
		err := tx.QueryRow(`SELECT name FROM categories WHERE id=?`, *id).Scan(&name)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		v["categoryId"], v["category"] = *id, name
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// ListAuditLog returns the most recent audit entries, newest first.
func (s *Store) ListAuditLog(limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(
		`SELECT a.id, a.user_id, COALESCE(u.username,''), a.source, a.action, a.entity, a.entity_id,
		        a.old_value, a.new_value, a.ref, a.created_at
		 FROM audit_log a LEFT JOIN users u ON u.id=a.user_id
		 ORDER BY a.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.UserID, &e.Username, &e.Source, &e.Action, &e.Entity, &e.EntityID,
			&e.OldValue, &e.NewValue, &e.Ref, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
