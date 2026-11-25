# ASP.NET Core example

A minimal-API app that shows the three things a .NET shop does with nepal-checkout:

- `POST /checkout` (form field `order_id`, optional `provider`): creates the payment with `POST /v1/payments` and redirects the customer to `checkout_url`.
- `POST /webhooks/payments`: verifies `X-Checkout-Signature` over the raw body, rejects timestamps more than 5 minutes off, handles each event `id` once, and marks the order paid when `payment.status` is `paid`.
- `GET /orders/done`: the return page. It reads the status from `GET /v1/payments/{payment_id}`, never from the query string.

Orders and processed event IDs live in memory here. A real app keeps both in its database.

## Run

Start the server with `MOCK=true`, `RETURN_URL=http://localhost:5099/orders/done` and `WEBHOOK_URL=http://localhost:5099/webhooks/payments`, then:

```sh
export CHECKOUT_URL=http://localhost:8080
export CHECKOUT_API_KEY=...        # the server's API_KEY
export WEBHOOK_SECRET=...          # the server's WEBHOOK_SECRET
dotnet run --urls http://localhost:5099
curl -i -d order_id=order-1042 http://localhost:5099/checkout
```

The same three settings can go in `appsettings.json` instead of the environment.
