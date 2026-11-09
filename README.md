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
empty key. `WithTransport` sends the trap requests through your own
`http.RoundTripper`.

## What it forwards and relays

- **Forwarded:** the method, the path and query as received, the headers
  (the `Host` header included, hop-by-hop headers left out), the body,
  streamed, and the client IP, taken from the connection's `RemoteAddr`.
  Mount the handler where `RemoteAddr` is the agent's IP (with or without
  a port): Alice refuses a request without one, such as one from a
  unix-socket listener, and the handler answers it with a 404.
- **Relayed:** the status, headers and body of Alice's response, without
  the trap marker (`Alice-Trap`) or hop-by-hop headers. Redirects are
  relayed, not followed.
- **Failing safe:** when Alice can't be reached, or its response has no
  trap marker (a refused key, a malformed request, a failure of Alice's
  own), the handler answers with a plain 404, as if the path weren't there.

## Testing

```sh
go test ./...
../../scripts/dev-checks.sh check alice/client   # vet, test and gofmt, as CI runs them
```

The tests run against a fake trap server; they need no network or Docker.
