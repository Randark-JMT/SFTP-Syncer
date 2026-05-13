package config

import "testing"

func TestValidatePasswordAuthRequiresPassword(t *testing.T) {
	cfg := Default()
	cfg.Host = "example.com"
	cfg.Username = "demo"
	cfg.RemoteDir = "/incoming"
	cfg.LocalDir = `C:\\downloads`
	cfg.Password = ""
	cfg.AuthMode = AuthModePassword

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected password auth to require password")
	}
}

func TestValidatePrivateKeyAuthRequiresKeyPath(t *testing.T) {
	cfg := Default()
	cfg.Host = "example.com"
	cfg.Username = "demo"
	cfg.RemoteDir = "/incoming"
	cfg.LocalDir = `C:\\downloads`
	cfg.AuthMode = AuthModePrivateKey
	cfg.PrivateKeyPath = ""

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected private key auth to require key path")
	}
}

func TestNormalizeDefaultsAuthMode(t *testing.T) {
	cfg := Config{}
	got := cfg.Normalized()
	if got.AuthMode != AuthModePassword {
		t.Fatalf("expected default auth mode %q, got %q", AuthModePassword, got.AuthMode)
	}
}
