package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const webhookGiveUp = 24 * time.Hour

type webhookBody struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	CreatedAt time.Time   `json:"created_at"`
	Payment   paymentJSON `json:"payment"`
}

// sign returns the X-Checkout-Signature value: HMAC-SHA256 of "<t>.<body>".
func sign(secret string, t int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.", t)
	_, _ = mac.Write(body)
	return "t=" + strconv.FormatInt(t, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// deliverDue sends every due webhook event once. Only the background loop calls it,
// so an event is never sent twice at the same time.
func (a *app) deliverDue(ctx context.Context) {
	evs, err := a.store.dueEvents(ctx, a.now(), 100)
	if err != nil {
		slog.Error("list due webhooks", "err", err)
		return
	}
	for _, e := range evs {
		now := a.now()
		if now.Sub(e.CreatedAt) >= webhookGiveUp {
			e.State = "failed"
		} else {
			e.Attempts++
			e.LastStatus, e.LastError = a.post(ctx, now, e.Body)
			if e.LastStatus >= 200 && e.LastStatus < 300 {
				e.State, e.DeliveredAt = "delivered", &now
			} else {
				e.NextAttemptAt = now.Add(min(time.Minute<<min(e.Attempts-1, 6), time.Hour))
				slog.Warn("webhook delivery failed", "event", e.ID, "payment", e.PaymentID, "attempt", e.Attempts, "status", e.LastStatus, "err", e.LastError)
			}
		}
		if err := a.store.saveEvent(ctx, e); err != nil {
			slog.Error("save webhook event", "event", e.ID, "err", err)
		}
	}
}

func (a *app) post(ctx context.Context, now time.Time, body string) (status int, errText string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.WebhookURL, bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Checkout-Signature", sign(a.cfg.WebhookSecret, now.Unix(), []byte(body)))
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, resp.Status
	}
	return resp.StatusCode, ""
}
