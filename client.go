// Package client is Alice's reverse-proxy middleware. A site mounts its
// Handler on bait paths; the handler forwards each request it gets to
// Alice's trap API and relays the response Alice decides, so the agent that
// sent the request only ever sees the site's own domain.
//
// The package uses the standard library only.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultURL is the Alice server a Handler forwards to, unless WithURL says
// otherwise.
const DefaultURL = "https://alice.coragents.ai"

// TrapPath is the trap API's route on alice-srv.
const TrapPath = "/api/trap"

// TrapContentType is the trap request's content type: a JSON head line,
// then the forwarded body.
const TrapContentType = "application/vnd.alice.trap"

// TrapMarkerHeader is the header every response of Alice's maze carries.
// A response without it (with the value "1") is never relayed.
const TrapMarkerHeader = "Alice-Trap"

// AgentCookieName is Alice's agent cookie: the maze sets it on the site's
// domain, and it identifies the agent from one request to the next. It is
// always forwarded: New refuses dropping it.
const AgentCookieName = "_vid"

// DefaultTimeout is how long a trap exchange may take, unless WithTimeout
// says otherwise.
const DefaultTimeout = 2 * time.Second

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
// response. When Alice can't be reached in time, or answers without the
// trap marker, it answers with a plain 404, as if the path weren't there.
type Handler struct {
	// aliceURL is the Alice server as given (DefaultURL or WithURL's);
	// trapURL is the trap API on it, built by New.
	aliceURL       string
	trapURL        string
	key            string
	auth           string
	transport      http.RoundTripper
	timeout        time.Duration
	onError        func(*http.Request, error)
	trustedProxies []netip.Prefix
	// trustsProxies and hasClientIP are whether WithTrustedProxies and
	// WithClientIP were given, so New can refuse both together even when
	// the prefixes are none or the function is nil.
	trustsProxies bool
	hasClientIP   bool
	clientIPFunc  func(*http.Request) string
	// cookiesToDrop and headersToDrop are the names the options give, as
	// given; droppedCookies and droppedHeaders are them checked by New, the
	// header names in canonical form.
	cookiesToDrop  []string
	headersToDrop  []string
	droppedCookies map[string]struct{}
	droppedHeaders map[string]struct{}
}

// Option configures a Handler.
type Option func(*Handler)

// WithURL forwards to the Alice server at aliceURL instead of DefaultURL:
// a self-hosted or local alice-srv, say. New refuses a URL that isn't http
// or https with a host, or that has a query or fragment.
func WithURL(aliceURL string) Option {
	return func(h *Handler) {
		h.aliceURL = aliceURL
	}
}

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

// WithTimeout sets the deadline of each trap exchange, DefaultTimeout
// without it. One deadline covers the whole exchange: connecting to Alice,
// sending the head and the forwarded body, Alice's response head and
// relaying the response body, so a slow, large upload counts against it.
// When it passes before the response head arrives, the handler answers its
// plain 404. New refuses a timeout that isn't positive.
func WithTimeout(d time.Duration) Option {
	return func(h *Handler) {
		h.timeout = d
	}
}

// WithErrorHandler calls fn once each time the handler answers its
// fail-safe 404, with the agent's request and why. The error matches
// (errors.Is) one of ErrRequest, ErrUnreachable, ErrTimeout, ErrNoMarker
// and ErrPanic, and is an *Error (errors.As). It never contains the API
// key, the trap request's headers or the agent's headers. fn runs on the
// request's goroutine, before the 404 is written; a panic in it is
// recovered.
func WithErrorHandler(fn func(*http.Request, error)) Option {
	return func(h *Handler) {
		h.onError = fn
	}
}

