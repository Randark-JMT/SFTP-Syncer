package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	appDirName          = "SFTP-Syncer"
	configFileName      = "config.json"
	hostsFileName       = "hosts.json"
	defaultPort         = 22
	defaultPollInterval = 30
	AuthModePassword    = "password"
	AuthModePrivateKey  = "private_key"
)

// 连接代理类型。ProxyModeNone 表示直连；HTTP/HTTPS 走 HTTP CONNECT 隧道，
// SOCKS5 走 SOCKS5 协议，均支持可选的用户名+密码认证。
const (
	ProxyModeNone   = "none"
	ProxyModeHTTP   = "http"
	ProxyModeHTTPS  = "https"
	ProxyModeSOCKS5 = "socks5"
)

// Config holds the settings of a single sync host. ID and Name identify the
// host within the host manager store; the remaining fields drive the syncer.
type Config struct {
	ID                    string `json:"id,omitempty"`
	Name                  string `json:"name,omitempty"`
	Host                  string `json:"host"`
	Port                  int    `json:"port"`
	Username              string `json:"username"`
	AuthMode              string `json:"authMode"`
	Password              string `json:"password"`
	PrivateKeyPath        string `json:"privateKeyPath"`
	PrivateKeyPassphrase  string `json:"privateKeyPassphrase"`
	RemoteDir             string `json:"remoteDir"`
	LocalDir              string `json:"localDir"`
	PollIntervalSeconds   int    `json:"pollIntervalSeconds"`
	SkipHostKeyValidation bool   `json:"skipHostKeyValidation"`
	KnownHostsPath        string `json:"knownHostsPath"`
	ProxyMode             string `json:"proxyMode"`
	ProxyHost             string `json:"proxyHost"`
	ProxyPort             int    `json:"proxyPort"`
	ProxyUsername         string `json:"proxyUsername"`
	ProxyPassword         string `json:"proxyPassword"`
}

// DisplayName returns the user-visible label of the host: the configured
// name when present, falling back to host:port.
func (c Config) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	if c.Host == "" {
		return "未命名主机"
	}
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// ProxyEnabled reports whether the host connects through a proxy.
func (c Config) ProxyEnabled() bool {
	return c.ProxyMode != "" && c.ProxyMode != ProxyModeNone
}

func Default() Config {
	return Config{
		Port:                  defaultPort,
		PollIntervalSeconds:   defaultPollInterval,
		SkipHostKeyValidation: true,
		AuthMode:              AuthModePassword,
	}
}

func (c Config) Validate() error {
	var missing []string
	cfg := c.Normalized()

	if strings.TrimSpace(cfg.Host) == "" {
		missing = append(missing, "服务器地址")
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return fmt.Errorf("端口必须在 1 到 65535 之间")
	}
	if strings.TrimSpace(cfg.Username) == "" {
		missing = append(missing, "用户名")
	}
	switch cfg.AuthMode {
	case AuthModePassword:
		if strings.TrimSpace(cfg.Password) == "" {
			missing = append(missing, "密码")
		}
	case AuthModePrivateKey:
		if strings.TrimSpace(cfg.PrivateKeyPath) == "" {
			missing = append(missing, "私钥路径")
		}
	default:
		return fmt.Errorf("认证方式无效：%s", cfg.AuthMode)
	}
	if strings.TrimSpace(cfg.RemoteDir) == "" {
		missing = append(missing, "远程目录")
	}
	if strings.TrimSpace(cfg.LocalDir) == "" {
		missing = append(missing, "本地目录")
	}
	if cfg.PollIntervalSeconds < 5 {
		return fmt.Errorf("轮询间隔不能小于 5 秒")
	}
	if !cfg.SkipHostKeyValidation && strings.TrimSpace(cfg.KnownHostsPath) == "" {
		if _, err := defaultKnownHostsPath(); err != nil {
			return fmt.Errorf("无法确定 known_hosts 路径: %w", err)
		}
	}
	switch cfg.ProxyMode {
	case ProxyModeNone:
	case ProxyModeHTTP, ProxyModeHTTPS, ProxyModeSOCKS5:
		if strings.TrimSpace(cfg.ProxyHost) == "" {
			missing = append(missing, "代理服务器地址")
		}
		if cfg.ProxyPort <= 0 || cfg.ProxyPort > 65535 {
			return fmt.Errorf("代理端口必须在 1 到 65535 之间")
		}
	default:
		return fmt.Errorf("代理类型无效：%s", cfg.ProxyMode)
	}
	if len(missing) > 0 {
		return fmt.Errorf("请填写完整配置：%s", strings.Join(missing, "、"))
	}

	return nil
}

