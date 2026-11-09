package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/furious-luke/go-projects/alice/client"
)

func TestSpec_Client_ForwardsTheRequestUnchanged(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	req := httptest.NewRequest(http.MethodPut, "/internal/admin/payroll?month=3&note=a%20b", strings.NewReader(`{"amount":100}`))
	req.Host = "shop.example"
	req.RemoteAddr = "203.0.113.7:51234"
	req.Header = http.Header{
		"Content-Type":  {"application/json"},
		"Cookie":        {"_vid=abc"},
		"Authorization": {"Basic YWRtaW46c2VjcmV0"},
		"X-Many":        {"one", "two"},
	}

	a.Proxy(trap).Serve(req)

	got := trap.MustLastRequest()
	equal(t, "method", got.Head.Method, http.MethodPut)
	equal(t, "target", got.Head.Target, "/internal/admin/payroll?month=3&note=a%20b")
	equal(t, "header", got.Head.Header, http.Header{
		"Host":          {"shop.example"},
		"Content-Type":  {"application/json"},
		"Cookie":        {"_vid=abc"},
		"Authorization": {"Basic YWRtaW46c2VjcmV0"},
		"X-Many":        {"one", "two"},
	})
	equal(t, "client IP", got.Head.ClientIP, "203.0.113.7")
	equal(t, "body", got.Body, `{"amount":100}`)
}

func TestSpec_Client_SpeaksTheTrapProtocol(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	req := httptest.NewRequest(http.MethodGet, "/.envrc", nil)
	req.Header.Set("Cookie", "_vid=abc")
	req.Header.Set("Authorization", "Basic YWRtaW46c2VjcmV0")

	a.Proxy(trap).Serve(req)

	// The key is the trap request's own credential; the agent's cookies and
	// credentials travel only inside the head.
	got := trap.MustLastRequest()
	equal(t, "authorization", got.TrapHeader.Values("Authorization"), []string{"Token " + testAPIKey})
	equal(t, "content type", got.TrapHeader.Get("Content-Type"), client.TrapContentType)
	equal(t, "trap request cookies", got.TrapHeader.Values("Cookie"), []string(nil))
}

func TestSpec_Client_ForwardsTheClientIPFromRemoteAddr(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{"IPv4 with a port", "203.0.113.7:51234", "203.0.113.7"},
		{"IPv6 with a port", "[2001:db8::1]:443", "2001:db8::1"},
		{"an address without a port", "198.51.100.2", "198.51.100.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			trap := a.TrapServer()
			req := httptest.NewRequest(http.MethodGet, "/.envrc", nil)
			req.RemoteAddr = tc.remoteAddr

			a.Proxy(trap).Serve(req)

			equal(t, "client IP", trap.MustLastRequest().Head.ClientIP, tc.want)
		})
	}
}

func TestSpec_Client_StripsHopByHopRequestHeaders(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	req := httptest.NewRequest(http.MethodGet, "/.envrc", nil)
	req.Host = "shop.example"
	req.Header = http.Header{
		"Connection":          {"keep-alive, X-Conn-Only"},
		"X-Conn-Only":         {"1"},
		"Keep-Alive":          {"timeout=5"},
		"Proxy-Connection":    {"keep-alive"},
		"Proxy-Authorization": {"Basic cHJveHk6cHJveHk="},
		"Te":                  {"trailers"},
		"Trailer":             {"X-Checksum"},
		"Transfer-Encoding":   {"chunked"},
		"Upgrade":             {"websocket"},
		"X-Kept":              {"yes"},
	}

	a.Proxy(trap).Serve(req)

	equal(t, "header", trap.MustLastRequest().Head.Header, http.Header{
		"Host":   {"shop.example"},
		"X-Kept": {"yes"},
	})
}

