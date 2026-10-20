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

func TestPrefixMatches(t *testing.T) {
	cases := []struct {
		prefix, p string
		want      bool
	}{
		{"/pshare", "/pshare", true},
		{"/pshare", "/pshare/x.txt", true},
		{"/pshare", "/pshare/a/b.txt", true},
		{"/pshare", "/psharex", false}, // segment boundary
		{"/pshare", "/other/x.txt", false},
		{"/pshare/", "/pshare/x.txt", true}, // trailing slash tolerated
		{"pshare", "/pshare", true},         // missing lead tolerated
		{"", "/pshare/x.txt", false},        // empty prefix never matches
		{"/", "/anything", true},            // bare "/" matches everything
		{"/", "/", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/bx", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, PrefixMatches(tc.prefix, tc.p), "PrefixMatches(%q, %q)", tc.prefix, tc.p)
	}
}

func TestNormalizePrefix(t *testing.T) {
	assert.Equal(t, "", NormalizePrefix(""))
	assert.Equal(t, "/", NormalizePrefix("/"))
	assert.Equal(t, "/", NormalizePrefix("///"))
	assert.Equal(t, "/pshare", NormalizePrefix("/pshare"))
	assert.Equal(t, "/pshare", NormalizePrefix("pshare/"))
	assert.Equal(t, "/a/b", NormalizePrefix("/a/b/"))
}

func TestRemapPath(t *testing.T) {
	assert.Equal(t, "/s3/a/b.txt", RemapPath("/s3", "/pshare", "/pshare/a/b.txt"))
	assert.Equal(t, "/s3/a b.txt", RemapPath("/s3", "/pshare", "/pshare/a b.txt"))
	// The prefix root maps to the bare mount.
	assert.Equal(t, "/s3", RemapPath("/s3", "/pshare", "/pshare"))
	assert.Equal(t, "/s3", RemapPath("/s3", "/", "/"))
	assert.Equal(t, "/s3/x", RemapPath("/s3", "/", "/x"))
	// Mounts with trailing slashes are tolerated.
	assert.Equal(t, "/s3/x", RemapPath("/s3/", "/pshare", "/pshare/x"))
}

func TestMatchGroupByPathLongestPrefix(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	short := &Group{Name: "short", UserID: 60, PathPrefix: "/p"}
	require.NoError(t, CreateGroup(short))
	long := &Group{Name: "long", UserID: 60, PathPrefix: "/p/share"}
	require.NoError(t, CreateGroup(long))
	headerOnly := &Group{Name: "header-only", UserID: 60, PathPrefix: ""}
	require.NoError(t, CreateGroup(headerOnly))
	otherUser := &Group{Name: "other", UserID: 61, PathPrefix: "/p/share"}
	require.NoError(t, CreateGroup(otherUser))

	g, ok := MatchGroupByPath(60, "/p/share/a.txt")
	require.True(t, ok)
	assert.Equal(t, "long", g.Name, "longest prefix wins")

	g, ok = MatchGroupByPath(60, "/p/other.txt")
	require.True(t, ok)
	assert.Equal(t, "short", g.Name)

	_, ok = MatchGroupByPath(60, "/unrelated")
	assert.False(t, ok)

	// An empty prefix never activates by path, and another user's groups
	// never activate for this user.
	_, ok = MatchGroupByPath(60, "/p/share/a.txt/anything-still-long")
	require.True(t, ok)
	_, ok = MatchGroupByPath(61, "/p/share/a.txt")
	require.True(t, ok)
	_, ok = MatchGroupByPath(62, "/p/share/a.txt")
	assert.False(t, ok)
}

func TestUploadRecordCRUD(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, RecordUpload(&UploadRecord{
		UserID: 70, GroupID: 1, GroupName: "g", SourcePath: "/p/x.txt",
		TargetMount: "/s3", TargetPath: "/s3/x.txt", Size: 7,
	}))
	require.NoError(t, RecordUpload(&UploadRecord{
		UserID: 70, GroupID: 1, GroupName: "g", SourcePath: "/p/y.txt",
		TargetMount: "/oss", TargetPath: "/oss/y.txt", Size: 8,
	}))
	// Another user's record.
	require.NoError(t, RecordUpload(&UploadRecord{
		UserID: 71, GroupID: 1, GroupName: "g", SourcePath: "/p/z.txt",
		TargetMount: "/s3", TargetPath: "/s3/z.txt",
	}))

	recs, err := ListUploads(70, 0)
	require.NoError(t, err)
	require.Len(t, recs, 2)
	// Newest first.
	assert.Equal(t, "/p/y.txt", recs[0].SourcePath)

	recs, err = ListUploads(0, 1)
	require.NoError(t, err)
	require.Len(t, recs, 1, "limit applies; user 0 lists all users")

	rec, err := FindUploadByPath(70, "/p/x.txt")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "/s3/x.txt", rec.TargetPath)

	rec, err = FindUploadByPath(70, "/p/none.txt")
	require.NoError(t, err)
	assert.Nil(t, rec, "missing record is (nil, nil), not an error")

	rec, err = FindUploadByPath(71, "/p/x.txt")
	require.NoError(t, err)
	assert.Nil(t, rec, "records are per-user")

	// Re-upload overwrites: newest record wins.
	require.NoError(t, RecordUpload(&UploadRecord{
		UserID: 70, GroupID: 1, GroupName: "g", SourcePath: "/p/x.txt",
		TargetMount: "/oss", TargetPath: "/oss/x.txt",
	}))
	rec, err = FindUploadByPath(70, "/p/x.txt")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "/oss/x.txt", rec.TargetPath, "newest record wins")

	// Purge by dir/name: exact match only. Two records exist for /p/x.txt
	// (the original and the overwrite — history is kept), both go.
	n, err := DeleteUploadsFor(70, "/p", "x.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	_, err = FindUploadByPath(70, "/p/x.txt")
	require.NoError(t, err)
	// y.txt survives.
	rec, err = FindUploadByPath(70, "/p/y.txt")
	require.NoError(t, err)
	require.NotNil(t, rec)

	// Purge a subtree.
	n, err = DeleteUploadsUnder(70, "/p")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	recs, err = ListUploads(70, 0)
	require.NoError(t, err)
	assert.Empty(t, recs)

	// Single-record delete.
	recs, err = ListUploads(71, 0)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.NoError(t, DeleteUpload(recs[0].ID))
	recs, err = ListUploads(71, 0)
	require.NoError(t, err)
	assert.Empty(t, recs)
}
