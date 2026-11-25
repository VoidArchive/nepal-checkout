using System.Collections.Concurrent;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Security.Cryptography;
using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;

var builder = WebApplication.CreateBuilder(args);
// From environment variables or appsettings.json.
var baseUrl = Required("CHECKOUT_URL").TrimEnd('/');
var apiKey = Required("CHECKOUT_API_KEY");
var webhookSecret = Required("WEBHOOK_SECRET");

builder.Services.AddHttpClient("checkout", c =>
{
    c.BaseAddress = new Uri(baseUrl + "/");
    c.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", apiKey);
});
var app = builder.Build();
var json = new JsonSerializerOptions { PropertyNamingPolicy = JsonNamingPolicy.SnakeCaseLower };

// Amounts come from your own orders, never from the browser. Paisa: Rs 100 = 10000.
var orders = new Dictionary<string, long> { ["order-1042"] = 10000 };
// shortcut: in-memory state is lost on restart and not shared between instances; a real app uses its database.
var paidOrders = new ConcurrentDictionary<string, bool>();
var seenEvents = new ConcurrentDictionary<string, bool>();

// 1. Create the payment, then send the customer to the checkout page.
app.MapPost("/checkout", async (HttpRequest req, IHttpClientFactory http) =>
{
    var form = await req.ReadFormAsync();
    string orderId = form["order_id"].ToString();
    if (!orders.TryGetValue(orderId, out var amount)) return Results.NotFound();

    var res = await http.CreateClient("checkout").PostAsJsonAsync("v1/payments",
        new { order_id = orderId, provider = form["provider"].FirstOrDefault() ?? "mock", amount }, json);
    if (!res.IsSuccessStatusCode)
        return Results.Problem($"payment server returned {(int)res.StatusCode}: {await res.Content.ReadAsStringAsync()}", statusCode: 502);
    var payment = (await res.Content.ReadFromJsonAsync<Payment>(json))!;
    return Results.Redirect(payment.CheckoutUrl);
});

// 2. The webhook: the only push notice you can trust, once its signature checks out.
app.MapPost("/webhooks/payments", async (HttpRequest req) =>
{
    using var buf = new MemoryStream();
    await req.Body.CopyToAsync(buf); // the signature covers the raw bytes, so read them before any JSON parsing
    var body = buf.ToArray();
    if (!Signature.Verify(webhookSecret, req.Headers["X-Checkout-Signature"].ToString(), body, DateTimeOffset.UtcNow))
        return Results.Unauthorized();

    var ev = JsonSerializer.Deserialize<WebhookEvent>(body, json)!;
    if (!seenEvents.TryAdd(ev.Id, true)) return Results.Ok(); // a delivery can repeat: handle each event id once
    // Complete the order only when the paid amount is the amount of your own order.
    if (ev.Payment.Status == "paid" && orders.TryGetValue(ev.Payment.OrderId, out var amount) && amount == ev.Payment.Amount)
        paidOrders[ev.Payment.OrderId] = true;
    return Results.Ok(); // answer fast; slow work (emails, fulfilment) belongs in a queue
});

// 3. The return page. The query string only says which payment; the status comes from the server.
app.MapGet("/orders/done", async (string? payment_id, IHttpClientFactory http) =>
{
    if (string.IsNullOrEmpty(payment_id)) return Results.BadRequest();
    var res = await http.CreateClient("checkout").GetAsync("v1/payments/" + Uri.EscapeDataString(payment_id));
    if (!res.IsSuccessStatusCode) return Results.StatusCode((int)res.StatusCode == 404 ? 404 : 502);
    var p = (await res.Content.ReadFromJsonAsync<Payment>(json))!;
    var h = HtmlEncoder.Default;
    var text = p.Status switch
    {
        "paid" => "Payment received. Thank you!",
        "pending" => "We are still waiting for the gateway to confirm. Refresh in a minute.",
        _ => $"The payment did not go through ({p.Status}).",
    };
    return Results.Content($"<!doctype html><title>Order {h.Encode(p.OrderId)}</title><p>Order {h.Encode(p.OrderId)}: {h.Encode(text)}</p>", "text/html; charset=utf-8");
});

app.Run();

string Required(string name) =>
    builder.Configuration[name] is { Length: > 0 } v ? v : throw new InvalidOperationException($"{name} is required");

record Payment(string Id, string OrderId, string Provider, long Amount, string Status, string CheckoutUrl);
record WebhookEvent(string Id, string Type, Payment Payment);

static class Signature
{
    static readonly TimeSpan Tolerance = TimeSpan.FromMinutes(5);

    // header is "t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">".
    public static bool Verify(string secret, string header, byte[] body, DateTimeOffset now)
    {
        string? t = null, v1 = null;
        foreach (var part in header.Split(','))
        {
            var kv = part.Split('=', 2);
            if (kv.Length != 2) continue;
            if (kv[0] == "t") t = kv[1];
            if (kv[0] == "v1") v1 = kv[1];
        }
        if (t is null || v1 is null || !long.TryParse(t, out var ts)) return false;
        // Reject old (replayed) deliveries, and ones from a clock far in the future.
        if ((now - DateTimeOffset.FromUnixTimeSeconds(ts)).Duration() > Tolerance) return false;

        byte[] got;
        try { got = Convert.FromHexString(v1); } catch (FormatException) { return false; }
        var want = HMACSHA256.HashData(Encoding.UTF8.GetBytes(secret), Encoding.UTF8.GetBytes(t + ".").Concat(body).ToArray());
        return CryptographicOperations.FixedTimeEquals(got, want);
    }
}
