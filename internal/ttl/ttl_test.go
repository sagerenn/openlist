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

func TestRecordUploadWithTTLOverride(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	// User default is 100s fixed.
	require.NoError(t, SetConfig(UserTTLConfig{UserID: 7, Enabled: true, Mode: ModeFixed, Duration: 100}))
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// No override -> user default (now + 100s).
	require.NoError(t, RecordUploadWithTTL(7, "/dflt.txt", now, 0))
	var dflt FileTTLRecord
	require.NoError(t, dbFirstByPath("/dflt.txt", &dflt))
	assert.Equal(t, now.Add(100*time.Second), dflt.ExpiresAt)

	// Override of 30s -> now + 30s, not the user's 100s.
	require.NoError(t, RecordUploadWithTTL(7, "/ovr.txt", now, 30))
	var ovr FileTTLRecord
	require.NoError(t, dbFirstByPath("/ovr.txt", &ovr))
	assert.Equal(t, now.Add(30*time.Second), ovr.ExpiresAt)

	// Negative override is ignored -> user default applies.
	require.NoError(t, RecordUploadWithTTL(7, "/neg.txt", now, -5))
	var neg FileTTLRecord
	require.NoError(t, dbFirstByPath("/neg.txt", &neg))
	assert.Equal(t, now.Add(100*time.Second), neg.ExpiresAt)
}

func TestRecordUploadWithTTLOverrideNoopWhenDisabled(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	// User has TTL off; an override cannot force it on.
	require.NoError(t, RecordUploadWithTTL(8, "/off.txt", time.Now(), 30))
	has, err := HasRecord("/off.txt")
	require.NoError(t, err)
	assert.False(t, has)
}

func TestRecordUploadWithTTLOverrideAccessMode(t *testing.T) {
	cleanup := testutil.SetupDB(t)
	defer cleanup()

	// User in access mode with 50s default; override to 20s keeps access mode
	// but uses the per-file duration.
	require.NoError(t, SetConfig(UserTTLConfig{UserID: 9, Enabled: true, Mode: ModeAccess, Duration: 50}))
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, RecordUploadWithTTL(9, "/a.txt", t0, 20))
	var r FileTTLRecord
	require.NoError(t, dbFirstByPath("/a.txt", &r))
	assert.Equal(t, t0.Add(20*time.Second), r.ExpiresAt)
	assert.Equal(t, ModeAccess, r.Mode)
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
