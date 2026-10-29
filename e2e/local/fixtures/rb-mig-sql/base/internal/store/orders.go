// Package store reads and writes orders in PostgreSQL.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Order is a row of the orders table. The legacy_ref column is no longer
// read by the service.
type Order struct {
	ID         int64
	CustomerID int64
	CreatedBy  string
	Status     string
	Total      string
	Note       sql.NullString
	CreatedAt  time.Time
}

// Orders is the orders repository.
type Orders struct{ db *sql.DB }

// NewOrders returns the repository.
func NewOrders(db *sql.DB) *Orders { return &Orders{db: db} }

// ByAuthor returns the orders created by email.
func (o *Orders) ByAuthor(ctx context.Context, email string) ([]Order, error) {
	rows, err := o.db.QueryContext(ctx, `
		SELECT id, customer_id, created_by, status, total, note, created_at
		FROM orders
		WHERE created_by = $1
		ORDER BY id DESC`, email)
	if err != nil {
		return nil, fmt.Errorf("orders by author: %w", err)
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		var r Order
		if err := rows.Scan(&r.ID, &r.CustomerID, &r.CreatedBy, &r.Status, &r.Total, &r.Note, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Create inserts an order.
func (o *Orders) Create(ctx context.Context, customerID int64, email, total string) (int64, error) {
	var id int64
	err := o.db.QueryRowContext(ctx,
		`INSERT INTO orders (customer_id, created_by, total) VALUES ($1, $2, $3) RETURNING id`,
		customerID, email, total).Scan(&id)
	return id, err
}

// SetStatus changes the status of an order.
func (o *Orders) SetStatus(ctx context.Context, id int64, status string) error {
	_, err := o.db.ExecContext(ctx, `UPDATE orders SET status = $1 WHERE id = $2`, status, id)
	return err
}