func TestSpec_Client_RelaysTheResponseUnchangedButTheMarker(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	trap.AnswersWith(trapResponse{
		Status: http.StatusCreated,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"Set-Cookie":   {"_vid=abc; Path=/; Max-Age=31536000; HttpOnly; SameSite=Lax"},
			"X-Many":       {"one", "two"},
		},
		Body:   `{"routes":["/internal/admin/payroll"]}`,
		Marker: "1",
	})

	res := a.Proxy(trap).Serve(httptest.NewRequest(http.MethodGet, "/internal/admin", nil))

	equal(t, "status", res.Code, http.StatusCreated)
	equal(t, "header", relayedHeader(res), http.Header{
		"Content-Type":   {"application/json"},
		"Content-Length": {strconv.Itoa(len(`{"routes":["/internal/admin/payroll"]}`))},
		"Set-Cookie":     {"_vid=abc; Path=/; Max-Age=31536000; HttpOnly; SameSite=Lax"},
		"X-Many":         {"one", "two"},
	})
	equal(t, "body", res.Body.String(), `{"routes":["/internal/admin/payroll"]}`)
}

func TestSpec_Client_StripsHopByHopResponseHeaders(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	trap.AnswersWith(trapResponse{
		Status: http.StatusOK,
		Header: http.Header{
			"Connection":         {"X-Conn-Only"},
			"X-Conn-Only":        {"1"},
			"Keep-Alive":         {"timeout=5"},
			"Proxy-Authenticate": {"Basic"},
			"Upgrade":            {"h2c"},
			"X-Kept":             {"yes"},
		},
		Body:   "ok",
		Marker: "1",
	})

	res := a.Proxy(trap).Serve(httptest.NewRequest(http.MethodGet, "/.envrc", nil))

	equal(t, "header", relayedHeader(res), http.Header{
		"Content-Length": {"2"},
		"Content-Type":   {"text/plain; charset=utf-8"},
		"X-Kept":         {"yes"},
	})
}

func TestSpec_Client_RelaysARedirectWithoutFollowingIt(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	trap.AnswersWith(trapResponse{
		Status: http.StatusFound,
		Header: http.Header{"Location": {"/internal/admin/login"}},
		Marker: "1",
	})

	res := a.Proxy(trap).Serve(httptest.NewRequest(http.MethodGet, "/internal/admin", nil))

	equal(t, "status", res.Code, http.StatusFound)
	equal(t, "location", res.Header().Get("Location"), "/internal/admin/login")
	equal(t, "trap requests", len(trap.Requests()), 1)
}

func TestSpec_Client_StreamsALargeMultipartUpload(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	agent := a.Agent(a.Proxy(trap))

	sent := agent.MustUploadInStages("/internal/admin/upload", 6<<20, trap)

	got := trap.MustLastRequest()
	equal(t, "status", sent.Status, http.StatusOK)
	equal(t, "body size", got.BodySize, sent.BodySize)
	equal(t, "body hash", got.BodySHA, sent.BodySHA)
	atLeast(t, "body size", got.BodySize, 5_000_000)
	atLeast(t, "bytes forwarded before the agent finished", sent.TrapReceivedBeforeEnd, streamStage/2)
}

func TestSpec_Client_ForwardsANilBodyAsEmpty(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	req := httptest.NewRequest(http.MethodGet, "/.envrc", nil)
	req.Body = nil

	res := a.Proxy(trap).Serve(req)

	equal(t, "status", res.Code, http.StatusOK)
	equal(t, "body size", trap.MustLastRequest().BodySize, int64(0))
}

