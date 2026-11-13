package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Temporal.Namespace != "it-news" {
		t.Fatalf("Namespace = %q, want it-news", cfg.Temporal.Namespace)
	}
	if cfg.Temporal.TaskQueue != "IT_NEWS_QUEUE" {
		t.Fatalf("TaskQueue = %q, want IT_NEWS_QUEUE", cfg.Temporal.TaskQueue)
	}
	if cfg.MinIO.Bucket != "it-news-media" {
		t.Fatalf("Bucket = %q, want it-news-media", cfg.MinIO.Bucket)
	}
	if cfg.Postgres.QueryTimeout != 5*time.Second {
		t.Fatalf("QueryTimeout = %s, want 5s", cfg.Postgres.QueryTimeout)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "invalid max conns",
			env:  map[string]string{"POSTGRES_MAX_CONNS": "0"},
		},
		{
			name: "invalid duration",
			env:  map[string]string{"POSTGRES_QUERY_TIMEOUT": "not-a-duration"},
		},
		{
			name: "invalid bool",
			env:  map[string]string{"MINIO_USE_SSL": "maybe"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(func(k string) (string, bool) {
				v, ok := tt.env[k]
				return v, ok
			})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestRedactDSN(t *testing.T) {
	const secret = "super_secret_marker_91x"
	cases := []struct {
		name         string
		dsn          string
		wantRedacted bool
		wantExact    string
	}{
		{
			name:         "url userinfo",
			dsn:          "postgres://appuser:" + secret + "@127.0.0.1:5433/it_news?sslmode=disable",
			wantRedacted: true,
		},
		{
			name:         "url query password",
			dsn:          "postgresql://appuser@127.0.0.1:5433/it_news?sslmode=disable&password=" + secret,
			wantRedacted: true,
		},
		{
			name:         "url query mixed-case password",
			dsn:          "postgres://127.0.0.1:5433/it_news?Password=" + secret,
			wantRedacted: true,
		},
		{
			name:      "malformed url fails closed",
			dsn:       "postgres://appuser:" + secret + "@[::1",
			wantExact: "<invalid-dsn>",
		},
		{
			name:      "malformed url semicolon query fails closed",
			dsn:       "postgres://app@127.0.0.1/it_news?password=" + secret + ";sslmode=disable",
			wantExact: "<invalid-dsn>",
		},
		{
			name:         "keyword unquoted",
			dsn:          "host=127.0.0.1 user=app password=" + secret + " dbname=it_news",
			wantRedacted: true,
		},
		{
			name:         "keyword single quoted with spaces and escape",
			dsn:          `host=127.0.0.1 password='` + secret + ` p\'2' dbname=it_news`,
			wantRedacted: true,
		},
		{
			name:         "keyword double quoted uppercase",
			dsn:          `host=127.0.0.1 PASSWORD="` + secret + `" dbname=it_news`,
			wantRedacted: true,
		},
		{
			name:      "malformed keyword unclosed quote fails closed",
			dsn:       "host=127.0.0.1 password='" + secret,
			wantExact: "<invalid-dsn>",
		},
		{
			name:      "malformed keyword stray token fails closed",
			dsn:       "host=127.0.0.1 password=" + secret + " straytoken",
			wantExact: "<invalid-dsn>",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := config.RedactDSN(tc.dsn)
			if strings.Contains(out, secret) {
				t.Fatalf("RedactDSN leaked secret: %s", out)
			}
			if tc.wantRedacted && !strings.Contains(out, "REDACTED") {
				t.Fatalf("RedactDSN did not mask password: %s", out)
			}
			if tc.wantExact != "" && out != tc.wantExact {
				t.Fatalf("RedactDSN = %q, want %q", out, tc.wantExact)
			}
		})
	}
}
