package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	client "github.com/furious-luke/alice-go"
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

// A relayed answer whose length Alice states is framed by the site's own
// server with that Content-Length, whatever its size, as the site frames
// its own pages: never chunked. One whose length isn't known still relays
// its whole body. A HEAD states the length a GET has and relays no body;
// a 204 relays neither.
func TestSpec_Client_FramesARelayedAnswerWithItsLength(t *testing.T) {
	large := strings.Repeat("<p>a page well over the server's buffer</p>\n", 400)
	cases := []struct {
		name          string
		method        string
		status        int
		header        http.Header
		body          string
		wantLength    int64
		wantBody      string
		wantNoChunked bool
	}{
		{"a large page of known length", http.MethodGet, http.StatusOK, http.Header{"Content-Length": {strconv.Itoa(len(large))}}, large, int64(len(large)), large, true},
		{"a small answer of known length", http.MethodPost, http.StatusUnprocessableEntity, http.Header{"Content-Length": {"2"}}, "{}", 2, "{}", true},
		{"a large page of unknown length", http.MethodGet, http.StatusOK, nil, large, -1, large, false},
		{"a HEAD, with the length a GET has", http.MethodHead, http.StatusOK, http.Header{"Content-Length": {strconv.Itoa(len(large))}}, large, int64(len(large)), "", true},
		{"a 204", http.MethodGet, http.StatusNoContent, nil, "", -2, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			trap := a.TrapServer()
			header := http.Header{"Content-Type": {"text/html; charset=utf-8"}}
			for name, values := range tc.header {
				header[name] = values
			}
			trap.AnswersWith(trapResponse{Status: tc.status, Header: header, Body: tc.body, Marker: "1"})

			res := a.Agent(a.Proxy(trap)).MustFetch(tc.method, "/internal/admin/payroll")

			equal(t, "status", res.Status, tc.status)
			equal(t, "body", res.Body, tc.wantBody)
			if tc.wantNoChunked {
				equal(t, "transfer encoding", len(res.TransferEncoding), 0)
			}
			if tc.wantLength >= -1 {
				equal(t, "content length", res.ContentLength, tc.wantLength)
			}
		})
	}
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
			name:  "a client-closed 499 without the marker",
			proxy: answering(trapResponse{Status: 499, Body: refusal}),
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

func TestSpec_Client_ForwardsToTheDefaultAliceWithoutWithURL(t *testing.T) {
	var got string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.URL.String()
		return nil, errors.New("not sent")
	})
	h, err := client.New(testAPIKey, client.WithTransport(transport))
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/.envrc", nil)
	req.RemoteAddr = "203.0.113.7:51234"

	h.ServeHTTP(httptest.NewRecorder(), req)

	equal(t, "default URL", client.DefaultURL, "https://alice.coragents.ai")
	equal(t, "trap URL", got, "https://alice.coragents.ai"+client.TrapPath)
}

