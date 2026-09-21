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
