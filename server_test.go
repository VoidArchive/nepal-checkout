package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/voidarchive/nepal-payment-go/v2/core"
)

const testKey = "test-api-key-0123456789abcdef0123456789"

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type hook struct {
	body []byte
	sig  string
}

type env struct {
	t     *testing.T
	a     *app
	url   string
	clock *fakeClock

	mu       sync.Mutex
	hooks    []hook
	statuses []int // replies of the webhook receiver, in order; 200 once used up
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, clock: &fakeClock{t: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)}}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.hooks = append(e.hooks, hook{body, r.Header.Get("X-Checkout-Signature")})
		status := 200
		if len(e.statuses) > 0 {
			status, e.statuses = e.statuses[0], e.statuses[1:]
		}
		e.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(receiver.Close)

	// Listen first: PUBLIC_URL must be known before the app (and its mock gateway) is built.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	e.url = "http://" + ln.Addr().String()
	a, err := newApp(Config{
		PublicURL:     e.url,
		APIKey:        testKey,
		WebhookURL:    receiver.URL,
		WebhookSecret: "whsec",
		ReturnURL:     "https://shop.example/done?lang=en",
		DBPath:        filepath.Join(t.TempDir(), "test.db"),
		Mock:          true,
	}, e.clock.Now)
	require.NoError(t, err)
	e.a = a
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: a.routes()}}
	srv.Start()
	t.Cleanup(func() { srv.Close(); _ = a.store.Close() })
	return e
}

func (e *env) setStatuses(s ...int) { e.mu.Lock(); e.statuses = s; e.mu.Unlock() }

