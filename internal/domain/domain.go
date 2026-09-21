// Package domain implements per-user custom domain mapping (feature 1).
//
// Each OpenList user may bind one or more custom domains. When a request
// arrives at the extension proxy, the Host header is looked up here; if it
// matches a user's domain, that user becomes the acting identity for the
// request (subject to the user being enabled and not disabled in OpenList).
//
// Storage is in the extension's own DB, keyed by OpenList user ID, so it is
// independent of OpenList's schema and forward-compatible.
package domain

import (
	"strings"

	"github.com/sagerenn/openlist/internal/db"
	"gorm.io/gorm"
)

func init() {
	db.RegisterModels(func() []interface{} { return []interface{}{&UserDomain{}} })
}

// UserDomain binds a single hostname to an OpenList user ID. A user may
// own multiple rows (multiple domains). Hostnames are normalized to
// lower-case, without port, and are unique.
type UserDomain struct {
	ID     uint   `json:"id" gorm:"primaryKey"`
	UserID uint   `json:"user_id" gorm:"index;not null"`
	Host   string `json:"host" gorm:"uniqueIndex;size:255;not null"`
}

// Normalize lower-cases a host and strips any :port suffix so that
// "Foo.COM:8080" and "foo.com" map to the same binding.
func Normalize(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(host, ":"); i >= 0 {
		// keep bracketed IPv6 literals intact
		if !strings.HasPrefix(host, "[") {
			host = host[:i]
		}
	}
	return host
}

// Set binds host to userID, replacing any previous binding for that host.
// Returns gorm errors as-is.
func Set(userID uint, host string) error {
	host = Normalize(host)
	if host == "" {
		return gorm.ErrInvalidData
	}
	return db.DB().Where("host = ?", host).
		Assign(UserDomain{UserID: userID}).
		FirstOrCreate(&UserDomain{Host: host}).Error
}

// Unbind removes a host binding. It is a no-op (no error) if the host is
// not bound.
func Unbind(host string) error {
	host = Normalize(host)
	return db.DB().Where("host = ?", host).Delete(&UserDomain{}).Error
}

// LookupUserID returns the OpenList user ID bound to host, or (0, false)
// if no binding exists.
func LookupUserID(host string) (uint, bool) {
	host = Normalize(host)
	var ud UserDomain
	if err := db.DB().Where("host = ?", host).Take(&ud).Error; err != nil {
		return 0, false
	}
	return ud.UserID, true
}

// ListByUser returns all domains bound to userID.
func ListByUser(userID uint) ([]UserDomain, error) {
	var uds []UserDomain
	err := db.DB().Where("user_id = ?", userID).Find(&uds).Error
	return uds, err
}

// All returns every binding (used for diagnostics / admin views).
func All() ([]UserDomain, error) {
	var uds []UserDomain
	err := db.DB().Find(&uds).Error
	return uds, err
}
