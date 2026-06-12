package main

import (
	"bytes"
	"testing"

	"github.com/tamnd/dbrest/config"
)

// TestJWTSecretBytes pins the jwt-secret-is-base64 contract: off means the
// literal bytes, on means URL-safe base64 with optional padding, and an
// undecodable value is an error rather than a wrong key.
func TestJWTSecretBytes(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		isB64  bool
		want   []byte
		bad    bool
	}{
		{name: "plain passthrough", secret: "reallysafe", want: []byte("reallysafe")},
		{name: "unpadded url-safe", secret: "c2VjcmV0LWJ5dGVz", isB64: true, want: []byte("secret-bytes")},
		{name: "padded url-safe", secret: "c2VjcmV0IQ==", isB64: true, want: []byte("secret!")},
		{name: "url alphabet", secret: "_-7-", isB64: true, want: []byte{0xff, 0xee, 0xfe}},
		{name: "surrounding space", secret: "  c2VjcmV0IQ==  ", isB64: true, want: []byte("secret!")},
		{name: "not base64", secret: "definitely not base64!!", isB64: true, bad: true},
		{name: "standard alphabet rejected", secret: "/+7+", isB64: true, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{JWTSecret: tc.secret, JWTSecretIsBase64: tc.isB64}
			got, err := jwtSecretBytes(cfg)
			if tc.bad {
				if err == nil {
					t.Fatalf("decoded %q without error to %q", tc.secret, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBase64OptionNoLongerWarns checks the option left the unenforced list
// when its behavior landed.
func TestBase64OptionNoLongerWarns(t *testing.T) {
	cfg, err := config.FromMap(map[string]string{
		"db-uri":               "x",
		"jwt-secret":           "c2VjcmV0IQ==",
		"jwt-secret-is-base64": "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range cfg.Warnings {
		if bytes.Contains([]byte(w), []byte("jwt-secret-is-base64")) {
			t.Errorf("unexpected warning: %s", w)
		}
	}
}
