package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents agent configuration
type Config struct {
	Agent       AgentConfig       `yaml:"agent"`
	Server      ServerConfig      `yaml:"server"`
	Tunnel      TunnelConfig      `yaml:"tunnel"`
	Tenant      TenantConfig      `yaml:"tenant"`
	Org         OrgConfig         `yaml:"organization"`
	Project     ProjectConfig     `yaml:"project"`
	PortalUser  PortalUserConfig  `yaml:"portal_user"`
	LinuxUser   LinuxUserConfig   `yaml:"linux_user"`
	Permissions PermissionsConfig `yaml:"permissions"`
	HostEntries []HostEntry       `yaml:"host_entries,omitempty"`
	Logging     LoggingConfig     `yaml:"logging"`
	Status      string            `yaml:"status"`
}

// TunnelConfig holds vsay-tunnel connection settings.
// If URL is empty the tunnel client is disabled.
type TunnelConfig struct {
	URL          string `yaml:"url"`           // vsay-tunnel base URL, e.g. "http://localhost:8083"
	Enabled      bool   `yaml:"enabled"`       // set to true to enable tunneling
	PollInterval string `yaml:"poll_interval"` // how often to check for new tunnels, default "10s"
}

type AgentConfig struct {
	ID       string            `yaml:"id"`
	Name     string            `yaml:"name"`
	Tags     []string          `yaml:"tags"`
	Metadata map[string]string `yaml:"metadata"`
}

type ServerConfig struct {
	Host       string `yaml:"host"`
	GRPCURL    string `yaml:"grpc_url"`
	APIHost    string `yaml:"api_host"` // Custom gRPC address (optional)
	TLS        bool   `yaml:"tls"`
	TokenHash  string `yaml:"token_hash"`
	Token      string `yaml:"token"`        // Actual token (encrypted in file)
	CACertFile string `yaml:"ca_cert_file"` // Server CA cert for TLS verification (empty = system CAs / Let's Encrypt)
}

type TenantConfig struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
}

type OrgConfig struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
}

type ProjectConfig struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
}

type PortalUserConfig struct {
	ID    string `yaml:"id"`
	Email string `yaml:"email"`
}

type LinuxUserConfig struct {
	Username    string   `yaml:"username"`
	UID         int      `yaml:"uid"`
	GID         int      `yaml:"gid"`
	Groups      []string `yaml:"groups"`
	SudoEnabled bool     `yaml:"sudo_enabled"`
	HomeDir     string   `yaml:"home_dir"`
	Shell       string   `yaml:"shell"`
}

type PermissionsConfig struct {
	AllowSudo       bool     `yaml:"allow_sudo"`
	BlockedCommands []string `yaml:"blocked_commands"`
}

// HostEntry represents a single /etc/hosts entry written before the agent connects.
type HostEntry struct {
	IP     string `yaml:"ip"`
	Domain string `yaml:"domain"`
}

type LoggingConfig struct {
	Level      string `yaml:"level"`
	File       string `yaml:"file"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxBackups int    `yaml:"max_backups"`
}

// configEncVersion is the version prefix byte for the encrypted config format.
// Valid YAML never starts with 0x01 so this cleanly distinguishes encrypted
// files from the old plaintext format for backward-compatible loading.
const configEncVersion = byte(0x01)

// Load loads a plaintext YAML config from path.
// Prefer LoadEncrypted for new code — this is kept for compatibility.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is this agent's own config path (CLI flag / fixed default), not request input
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Save writes cfg as plaintext YAML.
// Prefer SaveEncrypted for new code — this is kept for compatibility.
func Save(cfg *Config, path string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// SaveEncrypted marshals cfg to YAML then AES-256-GCM encrypts it before
// writing to disk. The encryption key is derived from /etc/machine-id so
// the file is only decryptable on the machine that wrote it — copying it
// to another machine yields an unreadable blob.
func SaveEncrypted(cfg *Config, path string) error {
	plain, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	enc, err := encryptConfigBytes(plain)
	if err != nil {
		return fmt.Errorf("encrypt config: %w", err)
	}
	return os.WriteFile(path, enc, 0600)
}

// LoadEncrypted loads a config file written by SaveEncrypted.
// If the file was written in plaintext (old format — first byte ≠ 0x01)
// it is parsed as-is so existing machines keep working without reconfiguring.
func LoadEncrypted(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is this agent's own config path (CLI flag / fixed default), not request input
	if err != nil {
		return nil, err
	}

	if len(data) > 0 && data[0] == configEncVersion {
		data, err = decryptConfigBytes(data)
		if err != nil {
			return nil, fmt.Errorf("decrypt config: %w", err)
		}
	}
	// else: plaintext YAML (pre-encryption format) — parse directly.

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// deriveConfigKey derives a 32-byte AES-256 key from /etc/machine-id.
// Uses a config-specific HKDF info string so the key is distinct from
// the cert-encryption key even though both come from the same machine-id.
func deriveConfigKey() ([]byte, error) {
	// machineID is /etc/machine-id on Unix, registry MachineGuid on Windows.
	machineID, err := readMachineID()
	if err != nil {
		return nil, fmt.Errorf("cannot derive config key: %w", err)
	}

	// HKDF-SHA256 (RFC 5869) — same scheme as cert key derivation, different info.
	salt := []byte("vsay-agent-config-enc-v1")
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(machineID))
	prk := mac.Sum(nil)

	info := []byte("vsay-agent-config-v1")
	mac = hmac.New(sha256.New, prk)
	mac.Write(info)
	mac.Write([]byte{0x01})
	return mac.Sum(nil), nil // 32 bytes = AES-256 key
}

func encryptConfigBytes(plain []byte) ([]byte, error) {
	key, err := deriveConfigKey()
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plain, nil)

	// Layout: version(1) | nonce(12) | ciphertext+GCM-tag
	out := make([]byte, 1+len(nonce)+len(ct))
	out[0] = configEncVersion
	copy(out[1:], nonce)
	copy(out[1+len(nonce):], ct)
	return out, nil
}

func decryptConfigBytes(encrypted []byte) ([]byte, error) {
	if len(encrypted) < 1 || encrypted[0] != configEncVersion {
		return nil, fmt.Errorf("unsupported config encryption version %d", encrypted[0])
	}
	key, err := deriveConfigKey()
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	data := encrypted[1:]
	if len(data) < gcm.NonceSize() {
		return nil, fmt.Errorf("encrypted config too short")
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// Fires when file was encrypted on a different machine or has been tampered with.
		return nil, fmt.Errorf("decryption failed — wrong machine or tampered file: %w", err)
	}
	return plain, nil
}