// WithTrustedProxies takes the client IP from X-Forwarded-For behind the
// site's own proxies. When the request's RemoteAddr is inside one of
// prefixes, the handler walks X-Forwarded-For from right to left, skipping
// addresses inside prefixes, and the first address outside them is the
// client IP; when every entry is inside them, the leftmost is. A missing
// header, or an entry the walk reaches that isn't an IP address, leaves
// the client IP as RemoteAddr's. Without this option, or when RemoteAddr
// is outside prefixes, X-Forwarded-For is ignored (it is still forwarded
// as a header). Trusting every peer is 0.0.0.0/0 and ::/0, given
// explicitly. New refuses an invalid prefix, and an IPv4-mapped one
// (::ffff:10.0.0.0/104): addresses are matched in their IPv4 form, so give
// the prefix in that form (10.0.0.0/8).
func WithTrustedProxies(prefixes ...netip.Prefix) Option {
	return func(h *Handler) {
		h.trustsProxies = true
		h.trustedProxies = append(h.trustedProxies, prefixes...)
	}
}

// WithClientIP takes the client IP from fn, the site's own code: for a
// site that already works it out, behind load balancers with no fixed
// addresses, say. fn is called once per request, on the request's
// goroutine, before the trap request is sent. Its result is an IP address,
// or an IP address and port (198.51.100.9:4711, [2001:db8::1]:443), whose
// port is dropped; an IPv4-mapped address is given in its IPv4 form, and
// a zone is dropped. When fn returns "" or something else that isn't one
// of those, or panics (the panic is recovered), the client IP is
// RemoteAddr's, as without the option, and the request is forwarded all
// the same. New refuses a nil fn, and WithClientIP together with
// WithTrustedProxies: give the client IP one way.
func WithClientIP(fn func(*http.Request) string) Option {
	return func(h *Handler) {
		h.hasClientIP = true
		h.clientIPFunc = fn
	}
}

// WithDroppedCookies takes the named cookies out of what is forwarded:
// the site's own session, CSRF and remember-me cookies, which Alice has no
// use for and should never hold. Names match exactly and case-sensitively,
// a __Host- or __Secure- prefix included; there are no wildcards. Every
// Cookie line is read, and each ;-separated pair whose name, trimmed of
// spaces, is listed is removed, a bare listed name without = included.
// Every other pair is kept byte for byte, malformed ones too. A line left
// with no pair is removed, and so is the header when no line is left. The
// agent's request itself is never changed. Given more than once, the
// lists add up. New refuses an empty name, one that isn't a valid cookie
// name, and AgentCookieName, which the maze needs.
func WithDroppedCookies(names ...string) Option {
	return func(h *Handler) {
		h.cookiesToDrop = append(h.cookiesToDrop, names...)
	}
}

// WithDroppedHeaders takes the named headers out of what is forwarded,
// matched case-insensitively: identity headers the site's own proxy adds,
// such as an auth proxy's X-Auth-Request-Email. The agent's request
// itself is never changed. Given more than once, the lists add up. New
// refuses an empty name, one that isn't a valid header name, and
// Authorization (the maze's login), Cookie (it carries AgentCookieName:
// drop the site's cookies with WithDroppedCookies) and Host.
func WithDroppedHeaders(names ...string) Option {
	return func(h *Handler) {
		h.headersToDrop = append(h.headersToDrop, names...)
	}
}

// keptHeaders are the headers WithDroppedHeaders can't drop, and why.
var keptHeaders = map[string]string{
	"Authorization": "the maze's login needs it",
	"Cookie":        "it carries Alice's agent cookie; drop the site's cookies with WithDroppedCookies",
	"Host":          "the trap needs it",
}

// The reasons the handler answers its fail-safe 404, matched with
// errors.Is on the error WithErrorHandler gets.
var (
	// ErrRequest is a trap request that couldn't be made from the agent's
	// request: its body couldn't be read, or the agent went away.
	ErrRequest = errors.New("the trap request could not be made from the agent's request")
	// ErrUnreachable is Alice not reached: the connection was refused, the
	// host wasn't found, or the exchange failed before a response head.
	ErrUnreachable = errors.New("Alice could not be reached")
	// ErrTimeout is the deadline passing before Alice's response head.
	ErrTimeout = errors.New("the trap request timed out")
	// ErrNoMarker is a response from Alice without the trap marker.
	ErrNoMarker = errors.New("Alice's response had no trap marker")
	// ErrPanic is a panic in the handler before it had written anything.
	ErrPanic = errors.New("the client panicked")
)

