package client_test

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// proxyActor is the client's handler, served requests directly.
type proxyActor struct {
	t       *testing.T
	handler http.Handler
}

// Serve hands req to the handler and returns what the agent would get. A
// panic in the handler fails the spec.
func (p *proxyActor) Serve(req *http.Request) *httptest.ResponseRecorder {
	p.t.Helper()
	rec := httptest.NewRecorder()
	defer func() {
		if v := recover(); v != nil {
			p.t.Fatalf("the handler panicked: %v", v)
		}
	}()
	p.handler.ServeHTTP(rec, req)
	return rec
}

// agentActor is an agent reaching the proxy over the network.
type agentActor struct {
	t   *testing.T
	url string
}

// upload is what an agent sent in a streamed upload, and what it got back.
type upload struct {
	Status   int
	BodySize int64
	BodySHA  [sha256.Size]byte
	// TrapReceivedBeforeEnd is how many bytes of the body the trap server
	// had received before the agent sent the end of it.
	TrapReceivedBeforeEnd int64
}

// streamStage is how much of an upload the agent sends before it waits for
// the trap server to have received some of it.
const streamStage = 1 << 20

// MustUploadInStages POSTs a multipart form with a fileSize-byte file to
// path, streamed. It sends the first part of the body, then waits until
// trap has received at least half of that before it sends the rest: an
// upload the client held back until the agent finished would stall here,
// and fail the spec.
func (a *agentActor) MustUploadInStages(path string, fileSize int64, trap *fakeTrap) upload {
	a.t.Helper()
	pr, pw := io.Pipe()
	// Ends the request if the spec fails part way through the upload.
	defer pw.Close()
	sum := sha256.New()
	// The multipart writer writes through the counter, so the hash and size
	// are of the exact bytes the agent sent.
	counter := &countingWriter{w: pw, hash: sum}
	form := multipart.NewWriter(counter)
	contentType := form.FormDataContentType()
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, a.url+path, pr)
		if err != nil {
			done <- result{err: err}
			return
		}
		req.Header.Set("Content-Type", contentType)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			pr.CloseWithError(err)
			done <- result{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		done <- result{status: res.StatusCode}
	}()

	if err := form.WriteField("note", "a large upload"); err != nil {
		a.t.Fatalf("write field: %v", err)
	}
	file, err := form.CreateFormFile("file", "big.bin")
	if err != nil {
		a.t.Fatalf("create form file: %v", err)
	}
	a.mustCopyRandom(file, streamStage)

	receivedBeforeEnd := a.mustWaitForTrap(trap, streamStage/2)

	a.mustCopyRandom(file, fileSize-streamStage)
	if err := form.Close(); err != nil {
		a.t.Fatalf("close form: %v", err)
	}
	pw.Close()

	var res result
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		a.t.Fatal("the upload got no response")
	}
	if res.err != nil {
		a.t.Fatalf("upload: %v", res.err)
	}
	up := upload{Status: res.status, BodySize: counter.n, TrapReceivedBeforeEnd: receivedBeforeEnd}
	copy(up.BodySHA[:], sum.Sum(nil))
	return up
}

func (a *agentActor) mustCopyRandom(w io.Writer, n int64) {
	a.t.Helper()
	if _, err := io.CopyN(w, rand.Reader, n); err != nil {
		a.t.Fatalf("write the upload: %v", err)
	}
}

// mustWaitForTrap waits until trap has received at least n bytes of a
// forwarded body, and returns how many it had.
func (a *agentActor) mustWaitForTrap(trap *fakeTrap, n int64) int64 {
	a.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case got := <-trap.received:
			if got >= n {
				return got
			}
		case <-timeout:
			a.t.Fatalf("the trap server had not received %d bytes of the body before the agent finished sending it", n)
			return 0
		}
	}
}

type countingWriter struct {
	w    io.Writer
	hash hash.Hash
	n    int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.hash.Write(p[:n])
	c.n += int64(n)
	return n, err
}

// closedBody is a request body that has been closed: every read fails, as
// a server's request body does after Close.
type closedBody struct{}

func (closedBody) Read([]byte) (int, error) {
	return 0, fmt.Errorf("http: invalid Read on closed Body")
}

func (closedBody) Close() error { return nil }

// equal fails the spec unless got equals want.
func equal[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got: %#v\nwant: %#v", what, got, want)
	}
}

// atLeast fails the spec unless got is at least min.
func atLeast(t *testing.T, what string, got, min int64) {
	t.Helper()
	if got < min {
		t.Errorf("%s: got %d, want at least %d", what, got, min)
	}
}
