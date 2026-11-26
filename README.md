# nepal-checkout

[![CI](https://github.com/VoidArchive/nepal-checkout/actions/workflows/ci.yml/badge.svg)](https://github.com/VoidArchive/nepal-checkout/actions/workflows/ci.yml)

A payment server that you host yourself. It gives your application one HTTP API for eSewa and Khalti. Your application can be in any language.

The server uses the [nepal-payment-go](https://github.com/VoidArchive/nepal-payment-go) SDK.

## What the server does

- It keeps a record of each payment in a SQLite database.
- It gives you one `checkout_url` for each payment. The URL is the same shape for all providers.
- It receives the redirect from the provider and makes sure that the payment is correct.
- It finds a payment when the customer pays and then closes the browser.
- It sends a signed webhook to your application when the status of a payment changes.

## Status

This is the first version. Read these limits before you use it.

- eSewa: tested with the eSewa sandbox. A payment was paid through the server, and the webhook arrived with a correct signature.
- Khalti: tested with the Khalti sandbox in the same way.
- For the two providers, a payment was also tested where the customer paid and did not come back to the server. The server found the payment and sent the webhook.
- Not tested with a real provider: a failed payment, a cancelled payment, and a production (live) merchant account.
- The server supports one merchant and one API key.
- Refunds, FonePay and ConnectIPS are not available.

## How it works

1. Your application sends `POST /v1/payments`. It gets a payment with a `checkout_url`.
2. Your application sends the customer to the `checkout_url`.
3. The customer pays on the page of the provider.
4. The provider sends the customer back to the server. The server examines the payment.
5. The server sends a webhook to your application. Your application completes the order.
6. The server sends the customer to your `RETURN_URL`.

The redirect to `RETURN_URL` has `payment_id` and `order_id` in the query. It does not have a status. Use only the webhook or `GET /v1/payments/{id}` to know the status.

## How to start

The quickest procedure uses the mock provider. You do not need a merchant account.

1. Copy the example configuration.

   ```bash
   cp .env.example .env
   ```

2. Set `MOCK=true` in `.env`. Set a value for `API_KEY` that has 32 characters or more.

3. Start the server.

   ```bash
   docker build -t nepal-checkout .
   docker run --env-file .env -p 8080:8080 -v checkout-data:/data nepal-checkout
   ```

   Each release also has an image, `ghcr.io/voidarchive/nepal-checkout`, and programs for Linux (amd64 and arm64) on the releases page.

   Without Docker:

   ```bash
   go build -o nepal-checkout .
   set -a; . ./.env; set +a
   ./nepal-checkout
   ```

4. Create a payment.

   ```bash
   curl -X POST http://localhost:8080/v1/payments \
     -H "Authorization: Bearer $API_KEY" \
     -d '{"order_id":"ORDER-1","provider":"mock","amount":10000}'
   ```

5. Open the `checkout_url` from the reply in a browser. Select "Pay".

## Configuration

The server reads its configuration from environment variables. If a variable is missing or incorrect, the server does not start. It shows all the problems together.

| Variable | Necessary | Description |
|---|---|---|
| `PUBLIC_URL` | Yes | The public address of this server. Do not put a slash at the end. |
| `API_KEY` | Yes | The key that your application sends. Minimum 32 characters. |
| `WEBHOOK_URL` | Yes | The address in your application that receives webhooks. |
| `WEBHOOK_SECRET` | Yes | The key that signs the webhooks. |
| `RETURN_URL` | Yes | The page in your application where the customer goes after the payment. |
| `DB_PATH` | No | The database file. Default: `checkout.db`. |
| `LISTEN_ADDR` | No | Default: `:8080`. |
| `ESEWA_SECRET_KEY`, `ESEWA_PRODUCT_CODE` | No | Set the two variables to enable eSewa. |
| `ESEWA_SANDBOX` | No | `true` uses the eSewa sandbox. |
| `KHALTI_SECRET_KEY` | No | Set this variable to enable Khalti. |
| `KHALTI_SANDBOX` | No | `true` uses the Khalti sandbox. |
| `MOCK` | No | `true` enables the mock provider. |

You must enable a minimum of one provider.

**Warning:** Do not set `MOCK=true` in production. With the mock provider, any person can mark a mock payment as paid.

## API

The file [`openapi.yaml`](openapi.yaml) contains the full description. You can make a client for your language from it.

All amounts are in paisa. NPR 100 is `10000`.

### Create a payment

```
POST /v1/payments
Authorization: Bearer <API_KEY>
```

```json
{ "order_id": "ORDER-1", "provider": "esewa", "amount": 10000 }
```

The values for `provider` are `esewa`, `khalti` and `mock`.

- If you send the same order, provider and amount again, you get the same payment. Thus, it is safe to send the request again after a network error.
- If you send the same order with a different amount, you get the error `409`.
- If the order is paid, you always get the paid payment.

### Get a payment

```
GET /v1/payments/{id}
Authorization: Bearer <API_KEY>
```

The reply contains the payment and a list of the webhooks for that payment. For each webhook, the list shows the number of attempts and the last error.

A payment has one of these statuses: `pending`, `paid`, `failed`, `expired`, `refunded`.

## Webhooks

The server sends a `POST` request to `WEBHOOK_URL` each time the status of a payment changes.

```json
{
  "id": "EVENT_ID",
  "type": "payment.status_changed",
  "created_at": "2026-01-01T10:00:00Z",
  "payment": { "id": "...", "order_id": "ORDER-1", "status": "paid", "amount": 10000 }
}
```

### How to make sure that a webhook is from the server

Each webhook has this header:

```
X-Checkout-Signature: t=1767261600,v1=5f2b...
```

1. Read the body of the request as bytes. Do not parse it first.
2. Calculate HMAC-SHA256 of `<t>.<body>` with `WEBHOOK_SECRET` as the key.
3. Compare the result with `v1`. Use a comparison that takes a constant time.
4. Reject the webhook if `t` is more than 5 minutes old.

### Rules for your webhook handler

- Reply with a `2xx` status when you accept the webhook.
- If the reply is not `2xx`, the server sends the webhook again. The time between attempts increases from 1 minute to 1 hour. The server stops after 24 hours.
- The server can send the same webhook more than one time. Keep the `id` of each event, and ignore an `id` that you know.
- Compare `payment.amount` with the amount of your order before you complete the order.

The directory [`examples/aspnet`](examples/aspnet) contains an ASP.NET Core example.

## Production

- Put a reverse proxy with TLS (for example, Caddy or nginx) in front of the server. The server does not do TLS.
- Set a limit on the request rate in the reverse proxy.
- Keep a backup of the database file.
- The server stores payments for one merchant. Use one server for each merchant.
- If your application is on the host and the server is in Docker, make sure that the container can connect to `WEBHOOK_URL`.
- A customer can start a payment with one provider and then pay with a different provider for the same order. If the customer pays the two payments, the two payments become `paid`. Your application gets two webhooks and must refund one payment.

## Tests

```bash
go test -race ./...
golangci-lint run ./...
```

## License

MIT
