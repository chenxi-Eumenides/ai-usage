package provider

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewHTTPClientRoutesOnlyRequestsWithExplicitProxy(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte("direct"))
	}))
	defer target.Close()

	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		_, _ = w.Write([]byte("proxied"))
	}))
	defer proxy.Close()

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	client := NewHTTPClient(time.Second)

	directResponse, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("direct request failed: %v", err)
	}
	if body := readResponseBody(t, directResponse); body != "direct" {
		t.Fatalf("direct response body = %q, want direct", body)
	}
	if proxyHits.Load() != 0 {
		t.Fatalf("direct request used HTTP_PROXY: proxy hits = %d", proxyHits.Load())
	}

	proxyURL, err := parseProxyURL(proxy.URL)
	if err != nil {
		t.Fatalf("parseProxyURL failed: %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, target.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	request = request.WithContext(withProxyURL(request.Context(), proxyURL))
	proxiedResponse, err := client.Do(request)
	if err != nil {
		t.Fatalf("proxied request failed: %v", err)
	}
	if body := readResponseBody(t, proxiedResponse); body != "proxied" {
		t.Fatalf("proxied response body = %q, want proxied", body)
	}
	if targetHits.Load() != 1 {
		t.Errorf("target hits = %d, want only the direct request", targetHits.Load())
	}
	if proxyHits.Load() != 1 {
		t.Errorf("proxy hits = %d, want 1", proxyHits.Load())
	}
}

func readResponseBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(body)
}
