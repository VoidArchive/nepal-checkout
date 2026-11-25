package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/voidarchive/nepal-payment-go/v2/payments"
	"github.com/voidarchive/nepal-payment-go/v2/payments/storetest"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "test.db"), "https://pay.example", time.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreConformance(t *testing.T) {
	storetest.Run(t, func() payments.Store { return newTestStore(t) })
}

func TestStoreOutbox(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()
	p := &payments.Payment{ID: "p1", OrderID: "o1", Provider: "mock", Amount: 100, Status: payments.StatusPending,
		CreatedAt: now, UpdatedAt: now, NextCheckAt: now}
	existing, err := s.Create(ctx, p)
	require.NoError(t, err)
	require.Nil(t, existing)

	p.Checks, p.NextCheckAt = 1, now.Add(time.Minute) // schedule-only update
	ok, err := s.Update(ctx, p, payments.StatusPending)
	require.NoError(t, err)
	require.True(t, ok)
	evs, err := s.events(ctx, "p1")
	require.NoError(t, err)
	require.Empty(t, evs)

	p.Status = payments.StatusPaid
	ok, err = s.Update(ctx, p, payments.StatusPending)
	require.NoError(t, err)
	require.True(t, ok)
	got, err := s.Get(ctx, "p1")
	require.NoError(t, err)
	require.Equal(t, payments.StatusPaid, got.Status)
	evs, err = s.events(ctx, "p1")
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "pending", evs[0].State)
	require.Contains(t, evs[0].Body, `"status":"paid"`)
	require.Contains(t, evs[0].Body, `"checkout_url":"https://pay.example/pay/p1"`)

	// A lost compare-and-swap writes neither the payment nor an event.
	p.Status = payments.StatusFailed
	ok, err = s.Update(ctx, p, payments.StatusPending)
	require.NoError(t, err)
	require.False(t, ok)
	evs, err = s.events(ctx, "p1")
	require.NoError(t, err)
	require.Len(t, evs, 1)
}
