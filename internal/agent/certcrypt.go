package agent

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"os"
)

// certEncVersion is a prefix byte so we can evolve the format later.
const certEncVersion = byte(1)

// deriveMachineKey derives a 32-byte AES-256 key from this machine's unique identity.
//
// Key material: /etc/machine-id — set at OS install time, never changes, unique per machine.
// This ensures that an encrypted key file copied to any other machine cannot be decrypted.
//
// NOTE: hostname is intentionally excluded because it changes (renames, cloud migrations).
// machine-id is stable for the lifetime of the OS installation.
func deriveMachineKey() ([]byte, error) {
	// machineID is /etc/machine-id on Unix, registry MachineGuid on Windows. Refusing
	// a static fallback keeps a copied key file undecryptable on any other machine.
	machineID, err := readMachineID()
	if err != nil {
		return nil, fmt.Errorf("cannot derive machine key: %w", err)
	}

	// HKDF-SHA256 (RFC 5869) implemented with HMAC.
	//
	// Extract: PRK = HMAC-SHA256(salt, machineID)
	salt := []byte("vsay-agent-key-enc-v1") // static, version-stamped
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(machineID))
	prk := mac.Sum(nil)

	// Expand: OKM = HMAC-SHA256(PRK, info || 0x01)
	info := []byte("vsay-agent-private-key-v1")
	mac = hmac.New(sha256.New, prk)
	mac.Write(info)
	mac.Write([]byte{0x01})
	return mac.Sum(nil), nil // 32 bytes = AES-256 key
}

// EncryptPrivateKeyDER encrypts DER-encoded private key bytes with AES-256-GCM.
//
// Output format: version(1) | nonce(12) | ciphertext+GCM-tag(keyLen+16)
//
// The encryption key is derived from this machine's /etc/machine-id.
// Encrypted files are ONLY decryptable on the same machine.
func EncryptPrivateKeyDER(keyDER []byte) ([]byte, error) {
	encKey, err := deriveMachineKey()
	if err != nil {
		return nil, fmt.Errorf("derive machine key: %w", err)
	}

	blk, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, keyDER, nil)

	// Layout: version || nonce || ciphertext
	out := make([]byte, 1+len(nonce)+len(ciphertext))
	out[0] = certEncVersion
	copy(out[1:], nonce)
	copy(out[1+len(nonce):], ciphertext)
	return out, nil
}

// EncryptPEM encrypts arbitrary bytes (cert PEM, key PEM, or any blob) with AES-256-GCM
// using the machine-derived key. Same scheme as EncryptPrivateKeyDER; the name reflects
// that it's the primary entry point for PEM files stored on disk.
func EncryptPEM(data []byte) ([]byte, error) {
	return EncryptPrivateKeyDER(data)
}

// DecryptPEM decrypts bytes produced by EncryptPEM (or EncryptPrivateKeyDER).
func DecryptPEM(encrypted []byte) ([]byte, error) {
	return DecryptPrivateKeyDER(encrypted)
}

// LoadEncryptedKeyPair reads the machine-encrypted cert and key from disk,
// decrypts them in memory, and returns a tls.Certificate ready for use.
// The decrypted PEM bytes never touch the filesystem after this call.
func LoadEncryptedKeyPair(certEncFile, keyEncFile string) (tls.Certificate, error) {
	certEnc, err := os.ReadFile(certEncFile) // #nosec G304 -- agent config path, not request input
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read encrypted cert %s: %w", certEncFile, err)
	}
	certPEM, err := DecryptPEM(certEnc)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("decrypt cert: %w", err)
	}

	keyEnc, err := os.ReadFile(keyEncFile) // #nosec G304 -- agent config path, not request input
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read encrypted key %s: %w", keyEncFile, err)
	}
	keyPEM, err := DecryptPEM(keyEnc)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("decrypt key: %w", err)
	}

	return tls.X509KeyPair(certPEM, keyPEM)
}

// DecryptPrivateKeyDER decrypts bytes produced by EncryptPrivateKeyDER.
// Returns an error if the file was encrypted on a different machine or has been tampered with.
func DecryptPrivateKeyDER(encrypted []byte) ([]byte, error) {
	if len(encrypted) < 1 {
		return nil, fmt.Errorf("encrypted key is empty")
	}
	if encrypted[0] != certEncVersion {
		return nil, fmt.Errorf("unsupported key encryption version %d (expected %d)", encrypted[0], certEncVersion)
	}

	encKey, err := deriveMachineKey()
	if err != nil {
		return nil, fmt.Errorf("derive machine key: %w", err)
	}

	blk, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}

	data := encrypted[1:]
	if len(data) < gcm.NonceSize() {
		return nil, fmt.Errorf("encrypted data too short")
	}

	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// This fires if:
		//   - File was encrypted on a different machine (different machine-id)
		//   - File was tampered with (GCM tag mismatch)
		//   - File is corrupted
		return nil, fmt.Errorf("decryption failed — wrong machine or tampered file: %w", err)
	}
	return plain, nil
}
