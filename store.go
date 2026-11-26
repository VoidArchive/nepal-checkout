package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/voidarchive/nepal-payment-go/v2/payments"
	_ "modernc.org/sqlite"
)

// errDB marks store failures so the API can tell them from gateway failures.
var errDB = errors.New("database error")

// shortcut: the schema is created with IF NOT EXISTS and there is no migration tool yet; add one before the first schema change.
const schema = `
CREATE TABLE IF NOT EXISTS payments (
	id TEXT PRIMARY KEY,
	order_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	amount INTEGER NOT NULL,
	status TEXT NOT NULL,
	provider_status TEXT NOT NULL,
	provider_ref TEXT NOT NULL,
	transaction_id TEXT NOT NULL,
	checkout TEXT NOT NULL,
	checks INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	next_check_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS payments_order ON payments (order_id);
CREATE INDEX IF NOT EXISTS payments_due ON payments (status, next_check_at);
CREATE TABLE IF NOT EXISTS webhook_events (
	seq INTEGER PRIMARY KEY, -- creation order, also within one clock tick
	id TEXT NOT NULL UNIQUE,
	payment_id TEXT NOT NULL REFERENCES payments (id),
	body TEXT NOT NULL,
	state TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER NOT NULL,
	last_status INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	delivered_at INTEGER
);
CREATE INDEX IF NOT EXISTS webhook_events_payment ON webhook_events (payment_id, seq);
CREATE INDEX IF NOT EXISTS webhook_events_due ON webhook_events (state, next_attempt_at);
`

// store is a SQLite payments.Store that also holds the webhook outbox.
type store struct {
	db        *sql.DB
	publicURL string // for the payment object in webhook bodies
	now       func() time.Time
}

func openStore(path, publicURL string, now func() time.Time) (*store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &store{db: db, publicURL: publicURL, now: now}, nil
}

func (s *store) Close() error { return s.db.Close() }

const paymentCols = `id, order_id, provider, amount, status, provider_status, provider_ref, transaction_id, checkout, checks, created_at, updated_at, next_check_at`

type scanner interface{ Scan(...any) error }

func scanPayment(row scanner) (*payments.Payment, error) {
	var p payments.Payment
	var checkout string
	var created, updated, next int64
	err := row.Scan(&p.ID, &p.OrderID, &p.Provider, &p.Amount, &p.Status, &p.ProviderStatus, &p.ProviderRef,
		&p.TransactionID, &checkout, &p.Checks, &created, &updated, &next)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(checkout), &p.Checkout); err != nil {
		return nil, fmt.Errorf("decode checkout of %s: %w", p.ID, err)
	}
	p.CreatedAt, p.UpdatedAt, p.NextCheckAt = fromNano(created), fromNano(updated), fromNano(next)
	return &p, nil
}

func fromNano(n int64) time.Time { return time.Unix(0, n).UTC() }

func (s *store) Create(ctx context.Context, p *payments.Payment) (*payments.Payment, error) {
	existing, err := s.create(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDB, err)
	}
	return existing, nil
}

func (s *store) create(ctx context.Context, p *payments.Payment) (*payments.Payment, error) {
	checkout, err := json.Marshal(p.Checkout)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if existing, err := findActive(ctx, tx, p.OrderID, p.Provider); existing != nil || err != nil {
		return existing, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO payments (`+paymentCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.OrderID, p.Provider, p.Amount, p.Status, p.ProviderStatus, p.ProviderRef, p.TransactionID,
		string(checkout), p.Checks, p.CreatedAt.UnixNano(), p.UpdatedAt.UnixNano(), p.NextCheckAt.UnixNano())
	if err != nil {
		return nil, err
	}
	return nil, tx.Commit()
}

// findActive returns the order's paid payment, or its pending payment with this provider, or nil.
func findActive(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, orderID, provider string) (*payments.Payment, error) {
	p, err := scanPayment(q.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM payments
		WHERE order_id = ? AND (status = 'paid' OR (status = 'pending' AND provider = ?))
		ORDER BY created_at LIMIT 1`, orderID, provider))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// FindActive implements payments.ActiveFinder: a repeated create does not call the gateway.
func (s *store) FindActive(ctx context.Context, orderID, provider string) (*payments.Payment, error) {
	p, err := findActive(ctx, s.db, orderID, provider)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDB, err)
	}
	return p, nil
}

func (s *store) Get(ctx context.Context, id string) (*payments.Payment, error) {
	p, err := scanPayment(s.db.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM payments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, payments.ErrNotFound
	}
	return p, err
}

