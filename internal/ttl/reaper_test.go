package ttl

import (
	"context"
	"testing"
	"time"

	"github.com/sagerenn/openlist/internal/openlist"
	"github.com/sagerenn/openlist/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReaperSweepDeletesExpired(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()
	stub := testutil.NewStubOpenList("tok")
	defer stub.Close()

	// User with fixed TTL of 1s; upload two files, one expired, one not.
	require.NoError(t, SetConfig(UserTTLConfig{UserID: 100, Enabled: true, Mode: ModeFixed, Duration: 1}))
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, RecordUpload(100, "/reap/old.txt", now))
	require.NoError(t, RecordUpload(100, "/reap/new.txt", now.Add(2*time.Hour)))

	r := NewReaper(openlist.New(stub.URL(), "tok"), time.Second, 50)
	r.SetNow(func() time.Time { return now.Add(2 * time.Second) })

	n := r.SweepOnce(context.Background())
	assert.Equal(t, 1, n, "only the expired file should be deleted")

	removes := stub.SnapshotRemoves()
	require.Len(t, removes, 1)
	assert.Equal(t, "/reap", removes[0].Dir)
	assert.Equal(t, []string{"old.txt"}, removes[0].Names)

	// Record for old.txt is gone; new.txt remains.
	has, err := HasRecord("/reap/old.txt")
	require.NoError(t, err)
	assert.False(t, has)
	has, err = HasRecord("/reap/new.txt")
	require.NoError(t, err)
	assert.True(t, has)
}

func TestReaperSweepNothingExpired(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()
	stub := testutil.NewStubOpenList("tok")
	defer stub.Close()

	require.NoError(t, SetConfig(UserTTLConfig{UserID: 101, Enabled: true, Mode: ModeFixed, Duration: 3600}))
	require.NoError(t, RecordUpload(101, "/reap/future.txt", time.Now()))

	r := NewReaper(openlist.New(stub.URL(), "tok"), time.Second, 50)
	n := r.SweepOnce(context.Background())
	assert.Equal(t, 0, n)
	assert.Empty(t, stub.SnapshotRemoves())
}

func TestReaperPrunesEmptyParentDirs(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()
	stub := testutil.NewStubOpenList("tok")
	defer stub.Close()

	// A share-style upload: a per-share directory directly under a mount,
	// holding one file. When the file expires, the directory is clutter —
	// the reaper must prune it once the listing comes back empty.
	require.NoError(t, SetConfig(UserTTLConfig{UserID: 102, Enabled: true, Mode: ModeFixed, Duration: 1}))
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, RecordUpload(102, "/infini/share1/gone.txt", now))

	r := NewReaper(openlist.New(stub.URL(), "tok"), time.Second, 50)
	r.SetNow(func() time.Time { return now.Add(2 * time.Second) })

	assert.Equal(t, 1, r.SweepOnce(context.Background()))

	removes := stub.SnapshotRemoves()
	require.Len(t, removes, 2, "the file, then the now-empty directory")
	assert.Equal(t, "/infini/share1", removes[0].Dir)
	assert.Equal(t, []string{"gone.txt"}, removes[0].Names)
	assert.Equal(t, "/infini", removes[1].Dir)
	assert.Equal(t, []string{"share1"}, removes[1].Names)

	has, err := HasRecord("/infini/share1/gone.txt")
	require.NoError(t, err)
	assert.False(t, has)
}

func TestReaperNeverPrunesRootLevelDirs(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()
	stub := testutil.NewStubOpenList("tok")
	defer stub.Close()

	// A file directly under a root-level directory: that directory may be a
	// storage mount root, so it is never pruned even when empty.
	require.NoError(t, SetConfig(UserTTLConfig{UserID: 103, Enabled: true, Mode: ModeFixed, Duration: 1}))
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, RecordUpload(103, "/s3/gone.txt", now))

	r := NewReaper(openlist.New(stub.URL(), "tok"), time.Second, 50)
	r.SetNow(func() time.Time { return now.Add(2 * time.Second) })

	assert.Equal(t, 1, r.SweepOnce(context.Background()))

	removes := stub.SnapshotRemoves()
	require.Len(t, removes, 1, "only the file; its parent sits at root level")
	assert.Equal(t, "/s3", removes[0].Dir)
	assert.Equal(t, []string{"gone.txt"}, removes[0].Names)
}

func TestReaperKeepsDirWithRemainingEntries(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()
	stub := testutil.NewStubOpenList("tok")
	defer stub.Close()

	// A parent that still has entries is left alone: OpenList's remove on
	// a directory is a recursive RemoveAll, so the emptiness check is what
	// keeps pruning from taking live files with it.
	stub.SetListContent([]map[string]interface{}{{"name": "live.txt", "is_dir": false}})

	require.NoError(t, SetConfig(UserTTLConfig{UserID: 104, Enabled: true, Mode: ModeFixed, Duration: 1}))
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, RecordUpload(104, "/infini/share2/gone.txt", now))

	r := NewReaper(openlist.New(stub.URL(), "tok"), time.Second, 50)
	r.SetNow(func() time.Time { return now.Add(2 * time.Second) })

	assert.Equal(t, 1, r.SweepOnce(context.Background()))

	removes := stub.SnapshotRemoves()
	require.Len(t, removes, 1, "only the file; its parent still has entries")
	assert.Equal(t, "/infini/share2", removes[0].Dir)
	assert.Equal(t, []string{"gone.txt"}, removes[0].Names)
}

func TestReaperStartStop(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()
	stub := testutil.NewStubOpenList("tok")
	defer stub.Close()

	r := NewReaper(openlist.New(stub.URL(), "tok"), 50*time.Millisecond, 10)
	r.Start()
	// Let it tick at least once.
	time.Sleep(120 * time.Millisecond)
	r.Stop()
	// Stop must return (not hang).
}
