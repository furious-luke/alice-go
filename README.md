# Alice client

The reverse-proxy middleware a site mounts on its bait paths. Each request
the handler gets is forwarded to Alice's trap API on `alice-srv`, and the
response Alice decides is relayed back, so a hacking agent only ever sees
the site's own domain. It is its own module and uses the standard library
only, so adding it brings in no other dependencies.

Design: the trap protocol and what the client does with it are in
[../designs/trap.md](../designs/trap.md).

## Mounting it

```go
import aliceclient "github.com/furious-luke/go-projects/alice/client"

trap, err := aliceclient.New("https://alice.example", os.Getenv("ALICE_API_KEY"))
if err != nil {
	log.Fatal(err)
}

mux := http.NewServeMux()
mux.Handle("/.envrc", trap)
mux.Handle("/internal/admin", trap)
mux.Handle("/internal/admin/", trap)
mux.HandleFunc("GET /api/products", products) // the site's real routes
```

The API key is a customer key with the `trap` perm (`alice key make` prints
one). `New` refuses a URL that isn't `http` or `https` with a host, and an
empty key.

## Options

| Option | Default | What it does |
| --- | --- | --- |
| `WithTimeout(d)` | `DefaultTimeout`, 2 s | The deadline of each trap exchange. `New` refuses one that isn't positive. |
| `WithErrorHandler(fn)` | none | Calls `fn(r, err)` once for each fail-safe 404. |
| `WithTrustedProxies(prefixes...)` | none | Takes the client IP from `X-Forwarded-For` behind your own proxies. `New` refuses an invalid prefix, and an IPv4-mapped one (give `10.0.0.0/8`, not `::ffff:10.0.0.0/104`). |
| `WithTransport(rt)` | `http.DefaultTransport` | Sends the trap requests through your own `http.RoundTripper`. |

```go
trap, err := aliceclient.New(aliceURL, apiKey,
	aliceclient.WithTimeout(time.Second),
	aliceclient.WithErrorHandler(func(r *http.Request, err error) {
		logger.Warn("alice bait answered 404", "path", r.URL.Path, "error", err)
	}),
	aliceclient.WithTrustedProxies(netip.MustParsePrefix("10.0.0.0/8")),
)
```

### The timeout

One deadline covers the whole trap exchange: connecting to Alice, sending
the request and its body, Alice's response head, and relaying the response
body. When it passes before the response head arrives, the agent gets the
plain 404; when it passes during the relay, the relay stops. **A slow, large
upload counts against it**: an agent sending megabytes over a slow link can
run out of time and get the 404. Raise the timeout if your bait paths take
uploads from slow clients. A paused or overloaded Alice still accepts
connections, so the deadline is what keeps your handler from hanging.

### The error handler

The client logs nothing. The handler you give it is called on the request's
goroutine, before the 404 is written, with the agent's request and an
`*aliceclient.Error`. Tell the reasons apart with `errors.Is`:

| Error | When |
| --- | --- |
| `ErrRequest` | The trap request couldn't be made from the agent's request: its body couldn't be read, or the agent went away. |
| `ErrUnreachable` | Alice couldn't be reached: the connection was refused, the host wasn't found, or the exchange failed before a response head. |
| `ErrTimeout` | The deadline passed before Alice's response head. |
| `ErrNoMarker` | Alice answered without the trap marker (`Error.Status` is its status): a refused key, a malformed request, a failure of Alice's own. |
| `ErrPanic` | The client panicked before it had written anything. |

The error never contains the API key, the trap request's headers or the
agent's headers, so it is safe to log. The request is the agent's: log its
method and path, not its headers, which hold the agent's credentials and
cookies. A panic in your handler is recovered.

### Trusted proxies

Without `WithTrustedProxies`, the client IP is the connection's
`RemoteAddr`, and `X-Forwarded-For` is forwarded to Alice as an ordinary
header and never used for the IP. Behind your own proxies or load balancer,
list their addresses: when `RemoteAddr` is inside a listed prefix, the
client walks `X-Forwarded-For` from right to left, skipping listed
addresses, and takes the first one that isn't listed. When every entry is
listed, it takes the leftmost. A missing header, or an entry the walk
reaches that isn't an IP address (one with a port, say), leaves the client
IP as `RemoteAddr`'s. When `RemoteAddr` isn't listed, the header is ignored.

Trusting every peer is an explicit opt-in, and lets any agent choose the IP
Alice records for it:

```go
aliceclient.WithTrustedProxies(netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0"))
```

## What it forwards and relays

- **Forwarded:** the method, the path and query as received, the headers
  (the `Host` header included, hop-by-hop headers left out), the body,
  streamed, and the client IP, taken from the connection's `RemoteAddr`
  (or from `X-Forwarded-For` behind trusted proxies, above). Mount the
  handler where `RemoteAddr` is the agent's IP (with or without a port) or
  a listed proxy's: Alice refuses a request without an IP, such as one from
  a unix-socket listener, and the handler answers it with a 404.
- **Relayed:** the status (4xx and 5xx included), headers and body of
  Alice's response, without the trap marker (`Alice-Trap`) or hop-by-hop
  headers. Redirects are relayed, not followed.
- **Failing safe:** when Alice can't be reached in time, or its response
  has no trap marker, or the client panics before writing anything, the
  handler answers with a plain 404 (`404 page not found`), as if the path
  weren't there. No panic leaves the handler.

## Testing

```sh
go test ./...
../../scripts/dev-checks.sh check alice/client   # vet, test and gofmt, as CI runs them
```

The tests run against a fake trap server; they need no network or Docker.
