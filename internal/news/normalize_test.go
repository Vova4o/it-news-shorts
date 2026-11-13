package news

import (
	"testing"
	"time"
)

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "tracking params and fragment",
			in:   "HTTPS://Example.COM:443/article?utm_source=hn&fbclid=123&gclid=456&id=42&page=2#comments",
			want: "https://example.com/article?id=42&page=2",
		},
		{
			name: "http default port",
			in:   "http://example.com:80",
			want: "http://example.com/",
		},
		{
			name: "non-default port preserved",
			in:   "https://example.com:8443/path",
			want: "https://example.com:8443/path",
		},
		{
			name: "unicode path",
			in:   "https://example.com/новости/ai",
			want: "https://example.com/%D0%BD%D0%BE%D0%B2%D0%BE%D1%81%D1%82%D0%B8/ai",
		},
		{
			name:    "invalid scheme",
			in:      "ftp://example.com/file",
			wantErr: true,
		},
		{
			name:    "empty",
			in:      "  ",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeURL(%q) error = %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeText(t *testing.T) {
	in := "  <p>Breaking:\t<b>Go &amp; Rust</b></p><script>evil()</script>\x00\x07  news &quot;2026&quot;  "
	got := NormalizeText(in, 100)
	want := `Breaking: Go & Rust news "2026"`
	if got != want {
		t.Fatalf("NormalizeText() = %q, want %q", got, want)
	}

	if empty := NormalizeText("   \n\t  ", 100); empty != "" {
		t.Fatalf("expected empty string, got %q", empty)
	}
}

func TestParseDate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *time.Time
	}{
		{
			name: "rfc3339 with offset",
			in:   "2026-11-13T09:00:00+03:00",
			want: ptrTime(time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)),
		},
		{
			name: "rfc1123z",
			in:   "Fri, 13 Nov 2026 06:00:00 +0000",
			want: ptrTime(time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)),
		},
		{
			name: "unix seconds",
			in:   "1794556800",
			want: ptrTime(time.Unix(1794556800, 0).UTC()),
		},
		{
			name: "invalid date",
			in:   "not-a-date",
			want: nil,
		},
		{
			name: "empty",
			in:   "",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseDate(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if got == nil || !got.Equal(*tc.want) {
				t.Fatalf("ParseDate(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