// Error is why the handler answered its fail-safe 404.
type Error struct {
	// Reason is one of ErrRequest, ErrUnreachable, ErrTimeout, ErrNoMarker
	// and ErrPanic.
	Reason error
	// Status is Alice's status, for ErrNoMarker; 0 otherwise.
	Status int
	// Err is the cause, when there is one: the transport's error. It
	// names no header of the trap request, and an error whose text
	// carries the API key all the same is replaced by its text with the
	// key redacted. A panic has no cause.
	Err error
}

func (e *Error) Error() string {
	msg := "alice client: " + e.Reason.Error()
	if e.Status != 0 {
		msg += fmt.Sprintf(" (status %d)", e.Status)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap is the reason and the cause, so errors.Is matches either.
func (e *Error) Unwrap() []error {
	if e.Err == nil {
		return []error{e.Reason}
	}
	return []error{e.Reason, e.Err}
}

// New is a handler that forwards to Alice (DefaultURL, unless WithURL
// says otherwise), authenticated by apiKey, a customer API key with the
// trap perm. By default, requests reach Alice with the IP of
// their connection, taken from the request's RemoteAddr, so mount the
// handler where RemoteAddr is the agent's IP (with or without a port),
// list the site's proxies with WithTrustedProxies, or give the IP from the
// site's own code with WithClientIP: a request from a unix-socket listener,
// or one with no RemoteAddr and no IP from WithClientIP, is refused by
// Alice and answered with a 404.
func New(apiKey string, opts ...Option) (*Handler, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("alice client: the API key is required")
	}
	h := &Handler{
		aliceURL:  DefaultURL,
		key:       apiKey,
		auth:      "Token " + apiKey,
		transport: http.DefaultTransport,
		timeout:   DefaultTimeout,
	}
	for _, opt := range opts {
		opt(h)
	}
	base, err := url.Parse(h.aliceURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("alice client: the Alice URL must be an http or https URL with a host")
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("alice client: the Alice URL must not have a query or fragment")
	}
	h.trapURL = base.JoinPath(TrapPath).String()
	if h.timeout <= 0 {
		return nil, errors.New("alice client: the timeout must be positive")
	}
	if h.hasClientIP && h.clientIPFunc == nil {
		return nil, errors.New("alice client: WithClientIP's function is nil")
	}
	if h.hasClientIP && h.trustsProxies {
		return nil, errors.New("alice client: WithClientIP and WithTrustedProxies can't be given together: give the client IP one way")
	}
	for i, p := range h.trustedProxies {
		if !p.IsValid() {
			return nil, errors.New("alice client: a trusted proxy prefix is invalid")
		}
		if p.Addr().Is4In6() {
			// Addresses are unmapped before they are matched, so a mapped
			// prefix would never match anything.
			return nil, fmt.Errorf("alice client: the trusted proxy prefix %s is IPv4-mapped; use its IPv4 form", p)
		}
		h.trustedProxies[i] = p.Masked()
	}
	if err := h.checkDrops(); err != nil {
		return nil, err
	}
	return h, nil
}

// checkDrops checks the names the drop options gave and makes the sets
// the handler drops by.
func (h *Handler) checkDrops() error {
	for _, name := range h.cookiesToDrop {
		switch {
		case name == "":
			return errors.New("alice client: a dropped cookie name is empty")
		case !isToken(name):
			return fmt.Errorf("alice client: the dropped cookie name %q isn't a valid cookie name", h.redact(name))
		case name == AgentCookieName:
			return fmt.Errorf("alice client: the cookie %s is Alice's agent cookie and can't be dropped", AgentCookieName)
		}
		if h.droppedCookies == nil {
			h.droppedCookies = map[string]struct{}{}
		}
		h.droppedCookies[name] = struct{}{}
	}
	for _, name := range h.headersToDrop {
		switch {
		case name == "":
			return errors.New("alice client: a dropped header name is empty")
		case !isToken(name):
			return fmt.Errorf("alice client: the dropped header name %q isn't a valid header name", h.redact(name))
		}
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if why, kept := keptHeaders[canonical]; kept {
			return fmt.Errorf("alice client: the %s header can't be dropped: %s", canonical, why)
		}
		if h.droppedHeaders == nil {
			h.droppedHeaders = map[string]struct{}{}
		}
		h.droppedHeaders[canonical] = struct{}{}
	}
	return nil
}

// redact is s with the API key taken out, for an error that quotes what
// the site configured.
func (h *Handler) redact(s string) string {
	return strings.ReplaceAll(s, h.key, "[redacted]")
}

// isToken is whether s is an RFC 9110 token, which both a header name and
// an RFC 6265 cookie name are: one or more of the visible ASCII
// characters other than the separators.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// trapHead is the first line of a trap request: the forwarded request
// without its body.
type trapHead struct {
	Method   string      `json:"method"`
	Target   string      `json:"target"`
	Header   http.Header `json:"header"`
	ClientIP string      `json:"client_ip"`
}

// ServeHTTP forwards r to the trap API and relays the response. No panic
// leaves it: one before anything is written is answered with the 404.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tw := &trackingWriter{ResponseWriter: w}
	defer func() {
		if v := recover(); v != nil && !tw.wrote {
			// The panic's value is left out: it could be anything, the
			// trap request and its key included.
			h.failSafe(tw, r, &Error{Reason: ErrPanic})
		}
	}()
	h.serve(tw, r)
}

