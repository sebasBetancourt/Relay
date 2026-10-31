package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIPFrom(t *testing.T) {
	tests := []struct {
		name, header, value, want string
	}{
		{"off by default", "", "198.51.100.1", "203.0.113.9"},
		{"ipv4", "Fly-Client-IP", "198.51.100.1", "198.51.100.1"},
		{"ipv6 keeps the /64 rule", "Fly-Client-IP", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"spaces", "Fly-Client-IP", " 198.51.100.1 ", "198.51.100.1"},
		{"missing", "Fly-Client-IP", "", "203.0.113.9"},
		{"garbage", "Fly-Client-IP", "not-an-ip", "203.0.113.9"},
		{"a list is not one address", "Fly-Client-IP", "198.51.100.1, 198.51.100.2", "203.0.113.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			h := ClientIPFrom(tt.header)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = clientIP(r)
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "203.0.113.9:1234"
			if tt.value != "" {
				req.Header.Set("Fly-Client-IP", tt.value)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != tt.want {
				t.Fatalf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

// Without the setting, a client cannot pick its own rate limit bucket.
func TestClientIPIsNotSpoofableByDefault(t *testing.T) {
	var got string
	h := ClientIPFrom("")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = clientIP(r)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("Fly-Client-IP", "198.51.100.1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "203.0.113.9" {
		t.Fatalf("clientIP = %q", got)
	}
}