func (e *env) hookList() []hook {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]hook(nil), e.hooks...)
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// do sends a request without following redirects. key "" sends no Authorization.
func (e *env) do(method, rawURL, key, body string) *http.Response {
	e.t.Helper()
	if strings.HasPrefix(rawURL, "/") {
		rawURL = e.url + rawURL
	}
	req, err := http.NewRequest(method, rawURL, strings.NewReader(body))
	require.NoError(e.t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if method == http.MethodPost && strings.HasPrefix(body, "id=") {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := noRedirect.Do(req)
	require.NoError(e.t, err)
	e.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

type apiError struct {
	Error struct{ Code, Message string } `json:"error"`
}

type paymentWithHooks struct {
	paymentJSON
	Webhooks []event `json:"webhooks"`
}

func (e *env) create(orderID string, amount int64, provider string) paymentJSON {
	e.t.Helper()
	resp := e.do("POST", "/v1/payments", testKey,
		`{"order_id":"`+orderID+`","provider":"`+provider+`","amount":`+strconv.FormatInt(amount, 10)+`}`)
	require.Equal(e.t, 200, resp.StatusCode)
	return decode[paymentJSON](e.t, resp)
}

func (e *env) get(id string) paymentWithHooks {
	e.t.Helper()
	resp := e.do("GET", "/v1/payments/"+id, testKey, "")
	require.Equal(e.t, 200, resp.StatusCode)
	return decode[paymentWithHooks](e.t, resp)
}

// payMock opens the checkout URL, presses a button on the mock gateway's page
// and returns that page's response.
func (e *env) payMock(p paymentJSON, action string) *http.Response {
	e.t.Helper()
	resp := e.do("GET", p.CheckoutURL, "", "")
	require.Equal(e.t, http.StatusSeeOther, resp.StatusCode)
	gw := resp.Header.Get("Location")
	require.True(e.t, strings.HasPrefix(gw, e.url+"/mock/pay?id="), gw)
	page := e.do("GET", gw, "", "")
	require.Equal(e.t, 200, page.StatusCode)
	html, _ := io.ReadAll(page.Body)
	require.Contains(e.t, string(html), `value="`+action+`"`)
	return e.do("POST", e.url+"/mock/pay", "", "id="+p.ID+"&action="+action)
}

func verify(t *testing.T, h hook) {
	t.Helper()
	var ts int64
	var v1 string
	for part := range strings.SplitSeq(h.sig, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			v1 = v
		}
	}
	require.Equal(t, h.sig, sign("whsec", ts, h.body), "signature")
	require.NotEmpty(t, v1)
}

func TestFullFlow(t *testing.T) {
	e := newEnv(t)
	p := e.create("order-1", 12345, "mock")
	assert.Equal(t, "pending", string(p.Status))
	assert.Equal(t, e.url+"/pay/"+p.ID, p.CheckoutURL)
	assert.Equal(t, "no-store", e.do("GET", p.CheckoutURL, "", "").Header.Get("Cache-Control"))

	cb := e.payMock(p, "pay")
	require.Equal(t, http.StatusSeeOther, cb.StatusCode)
	cbURL := cb.Header.Get("Location")
	require.Equal(t, e.url+"/callback/"+p.ID+"?id="+p.ID, cbURL)

	back := e.do("GET", cbURL, "", "")
	require.Equal(t, http.StatusSeeOther, back.StatusCode)
	ret, err := url.Parse(back.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "shop.example", ret.Host)
	assert.Equal(t, url.Values{"lang": {"en"}, "payment_id": {p.ID}, "order_id": {"order-1"}}, ret.Query())
	assert.Len(t, e.a.nudge, 1, "callback nudges the loop")

	got := e.get(p.ID)
	assert.Equal(t, "paid", string(got.Status))
	assert.Equal(t, "MOCK-"+p.ID, got.TransactionID)
	require.Len(t, got.Webhooks, 1)
	assert.Equal(t, "pending", got.Webhooks[0].State)

	e.a.tick(context.Background())
	hooks := e.hookList()
	require.Len(t, hooks, 1)
	verify(t, hooks[0])
	var body webhookBody
	require.NoError(t, json.Unmarshal(hooks[0].body, &body))
	assert.Equal(t, "payment.status_changed", body.Type)
	assert.Equal(t, got.Webhooks[0].ID, body.ID)
	assert.Equal(t, "paid", string(body.Payment.Status))
	assert.Equal(t, int64(12345), body.Payment.Amount)
	assert.NotContains(t, string(hooks[0].body), "provider_ref")

	got = e.get(p.ID)
	assert.Equal(t, "delivered", got.Webhooks[0].State)
	assert.Equal(t, 1, got.Webhooks[0].Attempts)
	assert.Equal(t, 200, got.Webhooks[0].LastStatus)
	require.NotNil(t, got.Webhooks[0].DeliveredAt)

	// A second callback for a paid payment changes nothing and sends nothing.
	assert.Equal(t, http.StatusSeeOther, e.do("GET", cbURL, "", "").StatusCode)
	e.a.tick(context.Background())
	assert.Len(t, e.hookList(), 1)
	assert.Len(t, e.get(p.ID).Webhooks, 1)

	// The checkout URL of a finished payment goes back to the shop.
	pay := e.do("GET", p.CheckoutURL, "", "")
	assert.Equal(t, http.StatusSeeOther, pay.StatusCode)
	assert.Equal(t, back.Header.Get("Location"), pay.Header.Get("Location"))
}

func TestAbandonedPaymentSettledInBackground(t *testing.T) {
	e := newEnv(t)
	p := e.create("order-2", 500, "mock")
	resp := e.payMock(p, "abandon")
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "pending", string(e.get(p.ID).Status))

	e.a.tick(context.Background()) // not due yet
	assert.Equal(t, "pending", string(e.get(p.ID).Status))
	assert.Empty(t, e.hookList())

	e.clock.Advance(time.Minute)
	e.a.tick(context.Background())
	assert.Equal(t, "paid", string(e.get(p.ID).Status))
	require.Len(t, e.hookList(), 1)
	verify(t, e.hookList()[0])

	e.clock.Advance(time.Hour)
	e.a.tick(context.Background())
	assert.Len(t, e.hookList(), 1)
}

func TestFailedVerificationStillRedirects(t *testing.T) {
	e := newEnv(t)
	p := e.create("order-3", 500, "mock")
	resp := e.do("GET", "/callback/"+p.ID+"?id=someone-else", "", "")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Location"), "payment_id="+p.ID)
	assert.Equal(t, "pending", string(e.get(p.ID).Status))
	assert.Empty(t, e.get(p.ID).Webhooks)

	assert.Equal(t, 404, e.do("GET", "/callback/nope", "", "").StatusCode)
	assert.Equal(t, 404, e.do("GET", "/pay/nope", "", "").StatusCode)
}

func TestWebhookRetryAndGiveUp(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.setStatuses(500)
	p := e.create("order-4", 500, "mock")
	e.payMock(p, "pay")
	e.do("GET", "/callback/"+p.ID+"?id="+p.ID, "", "")

	e.a.tick(ctx)
	w := e.get(p.ID).Webhooks[0]
	assert.Equal(t, "pending", w.State)
	assert.Equal(t, 1, w.Attempts)
	assert.Equal(t, 500, w.LastStatus)
	assert.Equal(t, "500 Internal Server Error", w.LastError)
	assert.Equal(t, e.clock.Now().Add(time.Minute), w.NextAttemptAt)

	e.clock.Advance(59 * time.Second)
	e.a.tick(ctx)
	assert.Len(t, e.hookList(), 1, "not retried before its time")

	e.clock.Advance(time.Second)
	e.a.tick(ctx)
	require.Len(t, e.hookList(), 2)
	w = e.get(p.ID).Webhooks[0]
	assert.Equal(t, "delivered", w.State)
	assert.Equal(t, 2, w.Attempts)
	assert.Equal(t, 200, w.LastStatus)
	assert.Empty(t, w.LastError)
	assert.Equal(t, e.hookList()[0].body, e.hookList()[1].body, "a retry sends the same snapshot")

	// A receiver that never answers 2xx: retries back off, then the event fails after 24 h.
	p2 := e.create("order-5", 500, "mock")
	e.payMock(p2, "pay")
	statuses := make([]int, 1000)
	for i := range statuses {
		statuses[i] = 503
	}
	e.setStatuses(statuses...)
	e.do("GET", "/callback/"+p2.ID+"?id="+p2.ID, "", "")
	for range 25 * 60 {
		e.a.tick(ctx)
		e.clock.Advance(time.Minute)
	}
	w = e.get(p2.ID).Webhooks[0]
	assert.Equal(t, "failed", w.State)
	assert.Equal(t, 503, w.LastStatus)
	// 1, 2, 4 ... 32 minutes apart (7 attempts by minute 63), then hourly until minute 1440.
	assert.Equal(t, 29, w.Attempts)
	assert.Nil(t, w.DeliveredAt)
}

func TestEventsOfOnePaymentGoInOrder(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	p := e.create("order-6", 500, "mock")
	e.payMock(p, "pay")
	e.do("GET", "/callback/"+p.ID+"?id="+p.ID, "", "")

	// Queue a second status change (paid -> refunded) directly while the first is failing.
	e.setStatuses(500)
	e.a.tick(ctx)
	stored, err := e.a.store.Get(ctx, p.ID)
	require.NoError(t, err)
	stored.Status = "refunded"
	ok, err := e.a.store.Update(ctx, stored, "paid")
	require.NoError(t, err)
	require.True(t, ok)

	e.a.tick(ctx) // the first event is not due, so the second must wait too
	assert.Len(t, e.hookList(), 1)
	e.clock.Advance(time.Minute)
	e.a.tick(ctx) // delivers the first event
	assert.Len(t, e.hookList(), 2)
	e.a.tick(ctx) // the second becomes eligible on the next pass
	hooks := e.hookList()
	require.Len(t, hooks, 3)
	assert.Contains(t, string(hooks[1].body), `"status":"paid"`)
	assert.Contains(t, string(hooks[2].body), `"status":"refunded"`)
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t)
	for name, key := range map[string]string{"missing": "", "wrong": testKey + "x"} {
		resp := e.do("POST", "/v1/payments", key, `{"order_id":"o","provider":"mock","amount":1}`)
		assert.Equal(t, 401, resp.StatusCode, name)
		assert.Equal(t, "unauthorized", decode[apiError](t, resp).Error.Code)
		assert.Equal(t, 401, e.do("GET", "/v1/payments/x", key, "").StatusCode, name)
	}

	for name, body := range map[string]string{
		"no order":         `{"provider":"mock","amount":100}`,
		"no provider":      `{"order_id":"o","amount":100}`,
		"unknown provider": `{"order_id":"o","provider":"paypal","amount":100}`,
		"zero amount":      `{"order_id":"o","provider":"mock","amount":0}`,
		"negative amount":  `{"order_id":"o","provider":"mock","amount":-5}`,
		"fraction amount":  `{"order_id":"o","provider":"mock","amount":1.5}`,
		"unknown field":    `{"order_id":"o","provider":"mock","amount":100,"currency":"NPR"}`,
		"not json":         `order_id=o`,
		"too large":        `{"order_id":"` + strings.Repeat("x", 1<<20) + `","provider":"mock","amount":1}`,
	} {
		resp := e.do("POST", "/v1/payments", testKey, body)
		assert.Equal(t, 400, resp.StatusCode, name)
		assert.Equal(t, "invalid_request", decode[apiError](t, resp).Error.Code, name)
	}

	first := e.create("order-7", 1000, "mock")
	assert.Equal(t, first.ID, e.create("order-7", 1000, "mock").ID, "same request twice gives the same payment")
	resp := e.do("POST", "/v1/payments", testKey, `{"order_id":"order-7","provider":"mock","amount":2000}`)
	assert.Equal(t, 409, resp.StatusCode)
	assert.Equal(t, "amount_conflict", decode[apiError](t, resp).Error.Code)

	resp = e.do("GET", "/v1/payments/nope", testKey, "")
	assert.Equal(t, 404, resp.StatusCode)
	assert.Equal(t, "not_found", decode[apiError](t, resp).Error.Code)

	ok := e.do("GET", "/healthz", "", "")
	assert.Equal(t, 200, ok.StatusCode)
}

