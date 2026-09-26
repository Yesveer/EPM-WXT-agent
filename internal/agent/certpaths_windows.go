//go:build windows

package agent

// On-disk cert storage paths (Windows). Mirrors the Unix layout under
// C:\ProgramData\vsay\certs, which the configure command creates.
var (
	// Encrypted on-disk storage — never store plaintext key on disk.
	mtlsCertFile = `C:\ProgramData\vsay\certs\client-cert.enc`
	mtlsKeyFile  = `C:\ProgramData\vsay\certs\client-key.enc`
	serverCACert = `C:\ProgramData\vsay\certs\server-ca.pem` // CA cert is public; no need to encrypt

	// Legacy plaintext paths — present only on agents that haven't been migrated yet.
	mtlsCertFileLegacy = `C:\ProgramData\vsay\certs\client-cert.pem`
	mtlsKeyFileLegacy  = `C:\ProgramData\vsay\certs\client-key.pem`

	// CA-rotation fingerprint — must be writable, else rotation re-triggers each poll.
	caFingerprintFile = `C:\ProgramData\vsay\certs\ca-fingerprint.txt`
)