func (h *Handler) serve(w *trackingWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	trapReq, body, err := h.trapRequest(ctx, r)
	if err != nil {
		h.failSafe(w, r, &Error{Reason: ErrRequest, Err: err})
		return
	}
	res, err := h.transport.RoundTrip(trapReq)
	if err != nil {
		h.failSafe(w, r, &Error{Reason: h.roundTripReason(ctx, r, body, err), Err: err})
		return
	}
	defer res.Body.Close()
	if res.Header.Get(TrapMarkerHeader) != "1" {
		// Drain a little so the connection can be reused; the rest is
		// discarded with it.
		_, _ = io.CopyN(io.Discard, res.Body, 4<<10)
		h.failSafe(w, r, &Error{Reason: ErrNoMarker, Status: res.StatusCode})
		return
	}

	header := w.Header()
	for name, values := range withoutHopByHop(res.Header) {
		header[name] = values
	}
	header.Del(TrapMarkerHeader)
	// A known length is relayed as the answer's Content-Length, so the
	// site's own server frames it as it frames its own pages, rather than
	// chunking anything over its buffer. A HEAD keeps the length, as the
	// site's server sends it for its own pages, and gets no body.
	header.Del("Content-Length")
	if res.ContentLength >= 0 && bodyAllowed(res.StatusCode) {
		header.Set("Content-Length", strconv.FormatInt(res.ContentLength, 10))
	}
	w.WriteHeader(res.StatusCode)
	// A failure here (the deadline included) is past the point where the
	// agent could be told anything else: the status is already sent.
	_, _ = io.Copy(w, res.Body)
}

// roundTripReason is why the trap request failed with err: the agent's
// request ending or its body failing, the deadline, or Alice not reached.
func (h *Handler) roundTripReason(ctx context.Context, r *http.Request, body *agentBody, err error) error {
	switch {
	case r.Context().Err() != nil, body.failed():
		return ErrRequest
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return ErrTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrTimeout
	}
	return ErrUnreachable
}

// failSafe answers the plain 404, after telling the error handler why.
func (h *Handler) failSafe(w http.ResponseWriter, r *http.Request, err *Error) {
	if err.Err != nil && strings.Contains(err.Err.Error(), h.key) {
		// The default transport's errors never carry the key; a custom
		// transport's might.
		err.Err = errors.New(strings.ReplaceAll(err.Err.Error(), h.key, "[redacted]"))
	}
	h.report(r, err)
	http.NotFound(w, r)
}