func TestSpec_Client_FailsSafeWithAPlain404(t *testing.T) {
	refusal := "a refusal of Alice's own"
	cases := []struct {
		name  string
		proxy func(a arranger) *proxyActor
		body  io.ReadCloser
	}{
		{
			name:  "Alice can't be reached",
			proxy: func(a arranger) *proxyActor { return a.ProxyTo(a.UnreachableAlice()) },
		},
		{
			name:  "a response without the marker",
			proxy: answering(trapResponse{Status: http.StatusOK, Body: refusal}),
		},
		{
			name:  "a refusal without the marker",
			proxy: answering(trapResponse{Status: http.StatusUnauthorized, Body: refusal}),
		},
		{
			name:  "Alice failing without the marker",
			proxy: answering(trapResponse{Status: http.StatusInternalServerError, Body: refusal}),
		},
		{
			name:  "a marker with another value",
			proxy: answering(trapResponse{Status: http.StatusOK, Body: refusal, Marker: "0"}),
		},
		{
			name:  "a closed request body",
			proxy: answering(trapResponse{Status: http.StatusOK, Body: refusal, Marker: "1"}),
			body:  closedBody{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			req := httptest.NewRequest(http.MethodPost, "/.envrc", strings.NewReader("x"))
			req.Body = orDefault(tc.body, req.Body)

			res := tc.proxy(a).Serve(req)

			equal(t, "status", res.Code, http.StatusNotFound)
			equal(t, "body", res.Body.String(), "404 page not found\n")
			equal(t, "marker", res.Header().Values(client.TrapMarkerHeader), []string(nil))
		})
	}
}

func TestSpec_Client_RefusesABadConfiguration(t *testing.T) {
	cases := []struct {
		name     string
		aliceURL string
		apiKey   string
	}{
		{"no URL", "", testAPIKey},
		{"a URL without a scheme", "alice.example", testAPIKey},
		{"a URL of another scheme", "ftp://alice.example", testAPIKey},
		{"a URL without a host", "http://", testAPIKey},
		{"a URL with a query", "https://alice.example/?key=x", testAPIKey},
		{"no API key", "https://alice.example", ""},
		{"a blank API key", "https://alice.example", "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.New(tc.aliceURL, tc.apiKey)

			equal(t, "refused", err != nil, true)
		})
	}
}

// answering is a proxy to a fake trap server answering with res.
func answering(res trapResponse) func(a arranger) *proxyActor {
	return func(a arranger) *proxyActor {
		trap := a.TrapServer()
		trap.AnswersWith(res)
		return a.Proxy(trap)
	}
}

// relayedHeader is the header the agent got, without the Date the server
// stamps.
func relayedHeader(res *httptest.ResponseRecorder) http.Header {
	header := res.Header().Clone()
	header.Del("Date")
	return header
}

func orDefault(body, def io.ReadCloser) io.ReadCloser {
	if body == nil {
		return def
	}
	return body
}

// timeoutMargin is how much later than its deadline a fail-safe 404 may
// come: scheduling slack on a busy machine, not a guarantee of speed.
const timeoutMargin = time.Second

func TestSpec_Client_TheDefaultTimeoutIsTwoSeconds(t *testing.T) {
	equal(t, "default timeout", client.DefaultTimeout, 2*time.Second)
}

func TestSpec_Client_AHungAliceGetsA404AtTheDeadline(t *testing.T) {
	cases := []struct {
		name     string
		opts     []client.Option
		deadline time.Duration
	}{
		{"the default timeout", nil, 2 * time.Second},
		{"a timeout of its own", []client.Option{client.WithTimeout(300 * time.Millisecond)}, 300 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			proxy := a.ReportingProxy(a.HungAlice(), tc.opts...)

			res, took := proxy.TimedServe(httptest.NewRequest(http.MethodGet, "/.envrc", nil))

			equal(t, "status", res.Code, http.StatusNotFound)
			equal(t, "body", res.Body.String(), "404 page not found\n")
			equal(t, "timed out", errors.Is(proxy.MustReportOnce(), client.ErrTimeout), true)
			atLeast(t, "time taken (ms)", took.Milliseconds(), tc.deadline.Milliseconds())
			atMost(t, "time taken (ms)", took.Milliseconds(), (tc.deadline + timeoutMargin).Milliseconds())
		})
	}
}

func TestSpec_Client_TheDeadlineCoversTheForwardedBody(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	proxy := a.ReportingProxy(trap.server.URL, client.WithTimeout(300*time.Millisecond))
	// A body the agent never finishes sending.
	body, w := io.Pipe()
	defer w.Close()
	go func() { _, _ = w.Write([]byte("the start of an upload")) }()
	req := httptest.NewRequest(http.MethodPost, "/internal/admin/upload", body)

	res, took := proxy.TimedServe(req)

	equal(t, "status", res.Code, http.StatusNotFound)
	equal(t, "timed out", errors.Is(proxy.MustReportOnce(), client.ErrTimeout), true)
	atMost(t, "time taken (ms)", took.Milliseconds(), (300*time.Millisecond + timeoutMargin).Milliseconds())
}