func (c Config) Normalized() Config {
	cfg := c
	cfg.ID = strings.TrimSpace(cfg.ID)
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.AuthMode = strings.ToLower(strings.TrimSpace(cfg.AuthMode))
	if cfg.AuthMode == "" {
		cfg.AuthMode = AuthModePassword
	}
	cfg.RemoteDir = strings.TrimSpace(strings.ReplaceAll(cfg.RemoteDir, `\`, `/`))
	if localDir := strings.TrimSpace(cfg.LocalDir); localDir != "" {
		cfg.LocalDir = filepath.Clean(localDir)
	} else {
		cfg.LocalDir = ""
	}
	if privateKeyPath := strings.TrimSpace(cfg.PrivateKeyPath); privateKeyPath != "" {
		cfg.PrivateKeyPath = filepath.Clean(privateKeyPath)
	} else {
		cfg.PrivateKeyPath = ""
	}
	if knownHostsPath := strings.TrimSpace(cfg.KnownHostsPath); knownHostsPath != "" {
		cfg.KnownHostsPath = filepath.Clean(knownHostsPath)
	} else {
		cfg.KnownHostsPath = ""
	}
	cfg.ProxyMode = strings.ToLower(strings.TrimSpace(cfg.ProxyMode))
	if cfg.ProxyMode == "" {
		cfg.ProxyMode = ProxyModeNone
	}
	cfg.ProxyHost = strings.TrimSpace(cfg.ProxyHost)
	cfg.ProxyUsername = strings.TrimSpace(cfg.ProxyUsername)
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.PollIntervalSeconds == 0 {
		cfg.PollIntervalSeconds = defaultPollInterval
	}
	return cfg
}

func ConfigPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appDirName, configFileName), nil
}

func Load() (Config, error) {
	cfg := Default()
	path, err := ConfigPath()
	if err != nil {
		return cfg, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return Default(), err
	}

	return cfg.Normalized(), nil
}

// Store is the persisted collection of hosts managed by the host manager.
type Store struct {
	Hosts []Config `json:"hosts"`
}

func StorePath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appDirName, hostsFileName), nil
}

// LoadStore loads hosts.json. When the file does not exist yet, a legacy
// single-host config.json (if present) is migrated into the store and saved.
func LoadStore() (*Store, error) {
	store := &Store{}
	path, err := StorePath()
	if err != nil {
		return store, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return store, err
		}
		return migrateLegacyStore()
	}
	if err := json.Unmarshal(data, store); err != nil {
		return store, fmt.Errorf("解析主机列表失败: %w", err)
	}
	for i := range store.Hosts {
		store.Hosts[i] = store.Hosts[i].Normalized()
		if store.Hosts[i].ID == "" {
			store.Hosts[i].ID = NewHostID()
		}
	}
	return store, nil
}

// migrateLegacyStore converts the pre-host-manager single-host config.json
// into a hosts.json entry on first launch.
func migrateLegacyStore() (*Store, error) {
	store := &Store{}
	legacy, err := Load()
	if err != nil || strings.TrimSpace(legacy.Host) == "" {
		return store, err
	}
	legacy.ID = NewHostID()
	legacy.Name = "默认主机"
	store.Hosts = append(store.Hosts, legacy)
	if err := store.Save(); err != nil {
		return store, fmt.Errorf("迁移旧配置失败: %w", err)
	}
	return store, nil
}

func (s *Store) Save() error {
	path, err := StorePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, payload, 0o600)
}

// Find returns the host with the given ID.
func (s *Store) Find(id string) (Config, bool) {
	for _, h := range s.Hosts {
		if h.ID == id {
			return h, true
		}
	}
	return Config{}, false
}

// Add appends a new host, assigning a fresh ID when the host has none.
func (s *Store) Add(cfg Config) Config {
	cfg = cfg.Normalized()
	if cfg.ID == "" {
		cfg.ID = NewHostID()
	}
	s.Hosts = append(s.Hosts, cfg)
	return cfg
}

// Update replaces the host that shares cfg's ID. It reports whether a host
// with that ID existed.
func (s *Store) Update(cfg Config) bool {
	if cfg.ID == "" {
		return false
	}
	for i := range s.Hosts {
		if s.Hosts[i].ID == cfg.ID {
			s.Hosts[i] = cfg.Normalized()
			return true
		}
	}
	return false
}

// Remove drops the host with the given ID. It reports whether a host with
// that ID existed.
func (s *Store) Remove(id string) bool {
	for i := range s.Hosts {
		if s.Hosts[i].ID == id {
			s.Hosts = append(s.Hosts[:i], s.Hosts[i+1:]...)
			return true
		}
	}
	return false
}

// NewHostID returns a random identifier for a host entry.
func NewHostID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("h%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func EffectiveKnownHostsPath(cfg Config) (string, error) {
	if strings.TrimSpace(cfg.KnownHostsPath) != "" {
		return filepath.Clean(strings.TrimSpace(cfg.KnownHostsPath)), nil
	}
	return defaultKnownHostsPath()
}

func defaultKnownHostsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}
