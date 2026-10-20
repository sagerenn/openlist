// Package lb implements upload load-balancing across multiple backend
// storage providers/accounts (feature 4).
//
// An admin defines a Group, which is an ordered set of Members. Each Member
// references an OpenList storage mount path (i.e. a configured storage
// account/provider) and carries a relative weight. When a user uploads a
// file through a group, the extension picks a target member according to
// the group's strategy and routes the upload there.
//
// A group can be activated two ways:
//
//   - explicitly, by the X-LB-Group header (or ?lb_group=) on the upload —
//     the original activation, where the group is chosen by name;
//   - by path: an upload whose File-Path falls under the group's PathPrefix
//     is routed through the group with no header at all. Path activation is
//     what makes a group behave like a virtual mount: clients (e.g. pshare)
//     address files by the prefix path, and the prefix is rewritten onto the
//     selected member's mount path.
//
// Every load-balanced upload is recorded (UploadRecord): where the client
// asked the file to go (the source path) and where it landed (the member's
// physical path). Records power the admin uploads view and the read routing
// that lets /api/fs/get, /api/fs/list, and /api/fs/remove under a group
// prefix find the file again.
//
// Strategies:
//   - StrategyRoundRobin: cycle through members in order, weighted.
//   - StrategyLeastUsed:  pick the member with the fewest recorded uploads.
//   - StrategyRandom:     pick a member at random, weighted.
//
// State lives in the extension DB. Members reference OpenList storage by
// mount path (a stable identifier), so this is decoupled from OpenList's
// internal storage IDs and forward-compatible.
package lb

import (
	"errors"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagerenn/openlist/internal/db"
	"gorm.io/gorm"
)

func init() {
	db.RegisterModels(func() []interface{} {
		return []interface{}{&Group{}, &GroupMember{}, &UploadRecord{}}
	})
}

// Strategy names a load-balancing selection algorithm.
type Strategy string

const (
	StrategyRoundRobin Strategy = "round_robin"
	StrategyLeastUsed  Strategy = "least_used"
	StrategyRandom     Strategy = "random"
)

// Group is a named load-balance group owned by a user.
type Group struct {
	ID       uint64   `json:"id" gorm:"primaryKey"`
	Name     string   `json:"name" gorm:"uniqueIndex;size:128;not null"`
	UserID   uint     `json:"user_id" gorm:"index;not null"`
	Strategy Strategy `json:"strategy" gorm:"size:32;default:round_robin"`
	// PathPrefix is the logical path prefix uploads to this group are
	// written under (e.g. "/lb/photos"). Optional.
	PathPrefix string `json:"path_prefix" gorm:"size:1024"`
}

// GroupMember is one backend storage account within a group, referenced by
// its OpenList mount path.
type GroupMember struct {
	ID        uint64 `json:"id" gorm:"primaryKey"`
	GroupID   uint64 `json:"group_id" gorm:"uniqueIndex:idx_group_member;not null"`
	MountPath string `json:"mount_path" gorm:"uniqueIndex:idx_group_member;size:512;not null"`
	Weight    int    `json:"weight" gorm:"default:1"`
	// UploadCount is incremented each time this member is selected.
	UploadCount int64 `json:"upload_count" gorm:"default:0"`
}

var (
	rrMu  sync.Mutex
	rrCtr = map[uint64]*uint64{}
)

// ErrNoMembers is returned when a group has no members to select from.
var ErrNoMembers = errors.New("lb: group has no members")

// ErrGroupNotFound is returned when a named group does not exist.
var ErrGroupNotFound = errors.New("lb: group not found")

// CreateGroup creates a new group for userID.
func CreateGroup(g *Group) error {
	if g.Strategy == "" {
		g.Strategy = StrategyRoundRobin
	}
	return db.DB().Create(g).Error
}

// AddMember adds a member to a group.
func AddMember(m *GroupMember) error {
	if m.Weight <= 0 {
		m.Weight = 1
	}
	return db.DB().Create(m).Error
}

// ListGroups returns all groups owned by userID.
func ListGroups(userID uint) ([]Group, error) {
	var gs []Group
	err := db.DB().Where("user_id = ?", userID).Find(&gs).Error
	return gs, err
}

// GroupMembers returns all members of a group.
func GroupMembers(groupID uint64) ([]GroupMember, error) {
	var ms []GroupMember
	err := db.DB().Where("group_id = ?", groupID).Order("id asc").Find(&ms).Error
	return ms, err
}

// GetGroupByName returns the group owned by userID with the given name.
func GetGroupByName(userID uint, name string) (Group, error) {
	var g Group
	err := db.DB().Where("user_id = ? AND name = ?", userID, name).Take(&g).Error
	if err == gorm.ErrRecordNotFound {
		return g, ErrGroupNotFound
	}
	return g, err
}

// GetGroup returns the group by ID.
func GetGroup(id uint64) (Group, error) {
	var g Group
	err := db.DB().First(&g, id).Error
	if err == gorm.ErrRecordNotFound {
		return g, ErrGroupNotFound
	}
	return g, err
}

