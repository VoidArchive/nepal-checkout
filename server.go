package main

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/voidarchive/nepal-payment-go/v2/core"
	"github.com/voidarchive/nepal-payment-go/v2/payments"
	"github.com/voidarchive/nepal-payment-go/v2/providers/esewa"
	"github.com/voidarchive/nepal-payment-go/v2/providers/khalti"
	"github.com/voidarchive/nepal-payment-go/v2/providers/mock"
)

type app struct {
	cfg    Config
	now    func() time.Time // one clock for the Service, the outbox and the sender
	store  *store
	svc    *payments.Service
	client *http.Client
	nudge  chan struct{}
	mock   http.Handler // nil unless MOCK=true
}

// newApp builds the server from cfg. Tests pass a fake clock.
func newApp(cfg Config, now func() time.Time) (*app, error) {
	provs := map[string]core.Provider{}
	a := &app{
		cfg: cfg,
		now: now,
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		nudge: make(chan struct{}, 1),
	}
	if cfg.EsewaSecretKey != "" {
		p, err := esewa.New(esewa.Config{SecretKey: cfg.EsewaSecretKey, ProductCode: cfg.EsewaProductCode, Sandbox: cfg.EsewaSandbox})
		if err != nil {
			return nil, err
		}
		provs["esewa"] = p
	}
	if cfg.KhaltiSecretKey != "" {
		u, err := url.Parse(cfg.ReturnURL)
		if err != nil {
			return nil, err
		}
		p, err := khalti.New(khalti.Config{SecretKey: cfg.KhaltiSecretKey, WebsiteURL: u.Scheme + "://" + u.Host, Sandbox: cfg.KhaltiSandbox})
		if err != nil {
			return nil, err
		}
		provs["khalti"] = p
	}
	if cfg.Mock {
		p, err := mock.New(mock.Config{BaseURL: cfg.PublicURL + "/mock"})
		if err != nil {
			return nil, err
		}
		provs["mock"] = p
		a.mock = p.Handler()
	}

	st, err := openStore(cfg.DBPath, cfg.PublicURL, now)
	if err != nil {
		return nil, err
	}
	a.store = st
	a.svc = &payments.Service{
		Store:       st,
		Providers:   provs,
		CallbackURL: func(id string) string { return cfg.PublicURL + "/callback/" + id }, // no query: eSewa appends ?data=
		Now:         now,
	}
	return a, nil
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/payments", a.auth(a.createPayment))
	mux.HandleFunc("GET /v1/payments/{id}", a.auth(a.getPayment))
	mux.HandleFunc("GET /pay/{id}", a.pay)
	mux.HandleFunc("GET /callback/{id}", a.callback)
	mux.HandleFunc("GET /healthz", a.healthz)
	if a.mock != nil {
		mux.Handle("/mock/", http.StripPrefix("/mock", a.mock))
	}
	return mux
}

// loop runs the background pass every 15 s, or right away when nudged.
func (a *app) loop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		a.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.nudge:
		}
	}
}

func (a *app) tick(ctx context.Context) {
	if _, err := a.svc.CheckDue(ctx); err != nil {
		slog.Error("check due payments", "err", err)
	}
	a.deliverDue(ctx)
}

