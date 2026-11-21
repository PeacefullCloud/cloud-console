package sshkeys

import (
	"encoding/pem"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerateRoundTrip(t *testing.T) {
	public, private, fingerprint, err := Generate("my laptop")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(public, "ssh-ed25519 ") {
		t.Fatalf("public key = %q, want an ssh-ed25519 line", public)
	}
	if !strings.HasSuffix(public, " my laptop") {
		t.Fatalf("comment not preserved: %q", public)
	}
	if !strings.HasPrefix(fingerprint, "SHA256:") {
		t.Fatalf("fingerprint = %q", fingerprint)
	}

	block, _ := pem.Decode([]byte(private))
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		t.Fatalf("private key is not an OpenSSH PEM block")
	}
	if _, err := ssh.ParsePrivateKey([]byte(private)); err != nil {
		t.Fatalf("generated private key does not parse: %v", err)
	}

	normalized, parsedFP, err := Parse(public)
	if err != nil {
		t.Fatal(err)
	}
	if normalized != public {
		t.Errorf("normalized = %q, want %q", normalized, public)
	}
	if parsedFP != fingerprint {
		t.Errorf("fingerprint = %q, want %q", parsedFP, fingerprint)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	valid := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHlYi7+rNn3KzKjKjKjKjKjKjKjKjKjKjKjKjK comment"

	cases := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"two keys", valid + "\n" + valid},
		{"not a key", "hello world"},
		{"bad base64", "ssh-ed25519 !!!not-base64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Parse(tc.value); err == nil {
				t.Fatalf("Parse(%q) unexpectedly succeeded", tc.value)
			}
		})
	}
}

func TestCleanCommentCannotInjectLines(t *testing.T) {
	public, _, _, err := Generate("laptop\r\nssh-rsa AAAA evil")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(public, "\n") || strings.Count(public, " ") < 2 {
		t.Fatalf("comment injection not neutralized: %q", public)
	}
	if strings.Contains(public, "\r") {
		t.Fatalf("carriage return leaked into key: %q", public)
	}
}
