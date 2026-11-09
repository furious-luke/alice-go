package client_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

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
		"Cookie":        {"alice_agent=abc"},
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
		"Cookie":        {"alice_agent=abc"},
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
	req.Header.Set("Cookie", "alice_agent=abc")
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
			"Set-Cookie":   {"alice_agent=abc; Path=/; Max-Age=31536000; HttpOnly; SameSite=Lax"},
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
		"Set-Cookie":     {"alice_agent=abc; Path=/; Max-Age=31536000; HttpOnly; SameSite=Lax"},
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