// Update is a compare-and-swap on status. A status change also queues its
// webhook event in the same transaction, so a crash cannot lose it.
func (s *store) Update(ctx context.Context, p *payments.Payment, from payments.Status) (bool, error) {
	checkout, err := json.Marshal(p.Checkout)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `UPDATE payments SET order_id = ?, provider = ?, amount = ?, status = ?,
		provider_status = ?, provider_ref = ?, transaction_id = ?, checkout = ?, checks = ?,
		created_at = ?, updated_at = ?, next_check_at = ? WHERE id = ? AND status = ?`,
		p.OrderID, p.Provider, p.Amount, p.Status, p.ProviderStatus, p.ProviderRef, p.TransactionID,
		string(checkout), p.Checks, p.CreatedAt.UnixNano(), p.UpdatedAt.UnixNano(), p.NextCheckAt.UnixNano(), p.ID, from)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			err = tx.QueryRowContext(ctx, `SELECT 1 FROM payments WHERE id = ?`, p.ID).Scan(new(int))
		}
		if errors.Is(err, sql.ErrNoRows) {
			return false, payments.ErrNotFound
		}
		return false, err
	}

	if p.Status != from {
		now := s.now()
		id := rand.Text()
		body, err := json.Marshal(webhookBody{ID: id, Type: "payment.status_changed", CreatedAt: now.UTC(), Payment: paymentView(p, s.publicURL)})
		if err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO webhook_events (id, payment_id, body, state, next_attempt_at, created_at)
			VALUES (?, ?, ?, 'pending', ?, ?)`, id, p.ID, string(body), now.UnixNano(), now.UnixNano())
		if err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (s *store) ListDue(ctx context.Context, now time.Time, limit int) ([]*payments.Payment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+paymentCols+` FROM payments
		WHERE status = 'pending' AND next_check_at <= ? ORDER BY created_at, id LIMIT ?`, now.UnixNano(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var due []*payments.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		due = append(due, p)
	}
	return due, rows.Err()
}

// event is one webhook outbox row; its JSON form is the delivery log entry.
type event struct {
	ID            string     `json:"id"`
	PaymentID     string     `json:"-"`
	Body          string     `json:"-"`
	State         string     `json:"state"` // pending, delivered, failed
	Attempts      int        `json:"attempts"`
	LastStatus    int        `json:"last_status"` // 0 when no HTTP response yet
	LastError     string     `json:"last_error"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	CreatedAt     time.Time  `json:"created_at"`
	DeliveredAt   *time.Time `json:"delivered_at"`
}

const eventCols = `id, payment_id, body, state, attempts, last_status, last_error, next_attempt_at, created_at, delivered_at`

func (s *store) queryEvents(ctx context.Context, query string, args ...any) ([]event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	evs := []event{}
	for rows.Next() {
		var e event
		var next, created int64
		var delivered sql.NullInt64
		if err := rows.Scan(&e.ID, &e.PaymentID, &e.Body, &e.State, &e.Attempts, &e.LastStatus, &e.LastError,
			&next, &created, &delivered); err != nil {
			return nil, err
		}
		e.NextAttemptAt, e.CreatedAt = fromNano(next), fromNano(created)
		if delivered.Valid {
			t := fromNano(delivered.Int64)
			e.DeliveredAt = &t
		}
		evs = append(evs, e)
	}
	return evs, rows.Err()
}

func (s *store) events(ctx context.Context, paymentID string) ([]event, error) {
	return s.queryEvents(ctx, `SELECT `+eventCols+` FROM webhook_events WHERE payment_id = ? ORDER BY seq`, paymentID)
}

// dueEvents returns pending events whose time has come, skipping any event
// whose payment still has an earlier pending event, so each payment's events go out in order.
func (s *store) dueEvents(ctx context.Context, now time.Time, limit int) ([]event, error) {
	return s.queryEvents(ctx, `SELECT `+eventCols+` FROM webhook_events e
		WHERE state = 'pending' AND next_attempt_at <= ?
		AND NOT EXISTS (SELECT 1 FROM webhook_events o WHERE o.payment_id = e.payment_id AND o.state = 'pending' AND o.seq < e.seq)
		ORDER BY seq LIMIT ?`, now.UnixNano(), limit)
}

func (s *store) saveEvent(ctx context.Context, e event) error {
	var delivered any
	if e.DeliveredAt != nil {
		delivered = e.DeliveredAt.UnixNano()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE webhook_events SET state = ?, attempts = ?, last_status = ?, last_error = ?,
		next_attempt_at = ?, delivered_at = ? WHERE id = ?`,
		e.State, e.Attempts, e.LastStatus, e.LastError, e.NextAttemptAt.UnixNano(), delivered, e.ID)
	return err
}
