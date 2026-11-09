package client_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/furious-luke/go-projects/alice/client"
)

// arranger builds the worlds the client's specs run in: a fake trap server
// speaking Alice's trap protocol, the client pointed at it, and an agent
// talking to the client over the network.
type arranger interface {
	// TrapServer is a fake trap server that answers every trap request with
	// a 200 and the trap marker, until told otherwise.
	TrapServer() *fakeTrap
	// Proxy is the client's handler, forwarding to trap.
	Proxy(trap *fakeTrap) *proxyActor
	// ProxyTo is the client's handler made with opts, forwarding to the
	// Alice server at aliceURL.
	ProxyTo(aliceURL string, opts ...client.Option) *proxyActor
	// ReportingProxy is the client's handler made with opts, forwarding to
	// the Alice server at aliceURL, with an error handler that records
	// every error it is called with.
	ReportingProxy(aliceURL string, opts ...client.Option) *proxyActor
	// UnreachableAlice is the URL of an Alice server that is not listening.
	UnreachableAlice() string
	// HungAlice is the URL of an Alice server that accepts connections and
	// never answers, as a paused one does.
	HungAlice() string
	// Agent is an agent that reaches the proxy over the network.
	Agent(proxy *proxyActor) *agentActor
}

type defaultArranger struct {
	t *testing.T
}

func newArranger(t *testing.T) arranger {
	t.Helper()
	return &defaultArranger{t: t}
}

const testAPIKey = "alice_testkey"

func (a *defaultArranger) TrapServer() *fakeTrap {
	f := &fakeTrap{
		t:        a.t,
		response: trapResponse{Status: http.StatusOK, Marker: "1"},
		received: make(chan int64, 1024),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	a.t.Cleanup(f.server.Close)
	return f
}

func (a *defaultArranger) Proxy(trap *fakeTrap) *proxyActor {
	return a.ProxyTo(trap.server.URL)
}

func (a *defaultArranger) ProxyTo(aliceURL string, opts ...client.Option) *proxyActor {
	a.t.Helper()
	h, err := client.New(aliceURL, testAPIKey, opts...)
	if err != nil {
		a.t.Fatalf("client.New(%q): %v", aliceURL, err)
	}
	return &proxyActor{t: a.t, handler: h}
}

func (a *defaultArranger) ReportingProxy(aliceURL string, opts ...client.Option) *proxyActor {
	a.t.Helper()
	p := &proxyActor{t: a.t}
	opts = append(opts, client.WithErrorHandler(p.record))
	h, err := client.New(aliceURL, testAPIKey, opts...)
	if err != nil {
		a.t.Fatalf("client.New(%q): %v", aliceURL, err)
	}
	p.handler = h
	return p
}

func (a *defaultArranger) HungAlice() string {
	a.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		a.t.Fatalf("listen: %v", err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Held open, never read or answered.
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	a.t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	return "http://" + ln.Addr().String()
}

func (a *defaultArranger) UnreachableAlice() string {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	return url
}

func (a *defaultArranger) Agent(proxy *proxyActor) *agentActor {
	server := httptest.NewServer(proxy.handler)
	a.t.Cleanup(server.Close)
	return &agentActor{t: a.t, url: server.URL}
}

// trapResponse is what the fake trap server answers with. Marker is the
// trap marker's value; empty sends no marker.
type trapResponse struct {
	Status int
	Header http.Header
	Body   string
	Marker string
}

// trapHead is the head line of a trap request, as the fake decodes it.
type trapHead struct {
	Method   string      `json:"method"`
	Target   string      `json:"target"`
	Header   http.Header `json:"header"`
	ClientIP string      `json:"client_ip"`
}

// trapRequest is one trap request as the fake trap server received it.
type trapRequest struct {
	// TrapHeader is the trap request's own header.
	TrapHeader http.Header
	Head       trapHead
	// Body is the forwarded body; BodySize and BodySHA describe it too, and
	// Body is kept only up to 64 KiB.
	Body     string
	BodySize int64
	BodySHA  [sha256.Size]byte
}

// fakeTrap is a fake of alice-srv's trap API: it reads the head and streams
// the forwarded body, records them, and answers with its response.
type fakeTrap struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	response trapResponse
	requests []trapRequest
	// received gets the running count of forwarded body bytes as they
	// arrive.
	received chan int64
}

// AnswersWith makes the fake answer every trap request with res.
func (f *fakeTrap) AnswersWith(res trapResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.response = res
}

// Requests are the trap requests the fake has answered.
func (f *fakeTrap) Requests() []trapRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]trapRequest(nil), f.requests...)
}

// MustLastRequest is the last trap request the fake answered.
func (f *fakeTrap) MustLastRequest() trapRequest {
	f.t.Helper()
	requests := f.Requests()
	if len(requests) == 0 {
		f.t.Fatal("the fake trap server got no trap request")
	}
	return requests[len(requests)-1]
}

func (f *fakeTrap) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != client.TrapPath {
		http.Error(w, "not the trap route", http.StatusNotFound)
		return
	}
	body := bufio.NewReader(r.Body)
	line, err := body.ReadBytes('\n')
	if err != nil {
		http.Error(w, "no head line", http.StatusBadRequest)
		return
	}
	req := trapRequest{TrapHeader: r.Header.Clone()}
	if err := json.Unmarshal(line, &req.Head); err != nil {
		http.Error(w, "bad head", http.StatusBadRequest)
		return
	}
	hash := sha256.New()
	kept := &limitedBuffer{limit: 64 << 10}
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
			kept.Write(buf[:n])
			req.BodySize += int64(n)
			select {
			case f.received <- req.BodySize:
			default:
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			// The real trap API refuses a body it can't read without the
			// marker.
			http.Error(w, "the forwarded body could not be read", http.StatusBadRequest)
			return
		}
	}
	copy(req.BodySHA[:], hash.Sum(nil))
	req.Body = string(kept.data)

	f.mu.Lock()
	f.requests = append(f.requests, req)
	res := f.response
	f.mu.Unlock()

	for name, values := range res.Header {
		w.Header()[name] = append([]string(nil), values...)
	}
	if res.Marker != "" {
		w.Header().Set(client.TrapMarkerHeader, res.Marker)
	}
	w.WriteHeader(res.Status)
	_, _ = io.WriteString(w, res.Body)
}

type limitedBuffer struct {
	limit int
	data  []byte
}

func (b *limitedBuffer) Write(p []byte) {
	if room := b.limit - len(b.data); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.data = append(b.data, p...)
	}
}