// paymentJSON is the payment object of the API and of webhook bodies.
type paymentJSON struct {
	ID             string          `json:"id"`
	OrderID        string          `json:"order_id"`
	Provider       string          `json:"provider"`
	Amount         int64           `json:"amount"`
	Status         payments.Status `json:"status"`
	ProviderStatus string          `json:"provider_status"`
	TransactionID  string          `json:"transaction_id"`
	CheckoutURL    string          `json:"checkout_url"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

func paymentView(p *payments.Payment, publicURL string) paymentJSON {
	return paymentJSON{
		ID: p.ID, OrderID: p.OrderID, Provider: p.Provider, Amount: p.Amount, Status: p.Status,
		ProviderStatus: p.ProviderStatus, TransactionID: p.TransactionID,
		CheckoutURL: publicURL + "/pay/" + p.ID,
		CreatedAt:   p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (a *app) auth(next http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + a.cfg.APIKey)
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or wrong API key")
			return
		}
		next(w, r)
	}
}

func (a *app) createPayment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderID       string `json:"order_id"`
		Provider      string `json:"provider"`
		Amount        int64  `json:"amount"`
		Description   string `json:"description"`
		CustomerEmail string `json:"customer_email"`
		CustomerPhone string `json:"customer_phone"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	p, err := a.svc.Create(r.Context(), payments.CreateParams{
		OrderID: in.OrderID, Provider: in.Provider, Amount: in.Amount,
		Description:   cmp.Or(in.Description, in.OrderID), // Khalti rejects a blank order name
		CustomerEmail: in.CustomerEmail, CustomerPhone: in.CustomerPhone,
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, paymentView(p, a.cfg.PublicURL))
	case errors.Is(err, core.ErrInvalidRequest), errors.Is(err, payments.ErrUnknownProvider):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, payments.ErrAmountConflict):
		writeError(w, http.StatusConflict, "amount_conflict", "this order already has a payment with a different amount")
	case errors.Is(err, errDB):
		slog.Error("create payment", "order", in.OrderID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	default:
		slog.Error("create payment: gateway", "order", in.OrderID, "provider", in.Provider, "err", err)
		writeError(w, http.StatusBadGateway, "gateway_error", "the payment gateway did not accept the payment")
	}
}

func (a *app) getPayment(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, payments.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "payment not found")
		return
	}
	var evs []event
	if err == nil {
		evs, err = a.store.events(r.Context(), p.ID)
	}
	if err != nil {
		slog.Error("get payment", "id", r.PathValue("id"), "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		paymentJSON
		Webhooks []event `json:"webhooks"`
	}{paymentView(p, a.cfg.PublicURL), evs})
}

// returnURL is RETURN_URL plus the payment and order IDs. It carries no status
// on purpose: the app must trust only the webhook or the GET.
func (a *app) returnURL(p *payments.Payment) string {
	u, _ := url.Parse(a.cfg.ReturnURL) // validated at start
	q := u.Query()
	q.Set("payment_id", p.ID)
	q.Set("order_id", p.OrderID)
	u.RawQuery = q.Encode()
	return u.String()
}

var formPage = template.Must(template.New("pay").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Redirecting to payment</title></head>
<body onload="document.forms[0].submit()" style="font-family:sans-serif;max-width:32rem;margin:2rem auto;padding:0 1rem">
<form method="post" action="{{.TargetURL}}">
{{range $k, $v := .FormFields}}<input type="hidden" name="{{$k}}" value="{{$v}}">
{{end}}<p>Taking you to the payment page.</p>
<button type="submit">Continue to payment</button>
</form>
</body></html>`))

func (a *app) pay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, err := a.svc.Store.Get(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, payments.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		slog.Error("pay page", "id", r.PathValue("id"), "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	case p.Status != payments.StatusPending:
		http.Redirect(w, r, a.returnURL(p), http.StatusSeeOther)
	case p.Checkout.IsFormPOST:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := formPage.Execute(w, p.Checkout); err != nil {
			slog.Error("render pay page", "id", p.ID, "err", err)
		}
	default:
		http.Redirect(w, r, p.Checkout.TargetURL, http.StatusSeeOther)
	}
}

// callback is where the gateway sends the customer back. A verification error
// is only logged: the payment stays pending and the background check settles it.
func (a *app) callback(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, payments.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("callback", "id", r.PathValue("id"), "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	params := map[string]string{}
	for k, v := range r.URL.Query() {
		params[k] = v[0]
	}
	_, changed, err := a.svc.HandleCallback(r.Context(), p.ID, params)
	if err != nil {
		slog.Warn("callback verification failed", "id", p.ID, "provider", p.Provider, "err", err)
	}
	if changed {
		select {
		case a.nudge <- struct{}{}:
		default:
		}
	}
	http.Redirect(w, r, a.returnURL(p), http.StatusSeeOther)
}

func (a *app) healthz(w http.ResponseWriter, r *http.Request) {
	if err := a.store.db.QueryRowContext(r.Context(), "SELECT 1").Scan(new(int)); err != nil {
		slog.Error("healthz", "err", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}
