package news

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPClient_Security(t *testing.T) {
	t.Run("disallowed scheme", func(t *testing.T) {
		c := NewHTTPClient(HTTPClientOptions{Timeout: time.Second})
		_, err := c.Get(context.Background(), "file:///etc/passwd", "", "")
		if !errors.Is(err, ErrInvalidScheme) {
			t.Fatalf("expected ErrInvalidScheme, got %v", err)
		}
	})

	t.Run("localhost blocked", func(t *testing.T) {
		c := NewHTTPClient(HTTPClientOptions{Timeout: time.Second})
		_, err := c.Get(context.Background(), "http://localhost:8080/feed", "", "")
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("expected ErrSSRFBlocked, got %v", err)
		}
	})

	t.Run("private ip blocked", func(t *testing.T) {
		c := NewHTTPClient(HTTPClientOptions{Timeout: time.Second})
		for _, target := range []string{"http://127.0.0.1/feed", "http://10.0.0.1/feed", "http://192.168.1.1/feed"} {
			_, err := c.Get(context.Background(), target, "", "")
			if !errors.Is(err, ErrSSRFBlocked) {
				t.Fatalf("expected ErrSSRFBlocked for %s, got %v", target, err)
			}
		}
	})

	t.Run("redirect to localhost blocked", func(t *testing.T) {
		// Use a custom RoundTripper so the initial request can target a public
		// hostname while redirecting to localhost.
		c := NewHTTPClient(HTTPClientOptions{
			Timeout: time.Second,
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"http://127.0.0.1:9000/secret"}},
					Body:       http.NoBody,
					Request:    req,
				}, nil
			}),
		})
		_, err := c.Get(context.Background(), "https://93.184.216.34/feed", "", "")
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("expected ErrSSRFBlocked on redirect, got %v", err)
		}
	})

	t.Run("too many redirects", func(t *testing.T) {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
		}))
		defer srv.Close()

		c := NewHTTPClient(HTTPClientOptions{
			Timeout:         2 * time.Second,
			MaxRedirects:    2,
			AllowPrivateIPs: true,
		})
		_, err := c.Get(context.Background(), srv.URL, "", "")
		if !errors.Is(err, ErrTooManyRedirects) {
			t.Fatalf("expected ErrTooManyRedirects, got %v", err)
		}
	})

	t.Run("oversized response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, strings.Repeat("a", 2048))
		}))
		defer srv.Close()

		c := NewHTTPClient(HTTPClientOptions{
			Timeout:         2 * time.Second,
			MaxBytes:        512,
			AllowPrivateIPs: true,
		})
		_, err := c.Get(context.Background(), srv.URL, "", "")
		if !errors.Is(err, ErrOversizedResponse) {
			t.Fatalf("expected ErrOversizedResponse, got %v", err)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
