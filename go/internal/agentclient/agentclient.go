// Package agentclient provides an HTTP client for talking to hearth-agent
// instances, replicating client.zig behavior exactly.
package agentclient

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Response mirrors the Zig client.Response.
type Response struct {
	Status int
	Body   []byte
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Request performs an HTTP request to the agent at host:port.
// token is attached as "Authorization: Bearer <token>" when non-empty.
func Request(host string, port uint16, method, path string, body []byte, token string) (*Response, error) {
	url := fmt.Sprintf("http://%s:%d%s", host, port, path)

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(body))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Connection", "close")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return &Response{Status: resp.StatusCode, Body: respBody}, nil
}

// SplitHostPort replicates splitHostPort from client.zig / main.zig:
// strips http:// or https:// scheme, strips any trailing path, then splits on
// the last colon. Default port is 9090.
func SplitHostPort(addr string) (string, uint16) {
	a := addr
	if strings.HasPrefix(a, "http://") {
		a = a[len("http://"):]
	} else if strings.HasPrefix(a, "https://") {
		a = a[len("https://"):]
	}
	// Strip any trailing path.
	if slash := strings.IndexByte(a, '/'); slash >= 0 {
		a = a[:slash]
	}
	if colon := strings.LastIndex(a, ":"); colon >= 0 {
		host := a[:colon]
		portStr := a[colon+1:]
		var port uint16
		if _, err := fmt.Sscan(portStr, &port); err == nil {
			return host, port
		}
	}
	return a, 9090
}