// stub is a form-POST gateway like eSewa.
type stub struct{ fail bool }

func (s stub) InitiatePayment(_ context.Context, req *core.PaymentRequest) (*core.PaymentResponse, error) {
	if s.fail {
		return nil, errors.New("gateway down: secret detail")
	}
	return &core.PaymentResponse{IsFormPOST: true, TargetURL: "https://gw.example/form", TransactionID: req.OrderID,
		FormFields: map[string]string{"amount": "10", "desc": req.Description, "evil": `"><script>alert(1)</script>`}}, nil
}

func (stub) VerifyPayment(context.Context, *core.VerifyRequest) (*core.VerifyResponse, error) {
	return nil, core.ErrSignatureMismatch
}

func TestFormPostPage(t *testing.T) {
	e := newEnv(t)
	e.a.svc.Providers["stub"] = stub{}
	e.a.svc.Providers["down"] = stub{fail: true}
	p := e.create("order-8", 1000, "stub")

	resp := e.do("GET", p.CheckoutURL, "", "")
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	html, _ := io.ReadAll(resp.Body)
	page := string(html)
	assert.Contains(t, page, `<form method="post" action="https://gw.example/form">`)
	assert.Contains(t, page, `<input type="hidden" name="amount" value="10">`)
	assert.Contains(t, page, `<input type="hidden" name="desc" value="order-8">`) // no description: the order ID is sent
	assert.Contains(t, page, `value="&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;"`)
	assert.NotContains(t, page, "<script>alert")
	assert.Contains(t, page, `<button type="submit">`)

	resp = e.do("POST", "/v1/payments", testKey, `{"order_id":"order-9","provider":"down","amount":1}`)
	assert.Equal(t, 502, resp.StatusCode)
	msg := decode[apiError](t, resp)
	assert.Equal(t, "gateway_error", msg.Error.Code)
	assert.NotContains(t, msg.Error.Message, "secret detail")
}

func TestRepeatCreateDoesNotCallGateway(t *testing.T) {
	e := newEnv(t)
	e.a.svc.Providers["stub"] = stub{}
	first := e.create("order-10", 1000, "stub")

	e.a.svc.Providers["stub"] = stub{fail: true} // the gateway is down for the retry
	assert.Equal(t, first.ID, e.create("order-10", 1000, "stub").ID)
}
