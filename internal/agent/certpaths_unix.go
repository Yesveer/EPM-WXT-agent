//go:build !windows

package agent

// On-disk cert storage paths (Unix). Values are unchanged from the original
// hardcoded constants in agent.go — this split only lets Windows use its own dir.
var (
	// Encrypted on-disk storage — never store plaintext key on disk.
	mtlsCertFile = "/etc/vsay/certs/client-cert.enc"
	mtlsKeyFile  = "/etc/vsay/certs/client-key.enc"
	serverCACert = "/etc/vsay/certs/server-ca.pem" // CA cert is public; no need to encrypt

	// Legacy plaintext paths — present only on agents that haven't been migrated yet.
	mtlsCertFileLegacy = "/etc/vsay/certs/client-cert.pem"
	mtlsKeyFileLegacy  = "/etc/vsay/certs/client-key.pem"

	// CA-rotation fingerprint — must be writable, else rotation re-triggers each poll.
	caFingerprintFile = "/etc/vsay/certs/ca-fingerprint.txt"
)
