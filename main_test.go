package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestScannerFindsReflected404FromDiscoveredLink(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<a href="/missing-page">missing</a>`)
		case "/missing-page":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "<html>not found: %s</html>", r.URL.Path)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	s := newTestScanner(t, server.URL, false)
	if err := s.run(t.Context()); err != nil {
		t.Fatal(err)
	}

	if s.findings != 1 {
		t.Fatalf("expected 1 finding, got %d", s.findings)
	}
}

func TestScannerProbesDiscoveredDirectories(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<a href="/docs/page.html">docs</a>`)
		case "/docs/page.html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html>ok</html>`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "<html>not found: %s</html>", r.URL.Path)
		}
	}))
	defer server.Close()

	s := newTestScanner(t, server.URL, true)
	if err := s.run(t.Context()); err != nil {
		t.Fatal(err)
	}

	if s.findings == 0 {
		t.Fatal("expected at least one probe finding")
	}
}

func newTestScanner(t *testing.T, rawTarget string, probeDirs bool) *scanner {
	t.Helper()

	target, err := url.Parse(rawTarget)
	if err != nil {
		t.Fatal(err)
	}

	return newScanner(config{
		target:          target,
		maxDepth:        2,
		maxURLs:         20,
		rate:            0,
		userAgent:       defaultUserAgent,
		timeout:         2 * time.Second,
		bodyLimit:       defaultBodyLimit,
		followRedirects: true,
		probeDirs:       probeDirs,
		jsonOutput:      true,
	})
}