// Each fail-safe 404 calls the error handler once, with an error that names
// its reason and carries neither the key nor the agent's credentials.
func TestSpec_Client_ReportsWhyItFailedSafe(t *testing.T) {
	cases := []struct {
		name   string
		alice  func(a arranger) string
		opts   []client.Option
		body   io.ReadCloser
		gone   bool
		reason error
		status int
		// within, when set, bounds the time the 404 takes.
		within time.Duration
	}{
		// Well under the default deadline: a refused connection is a 404
		// straight away.
		{name: "Alice not listening", alice: arranger.UnreachableAlice, reason: client.ErrUnreachable, within: 500 * time.Millisecond},
		{name: "Alice hung", alice: arranger.HungAlice, opts: []client.Option{client.WithTimeout(100 * time.Millisecond)}, reason: client.ErrTimeout},
		{name: "a 200 without the marker", alice: answeringAt(trapResponse{Status: http.StatusOK}), reason: client.ErrNoMarker, status: http.StatusOK},
		{name: "a 401 without the marker", alice: answeringAt(trapResponse{Status: http.StatusUnauthorized}), reason: client.ErrNoMarker, status: http.StatusUnauthorized},
		{name: "a marker with another value", alice: answeringAt(trapResponse{Status: http.StatusOK, Marker: "0"}), reason: client.ErrNoMarker, status: http.StatusOK},
		{name: "the agent's body failing", alice: answeringAt(trapResponse{Status: http.StatusOK, Marker: "1"}), body: closedBody{}, reason: client.ErrRequest},
		{name: "the agent's body panicking", alice: answeringAt(trapResponse{Status: http.StatusOK, Marker: "1"}), body: io.NopCloser(&panickingReader{}), reason: client.ErrRequest},
		{name: "the agent gone", alice: answeringAt(trapResponse{Status: http.StatusOK, Marker: "1"}), gone: true, reason: client.ErrRequest},
		{name: "a panicking transport", alice: answeringAt(trapResponse{Status: http.StatusOK, Marker: "1"}), opts: []client.Option{client.WithTransport(panickingTransport)}, reason: client.ErrPanic},
		{name: "a transport error quoting the key", alice: answeringAt(trapResponse{Status: http.StatusOK, Marker: "1"}), opts: []client.Option{client.WithTransport(leakyTransport)}, reason: client.ErrUnreachable},
	}
	reasons := []error{client.ErrRequest, client.ErrUnreachable, client.ErrTimeout, client.ErrNoMarker, client.ErrPanic}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			proxy := a.ReportingProxy(tc.alice(a), tc.opts...)
			req := agentRequestWithSecrets()
			req.Body = orDefault(tc.body, req.Body)
			req = withAgentGone(req, tc.gone)

			res, took := proxy.TimedServe(req)

			err := proxy.MustReportOnce()
			var cerr *client.Error
			equal(t, "status", res.Code, http.StatusNotFound)
			if tc.within > 0 {
				atMost(t, "time taken (ms)", took.Milliseconds(), tc.within.Milliseconds())
			}
			equal(t, "an *Error", errors.As(err, &cerr), true)
			for _, reason := range reasons {
				equal(t, "matches "+reason.Error(), errors.Is(err, reason), reason == tc.reason)
			}
			equal(t, "Alice's status", cerr.Status, tc.status)
			for _, secret := range []string{testAPIKey, agentPassword, agentCookie} {
				equal(t, "error text contains "+secret, strings.Contains(err.Error(), secret), false)
			}
		})
	}
}

func TestSpec_Client_ReportsNothingForARelayedResponse(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	proxy := a.ReportingProxy(trap.server.URL)

	res := proxy.Serve(httptest.NewRequest(http.MethodGet, "/.envrc", nil))

	equal(t, "status", res.Code, http.StatusOK)
	equal(t, "reports", len(proxy.Reported()), 0)
}

