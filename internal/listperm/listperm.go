// Package listperm implements per-user file-listing permission control
// (feature 3).
//
// Some users should be able to read and write individual files (by exact
// path) but NOT enumerate the contents of directories. This package stores
// a per-user flag; the proxy middleware consults it to decide whether a
// directory-listing request is allowed.
//
// State lives in the extension DB, keyed by OpenList user ID.
package listperm

import (
	"github.com/sagerenn/openlist/internal/db"
	"gorm.io/gorm"
)

func init() {
	db.RegisterModels(func() []interface{} { return []interface{}{&UserListPerm{}} })
}

// UserListPerm is the per-user list-permission setting. At most one row
// per user.
//
// When CanList is false, the user may still GET a specific file by path
// (read) and upload/remove/rename (write), but directory listing
// operations are denied.
type UserListPerm struct {
	UserID  uint  `json:"user_id" gorm:"primaryKey"`
	CanList bool  `json:"can_list"`
	// CanRead controls direct file access by path. Defaults true. When
	// false, only listing (if allowed) is permitted — useful for
	// write-only drop boxes.
	CanRead bool `json:"can_read"`
}

// Get returns the user's list-permission setting, defaulting to fully
// permitted (CanList=true, CanRead=true) when no row exists.
func Get(userID uint) (UserListPerm, error) {
	var p UserListPerm
	err := db.DB().Where("user_id = ?", userID).Take(&p).Error
	if err == gorm.ErrRecordNotFound {
		return UserListPerm{UserID: userID, CanList: true, CanRead: true}, nil
	}
	return p, err
}

// Set upserts the user's list-permission setting.
func Set(p UserListPerm) error {
	return db.DB().Save(&p).Error
}

// MayList reports whether the user is allowed to list directories.
func MayList(userID uint) (bool, error) {
	p, err := Get(userID)
	if err != nil {
		return false, err
	}
	return p.CanList, nil
}

// MayRead reports whether the user is allowed to read a file by path.
func MayRead(userID uint) (bool, error) {
	p, err := Get(userID)
	if err != nil {
		return false, err
	}
	return p.CanRead, nil
}