// report calls the error handler, if there is one; a panic in it is
// recovered, so the 404 is still written.
func (h *Handler) report(r *http.Request, err error) {
	if h.onError == nil {
		return
	}
	defer func() { _ = recover() }()
	h.onError(r, err)
}

// trackingWriter records whether anything has been written to the agent.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *trackingWriter) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(p)
}

// agentBody is the agent's body as the trap request reads it. A goroutine
// copies the agent's body into a pipe the transport reads, so when the
// exchange ends (the deadline included) the pipe is closed and the
// transport stops waiting on an agent that has stopped sending: Go's
// transport doesn't return from a failed round trip until it is done
// with the request body. It also records whether a read of the agent's
// body failed, so that failure isn't taken for Alice's.
type agentBody struct {
	pr *io.PipeReader

	mu  sync.Mutex
	err error
}

// newAgentBody starts copying body, until it ends or ctx is done.
func newAgentBody(ctx context.Context, body io.Reader) *agentBody {
	pr, pw := io.Pipe()
	b := &agentBody{pr: pr}
	context.AfterFunc(ctx, func() { pr.CloseWithError(ctx.Err()) })
	// The copy can outlive the handler: closing the pipe ends the trap
	// request, but a read of the agent's body already blocked goes on until
	// it returns. That relies on the standard library's server, which locks
	// the body so its Close waits for that read and later reads fail, and
	// whose ReadTimeout bounds a read from an agent that stopped sending.
	go func() {
		defer func() {
			if v := recover(); v != nil {
				// The panic's value is left out, as in ServeHTTP.
				err := errors.New("alice client: reading the agent's body panicked")
				b.mu.Lock()
				b.err = err
				b.mu.Unlock()
				pw.CloseWithError(err)
			}
		}()
		_, err := io.Copy(pw, readerFunc(func(p []byte) (int, error) {
			n, err := body.Read(p)
			if err != nil && err != io.EOF {
				b.mu.Lock()
				b.err = err
				b.mu.Unlock()
			}
			return n, err
		}))
		pw.CloseWithError(err)
	}()
	return b
}

func (b *agentBody) Read(p []byte) (int, error) { return b.pr.Read(p) }