// Select picks a member for the next upload according to the group's
// strategy, increments that member's UploadCount, and returns it.
func Select(g Group) (GroupMember, error) {
	members, err := GroupMembers(g.ID)
	if err != nil {
		return GroupMember{}, err
	}
	if len(members) == 0 {
		return GroupMember{}, ErrNoMembers
	}

	idx, err := pickIndex(g, members, true)
	if err != nil {
		return GroupMember{}, err
	}
	chosen := members[idx]

	// increment upload count
	if e := db.DB().Model(&GroupMember{}).
		Where("id = ?", chosen.ID).
		UpdateColumn("upload_count", gorm.Expr("upload_count + 1")).Error; e != nil {
		return chosen, e
	}
	return chosen, nil
}

// pickIndex returns the index into members chosen by the group's strategy.
// advance=false peeks at the current position without moving it, so reads
// routed under a group prefix do not distort the upload balancing the
// strategies track.
func pickIndex(g Group, members []GroupMember, advance bool) (int, error) {
	switch g.Strategy {
	case StrategyLeastUsed:
		best := 0
		for i := 1; i < len(members); i++ {
			if members[i].UploadCount < members[best].UploadCount {
				best = i
			}
		}
		return best, nil
	case StrategyRandom:
		return weightedRandom(members), nil
	case StrategyRoundRobin:
		return roundRobin(g.ID, members, advance), nil
	default:
		return roundRobin(g.ID, members, advance), nil
	}
}

// roundRobin returns the next index in a weighted round-robin sequence.
// Weight is honored by repeating a member's slot Weight times. With
// advance=false it reports the position the NEXT advance would pick,
// without consuming it.
func roundRobin(groupID uint64, members []GroupMember, advance bool) int {
	rrMu.Lock()
	defer rrMu.Unlock()
	slots := 0
	for _, m := range members {
		w := m.Weight
		if w <= 0 {
			w = 1
		}
		slots += w
	}
	if slots == 0 {
		return 0
	}
	ptr, ok := rrCtr[groupID]
	if !ok {
		var z uint64
		ptr = &z
		rrCtr[groupID] = ptr
	}
	var pos int
	if advance {
		pos = int(atomic.AddUint64(ptr, 1)-1) % slots
	} else {
		pos = int(atomic.LoadUint64(ptr)) % slots
	}
	for i, m := range members {
		w := m.Weight
		if w <= 0 {
			w = 1
		}
		if pos < w {
			return i
		}
		pos -= w
	}
	return 0
}

// weightedRandom returns an index chosen at random, weighted by Weight.
func weightedRandom(members []GroupMember) int {
	total := 0
	for _, m := range members {
		w := m.Weight
		if w <= 0 {
			w = 1
		}
		total += w
	}
	if total <= 0 {
		return 0
	}
	r := rand.Intn(total)
	for i, m := range members {
		w := m.Weight
		if w <= 0 {
			w = 1
		}
		if r < w {
			return i
		}
		r -= w
	}
	return 0
}

// DeleteGroup removes a group and its members.
func DeleteGroup(groupID uint64) error {
	if err := db.DB().Where("group_id = ?", groupID).Delete(&GroupMember{}).Error; err != nil {
		return err
	}
	return db.DB().Delete(&Group{}, groupID).Error
}

// ---- path-based activation ----

// MatchGroupByPath returns the user's group whose PathPrefix contains p,
// longest prefix first. The second return is false when no group's prefix
// contains p (including the empty-prefix case: an empty PathPrefix never
// activates a group by path, so groups used purely through the X-LB-Group
// header keep their exact previous behavior).
func MatchGroupByPath(userID uint, p string) (Group, bool) {
	gs, err := ListGroups(userID)
	if err != nil {
		return Group{}, false
	}
	var best Group
	bestLen := -1
	for _, g := range gs {
		if !PrefixMatches(g.PathPrefix, p) {
			continue
		}
		if l := len(NormalizePrefix(g.PathPrefix)); l > bestLen {
			best, bestLen = g, l
		}
	}
	return best, bestLen >= 0
}

// PrefixMatches reports whether p falls under prefix on a path-segment
// boundary: "/pshare" matches "/pshare" and "/pshare/x" but not "/psharex".
// An empty prefix never matches; a bare "/" matches everything.
func PrefixMatches(prefix, p string) bool {
	prefix = NormalizePrefix(prefix)
	if prefix == "" {
		return false
	}
	if prefix == "/" {
		return true
	}
	p = "/" + strings.TrimPrefix(p, "/")
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// NormalizePrefix trims trailing slashes and re-leads with one, so callers
// can join/compare uniformly. "" stays "" (never matches); "/" stays "/"
// (matches everything).
func NormalizePrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return "/"
	}
	return "/" + strings.TrimPrefix(prefix, "/")
}

