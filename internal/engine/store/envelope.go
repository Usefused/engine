package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

var (
	// ErrInvalidMasterKey is returned when the FUSED_ENCRYPTION_KEY is missing or invalid.
	ErrInvalidMasterKey = errors.New("FUSED_ENCRYPTION_KEY must be exactly 32 bytes (base64 encoded)")
	// ErrInvalidCiphertext is returned when AES-GCM decryption fails.
	ErrInvalidCiphertext = errors.New("invalid ciphertext")
)

// MasterKeyFromEnv loads the master key from FUSED_ENCRYPTION_KEY.
// We expect it to be a base64 encoded 32-byte key.
func MasterKeyFromEnv() ([]byte, error) {
	keyStr := os.Getenv("FUSED_ENCRYPTION_KEY")
	if keyStr == "" {
		return nil, ErrInvalidMasterKey
	}
	key, err := base64.StdEncoding.DecodeString(keyStr)
	if err != nil || len(key) != 32 {
		return nil, ErrInvalidMasterKey
	}
	return key, nil
}

// WrapDEK generates a new random 32-byte DEK, wraps it with the master key,
// and returns the wrapped string ("v1:<base64>") and the raw DEK bytes.
func WrapDEK(masterKey []byte) (string, []byte, error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return "", nil, fmt.Errorf("failed to generate dek: %w", err)
	}

	wrapped, err := encrypt(masterKey, dek)
	if err != nil {
		return "", nil, err
	}

	return "v1:" + wrapped, dek, nil
}

// UnwrapDEK takes the wrapped DEK ("v1:<base64>") and decrypts it with the master key.
func UnwrapDEK(masterKey []byte, wrappedDEK string) ([]byte, error) {
	if !strings.HasPrefix(wrappedDEK, "v1:") {
		return nil, errors.New("unsupported dek version")
	}
	ciphertext := strings.TrimPrefix(wrappedDEK, "v1:")
	return decrypt(masterKey, ciphertext)
}

// EncryptWithDEK encrypts plaintext using the given raw DEK.
func EncryptWithDEK(dek []byte, plaintext string) (string, error) {
	return encrypt(dek, []byte(plaintext))
}

// DecryptWithDEK decrypts ciphertext using the given raw DEK.
func DecryptWithDEK(dek []byte, ciphertext string) (string, error) {
	plaintext, err := decrypt(dek, ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// encrypt performs AES-GCM encryption and returns base64 string
func encrypt(key []byte, plaintext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decrypt performs AES-GCM decryption from a base64 string
func decrypt(key []byte, ciphertextStr string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextStr)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, ErrInvalidCiphertext
	}

	nonce, ciphertextBytes := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertextBytes, nil)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}

	return plaintext, nil
}
