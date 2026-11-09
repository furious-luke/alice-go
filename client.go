// Package client is Alice's reverse-proxy middleware. A site mounts its
// Handler on bait paths; the handler forwards each request it gets to
// Alice's trap API and relays the response Alice decides, so the agent that
// sent the request only ever sees the site's own domain.
//
// The package uses the standard library only.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// TrapPath is the trap API's route on alice-srv.
const TrapPath = "/api/trap"

// TrapContentType is the trap request's content type: a JSON head line,
// then the forwarded body.
const TrapContentType = "application/vnd.alice.trap"

// TrapMarkerHeader is the header every response of Alice's maze carries.
// A response without it (with the value "1") is never relayed.
const TrapMarkerHeader = "Alice-Trap"

// hopByHopHeaders are the headers that belong to one connection, not to the
// request or response: they are never forwarded or relayed (RFC 9110,
// section 7.6.1, plus the obsolete ones proxies still meet).
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Handler forwards every request to Alice's trap API and relays the
// response. When Alice can't be reached, or answers without the trap
// marker, it answers with a plain 404, as if the path weren't there.
type Handler struct {
	trapURL   string
	auth      string
	transport http.RoundTripper
}

// Option configures a Handler.
type Option func(*Handler)

// WithTransport sends the trap requests through rt instead of
// http.DefaultTransport. The handler calls rt.RoundTrip itself, so
// redirects are never followed and no cookies are kept.
func WithTransport(rt http.RoundTripper) Option {
	return func(h *Handler) {
		if rt != nil {
			h.transport = rt
		}
	}
}

// New is a handler that forwards to the Alice server at aliceURL (for
// example https://alice.example), authenticated by apiKey, a customer API
// key with the trap perm. Requests reach Alice with the IP of their
// connection, taken from the request's RemoteAddr, so mount the handler
// where RemoteAddr is the agent's IP (with or without a port): a request
// from a unix-socket listener, or one with no RemoteAddr, is refused by
// Alice and answered with a 404.
func New(aliceURL, apiKey string, opts ...Option) (*Handler, error) {
	base, err := url.Parse(aliceURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("alice client: the Alice URL must be an http or https URL with a host")
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("alice client: the Alice URL must not have a query or fragment")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("alice client: the API key is required")
	}
	h := &Handler{
		trapURL:   base.JoinPath(TrapPath).String(),
		auth:      "Token " + apiKey,
		transport: http.DefaultTransport,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// trapHead is the first line of a trap request: the forwarded request
// without its body.
type trapHead struct {
	Method   string      `json:"method"`
	Target   string      `json:"target"`
	Header   http.Header `json:"header"`
	ClientIP string      `json:"client_ip"`
}

// ServeHTTP forwards r to the trap API and relays the response.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	trapReq, err := h.trapRequest(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	res, err := h.transport.RoundTrip(trapReq)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer res.Body.Close()
	if res.Header.Get(TrapMarkerHeader) != "1" {
		// Drain a little so the connection can be reused; the rest is
		// discarded with it.
		_, _ = io.CopyN(io.Discard, res.Body, 4<<10)
		http.NotFound(w, r)
		return
	}

	header := w.Header()
	for name, values := range withoutHopByHop(res.Header) {
		header[name] = values
	}
	header.Del(TrapMarkerHeader)
	w.WriteHeader(res.StatusCode)
	// A failure here is past the point where the agent could be told
	// anything else: the status is already sent.
	_, _ = io.Copy(w, res.Body)
}

// trapRequest is the trap request for the forwarded request r: its head
// line, then r's body, streamed.
func (h *Handler) trapRequest(r *http.Request) (*http.Request, error) {
	header := withoutHopByHop(r.Header)
	if r.Host != "" {
		header.Set("Host", r.Host)
	}
	head, err := json.Marshal(trapHead{
		Method:   r.Method,
		Target:   requestTarget(r),
		Header:   header,
		ClientIP: clientIP(r.RemoteAddr),
	})
	if err != nil {
		return nil, err
	}
	head = append(head, '\n')

	var body io.Reader = bytes.NewReader(head)
	if r.Body != nil && r.Body != http.NoBody {
		body = io.MultiReader(body, r.Body)
	}
	trapReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.trapURL, body)
	if err != nil {
		return nil, err
	}
	if r.Body == nil || r.Body == http.NoBody {
		trapReq.ContentLength = int64(len(head))
	} else if r.ContentLength > 0 {
		trapReq.ContentLength = int64(len(head)) + r.ContentLength
	} else {
		// Unknown length (a body with no length is unknown too, as for a
		// client request): sent chunked.
		trapReq.ContentLength = -1
	}
	trapReq.Header.Set("Authorization", h.auth)
	trapReq.Header.Set("Content-Type", TrapContentType)
	return trapReq, nil
}

// requestTarget is the path and query the agent asked for: the request URI
// as received, so a prefix the site strips before this handler is kept.
func requestTarget(r *http.Request) string {
	if strings.HasPrefix(r.RequestURI, "/") {
		return r.RequestURI
	}
	if r.URL == nil {
		return "/"
	}
	return r.URL.RequestURI()
}

// clientIP is the IP of the agent's connection: RemoteAddr without its
// port.
func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// withoutHopByHop is a copy of header without its hop-by-hop headers: the
// fixed ones, and those the Connection header names.
func withoutHopByHop(header http.Header) http.Header {
	out := header.Clone()
	if out == nil {
		out = http.Header{}
	}
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = textproto.TrimString(name); name != "" {
				out.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		out.Del(name)
	}
	return out
}
