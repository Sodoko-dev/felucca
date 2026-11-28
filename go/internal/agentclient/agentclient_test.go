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