func TestSpec_Client_RelaysAMarked429Unchanged(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	trap.AnswersWith(trapResponse{
		Status: http.StatusTooManyRequests,
		Header: http.Header{"Retry-After": {"30"}, "Content-Type": {"application/json"}},
		Body:   `{"error":"slow down"}`,
		Marker: "1",
	})
	proxy := a.ReportingProxy(trap.server.URL)

	res := proxy.Serve(httptest.NewRequest(http.MethodGet, "/internal/admin", nil))

	equal(t, "status", res.Code, http.StatusTooManyRequests)
	equal(t, "header", relayedHeader(res), http.Header{
		"Retry-After":    {"30"},
		"Content-Type":   {"application/json"},
		"Content-Length": {strconv.Itoa(len(`{"error":"slow down"}`))},
	})
	equal(t, "body", res.Body.String(), `{"error":"slow down"}`)
	equal(t, "reports", len(proxy.Reported()), 0)
}

// No panic leaves ServeHTTP (the actor fails the spec on one): before
// anything is written the agent gets the 404, and after, the relay just
// stops, with nothing added to what was relayed and nothing reported.
func TestSpec_Client_APanicNeverEscapes(t *testing.T) {
	const notFound = "404 page not found\n"
	panickingHandler := client.WithErrorHandler(func(*http.Request, error) { panic("the handler broke") })
	cases := []struct {
		name string
		opts []client.Option
		// reporting makes the proxy with the recording error handler.
		reporting bool
		status    int
		body      string
		reports   int
	}{
		{"in the transport", []client.Option{client.WithTransport(panickingTransport)}, true, http.StatusNotFound, notFound, 1},
		{"reading the relayed body", []client.Option{client.WithTransport(panickingBodyTransport)}, true, http.StatusOK, relayedBeforeThePanic, 0},
		{"in the error handler", []client.Option{client.WithTransport(panickingTransport), panickingHandler}, false, http.StatusNotFound, notFound, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			var proxy *proxyActor
			if tc.reporting {
				proxy = a.ReportingProxy("http://alice.invalid", tc.opts...)
			} else {
				proxy = a.ProxyTo("http://alice.invalid", tc.opts...)
			}

			res := proxy.Serve(httptest.NewRequest(http.MethodGet, "/.envrc", nil))

			equal(t, "status", res.Code, tc.status)
			equal(t, "body", res.Body.String(), tc.body)
			equal(t, "reports", len(proxy.Reported()), tc.reports)
		})
	}
}

func TestSpec_Client_RefusesABadTimeoutOrProxyPrefix(t *testing.T) {
	cases := []struct {
		name string
		opt  client.Option
		// says is part of the error's text.
		says string
	}{
		{"a zero timeout", client.WithTimeout(0), "timeout"},
		{"a negative timeout", client.WithTimeout(-time.Second), "timeout"},
		{"an invalid prefix", client.WithTrustedProxies(netip.Prefix{}), "invalid"},
		{"an IPv4-mapped prefix", client.WithTrustedProxies(netip.MustParsePrefix("::ffff:10.0.0.0/104")), "use its IPv4 form"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.New("https://alice.example", testAPIKey, tc.opt)

			if err == nil {
				t.Fatal("not refused")
			}
			equal(t, "error says "+tc.says, strings.Contains(err.Error(), tc.says), true)
		})
	}
}

