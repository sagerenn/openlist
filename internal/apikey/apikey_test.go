package apikey

import (
	"testing"

	"github.com/sagerenn/openlist/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateAndVerify(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	ck, err := Create(1, "test-key", []Scope{ScopeRead, ScopeList})
	require.NoError(t, err)
	assert.NotEmpty(t, ck.Secret, "secret must be returned once")
	assert.Len(t, ck.Secret, secretLen*2, "hex-encoded 24 bytes")

	// Hash is stored, not the secret.
	assert.NotEqual(t, ck.Secret, ck.KeyHash)

	// Verify the secret resolves back to the key.
	k, err := Verify(ck.Secret)
	require.NoError(t, err)
	assert.Equal(t, ck.ID, k.ID)
	assert.Equal(t, uint(1), k.UserID)
}

func TestVerifyRejectsBogusSecret(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	_, err := Create(1, "k", []Scope{ScopeAll})
	require.NoError(t, err)

	_, err = Verify("deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	assert.ErrorIs(t, err, ErrInvalidKey)
}

func TestHasScope(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	ck, err := Create(2, "scoped", []Scope{ScopeRead, ScopeList})
	require.NoError(t, err)
	k, _ := Verify(ck.Secret)

	assert.True(t, k.HasScope(ScopeRead))
	assert.True(t, k.HasScope(ScopeList))
	assert.False(t, k.HasScope(ScopeWrite))
	assert.False(t, k.HasScope(ScopeRemove))

	// Wildcard key.
	ck2, err := Create(2, "wild", []Scope{ScopeAll})
	require.NoError(t, err)
	k2, _ := Verify(ck2.Secret)
	assert.True(t, k2.HasScope(ScopeWrite))
	assert.True(t, k2.HasScope(ScopeRemove))
}

func TestDisableKeyBlocksVerify(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	ck, err := Create(3, "dis", []Scope{ScopeAll})
	require.NoError(t, err)

	require.NoError(t, SetEnabled(3, ck.ID, false))

	_, err = Verify(ck.Secret)
	assert.ErrorIs(t, err, ErrInvalidKey, "disabled key must not verify")
}

func TestDeleteKey(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	ck, err := Create(4, "del", []Scope{ScopeAll})
	require.NoError(t, err)

	require.NoError(t, Delete(4, ck.ID))
	_, err = Verify(ck.Secret)
	assert.ErrorIs(t, err, ErrInvalidKey)

	// Deleting another user's key fails.
	ck2, _ := Create(5, "other", []Scope{ScopeAll})
	err = Delete(4, ck2.ID)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListByUser(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	_, _ = Create(6, "a", []Scope{ScopeRead})
	_, _ = Create(6, "b", []Scope{ScopeRead})
	_, _ = Create(7, "c", []Scope{ScopeRead})

	ks, err := ListByUser(6)
	require.NoError(t, err)
	assert.Len(t, ks, 2)
}

func TestFromHeader(t *testing.T) {
	hdr := func(k string) string {
		m := map[string]string{
			"X-Api-Key":     "abc123",
			"Authorization": "Bearer xyz789",
		}
		return m[k]
	}
	got, err := FromHeader(hdr)
	require.NoError(t, err)
	assert.Equal(t, "abc123", got, "X-Api-Key takes precedence")

	hdr2 := func(k string) string {
		if k == "X-Api-Key" {
			return ""
		}
		return "Bearer xyz789"
	}
	got, err = FromHeader(hdr2)
	require.NoError(t, err)
	assert.Equal(t, "xyz789", got)

	hdr3 := func(k string) string { return "" }
	_, err = FromHeader(hdr3)
	assert.ErrorIs(t, err, ErrEmpty)
}
