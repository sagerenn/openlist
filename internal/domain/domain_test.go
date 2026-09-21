package domain

import (
	"testing"

	"github.com/sagerenn/openlist/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Foo.COM:8080", "foo.com"},
		{"  Bar.Example.org  ", "bar.example.org"},
		{"baz.org", "baz.org"},
		{"", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, Normalize(c.in))
	}
}

func TestSetAndLookup(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, Set(1, "alice.example.com"))
	require.NoError(t, Set(2, "bob.example.com:443"))

	uid, ok := LookupUserID("alice.example.com")
	require.True(t, ok)
	assert.Equal(t, uint(1), uid)

	uid, ok = LookupUserID("ALICE.EXAMPLE.COM") // case-insensitive
	require.True(t, ok)
	assert.Equal(t, uint(1), uid)

	uid, ok = LookupUserID("bob.example.com") // port stripped
	require.True(t, ok)
	assert.Equal(t, uint(2), uid)

	_, ok = LookupUserID("nobody.example.com")
	assert.False(t, ok)
}

func TestSetRebindsExistingHost(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, Set(1, "shared.example.com"))
	require.NoError(t, Set(2, "shared.example.com")) // rebind to user 2

	uid, ok := LookupUserID("shared.example.com")
	require.True(t, ok)
	assert.Equal(t, uint(2), uid, "host should now point to user 2")
}

func TestUnbind(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, Set(1, "gone.example.com"))
	require.NoError(t, Unbind("gone.example.com"))
	_, ok := LookupUserID("gone.example.com")
	assert.False(t, ok)

	// Unbinding a non-existent host is a no-op.
	require.NoError(t, Unbind("never-bound.example.com"))
}

func TestListByUser(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, Set(3, "a.example.com"))
	require.NoError(t, Set(3, "b.example.com"))
	require.NoError(t, Set(4, "c.example.com"))

	uds, err := ListByUser(3)
	require.NoError(t, err)
	assert.Len(t, uds, 2)
}
