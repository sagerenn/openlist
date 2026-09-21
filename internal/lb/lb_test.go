package lb

import (
	"testing"

	"github.com/sagerenn/openlist/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeGroup(t *testing.T, userID uint, name string, strategy Strategy, members []GroupMember) Group {
	t.Helper()
	g := &Group{Name: name, UserID: userID, Strategy: strategy}
	require.NoError(t, CreateGroup(g))
	for _, m := range members {
		m.GroupID = g.ID
		require.NoError(t, AddMember(&m))
	}
	return *g
}

func TestRoundRobinDistribution(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	g := makeGroup(t, 1, "rr", StrategyRoundRobin, []GroupMember{
		{MountPath: "/s3", Weight: 1},
		{MountPath: "/oss", Weight: 1},
		{MountPath: "/cos", Weight: 1},
	})

	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		m, err := Select(g)
		require.NoError(t, err)
		counts[m.MountPath]++
	}
	assert.Equal(t, 3, counts["/s3"])
	assert.Equal(t, 3, counts["/oss"])
	assert.Equal(t, 3, counts["/cos"])
}

func TestRoundRobinWeighted(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	g := makeGroup(t, 2, "rrw", StrategyRoundRobin, []GroupMember{
		{MountPath: "/a", Weight: 3},
		{MountPath: "/b", Weight: 1},
	})

	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		m, err := Select(g)
		require.NoError(t, err)
		counts[m.MountPath]++
	}
	assert.Equal(t, 6, counts["/a"], "weight 3:1 over 8 picks => 6 and 2")
	assert.Equal(t, 2, counts["/b"])
}

func TestLeastUsedStrategy(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	g := makeGroup(t, 3, "lu", StrategyLeastUsed, []GroupMember{
		{MountPath: "/x", Weight: 1},
		{MountPath: "/y", Weight: 1},
	})

	// First pick: both have 0, picks first (x).
	m, err := Select(g)
	require.NoError(t, err)
	assert.Equal(t, "/x", m.MountPath)

	// Now x has 1, y has 0 => should pick y.
	m, err = Select(g)
	require.NoError(t, err)
	assert.Equal(t, "/y", m.MountPath)

	// Both have 1 => picks first (x).
	m, err = Select(g)
	require.NoError(t, err)
	assert.Equal(t, "/x", m.MountPath)
}

func TestRandomStrategyPicksValidMember(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	g := makeGroup(t, 4, "rnd", StrategyRandom, []GroupMember{
		{MountPath: "/p", Weight: 1},
		{MountPath: "/q", Weight: 1},
	})
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		m, err := Select(g)
		require.NoError(t, err)
		seen[m.MountPath] = true
	}
	// Over 50 picks both members should appear (probabilistically near-certain).
	assert.True(t, seen["/p"] && seen["/q"], "random should hit both members: %v", seen)
}

func TestSelectEmptyGroupErrors(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	g := makeGroup(t, 5, "empty", StrategyRoundRobin, nil)
	_, err := Select(g)
	assert.ErrorIs(t, err, ErrNoMembers)
}

func TestGetGroupByName(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	g := makeGroup(t, 6, "named", StrategyRoundRobin, []GroupMember{{MountPath: "/z", Weight: 1}})
	got, err := GetGroupByName(6, "named")
	require.NoError(t, err)
	assert.Equal(t, g.ID, got.ID)

	_, err = GetGroupByName(6, "nope")
	assert.ErrorIs(t, err, ErrGroupNotFound)

	// Another user cannot see this group.
	_, err = GetGroupByName(7, "named")
	assert.ErrorIs(t, err, ErrGroupNotFound)
}

func TestDeleteGroupRemovesMembers(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	g := makeGroup(t, 8, "del", StrategyRoundRobin, []GroupMember{
		{MountPath: "/m1", Weight: 1},
		{MountPath: "/m2", Weight: 1},
	})
	require.NoError(t, DeleteGroup(g.ID))

	members, err := GroupMembers(g.ID)
	require.NoError(t, err)
	assert.Empty(t, members)

	_, err = GetGroup(g.ID)
	assert.ErrorIs(t, err, ErrGroupNotFound)
}
