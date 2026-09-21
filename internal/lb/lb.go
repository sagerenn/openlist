// Package lb implements upload load-balancing across multiple backend
// storage providers/accounts (feature 4).
//
// An admin defines a Group, which is an ordered set of Members. Each Member
// references an OpenList storage mount path (i.e. a configured storage
// account/provider) and carries a relative weight. When a user uploads a
// file through a group, the extension picks a target member according to
// the group's strategy and routes the upload there.
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
	"sync"
	"sync/atomic"

	"github.com/sagerenn/openlist/internal/db"
	"gorm.io/gorm"
)

func init() {
	db.RegisterModels(func() []interface{} {
		return []interface{}{&Group{}, &GroupMember{}}
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

	idx, err := pickIndex(g, members)
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
func pickIndex(g Group, members []GroupMember) (int, error) {
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
		return roundRobin(g.ID, members), nil
	default:
		return roundRobin(g.ID, members), nil
	}
}

// roundRobin returns the next index in a weighted round-robin sequence.
// Weight is honored by repeating a member's slot Weight times.
func roundRobin(groupID uint64, members []GroupMember) int {
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
	pos := int(atomic.AddUint64(ptr, 1)-1) % slots
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
