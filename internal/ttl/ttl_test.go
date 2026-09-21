package ttl

import (
	"testing"
	"time"

	"github.com/sagerenn/openlist/internal/db"
	"github.com/sagerenn/openlist/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetConfigValidation(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	// Enabled with no duration is invalid.
	err := SetConfig(UserTTLConfig{UserID: 1, Enabled: true, Mode: ModeFixed, Duration: 0})
	assert.Error(t, err)

	// Disabled is always valid and clears mode.
	require.NoError(t, SetConfig(UserTTLConfig{UserID: 1, Enabled: true, Mode: ModeFixed, Duration: 60}))
	require.NoError(t, SetConfig(UserTTLConfig{UserID: 1, Enabled: false, Mode: ModeFixed, Duration: 60}))
	cfg, err := GetConfig(1)
	require.NoError(t, err)
	assert.False(t, cfg.Enabled)
	assert.Equal(t, ModeDisabled, cfg.Mode)
}

func TestRecordUploadFixed(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, SetConfig(UserTTLConfig{UserID: 1, Enabled: true, Mode: ModeFixed, Duration: 100}))
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, RecordUpload(1, "/photos/a.jpg", now))

	rec, err := HasRecord("/photos/a.jpg")
	require.NoError(t, err)
	assert.True(t, rec)

	// Expiry should be now + 100s.
	var r FileTTLRecord
	require.NoError(t, dbFirstByPath("/photos/a.jpg", &r))
	assert.Equal(t, now.Add(100*time.Second), r.ExpiresAt)
	assert.Equal(t, ModeFixed, r.Mode)
}

func TestRecordUploadDisabledIsNoop(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, RecordUpload(2, "/x.txt", time.Now()))
	has, err := HasRecord("/x.txt")
	require.NoError(t, err)
	assert.False(t, has)
}

func TestRecordAccessSlidesAccessMode(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, SetConfig(UserTTLConfig{UserID: 3, Enabled: true, Mode: ModeAccess, Duration: 50}))
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, RecordUpload(3, "/docs/a.pdf", t0))

	var r FileTTLRecord
	require.NoError(t, dbFirstByPath("/docs/a.pdf", &r))
	firstExpiry := r.ExpiresAt
	assert.Equal(t, t0.Add(50*time.Second), firstExpiry)

	// Access later slides the window.
	t1 := t0.Add(30 * time.Second)
	require.NoError(t, RecordAccess("/docs/a.pdf", t1))
	require.NoError(t, dbFirstByPath("/docs/a.pdf", &r))
	assert.Equal(t, t1.Add(50*time.Second), r.ExpiresAt)
	assert.True(t, r.ExpiresAt.After(firstExpiry))
	assert.Equal(t, t1, r.LastAccess)
}

func TestRecordAccessNoopForFixedMode(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, SetConfig(UserTTLConfig{UserID: 4, Enabled: true, Mode: ModeFixed, Duration: 100}))
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, RecordUpload(4, "/f.txt", t0))

	require.NoError(t, RecordAccess("/f.txt", t0.Add(40*time.Second)))
	var r FileTTLRecord
	require.NoError(t, dbFirstByPath("/f.txt", &r))
	assert.Equal(t, t0.Add(100*time.Second), r.ExpiresAt, "fixed mode must not slide")
}

func TestExpiredReturnsOnlyDue(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, SetConfig(UserTTLConfig{UserID: 5, Enabled: true, Mode: ModeFixed, Duration: 10}))
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, RecordUpload(5, "/old.txt", now))                 // expires now+10s
	require.NoError(t, RecordUpload(5, "/future.txt", now.Add(1*time.Hour))) // expires far future

	expired, err := Expired(now.Add(20*time.Second), 100)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, "/old.txt", expired[0].Path)
}

func TestRemoveRecord(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	require.NoError(t, SetConfig(UserTTLConfig{UserID: 6, Enabled: true, Mode: ModeFixed, Duration: 10}))
	require.NoError(t, RecordUpload(6, "/r.txt", time.Now()))
	require.NoError(t, RemoveRecord("/r.txt"))
	has, err := HasRecord("/r.txt")
	require.NoError(t, err)
	assert.False(t, has)
}

// dbFirstByPath is a tiny helper to fetch a record by path.
func dbFirstByPath(path string, r *FileTTLRecord) error {
	return db.DB().Where("path = ?", path).First(r).Error
}
