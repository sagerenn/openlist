// Package ttl implements per-user file time-to-live with automatic
// deletion (feature 2).
//
// A user may enable TTL for files they upload. Two expiry modes are
// supported:
//
//   - ModeFixed:   the file expires at a fixed instant = uploadTime + Duration.
//   - ModeAccess:  the file expires Duration after its LAST access (download);
//                  each access slides the expiry forward.
//
// When a file is uploaded through the extension proxy, a FileTTLRecord is
// created (if the uploading user has TTL enabled). When a file is
// downloaded, the record's LastAccess is refreshed (ModeAccess only). A
// background Reaper periodically scans for expired records and deletes the
// corresponding file from OpenList via the admin API, then removes the
// record.
//
// All state lives in the extension DB, keyed by OpenList user ID, so it is
// independent of OpenList's schema and forward-compatible.
package ttl

import (
	"time"

	"github.com/sagerenn/openlist/internal/db"
	"gorm.io/gorm"
)

func init() {
	db.RegisterModels(func() []interface{} {
		return []interface{}{&UserTTLConfig{}, &FileTTLRecord{}}
	})
}

// Mode controls how a file's expiry is computed.
type Mode string

const (
	// ModeDisabled means TTL is off for the user; no records are kept.
	ModeDisabled Mode = ""
	// ModeFixed expires at uploadTime + Duration.
	ModeFixed Mode = "fixed"
	// ModeAccess expires Duration after the last access (sliding window).
	ModeAccess Mode = "access"
)

// UserTTLConfig is the per-user TTL setting. At most one row per user.
type UserTTLConfig struct {
	UserID    uint      `json:"user_id" gorm:"primaryKey"`
	Mode      Mode      `json:"mode" gorm:"size:16"`
	Duration  int64     `json:"duration_seconds"` // seconds; 0 invalid unless disabled
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FileTTLRecord tracks a single file subject to TTL.
//
// Path is the full logical path inside OpenList (the path the user sees,
// i.e. already joined with the user's base path). UserID is the uploading
// user. ExpiresAt is precomputed for ModeFixed and recomputed on each
// access for ModeAccess.
type FileTTLRecord struct {
	ID         uint      `json:"id" gorm:"primaryKey"`
	UserID     uint      `json:"user_id" gorm:"index;not null"`
	Path       string    `json:"path" gorm:"uniqueIndex;size:1024;not null"`
	Mode       Mode      `json:"mode" gorm:"size:16"`
	UploadedAt time.Time `json:"uploaded_at"`
	LastAccess time.Time `json:"last_access"`
	ExpiresAt  time.Time `json:"expires_at" gorm:"index"`
}

// GetConfig returns the user's TTL config, or a zero (disabled) value if
// none exists.
func GetConfig(userID uint) (UserTTLConfig, error) {
	var cfg UserTTLConfig
	err := db.DB().Where("user_id = ?", userID).Take(&cfg).Error
	if err == gorm.ErrRecordNotFound {
		return UserTTLConfig{UserID: userID}, nil
	}
	return cfg, err
}

// SetConfig upserts the user's TTL config.
func SetConfig(cfg UserTTLConfig) error {
	cfg.UpdatedAt = time.Now()
	if cfg.Enabled && cfg.Mode != ModeDisabled && cfg.Duration <= 0 {
		return gorm.ErrInvalidData
	}
	if !cfg.Enabled {
		cfg.Mode = ModeDisabled
	}
	return db.DB().Save(&cfg).Error
}

// computeExpiry returns the expiry time for a record given its mode.
func computeExpiry(cfg UserTTLConfig, now time.Time) time.Time {
	d := time.Duration(cfg.Duration) * time.Second
	switch cfg.Mode {
	case ModeFixed:
		return now.Add(d)
	case ModeAccess:
		return now.Add(d)
	default:
		return time.Time{} // zero => never expires (should not happen when enabled)
	}
}

// RecordUpload creates (or refreshes) a TTL record for a newly uploaded
// file. It is a no-op if the user has TTL disabled.
func RecordUpload(userID uint, path string, now time.Time) error {
	cfg, err := GetConfig(userID)
	if err != nil {
		return err
	}
	if !cfg.Enabled || cfg.Mode == ModeDisabled {
		return nil
	}
	rec := FileTTLRecord{
		UserID:     userID,
		Path:       path,
		Mode:       cfg.Mode,
		UploadedAt: now,
		LastAccess: now,
		ExpiresAt:  computeExpiry(cfg, now),
	}
	// Upsert by path.
	return db.DB().Where("path = ?", path).
		Assign(rec).
		FirstOrCreate(&FileTTLRecord{Path: path}).Error
}

// RecordAccess refreshes LastAccess and slides ExpiresAt forward for
// ModeAccess records. No-op for ModeFixed and for users without TTL.
func RecordAccess(path string, now time.Time) error {
	var rec FileTTLRecord
	if err := db.DB().Where("path = ?", path).Take(&rec).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil
		}
		return err
	}
	if rec.Mode != ModeAccess {
		return nil
	}
	rec.LastAccess = now
	cfg, err := GetConfig(rec.UserID)
	if err != nil {
		return err
	}
	rec.ExpiresAt = computeExpiry(cfg, now)
	return db.DB().Save(&rec).Error
}

// RemoveRecord deletes the TTL record for path (e.g. after the file itself
// was deleted). No-op if no record exists.
func RemoveRecord(path string) error {
	return db.DB().Where("path = ?", path).Delete(&FileTTLRecord{}).Error
}

// Expired returns all records whose ExpiresAt is at or before now, in
// ascending expiry order.
func Expired(now time.Time, limit int) ([]FileTTLRecord, error) {
	var recs []FileTTLRecord
	q := db.DB().Where("expires_at <= ?", now).Order("expires_at asc")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Find(&recs).Error
	return recs, err
}

// HasRecord reports whether a TTL record exists for path.
func HasRecord(path string) (bool, error) {
	var rec FileTTLRecord
	err := db.DB().Where("path = ?", path).Take(&rec).Error
	if err == gorm.ErrRecordNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