// RemapPath rewrites a logical path under a group prefix onto a member
// mount: the prefix is replaced by the mount, everything below it is kept
// ("/pshare/a/b.txt" under prefix "/pshare" on mount "/s3" becomes
// "/s3/a/b.txt"). prefix must be NormalizePrefix'd; p must be under it.
// The prefix root itself maps to the bare mount ("/s3", no trailing slash),
// matching how OpenList cleans directory paths.
func RemapPath(mount, prefix, p string) string {
	rest := strings.TrimPrefix(p, prefix)
	rest = "/" + strings.TrimPrefix(rest, "/")
	if rest == "/" {
		return strings.TrimRight(mount, "/")
	}
	return strings.TrimRight(mount, "/") + rest
}

// PickForRead selects a member for a read routed under a group prefix
// WITHOUT incrementing its upload counter: reads must not distort the
// upload balancing the strategies track.
func PickForRead(g Group) (GroupMember, error) {
	members, err := GroupMembers(g.ID)
	if err != nil {
		return GroupMember{}, err
	}
	if len(members) == 0 {
		return GroupMember{}, ErrNoMembers
	}
	idx, err := pickIndex(g, members, false)
	if err != nil {
		return GroupMember{}, err
	}
	return members[idx], nil
}

// ---- upload records ----

// UploadRecord is one load-balanced upload. SourcePath is the logical path
// the client addressed (for path activation, under the group's prefix);
// TargetMount/TargetPath are the member it landed on and the physical path
// written. Records drive the admin uploads view and read routing: a later
// fs/get on SourcePath resolves to TargetPath.
type UploadRecord struct {
	ID          uint64    `json:"id" gorm:"primaryKey"`
	UserID      uint      `json:"user_id" gorm:"index;not null"`
	GroupID     uint64    `json:"group_id" gorm:"index"`
	GroupName   string    `json:"group_name" gorm:"size:128"`
	SourcePath  string    `json:"source_path" gorm:"size:1024;not null"`
	TargetMount string    `json:"target_mount" gorm:"size:512"`
	TargetPath  string    `json:"target_path" gorm:"size:1024"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at" gorm:"index"`
}

// RecordUpload persists an upload record.
func RecordUpload(rec *UploadRecord) error {
	return db.DB().Create(rec).Error
}

// ListUploads returns the most recent upload records, newest first, for
// userID — or for every user when userID is 0. limit <= 0 means the default
// of 200; it is capped at 1000 so a huge history cannot be dumped in one
// response.
func ListUploads(userID uint, limit int) ([]UploadRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	q := db.DB().Order("created_at desc, id desc").Limit(limit)
	if userID != 0 {
		q = q.Where("user_id = ?", userID)
	}
	var recs []UploadRecord
	return recs, q.Find(&recs).Error
}

// FindUploadByPath returns the newest record whose SourcePath equals p for
// userID — the record that pins which member holds that file. gorm's
// ErrRecordNotFound maps to (nil, nil): "no record" is not an error, the
// caller falls back to strategy-based routing.
func FindUploadByPath(userID uint, p string) (*UploadRecord, error) {
	var rec UploadRecord
	err := db.DB().Where("user_id = ? AND source_path = ?", userID, p).
		Order("id desc").Take(&rec).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// DeleteUpload removes one record (metadata only — the stored file is
// untouched).
func DeleteUpload(id uint64) error {
	return db.DB().Delete(&UploadRecord{}, id).Error
}

// DeleteUploadsUnder removes the records whose SourcePath is dir itself or
// lies under it. Every record of the user is scanned (not the capped
// ListUploads view), and prefix matching is done in Go (not SQL LIKE) so %
// and _ in paths match literally. The count of removed records is returned.
func DeleteUploadsUnder(userID uint, dir string) (int64, error) {
	var recs []UploadRecord
	if err := db.DB().Where("user_id = ?", userID).Find(&recs).Error; err != nil {
		return 0, err
	}
	dir = "/" + strings.TrimPrefix(dir, "/")
	var ids []uint64
	for _, r := range recs {
		sp := "/" + strings.TrimPrefix(r.SourcePath, "/")
		if sp == dir || strings.HasPrefix(sp, dir+"/") {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return db.DB().Where("id IN ?", ids).Delete(&UploadRecord{}).RowsAffected, nil
}

// DeleteUploadsFor removes the records for the file dir/name — called when
// a remove under a group prefix deletes that file, so the uploads view and
// read routing stop pointing at it. Exact-path matching, in Go, for the
// same reason as DeleteUploadsUnder.
func DeleteUploadsFor(userID uint, dir, name string) (int64, error) {
	full := "/" + strings.TrimPrefix(dir, "/")
	full = strings.TrimRight(full, "/") + "/" + strings.TrimPrefix(name, "/")
	return db.DB().
		Where("user_id = ? AND source_path = ?", userID, full).
		Delete(&UploadRecord{}).RowsAffected, nil
}
