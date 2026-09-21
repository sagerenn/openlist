package listperm

import (
	"testing"

	"github.com/sagerenn/openlist/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultsToPermitted(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	p, err := Get(1)
	require.NoError(t, err)
	assert.True(t, p.CanList, "default CanList should be true")
	assert.True(t, p.CanRead, "default CanRead should be true")
}

func TestSetAndQuery(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	// User 2 can read/write but NOT list.
	require.NoError(t, Set(UserListPerm{UserID: 2, CanList: false, CanRead: true}))

	ok, err := MayList(2)
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = MayRead(2)
	require.NoError(t, err)
	assert.True(t, ok)

	// User 3 is write-only (drop box): no list, no read.
	require.NoError(t, Set(UserListPerm{UserID: 3, CanList: false, CanRead: false}))
	ok, err = MayRead(3)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestSetUpsert(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, Set(UserListPerm{UserID: 4, CanList: true, CanRead: true}))
	require.NoError(t, Set(UserListPerm{UserID: 4, CanList: false, CanRead: true}))
	ok, err := MayList(4)
	require.NoError(t, err)
	assert.False(t, ok)
}
