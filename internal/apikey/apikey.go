// Package apikey implements API-key authentication and operation scoping
// for users (feature 5).
//
// A user may create one or more API keys. Each key has a secret token
// (presented to the client exactly once at creation time) and a stored
// SHA-256 hash. Requests carrying the key (via the X-Api-Key header or an
// Authorization: Bearer <key> header) are authenticated as the owning
// user, subject to the key's scopes and enabled flag.
//
// State lives in the extension DB, keyed by OpenList user ID.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/sagerenn/openlist/internal/db"
	"gorm.io/gorm"
)

func init() {
	db.RegisterModels(func() []interface{} { return []interface{}{&APIKey{}} })
}

// Scope is a capability granted to an API key. The wildcard "*" grants all
// operations. Otherwise scopes are OpenList-style operation names such as
// "fs.list", "fs.get", "fs.put", "fs.rm".
type Scope string

const (
	ScopeAll    Scope = "*"
	ScopeList   Scope = "fs.list"
	ScopeRead   Scope = "fs.get"
	ScopeWrite  Scope = "fs.put"
	ScopeRemove Scope = "fs.rm"
	ScopeMkdir  Scope = "fs.mkdir"
)

// APIKey is a stored (hashed) API key.
type APIKey struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	UserID    uint      `json:"user_id" gorm:"index;not null"`
	Name      string    `json:"name" gorm:"size:128"` // human label
	Prefix    string    `json:"prefix" gorm:"size:16;index"` // first chars of secret, for lookup
	KeyHash   string    `json:"-" gorm:"size:64;uniqueIndex"` // sha256 hex of secret
	Scopes    string    `json:"scopes" gorm:"size:512"` // comma-separated
	Enabled   bool      `json:"enabled" gorm:"default:true"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used"`
}

// CreatedKey is returned when a key is created: it carries the plaintext
// secret exactly once.
type CreatedKey struct {
	APIKey
	Secret string `json:"secret"`
}

const (
	// secretLen is the number of random bytes in a secret. Hex-encoded =>
	// 2*secretLen chars.
	secretLen = 24
	// prefixLen is the number of leading hex chars stored for fast lookup.
	prefixLen = 8
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("apikey: not found")

// ErrInvalidKey is returned when a presented key does not match any stored
// key.
var ErrInvalidKey = errors.New("apikey: invalid key")

// generateSecret returns a hex-encoded random secret.
func generateSecret() (string, error) {
	b := make([]byte, secretLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashSecret returns the sha256 hex hash of a secret.
func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Create generates a new API key for userID with the given name and scopes.
// The plaintext secret is returned exactly once.
func Create(userID uint, name string, scopes []Scope) (CreatedKey, error) {
	secret, err := generateSecret()
	if err != nil {
		return CreatedKey{}, err
	}
	k := APIKey{
		UserID:    userID,
		Name:      name,
		Prefix:    secret[:prefixLen],
		KeyHash:   hashSecret(secret),
		Scopes:    joinScopes(scopes),
		Enabled:   true,
		CreatedAt: time.Now(),
	}
	if err := db.DB().Create(&k).Error; err != nil {
		return CreatedKey{}, err
	}
	return CreatedKey{APIKey: k, Secret: secret}, nil
}

// joinScopes joins scopes into a comma-separated string.
func joinScopes(scopes []Scope) string {
	parts := make([]string, len(scopes))
	for i, s := range scopes {
		parts[i] = string(s)
	}
	return strings.Join(parts, ",")
}

// parseScopes splits a stored scopes string into a slice.
func parseScopes(s string) []Scope {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]Scope, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, Scope(p))
		}
	}
	return out
}

// LookupByPrefix returns all enabled keys whose prefix matches, so the
// caller can do a constant-time compare against each hash.
func LookupByPrefix(prefix string) ([]APIKey, error) {
	if len(prefix) < prefixLen {
		return nil, nil
	}
	var ks []APIKey
	err := db.DB().Where("prefix = ? AND enabled = ?", prefix[:prefixLen], true).Find(&ks).Error
	return ks, err
}

// Verify resolves a presented secret to its key, marking LastUsed. Returns
// ErrInvalidKey if no enabled key matches.
func Verify(secret string) (APIKey, error) {
	if len(secret) < prefixLen {
		return APIKey{}, ErrInvalidKey
	}
	candidates, err := LookupByPrefix(secret[:prefixLen])
	if err != nil {
		return APIKey{}, err
	}
	hash := hashSecret(secret)
	for _, k := range candidates {
		if subtle.ConstantTimeCompare([]byte(k.KeyHash), []byte(hash)) == 1 {
			_ = db.DB().Model(&APIKey{}).Where("id = ?", k.ID).
				UpdateColumn("last_used", time.Now()).Error
			return k, nil
		}
	}
	return APIKey{}, ErrInvalidKey
}

// HasScope reports whether the key grants the given scope (or has "*").
func (k APIKey) HasScope(s Scope) bool {
	if !k.Enabled {
		return false
	}
	for _, sc := range parseScopes(k.Scopes) {
		if sc == ScopeAll || sc == s {
			return true
		}
	}
	return false
}

// ListByUser returns all keys for userID (without hashes, which are json:"-").
func ListByUser(userID uint) ([]APIKey, error) {
	var ks []APIKey
	err := db.DB().Where("user_id = ?", userID).Find(&ks).Error
	return ks, err
}

// Delete removes a key by ID, scoped to userID for safety.
func Delete(userID uint, id uint) error {
	res := db.DB().Where("id = ? AND user_id = ?", id, userID).Delete(&APIKey{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetEnabled toggles a key's enabled flag.
func SetEnabled(userID uint, id uint, enabled bool) error {
	res := db.DB().Model(&APIKey{}).
		Where("id = ? AND user_id = ?", id, userID).
		Update("enabled", enabled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ErrEmpty is returned by FromHeader when no key is present.
var ErrEmpty = errors.New("apikey: no key in request")

// FromHeader extracts an API key from a request header set. It accepts
// either "X-Api-Key: <key>" or "Authorization: Bearer <key>". Returns
// ErrEmpty if neither is present.
func FromHeader(get func(string) string) (string, error) {
	if k := strings.TrimSpace(get("X-Api-Key")); k != "" {
		return k, nil
	}
	auth := strings.TrimSpace(get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:]), nil
	}
	return "", ErrEmpty
}

// Ensure gorm import is used in error sentinels referencing gorm.
var _ = gorm.ErrRecordNotFound
