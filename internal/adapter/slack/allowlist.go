package slack

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
)

const (
	redisKeyAllowUsers    = "slack:allowlist:users"
	redisKeyAllowChannels = "slack:allowlist:channels"
)

// AllowListStore persists the dynamic (runtime-mutable) allowlist.
// All methods must be safe for concurrent use.
type AllowListStore interface {
	LoadUsers(ctx context.Context) ([]string, error)
	LoadChannels(ctx context.Context) ([]string, error)
	AddUser(ctx context.Context, userID string) error
	RemoveUser(ctx context.Context, userID string) error
	AddChannel(ctx context.Context, channelID string) error
	RemoveChannel(ctx context.Context, channelID string) error
}

// allowList holds the two-tier access state:
//   - adminIDs: static user IDs from config (admins); never mutated at runtime
//   - staticChanIDs: static channel IDs from config; never mutated at runtime
//   - userIDs/chanIDs: dynamic grants added by admins at runtime, persisted via store
//
// The bot is unrestricted (allow all) when both adminIDs and staticChanIDs are
// empty. Otherwise, a message is allowed if the sender is an admin, in the
// dynamic user list, or the channel is in either the static or dynamic channel list.
//
// All map lookups are O(1). Reads are protected by RWMutex so concurrent
// message handling does not block on writes.
type allowList struct {
	mu            sync.RWMutex
	adminIDs      map[string]struct{} // static; from AdminUserIDs config
	staticChanIDs map[string]struct{} // static; from AllowedChannelIDs config
	userIDs       map[string]struct{} // dynamic; persisted via store
	chanIDs       map[string]struct{} // dynamic; persisted via store
	store         AllowListStore      // nil = in-memory only (no persistence)
}

// newAllowList constructs an allowList from the static admin and channel lists,
// then loads the dynamic list from store. If store is nil, the dynamic list is
// in-memory only and will not survive a restart.
func newAllowList(adminIDs, allowedChannelIDs []string, store AllowListStore) *allowList {
	al := &allowList{
		adminIDs:      make(map[string]struct{}, len(adminIDs)),
		staticChanIDs: make(map[string]struct{}, len(allowedChannelIDs)),
		userIDs:       make(map[string]struct{}),
		chanIDs:       make(map[string]struct{}),
		store:         store,
	}
	for _, id := range adminIDs {
		al.adminIDs[id] = struct{}{}
	}
	for _, id := range allowedChannelIDs {
		al.staticChanIDs[id] = struct{}{}
	}
	return al
}

// load hydrates the dynamic list from the store. Called once at Initialize time.
func (al *allowList) load(ctx context.Context) {
	if al.store == nil {
		return
	}
	users, err := al.store.LoadUsers(ctx)
	if err != nil {
		log.Printf("[Slack] allowList: failed to load users from store: %v", err)
	}
	channels, err := al.store.LoadChannels(ctx)
	if err != nil {
		log.Printf("[Slack] allowList: failed to load channels from store: %v", err)
	}

	al.mu.Lock()
	defer al.mu.Unlock()
	for _, id := range users {
		al.userIDs[id] = struct{}{}
	}
	for _, id := range channels {
		al.chanIDs[id] = struct{}{}
	}
}

// isRestricted reports whether access control is active.
// When false (both admin and static channel lists are empty), the bot is open
// to everyone and admin commands are disabled.
func (al *allowList) isRestricted() bool {
	return len(al.adminIDs) > 0 || len(al.staticChanIDs) > 0
}

// isAdmin reports whether userID is in the static admin list.
func (al *allowList) isAdmin(userID string) bool {
	_, ok := al.adminIDs[userID]
	return ok
}

// isAllowed reports whether a message from (channelID, userID) should be processed.
func (al *allowList) isAllowed(channelID, userID string) bool {
	if !al.isRestricted() {
		return true
	}
	if al.isAdmin(userID) {
		return true
	}
	_, inStaticChans := al.staticChanIDs[channelID]
	if inStaticChans {
		return true
	}
	al.mu.RLock()
	defer al.mu.RUnlock()
	_, inUsers := al.userIDs[userID]
	_, inChans := al.chanIDs[channelID]
	return inUsers || inChans
}

