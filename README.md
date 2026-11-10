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
| `WithDroppedCookies(names...)` | none | Takes the named cookies out of what is forwarded: your own session, CSRF and remember-me cookies ([below](#your-own-cookies-and-headers)). `New` refuses an empty or invalid name, and `_vid`, Alice's agent cookie. |
| `WithDroppedHeaders(names...)` | none | Takes the named headers out of what is forwarded, in any case: identity headers your own proxy adds. `New` refuses an empty or invalid name, and `Authorization`, `Cookie` and `Host`. |
| `WithTrustedProxies(prefixes...)` | none | Takes the client IP from `X-Forwarded-For` behind your own proxies. `New` refuses an invalid prefix, and an IPv4-mapped one (give `10.0.0.0/8`, not `::ffff:10.0.0.0/104`). |
| `WithTransport(rt)` | `http.DefaultTransport` | Sends the trap requests through your own `http.RoundTripper`. |

```go
trap, err := aliceclient.New(aliceURL, apiKey,
	aliceclient.WithTimeout(time.Second),
	aliceclient.WithErrorHandler(func(r *http.Request, err error) {
		logger.Warn("alice bait answered 404", "path", r.URL.Path, "error", err)
	}),
	aliceclient.WithTrustedProxies(netip.MustParsePrefix("10.0.0.0/8")),
	aliceclient.WithDroppedCookies("session", "csrf_token", "remember_me"),
	aliceclient.WithDroppedHeaders("X-Auth-Request-Email"),
)
```

### Your own cookies and headers

Everything the agent sends is forwarded to Alice and stored on its trail,
cookies included. The bait paths are on your own domain, so a request to
them also carries your site's own cookies: a logged-in member of staff who
opens `/internal/admin` would hand Alice a live session. List them, and the
client takes them out before anything is forwarded:

- **Cookies** (`WithDroppedCookies`): your session cookie, your CSRF
  cookie, your remember-me cookie, and any other cookie your site sets that
  holds a credential. Names match exactly and case-sensitively, so a
  `__Host-` or `__Secure-` prefix is part of the name (`__Host-session`);
  there are no wildcards. Every `Cookie` line is read, and each pair named
  as listed is removed. Every other pair is kept as the agent sent it,
  malformed ones included, so the cookies an agent plants are still
  captured. A line left with nothing is removed, and so is the header.
- **Headers** (`WithDroppedHeaders`): any identity header a proxy in front
  of your site adds, such as an auth proxy's `X-Auth-Request-Email` or
  `X-Auth-Request-User`, or an internal session header. `Authorization`,
  `Cookie` and `Host` can't be dropped: the maze's login needs
  `Authorization`, `Cookie` carries Alice's agent cookie (`_vid`, exported
  as `AgentCookieName`; list your own cookies instead), and the trap needs
  `Host`.

Either option can be given more than once; the lists add up. The agent's
request itself is never changed, so the error handler sees it as received.

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
  (the `Host` header included; hop-by-hop headers, and the cookies and
  headers you drop, left out), the body,
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