func TestSpec_Client_TakesTheClientIPFromForwardedForOnlyBehindTrustedProxies(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:f::/48")}
	everyone := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	cases := []struct {
		name       string
		trusted    []netip.Prefix
		remoteAddr string
		forwarded  []string
		want       string
	}{
		{"no trusted proxies", nil, "10.0.0.1:5000", []string{"203.0.113.7"}, "10.0.0.1"},
		{"a trusted peer", proxies, "10.0.0.1:5000", []string{"203.0.113.7"}, "203.0.113.7"},
		{"a chain whose right end is trusted", proxies, "10.0.0.1:5000", []string{"198.51.100.9, 10.0.0.2"}, "198.51.100.9"},
		{"a chain spoofed on the left", proxies, "10.0.0.1:5000", []string{"198.51.100.9, 203.0.113.7"}, "203.0.113.7"},
		{"garbage left of the client", proxies, "10.0.0.1:5000", []string{"not-an-ip, 203.0.113.7"}, "203.0.113.7"},
		{"a chain over two lines", proxies, "10.0.0.1:5000", []string{"198.51.100.9", "10.0.0.2"}, "198.51.100.9"},
		{"every entry trusted", proxies, "10.0.0.1:5000", []string{"10.0.0.3, 10.0.0.2"}, "10.0.0.3"},
		{"an untrusted peer", proxies, "203.0.113.50:5000", []string{"198.51.100.9"}, "203.0.113.50"},
		{"no header", proxies, "10.0.0.1:5000", nil, "10.0.0.1"},
		{"an empty header", proxies, "10.0.0.1:5000", []string{""}, "10.0.0.1"},
		{"a malformed entry", proxies, "10.0.0.1:5000", []string{"not-an-ip"}, "10.0.0.1"},
		{"an empty entry", proxies, "10.0.0.1:5000", []string{"203.0.113.7, "}, "10.0.0.1"},
		{"an entry with a port", proxies, "10.0.0.1:5000", []string{"203.0.113.7:4711"}, "10.0.0.1"},
		{"a malformed entry right of the client", proxies, "10.0.0.1:5000", []string{"203.0.113.7, bogus, 10.0.0.2"}, "10.0.0.1"},
		{"IPv6 behind a trusted IPv6 peer", proxies, "[2001:db8:f::1]:443", []string{"2001:db8::7"}, "2001:db8::7"},
		{"an IPv4-mapped trusted peer", proxies, "[::ffff:10.0.0.1]:443", []string{"203.0.113.7"}, "203.0.113.7"},
		{"a trusted peer with a zone", []netip.Prefix{netip.MustParsePrefix("fe80::/10")}, "[fe80::1%eth0]:443", []string{"203.0.113.7"}, "203.0.113.7"},
		{"a peer without a port", proxies, "10.0.0.1", []string{"203.0.113.7"}, "203.0.113.7"},
		{"every peer trusted", everyone, "203.0.113.50:5000", []string{"198.51.100.9, 203.0.113.8"}, "198.51.100.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			trap := a.TrapServer()
			req := httptest.NewRequest(http.MethodGet, "/.envrc", nil)
			req.RemoteAddr = tc.remoteAddr
			req.Header["X-Forwarded-For"] = tc.forwarded

			a.ReportingProxy(trap.server.URL, client.WithTrustedProxies(tc.trusted...)).Serve(req)

			got := trap.MustLastRequest().Head
			equal(t, "client IP", got.ClientIP, tc.want)
			equal(t, "forwarded header", got.Header["X-Forwarded-For"], tc.forwarded)
		})
	}
}

// answeringAt is the URL of a fake trap server answering with res.
func answeringAt(res trapResponse) func(a arranger) string {
	return func(a arranger) string {
		trap := a.TrapServer()
		trap.AnswersWith(res)
		return trap.server.URL
	}
}

// The agent's own credentials and cookie, which no reported error may
// carry.
const (
	agentPassword = "agent-secret-password"
	agentCookie   = "agent-session-cookie"
)

// agentRequestWithSecrets is a bait request carrying the agent's
// credentials and cookie.
func agentRequestWithSecrets() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/.envrc?token="+agentPassword, strings.NewReader("x"))
	req.SetBasicAuth("admin", agentPassword)
	req.Header.Set("Cookie", "session="+agentCookie)
	return req
}

// withAgentGone is req, its context already cancelled when gone, as for an
// agent that hung up.
func withAgentGone(req *http.Request, gone bool) *http.Request {
	if !gone {
		return req
	}
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	return req.WithContext(ctx)
}