// failed is whether a read of the agent's body failed.
func (b *agentBody) failed() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err != nil
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// trapRequest is the trap request for the forwarded request r, bound to
// ctx: its head line, then r's body, streamed; body is r's body as it
// reads it, nil for a request without one.
func (h *Handler) trapRequest(ctx context.Context, r *http.Request) (trapReq *http.Request, body *agentBody, err error) {
	header := withoutHopByHop(r.Header)
	h.drop(header)
	if r.Host != "" {
		header.Set("Host", r.Host)
	}
	head, err := json.Marshal(trapHead{
		Method:   r.Method,
		Target:   requestTarget(r),
		Header:   header,
		ClientIP: h.clientIP(r),
	})
	if err != nil {
		return nil, nil, err
	}
	head = append(head, '\n')

	var content io.Reader = bytes.NewReader(head)
	if r.Body != nil && r.Body != http.NoBody {
		body = newAgentBody(ctx, r.Body)
		content = io.MultiReader(content, body)
	}
	trapReq, err = http.NewRequestWithContext(ctx, http.MethodPost, h.trapURL, content)
	if err != nil {
		return nil, nil, err
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
	return trapReq, body, nil
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

// clientIP is the agent's IP: the one the site's function gives, or
// RemoteAddr's, or, when RemoteAddr is a trusted proxy, the one
// X-Forwarded-For gives.
func (h *Handler) clientIP(r *http.Request) string {
	remote := remoteIP(r.RemoteAddr)
	if h.clientIPFunc != nil {
		if addr, ok := h.siteClientIP(r); ok {
			return addr.String()
		}
		return remote
	}
	if len(h.trustedProxies) == 0 {
		return remote
	}
	peer, err := netip.ParseAddr(remote)
	if err != nil || !h.trusted(peer.Unmap().WithZone("")) {
		return remote
	}
	if forwarded, ok := h.forwardedFor(r.Header.Values("X-Forwarded-For")); ok {
		return forwarded.String()
	}
	return remote
}

// siteClientIP is the client IP the site's function gives for r,
// unmapped and without a zone, and false when it gives nothing usable or
// panics. The panic is recovered here, so the request is still trapped.
func (h *Handler) siteClientIP(r *http.Request) (addr netip.Addr, ok bool) {
	defer func() {
		if recover() != nil {
			addr, ok = netip.Addr{}, false
		}
	}()
	s := textproto.TrimString(h.clientIPFunc(r))
	if s == "" {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		addrPort, err := netip.ParseAddrPort(s)
		if err != nil {
			return netip.Addr{}, false
		}
		addr = addrPort.Addr()
	}
	return addr.Unmap().WithZone(""), true
}

// forwardedFor walks the X-Forwarded-For lines from the right, skipping
// trusted addresses, and is the first untrusted one, or the leftmost when
// all are trusted. It is false when there is no entry, or the walk reaches
// one that isn't an IP address. Entries left of the result are never read.
func (h *Handler) forwardedFor(lines []string) (netip.Addr, bool) {
	var last netip.Addr
	for i := len(lines) - 1; i >= 0; i-- {
		rest := lines[i]
		for {
			entry := rest
			cut := strings.LastIndexByte(rest, ',')
			if cut >= 0 {
				entry, rest = rest[cut+1:], rest[:cut]
			}
			addr, err := netip.ParseAddr(textproto.TrimString(entry))
			if err != nil {
				return netip.Addr{}, false
			}
			addr = addr.Unmap().WithZone("")
			if !h.trusted(addr) {
				return addr, true
			}
			last = addr
			if cut < 0 {
				break
			}
		}
	}
	return last, last.IsValid()
}

// trusted is whether addr, unmapped and without a zone, is inside a
// trusted proxy prefix.
func (h *Handler) trusted(addr netip.Addr) bool {
	for _, p := range h.trustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// remoteIP is the IP of the agent's connection: RemoteAddr without its
// port.
func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// drop takes the dropped headers and cookies out of header, a copy of the
// agent's. Names are matched in canonical form, so a header map built by
// hand with a lower-case key is no way past it.
func (h *Handler) drop(header http.Header) {
	if len(h.droppedHeaders) == 0 && len(h.droppedCookies) == 0 {
		return
	}
	for name, lines := range header {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if _, dropped := h.droppedHeaders[canonical]; dropped {
			delete(header, name)
			continue
		}
		if canonical != "Cookie" || len(h.droppedCookies) == 0 {
			continue
		}
		kept := make([]string, 0, len(lines))
		for _, line := range lines {
			if line, ok := h.dropCookies(line); ok {
				kept = append(kept, line)
			}
		}
		if len(kept) == 0 {
			delete(header, name)
		} else {
			header[name] = kept
		}
	}
}

// dropCookies is the Cookie line without its dropped pairs, and false
// when nothing but separators and spaces is left of a line that had one.
// A line without a dropped pair is kept as it is. The pairs kept are
// joined as they were, by ";"; when the line's first pair is dropped, the
// spaces that followed its ";" go with it.
func (h *Handler) dropCookies(line string) (string, bool) {
	var (
		out     strings.Builder
		dropped bool
		kept    int
	)
	for rest, more := line, true; more; {
		var pair string
		pair, rest, more = strings.Cut(rest, ";")
		name, _, _ := strings.Cut(pair, "=")
		if _, drop := h.droppedCookies[textproto.TrimString(name)]; drop {
			dropped = true
			continue
		}
		if kept > 0 {
			out.WriteByte(';')
		} else if dropped {
			pair = strings.TrimLeft(pair, " \t")
		}
		out.WriteString(pair)
		kept++
	}
	if !dropped {
		return line, true
	}
	result := out.String()
	if strings.Trim(result, " \t;") == "" {
		return "", false
	}
	return result, true
}

// bodyAllowed is whether a response with the status may carry a body (and
// so a Content-Length): not a 1xx, a 204 or a 304.
func bodyAllowed(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
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
