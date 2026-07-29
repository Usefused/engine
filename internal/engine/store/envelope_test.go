package store

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMasterKeyFromEnv(t *testing.T) {
	os.Unsetenv("FUSED_ENCRYPTION_KEY")
	_, err := MasterKeyFromEnv()
	require.ErrorIs(t, err, ErrInvalidMasterKey)

	os.Setenv("FUSED_ENCRYPTION_KEY", "not-base64")
	_, err = MasterKeyFromEnv()
	require.ErrorIs(t, err, ErrInvalidMasterKey)

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	b64Key := base64.StdEncoding.EncodeToString(key)

	os.Setenv("FUSED_ENCRYPTION_KEY", b64Key)
	loaded, err := MasterKeyFromEnv()
	require.NoError(t, err)
	require.Len(t, loaded, 32)
}

func TestEnvelopeEncryptionRoundTrip(t *testing.T) {
	masterKey := make([]byte, 32)
	io.ReadFull(rand.Reader, masterKey)

	// 1. Wrap DEK
	wrappedDEK, dek, err := WrapDEK(masterKey)
	require.NoError(t, err)
	require.NotEmpty(t, wrappedDEK)
	require.Len(t, dek, 32)
	require.Contains(t, wrappedDEK, "v1:")

	// 2. Encrypt value
	plaintext := "sk_test_12345"
	encryptedVal, err := EncryptWithDEK(dek, plaintext)
	require.NoError(t, err)
	require.NotEqual(t, plaintext, encryptedVal)

	// 3. Unwrap DEK
	unwrappedDEK, err := UnwrapDEK(masterKey, wrappedDEK)
	require.NoError(t, err)
	require.Equal(t, dek, unwrappedDEK)

	// 4. Decrypt value
	decryptedVal, err := DecryptWithDEK(unwrappedDEK, encryptedVal)
	require.NoError(t, err)
	require.Equal(t, plaintext, decryptedVal)
}

func TestUnwrapDEK_WrongKey(t *testing.T) {
	masterKey1 := make([]byte, 32)
	io.ReadFull(rand.Reader, masterKey1)
	masterKey2 := make([]byte, 32)
	io.ReadFull(rand.Reader, masterKey2)

	wrappedDEK, _, err := WrapDEK(masterKey1)
	require.NoError(t, err)

	_, err = UnwrapDEK(masterKey2, wrappedDEK)
	require.Error(t, err)
}

func TestUnwrapDEK_InvalidVersion(t *testing.T) {
	masterKey := make([]byte, 32)
	io.ReadFull(rand.Reader, masterKey)

	_, err := UnwrapDEK(masterKey, "v2:someciphertext")
	require.ErrorContains(t, err, "unsupported dek version")
}
