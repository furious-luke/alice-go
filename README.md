# Alice client

The reverse-proxy middleware a site mounts on its bait paths. Each request
the handler gets is forwarded to Alice's trap API on `alice-srv`, and the
response Alice decides is relayed back, so a hacking agent only ever sees
the site's own domain. It is its own module and uses the standard library
only, so adding it brings in no other dependencies.

```sh
go get github.com/furious-luke/alice-go
```

```go
import aliceclient "github.com/furious-luke/alice-go"
```

The package is named `client`; the samples here import it as `aliceclient`.

The module is developed in Alice's monorepo, at `alice/client`, and released
from it to [github.com/furious-luke/alice-go](https://github.com/furious-luke/alice-go)
(see "Releasing the client" in Alice's README). Make changes in the
monorepo, not in the public repo: a release pushes `alice/client`'s own
history, split out of the monorepo, so a commit made only in the public
repo would be overwritten or block the next release. The trap protocol and
what the client does with it are in Alice's `designs/trap.md`
([../designs/trap.md](../designs/trap.md) from `alice/client`). In the
monorepo, `../../scripts/dev-checks.sh check alice/client` runs the vet,
test and gofmt checks CI runs.

## Mounting it

```go
import aliceclient "github.com/furious-luke/alice-go"

trap, err := aliceclient.New(os.Getenv("ALICE_API_KEY"))
if err != nil {
	log.Fatal(err)
}

mux := http.NewServeMux()
mux.Handle("/.envrc", trap)
mux.Handle("/.env", trap)
mux.Handle("/.env.local", trap)
mux.Handle("/.env.production", trap)
mux.Handle("/.git/config", trap)
mux.Handle("/wp-login.php", trap)
mux.Handle("/phpinfo.php", trap)
mux.Handle("/info.php", trap)
mux.Handle("/openapi.json", trap)
mux.Handle("/swagger.json", trap)
mux.Handle("/api-docs", trap)
mux.Handle("/v3/api-docs", trap)
mux.Handle("/internal/admin", trap)
mux.Handle("/internal/admin/", trap)
mux.Handle("/internal/api", trap)
mux.Handle("/internal/api/", trap)
mux.HandleFunc("GET /api/products", products) // the site's real routes
```

These are the bait paths, and Alice answers each of them:

| Path | Alice's answer |
| --- | --- |
| `/.envrc` | A direnv file (`export KEY=value` lines) leaking the fake admin's URL and a password of the agent's own. |
| `/.env`, `/.env.local`, `/.env.production` | Dotenv files (`KEY=value` lines), each a different variant (production, local, production overrides) with fake third-party settings, the same admin URL and password included. |
| `/.git/config` | An exposed git config whose remote URL carries the same password. |
| `/wp-login.php` | A WordPress login page with a stale comment giving the admin's URL and the same password. A POST of its form gets the page back with a wrong-password error. |
| `/phpinfo.php`, `/info.php` | A `phpinfo()` page whose environment shows the admin's URL and the same password. |
| `/openapi.json`, `/swagger.json`, `/api-docs`, `/v3/api-docs` | An OpenAPI document of your product's API: a join that hands out a bearer token, the operations that token opens, and, marked staff-only, an elevate operation and the admin's routes. It carries no credentials. |
| `/internal/admin`, `/internal/admin/` and below | The fake admin, behind the leaked password or an elevated token. |
| `/internal/api`, `/internal/api/` and below | The API the OpenAPI document describes: join, the joined session's operations, and elevate. |

What each says (your product's name, your stack's variables, the admin's
routes) comes from your account's persona, which you set with `alice
persona set`; the PHP pages answer only when your persona's stack is PHP,
and are a plain 404 otherwise, so you can mount them whatever your stack.

Mount each for every method, as above: the login page's form posts to it.
Mount only the ones your site doesn't serve itself: a path you serve for
real stays yours, and a path you don't mount just isn't bait. The admin
needs both `/internal/admin` and `/internal/admin/`, so a bait file's
password leads somewhere, and the API both `/internal/api` and
`/internal/api/`, so the OpenAPI document's operations answer.

The API key is a customer key with the `trap` perm (`alice key make` prints
one). `New` refuses an empty key. The handler forwards to Alice at
`DefaultURL`, `https://alice.coragents.ai`; `WithURL` points it at another
alice-srv (a self-hosted or local one).

### Breadcrumbs on your own pages

Agents that explore a site read what scanners skip. Your own pages can
point at the admin too, with nothing of Alice's in them:

- `robots.txt`: `Disallow: /internal/admin`.
- An HTML comment in your app's shell, such as
  `<!-- ops: /internal/admin (see .env.local) -->`.

Neither leaks a password: they lead an agent to the admin's login and to
the bait files that do.

## Options

| Option | Default | What it does |
| --- | --- | --- |
| `WithURL(u)` | `DefaultURL`, `https://alice.coragents.ai` | The Alice server to forward to. `New` refuses a URL that isn't `http` or `https` with a host, or that has a query or fragment. |
| `WithTimeout(d)` | `DefaultTimeout`, 2 s | The deadline of each trap exchange. `New` refuses one that isn't positive. |
| `WithErrorHandler(fn)` | none | Calls `fn(r, err)` once for each fail-safe 404. |
| `WithDroppedCookies(names...)` | none | Takes the named cookies out of what is forwarded: your own session, CSRF and remember-me cookies ([below](#your-own-cookies-and-headers)). `New` refuses an empty or invalid name, and `_vid`, Alice's agent cookie. |
| `WithDroppedHeaders(names...)` | none | Takes the named headers out of what is forwarded, in any case: identity headers your own proxy adds. `New` refuses an empty or invalid name, and `Authorization`, `Cookie` and `Host`. |
| `WithTrustedProxies(prefixes...)` | none | Takes the client IP from `X-Forwarded-For` behind your own proxies. `New` refuses an invalid prefix, and an IPv4-mapped one (give `10.0.0.0/8`, not `::ffff:10.0.0.0/104`). |
| `WithClientIP(fn)` | none | Takes the client IP from your own code, `fn(r)`, for each request ([below](#client-ip-from-your-own-code)). `New` refuses a nil `fn`, and `WithClientIP` together with `WithTrustedProxies`, even one with no prefixes. |
| `WithTransport(rt)` | `http.DefaultTransport` | Sends the trap requests through your own `http.RoundTripper`. |

```go
trap, err := aliceclient.New(apiKey,
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

### Client IP from your own code

If your site already works out the client IP, hand it to the client with
`WithClientIP` instead of listing proxies: behind load balancers with no
fixed addresses, say, where you count hops from the right of
`X-Forwarded-For`. The function is called once for each request, on the
request's goroutine, before the trap request is sent:

```go
// trustedHops is how many proxies of yours append to X-Forwarded-For.
const trustedHops = 2

clientIP := func(r *http.Request) string {
	var entries []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		for _, entry := range strings.Split(line, ",") {
			entries = append(entries, strings.TrimSpace(entry))
		}
	}
	if len(entries) < trustedHops {
		return "" // RemoteAddr's IP
	}
	return entries[len(entries)-trustedHops] // the Nth entry from the right
}

trap, err := aliceclient.New(apiKey, aliceclient.WithClientIP(clientIP))
```

- **What it may return:** an IP address (`198.51.100.9`), or an address and
  port (`198.51.100.9:4711`, `[2001:db8::1]:443`), whose port is dropped.
  An IPv4-mapped address is sent in its IPv4 form, and a zone
  (`fe80::1%eth0`) is dropped.
- **Falling back:** when the function returns `""`, something that isn't
  one of those, or panics (the panic is recovered), the client IP is
  `RemoteAddr`'s, as without the option. The request is still forwarded and
  the agent still gets Alice's answer, not the fail-safe 404: a bug in your
  function costs Alice a better IP, never a capture. The fallback is silent:
  nothing is reported to `WithErrorHandler`, so test your function, or a
  load balancer's IP becomes every agent's.
- **Refused:** `New` refuses a nil function, and `WithClientIP` together
  with `WithTrustedProxies`, in either order, even when `WithTrustedProxies`
  is given no prefixes. Give the client IP one way.

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
  (or from `X-Forwarded-For` behind trusted proxies, or from your own code,
  above). Mount the handler where `RemoteAddr` is the agent's IP (with or
  without a port) or a listed proxy's, or give the IP with `WithClientIP`:
  Alice refuses a request without an IP, such as one from a unix-socket
  listener, and the handler answers it with a 404.
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
```

The tests run against a fake trap server; they need no network or Docker.