func TestSpec_Client_RefusesABadConfiguration(t *testing.T) {
	cases := []struct {
		name     string
		aliceURL string
		apiKey   string
	}{
		{"an empty URL", "", testAPIKey},
		{"a URL without a scheme", "alice.example", testAPIKey},
		{"a URL of another scheme", "ftp://alice.example", testAPIKey},
		{"a URL without a host", "http://", testAPIKey},
		{"a URL with a query", "https://alice.example/?key=x", testAPIKey},
		{"no API key", "https://alice.example", ""},
		{"a blank API key", "https://alice.example", "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.New(tc.apiKey, client.WithURL(tc.aliceURL))

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
		{name: "a 499 without the marker", alice: answeringAt(trapResponse{Status: 499}), reason: client.ErrNoMarker, status: 499},
		{name: "a 500 without the marker", alice: answeringAt(trapResponse{Status: http.StatusInternalServerError}), reason: client.ErrNoMarker, status: http.StatusInternalServerError},
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
			_, err := client.New(testAPIKey, tc.opt)

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

func TestSpec_Client_TheAgentCookieIsVid(t *testing.T) {
	equal(t, "agent cookie name", client.AgentCookieName, "_vid")
}

// The site's own cookie is taken out of the forwarded Cookie header, name
// and value, and nothing of it is anywhere in the head; the agent's other
// cookies, its credentials and Alice's agent cookie go through as sent.
func TestSpec_Client_DropsTheSitesOwnCookie(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	req := httptest.NewRequest(http.MethodGet, "/internal/admin", nil)
	req.Header.Set("Cookie", "shop_session=staff-token-7f3a; planted=attacker-value-91; _vid=agent-cookie-id")
	req.Header.Set("Authorization", "Basic YWRtaW46c2VjcmV0")

	a.ProxyTo(trap.server.URL, client.WithDroppedCookies("shop_session")).Serve(req)

	got := trap.MustLastRequest()
	equal(t, "cookies", got.Head.Header.Values("Cookie"), []string{"planted=attacker-value-91; _vid=agent-cookie-id"})
	equal(t, "authorization", got.Head.Header.Values("Authorization"), []string{"Basic YWRtaW46c2VjcmV0"})
	head, err := json.Marshal(got.Head)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "the head names the dropped cookie", strings.Contains(string(head), "shop_session"), false)
	equal(t, "the head carries its value", strings.Contains(string(head), "staff-token-7f3a"), false)
}

// Edge and malformed Cookie input, with shop_session and __Host-csrf
// dropped: every line is read, every pair named exactly as a dropped
// cookie is removed, and everything else is kept byte for byte.
func TestSpec_Client_DropsCookiesFromEveryShapeOfCookieHeader(t *testing.T) {
	many := strings.Repeat("shop_session=t; planted=v; ", 20000)
	cases := []struct {
		name string
		in   http.Header
		// want is the forwarded header, less Host.
		want http.Header
	}{
		{"several Cookie lines",
			http.Header{"Cookie": {"shop_session=t1", "planted=v", "a=1; shop_session=t2"}},
			http.Header{"Cookie": {"planted=v", "a=1"}}},
		{"a pair with no =",
			http.Header{"Cookie": {"flag; shop_session=t; other"}},
			http.Header{"Cookie": {"flag; other"}}},
		{"a dropped name with no =",
			http.Header{"Cookie": {"shop_session; planted=v"}},
			http.Header{"Cookie": {"planted=v"}}},
		{"empty pairs",
			http.Header{"Cookie": {"a=1;;shop_session=t;; b=2"}},
			http.Header{"Cookie": {"a=1;;; b=2"}}},
		{"a dropped name twice",
			http.Header{"Cookie": {"shop_session=a; planted=v; shop_session=b"}},
			http.Header{"Cookie": {"planted=v"}}},
		{"names that differ only in case",
			http.Header{"Cookie": {"Shop_Session=t; SHOP_SESSION=u; __host-csrf=c"}},
			http.Header{"Cookie": {"Shop_Session=t; SHOP_SESSION=u; __host-csrf=c"}}},
		{"a prefixed name, and another prefix",
			http.Header{"Cookie": {"__Host-csrf=x; __Secure-csrf=y; csrf=z"}},
			http.Header{"Cookie": {"__Secure-csrf=y; csrf=z"}}},
		{"a quoted value",
			http.Header{"Cookie": {`planted="a b"; shop_session="t"`}},
			http.Header{"Cookie": {`planted="a b"`}}},
		{"odd spacing",
			http.Header{"Cookie": {"  planted=v ;  shop_session = t ;\tb=2  "}},
			http.Header{"Cookie": {"  planted=v ;\tb=2  "}}},
		{"the dropped name as a value",
			http.Header{"Cookie": {"planted=shop_session"}},
			http.Header{"Cookie": {"planted=shop_session"}}},
		{"non-ASCII pairs",
			http.Header{"Cookie": {"café=1; shop_session=t; ü=é"}},
			http.Header{"Cookie": {"café=1; ü=é"}}},
		{"a line that becomes empty",
			http.Header{"Cookie": {"shop_session=t; ;", "planted=v"}},
			http.Header{"Cookie": {"planted=v"}}},
		{"every line becomes empty",
			http.Header{"Cookie": {"shop_session=t", " __Host-csrf=c ;"}, "X-Kept": {"1"}},
			http.Header{"X-Kept": {"1"}}},
		{"a malformed line without a dropped pair",
			http.Header{"Cookie": {"=; ;x", ""}},
			http.Header{"Cookie": {"=; ;x", ""}}},
		{"a header map keyed in lower case",
			http.Header{"cookie": {"shop_session=t; a=1"}},
			http.Header{"cookie": {"a=1"}}},
		{"a long line",
			http.Header{"Cookie": {many + "_vid=id"}},
			http.Header{"Cookie": {strings.Repeat("planted=v; ", 20000) + "_vid=id"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			trap := a.TrapServer()
			req := httptest.NewRequest(http.MethodGet, "/.envrc", nil)
			req.Header = tc.in

			a.ProxyTo(trap.server.URL, client.WithDroppedCookies("shop_session", "__Host-csrf")).Serve(req)

			got := trap.MustLastRequest().Head.Header
			got.Del("Host")
			equal(t, "header", got, tc.want)
		})
	}
}

// Each WithDroppedCookies and WithDroppedHeaders adds to the lists the
// earlier ones gave.
func TestSpec_Client_DropListsAddUp(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	req := httptest.NewRequest(http.MethodGet, "/.envrc", nil)
	req.Header = http.Header{
		"Cookie":               {"one=1; two=2; three=3"},
		"X-Auth-Request-Email": {"staff@shop.example"},
		"X-Internal-Session":   {"s"},
		"X-Kept":               {"yes"},
	}

	a.ProxyTo(trap.server.URL,
		client.WithDroppedCookies("one"), client.WithDroppedCookies("three"),
		client.WithDroppedHeaders("X-Auth-Request-Email"), client.WithDroppedHeaders("X-Internal-Session"),
	).Serve(req)

	got := trap.MustLastRequest().Head.Header
	got.Del("Host")
	equal(t, "header", got, http.Header{"Cookie": {"two=2"}, "X-Kept": {"yes"}})
}

// A dropped header is removed however its name is cased, in the option or
// in the request; Authorization, the cookies and every header not listed
// are forwarded as sent.
func TestSpec_Client_DropsTheNamedHeadersInAnyCase(t *testing.T) {
	cases := []struct {
		name   string
		option string
		in     http.Header
	}{
		{"as given", "X-Auth-Request-Email", http.Header{"X-Auth-Request-Email": {"staff@shop.example"}}},
		{"an option in another case", "x-AUTH-request-EMAIL", http.Header{"X-Auth-Request-Email": {"staff@shop.example"}}},
		{"a header map keyed in lower case", "X-Auth-Request-Email", http.Header{"x-auth-request-email": {"staff@shop.example"}}},
		{"several lines", "X-Auth-Request-Email", http.Header{"X-Auth-Request-Email": {"a@shop.example", "b@shop.example"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			trap := a.TrapServer()
			req := httptest.NewRequest(http.MethodGet, "/internal/admin", nil)
			req.Header = tc.in.Clone()
			req.Header["Authorization"] = []string{"Basic YWRtaW46c2VjcmV0"}
			req.Header["Cookie"] = []string{"_vid=abc; planted=v"}
			req.Header["X-Auth-Request-User"] = []string{"staff"}

			a.ProxyTo(trap.server.URL, client.WithDroppedHeaders(tc.option)).Serve(req)

			got := trap.MustLastRequest().Head.Header
			got.Del("Host")
			equal(t, "header", got, http.Header{
				"Authorization":       {"Basic YWRtaW46c2VjcmV0"},
				"Cookie":              {"_vid=abc; planted=v"},
				"X-Auth-Request-User": {"staff"},
			})
		})
	}
}

// The agent's request is never changed: after the handler, and in the
// error handler, its headers are as received, dropped cookies and headers
// included.
func TestSpec_Client_LeavesTheAgentsRequestAsReceived(t *testing.T) {
	received := http.Header{
		"Cookie":               {"shop_session=t; planted=v", "shop_session=u"},
		"X-Auth-Request-Email": {"staff@shop.example"},
		"Authorization":        {"Basic YWRtaW46c2VjcmV0"},
	}
	drops := []client.Option{client.WithDroppedCookies("shop_session"), client.WithDroppedHeaders("X-Auth-Request-Email")}
	t.Run("relayed", func(t *testing.T) {
		a := newArranger(t)
		trap := a.TrapServer()
		req := httptest.NewRequest(http.MethodGet, "/.envrc", nil)
		req.Header = received.Clone()

		a.ProxyTo(trap.server.URL, drops...).Serve(req)

		equal(t, "the request's header", req.Header, received)
		equal(t, "forwarded cookies", trap.MustLastRequest().Head.Header.Values("Cookie"), []string{"planted=v"})
	})
	t.Run("failed safe", func(t *testing.T) {
		a := newArranger(t)
		proxy := a.ReportingProxy(a.UnreachableAlice(), drops...)
		req := httptest.NewRequest(http.MethodGet, "/.envrc", nil)
		req.Header = received.Clone()

		proxy.Serve(req)

		equal(t, "the header the error handler saw", proxy.MustReportedOnce().Header, received)
		equal(t, "the request's header", req.Header, received)
	})
}

// New refuses a drop it can't honour, each with an error saying what is
// wrong and never the API key.
func TestSpec_Client_RefusesABadDrop(t *testing.T) {
	cases := []struct {
		name string
		opts []client.Option
		// says is part of the error's text.
		says string
	}{
		{"an empty cookie name", []client.Option{client.WithDroppedCookies("")}, "cookie name is empty"},
		{"a cookie name with a space", []client.Option{client.WithDroppedCookies("shop session")}, "isn't a valid cookie name"},
		{"a cookie name with =", []client.Option{client.WithDroppedCookies("shop=session")}, "isn't a valid cookie name"},
		{"a cookie name with ;", []client.Option{client.WithDroppedCookies("a;b")}, "isn't a valid cookie name"},
		{"a non-ASCII cookie name", []client.Option{client.WithDroppedCookies("café")}, "isn't a valid cookie name"},
		{"a cookie name carrying the key", []client.Option{client.WithDroppedCookies(testAPIKey + " x")}, "isn't a valid cookie name"},
		{"the agent cookie", []client.Option{client.WithDroppedCookies("shop_session", "_vid")}, "_vid is Alice's agent cookie"},
		{"the agent cookie in a later option", []client.Option{client.WithDroppedCookies("a"), client.WithDroppedCookies("_vid")}, "_vid is Alice's agent cookie"},
		{"an empty header name", []client.Option{client.WithDroppedHeaders("")}, "header name is empty"},
		{"a header name with a space", []client.Option{client.WithDroppedHeaders("X Auth")}, "isn't a valid header name"},
		{"a header name with :", []client.Option{client.WithDroppedHeaders("X-Auth:")}, "isn't a valid header name"},
		{"a header name carrying the key", []client.Option{client.WithDroppedHeaders(testAPIKey + ":")}, "isn't a valid header name"},
		{"Authorization", []client.Option{client.WithDroppedHeaders("Authorization")}, "Authorization header can't be dropped"},
		{"authorization", []client.Option{client.WithDroppedHeaders("authorization")}, "Authorization header can't be dropped"},
		{"Cookie", []client.Option{client.WithDroppedHeaders("Cookie")}, "Cookie header can't be dropped"},
		{"COOKIE", []client.Option{client.WithDroppedHeaders("X-Ok", "COOKIE")}, "Cookie header can't be dropped"},
		{"Host", []client.Option{client.WithDroppedHeaders("Host")}, "Host header can't be dropped"},
		{"host", []client.Option{client.WithDroppedHeaders("host")}, "Host header can't be dropped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.New(testAPIKey, tc.opts...)

			if err == nil {
				t.Fatal("not refused")
			}
			equal(t, "error says "+tc.says, strings.Contains(err.Error(), tc.says), true)
			equal(t, "error carries the key", strings.Contains(err.Error(), testAPIKey), false)
		})
	}
}

// With WithClientIP, the client IP is the one the site's function gives,
// as an address or an address and port, unmapped and without a zone,
// whatever RemoteAddr and X-Forwarded-For say. X-Forwarded-For is still
// forwarded as a header.
func TestSpec_Client_TakesTheClientIPFromTheSitesFunction(t *testing.T) {
	cases := []struct {
		name  string
		given string
		want  string
	}{
		{"an IPv4 address", "198.51.100.9", "198.51.100.9"},
		{"an IPv4 address and port", "198.51.100.9:4711", "198.51.100.9"},
		{"an IPv6 address and port", "[2001:db8::1]:443", "2001:db8::1"},
		{"an IPv6 address", "2001:db8::1", "2001:db8::1"},
		{"an IPv4-mapped address", "::ffff:198.51.100.9", "198.51.100.9"},
		{"an IPv4-mapped address and port", "[::ffff:198.51.100.9]:80", "198.51.100.9"},
		{"an address with a zone", "fe80::1%eth0", "fe80::1"},
		{"an address with a zone and port", "[fe80::1%eth0]:443", "fe80::1"},
		{"an address with spaces around it", " 198.51.100.9 ", "198.51.100.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			trap := a.TrapServer()
			req := httptest.NewRequest(http.MethodGet, "/.env", nil)
			req.RemoteAddr = "10.0.0.1:5000"
			req.Header.Set("X-Forwarded-For", "203.0.113.7")
			proxy := a.ReportingProxy(trap.server.URL, client.WithClientIP(func(*http.Request) string { return tc.given }))

			res := proxy.Serve(req)

			got := trap.MustLastRequest().Head
			equal(t, "status", res.Code, http.StatusOK)
			equal(t, "client IP", got.ClientIP, tc.want)
			equal(t, "forwarded header", got.Header["X-Forwarded-For"], []string{"203.0.113.7"})
			equal(t, "reports", len(proxy.Reported()), 0)
		})
	}
}

// The site's function is called once for each request, with the agent's
// request.
func TestSpec_Client_CallsTheClientIPFunctionOncePerRequest(t *testing.T) {
	a := newArranger(t)
	trap := a.TrapServer()
	var seen []string
	proxy := a.ProxyTo(trap.server.URL, client.WithClientIP(func(r *http.Request) string {
		seen = append(seen, r.URL.Path)
		return r.Header.Get("X-Site-Client")
	}))
	for _, ip := range []string{"198.51.100.1", "198.51.100.2"} {
		req := httptest.NewRequest(http.MethodGet, "/.env", nil)
		req.Header.Set("X-Site-Client", ip)
		proxy.Serve(req)
	}

	requests := trap.Requests()
	equal(t, "calls", seen, []string{"/.env", "/.env"})
	equal(t, "trap requests", len(requests), 2)
	equal(t, "first client IP", requests[0].Head.ClientIP, "198.51.100.1")
	equal(t, "second client IP", requests[1].Head.ClientIP, "198.51.100.2")
}

// When the site's function gives nothing usable, or panics, the client IP
// is RemoteAddr's, as without the option: the request is still forwarded,
// Alice's answer is relayed and no error is reported.
func TestSpec_Client_FallsBackToRemoteAddrWhenTheClientIPFunctionFails(t *testing.T) {
	returning := func(s string) func(*http.Request) string { return func(*http.Request) string { return s } }
	cases := []struct {
		name string
		fn   func(*http.Request) string
	}{
		{"an empty result", returning("")},
		{"only spaces", returning("  ")},
		{"not an IP", returning("not-an-ip")},
		{"a host name and port", returning("shop.example:443")},
		{"an empty port", returning("198.51.100.9:")},
		{"a port out of range", returning("198.51.100.9:99999")},
		{"a bracketed address without a port", returning("[2001:db8::1]")},
		{"a list", returning("198.51.100.9, 203.0.113.8")},
		{"a panic", func(*http.Request) string { panic("the site's code broke") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newArranger(t)
			trap := a.TrapServer()
			trap.AnswersWith(trapResponse{Status: http.StatusOK, Body: "bait", Marker: "1"})
			req := httptest.NewRequest(http.MethodGet, "/.env", nil)
			req.RemoteAddr = "203.0.113.7:51234"
			req.Header.Set("X-Forwarded-For", "198.51.100.50")
			proxy := a.ReportingProxy(trap.server.URL, client.WithClientIP(tc.fn))

			res := proxy.Serve(req)

			equal(t, "status", res.Code, http.StatusOK)
			equal(t, "body", res.Body.String(), "bait")
			equal(t, "client IP", trap.MustLastRequest().Head.ClientIP, "203.0.113.7")
			equal(t, "reports", len(proxy.Reported()), 0)
		})
	}
}

// New refuses a nil client-IP function, and WithClientIP together with
// WithTrustedProxies, in either order and even with no prefixes, with an
// error naming the problem.
func TestSpec_Client_RefusesABadClientIPOption(t *testing.T) {
	fn := func(*http.Request) string { return "198.51.100.9" }
	proxies := client.WithTrustedProxies(netip.MustParsePrefix("10.0.0.0/8"))
	both := "WithClientIP and WithTrustedProxies can't be given together"
	cases := []struct {
		name string
		opts []client.Option
		says string
	}{
		{"a nil function", []client.Option{client.WithClientIP(nil)}, "WithClientIP's function is nil"},
		{"WithClientIP then WithTrustedProxies", []client.Option{client.WithClientIP(fn), proxies}, both},
		{"WithTrustedProxies then WithClientIP", []client.Option{proxies, client.WithClientIP(fn)}, both},
		{"WithClientIP and an empty WithTrustedProxies", []client.Option{client.WithTrustedProxies(), client.WithClientIP(fn)}, both},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.New(testAPIKey, tc.opts...)

			if err == nil {
				t.Fatal("not refused")
			}
			equal(t, "error says "+tc.says, strings.Contains(err.Error(), tc.says), true)
		})
	}

	t.Run("the control: WithClientIP alone", func(t *testing.T) {
		_, err := client.New(testAPIKey, client.WithClientIP(fn))
		equal(t, "error", err, nil)
	})
}
