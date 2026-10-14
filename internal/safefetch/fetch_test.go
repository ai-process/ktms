package safefetch

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"testing"
)

func TestIsProhibitedIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.1.5", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"100.64.0.1", true},
		{"0.0.0.0", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
	}

	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if got := IsProhibitedIP(ip); got != tt.want {
			t.Errorf("IsProhibitedIP(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestParseDataURI(t *testing.T) {
	raw := []byte("hello png image data")
	b64 := base64.StdEncoding.EncodeToString(raw)
	uri := "data:image/png;base64," + b64

	mime, data, err := ParseDataURI(uri)
	if err != nil {
		t.Fatalf("ParseDataURI failed: %v", err)
	}
	if mime != "image/png" {
		t.Errorf("got mime %s, want image/png", mime)
	}
	if string(data) != string(raw) {
		t.Errorf("got data %q, want %q", data, raw)
	}

	if _, _, err := ParseDataURI("http://example.com/test.png"); err == nil {
		t.Error("expected error for non-data URI")
	}

	if _, _, err := ParseDataURI("data:image/png;base64,invalid!base64"); err == nil {
		t.Error("expected error for invalid base64")
	}
}

func TestFetchURL_ProhibitedScheme(t *testing.T) {
	ctx := context.Background()
	_, _, err := FetchURL(ctx, "http://example.com/image.png", 1024)
	if !errors.Is(err, ErrProhibitedScheme) {
		t.Errorf("expected ErrProhibitedScheme, got %v", err)
	}
}

func TestFetchURL_ProhibitedHost(t *testing.T) {
	ctx := context.Background()
	_, _, err := FetchURL(ctx, "https://127.0.0.1/image.png", 1024)
	if err == nil || !errors.Is(err, ErrProhibitedIP) {
		t.Errorf("expected ErrProhibitedIP for 127.0.0.1, got %v", err)
	}
}

func TestValidateMediaMimeType(t *testing.T) {
	valid := []string{"image/jpeg", "IMAGE/PNG", "image/webp", "image/gif", "audio/wav", "audio/mp3", "application/pdf"}
	for _, m := range valid {
		if err := ValidateMediaMimeType(m); err != nil {
			t.Errorf("expected valid MIME %s, got error: %v", m, err)
		}
	}

	invalid := []string{"text/html", "application/javascript", "video/mp4", "application/x-sh"}
	for _, m := range invalid {
		if err := ValidateMediaMimeType(m); err == nil {
			t.Errorf("expected invalid MIME for %s", m)
		}
	}
}
