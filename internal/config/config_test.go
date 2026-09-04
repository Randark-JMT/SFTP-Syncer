package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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

func TestStoreAddUpdateRemove(t *testing.T) {
	s := &Store{}
	cfg := Default()
	cfg.Host = "example.com"
	cfg.Username = "demo"
	cfg.Password = "secret"
	cfg.RemoteDir = "/incoming"
	cfg.LocalDir = `C:\downloads`

	added := s.Add(cfg)
	if added.ID == "" {
		t.Fatal("expected Add to assign an ID")
	}
	if len(s.Hosts) != 1 {
		t.Fatalf("expected 1 host, got %d", len(s.Hosts))
	}

	added.Password = "changed"
	if !s.Update(added) {
		t.Fatal("expected Update to succeed")
	}
	if got, ok := s.Find(added.ID); !ok || got.Password != "changed" {
		t.Fatalf("expected updated host, got %+v ok=%v", got, ok)
	}

	if !s.Remove(added.ID) {
		t.Fatal("expected Remove to succeed")
	}
	if len(s.Hosts) != 0 {
		t.Fatalf("expected 0 hosts, got %d", len(s.Hosts))
	}
	if s.Remove(added.ID) {
		t.Fatal("expected second Remove to fail")
	}
}

func TestStoreUpdateRequiresID(t *testing.T) {
	s := &Store{}
	if s.Update(Default()) {
		t.Fatal("expected Update without ID to fail")
	}
}

func TestConfigDisplayName(t *testing.T) {
	cfg := Default()
	if got := cfg.DisplayName(); got != "未命名主机" {
		t.Fatalf("expected fallback name, got %q", got)
	}
	cfg.Host = "example.com"
	if got := cfg.DisplayName(); got != "example.com:22" {
		t.Fatalf("expected host:port fallback, got %q", got)
	}
	cfg.Name = "测试主机"
	if got := cfg.DisplayName(); got != "测试主机" {
		t.Fatalf("expected explicit name, got %q", got)
	}
}

func TestNewHostIDUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id := NewHostID()
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestLoadStoreMigratesLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)

	appDir := filepath.Join(dir, appDirName)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := Default()
	legacy.Host = "legacy.example.com"
	legacy.Username = "demo"
	legacy.Password = "secret"
	legacy.RemoteDir = "/incoming"
	legacy.LocalDir = `C:\downloads`
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, configFileName), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if len(store.Hosts) != 1 {
		t.Fatalf("expected migrated store with 1 host, got %d", len(store.Hosts))
	}
	got := store.Hosts[0]
	if got.ID == "" {
		t.Fatal("expected migrated host to receive an ID")
	}
	if got.Name != "默认主机" || got.Host != "legacy.example.com" {
		t.Fatalf("unexpected migrated host: %+v", got)
	}

	// Migration must persist hosts.json so the legacy path is only taken once.
	if _, err := os.Stat(filepath.Join(appDir, hostsFileName)); err != nil {
		t.Fatalf("expected hosts.json to be written: %v", err)
	}
}

func TestLoadStoreWithoutLegacyConfigIsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)

	store, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if len(store.Hosts) != 0 {
		t.Fatalf("expected empty store, got %d hosts", len(store.Hosts))
	}
}
