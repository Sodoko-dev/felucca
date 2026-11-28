package agentclient

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/ingress"
)

// The port quarantine is sized on the longest a worker call can still be under
// way; the client must time out at exactly that.
func TestClientTimeoutIsTheWorkerCallTimeout(t *testing.T) {
	if httpClient.Timeout != ingress.WorkerCallTimeout {
		t.Fatalf("agent client timeout %v, ingress.WorkerCallTimeout %v", httpClient.Timeout, ingress.WorkerCallTimeout)
	}
}

func serveBytes(t *testing.T, n int) (string, uint16) {
	t.Helper()
	body := bytes.Repeat([]byte("a"), n)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return SplitHostPort(ts.URL)
}

func TestRequestReadsAtMostMaxBody(t *testing.T) {
	host, port := serveBytes(t, MaxBody)
	resp, err := Request(host, port, http.MethodGet, "/v1/vms", nil, "", "")
	if err != nil || len(resp.Body) != MaxBody {
		t.Fatalf("answer of exactly MaxBody: err %v", err)
	}
	host, port = serveBytes(t, MaxBody+1)
	if _, err := Request(host, port, http.MethodGet, "/v1/vms", nil, "", ""); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("answer of MaxBody+1: err %v, want an over-long answer error", err)
	}
}

// RequestWithin ends a call at its own limit, well before the client's 30 s.
func TestRequestWithinEndsAtItsLimit(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); ts.Close() })
	host, port := SplitHostPort(ts.URL)
	start := time.Now()
	_, err := RequestWithin(host, port, http.MethodDelete, "/v1/vms/x/expose", []byte(`{}`), "", "", 300*time.Millisecond)
	if err == nil {
		t.Fatal("a call to a worker that never answers succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the call took %v, want about 300 ms", d)
	}
}

func TestExecReadsAtMostMaxExecBody(t *testing.T) {
	host, port := serveBytes(t, MaxExecBody)
	resp, err := ExecVM(host, port, "sb-1", []byte(`{}`), "", 10*time.Second, "")
	if err != nil || len(resp.Body) != MaxExecBody {
		t.Fatalf("exec answer of exactly MaxExecBody: err %v", err)
	}
	host, port = serveBytes(t, MaxExecBody+1)
	if _, err := ExecVM(host, port, "sb-1", []byte(`{}`), "", 10*time.Second, ""); err == nil {
		t.Fatal("exec answer of MaxExecBody+1 accepted")
	}
}