// addUser grants a user access and persists the change.
func (al *allowList) addUser(ctx context.Context, userID string) error {
	al.mu.Lock()
	al.userIDs[userID] = struct{}{}
	al.mu.Unlock()
	if al.store != nil {
		if err := al.store.AddUser(ctx, userID); err != nil {
			return fmt.Errorf("persist add user: %w", err)
		}
	}
	return nil
}

// removeUser revokes a user's access and persists the change.
func (al *allowList) removeUser(ctx context.Context, userID string) error {
	al.mu.Lock()
	delete(al.userIDs, userID)
	al.mu.Unlock()
	if al.store != nil {
		if err := al.store.RemoveUser(ctx, userID); err != nil {
			return fmt.Errorf("persist remove user: %w", err)
		}
	}
	return nil
}

// addChannel grants a channel access and persists the change.
func (al *allowList) addChannel(ctx context.Context, channelID string) error {
	al.mu.Lock()
	al.chanIDs[channelID] = struct{}{}
	al.mu.Unlock()
	if al.store != nil {
		if err := al.store.AddChannel(ctx, channelID); err != nil {
			return fmt.Errorf("persist add channel: %w", err)
		}
	}
	return nil
}

// removeChannel revokes a channel's access and persists the change.
func (al *allowList) removeChannel(ctx context.Context, channelID string) error {
	al.mu.Lock()
	delete(al.chanIDs, channelID)
	al.mu.Unlock()
	if al.store != nil {
		if err := al.store.RemoveChannel(ctx, channelID); err != nil {
			return fmt.Errorf("persist remove channel: %w", err)
		}
	}
	return nil
}

// listUsers returns a snapshot of the dynamic user list.
func (al *allowList) listUsers() []string {
	al.mu.RLock()
	defer al.mu.RUnlock()
	ids := make([]string, 0, len(al.userIDs))
	for id := range al.userIDs {
		ids = append(ids, id)
	}
	return ids
}

// listChannels returns a snapshot of the dynamic channel list.
func (al *allowList) listChannels() []string {
	al.mu.RLock()
	defer al.mu.RUnlock()
	ids := make([]string, 0, len(al.chanIDs))
	for id := range al.chanIDs {
		ids = append(ids, id)
	}
	return ids
}

// ============================================================================
// Redis implementation
// ============================================================================

// RedisAllowListStore persists the dynamic allowlist in Redis using native SET
// operations. Each entry in the allowlist is a member of a Redis set, making
// individual add/remove operations O(1).
type RedisAllowListStore struct {
	client *redis.Client
}

// NewRedisAllowListStore creates a RedisAllowListStore connected to redisURL.
func NewRedisAllowListStore(redisURL string) (*RedisAllowListStore, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Redis URL: %w", err)
	}
	client := redis.NewClient(opts)
	return &RedisAllowListStore{client: client}, nil
}

func (s *RedisAllowListStore) LoadUsers(ctx context.Context) ([]string, error) {
	return s.client.SMembers(ctx, redisKeyAllowUsers).Result()
}

func (s *RedisAllowListStore) LoadChannels(ctx context.Context) ([]string, error) {
	return s.client.SMembers(ctx, redisKeyAllowChannels).Result()
}

func (s *RedisAllowListStore) AddUser(ctx context.Context, userID string) error {
	return s.client.SAdd(ctx, redisKeyAllowUsers, userID).Err()
}

func (s *RedisAllowListStore) RemoveUser(ctx context.Context, userID string) error {
	return s.client.SRem(ctx, redisKeyAllowUsers, userID).Err()
}

func (s *RedisAllowListStore) AddChannel(ctx context.Context, channelID string) error {
	return s.client.SAdd(ctx, redisKeyAllowChannels, channelID).Err()
}

func (s *RedisAllowListStore) RemoveChannel(ctx context.Context, channelID string) error {
	return s.client.SRem(ctx, redisKeyAllowChannels, channelID).Err()
}
