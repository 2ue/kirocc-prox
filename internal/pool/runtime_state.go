package pool

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RuntimeReservation is a short-lived Redis lease for one in-flight request.
type RuntimeReservation struct {
	ID     string
	CredID string
	Model  string
}

// RuntimeStateStore coordinates scheduling state that must be shared across
// processes: in-flight counters, cooldowns and session affinity.
type RuntimeStateStore interface {
	GetAffinity(ctx context.Context, sessionID string, ttl time.Duration) (string, bool, error)
	SetAffinity(ctx context.Context, sessionID, credID string, ttl time.Duration) error
	SyncInFlight(ctx context.Context, creds []*Credential, model string) error
	TryReserve(ctx context.Context, cred *Credential, model string, leaseTTL time.Duration) (RuntimeReservation, bool, error)
	Extend(ctx context.Context, res RuntimeReservation, leaseTTL time.Duration) (bool, error)
	Release(ctx context.Context, res RuntimeReservation) error
	SetCooldown(ctx context.Context, credID, model string, ttl time.Duration) error
	ClearCooldown(ctx context.Context, credID, model string) error
}

// RedisRuntimeStore is the production runtime-state implementation.
type RedisRuntimeStore struct {
	client *redis.Client
	prefix string
}

func NewRedisRuntimeStore(client *redis.Client, prefix string) *RedisRuntimeStore {
	if prefix == "" {
		prefix = "kirocc:"
	}
	return &RedisRuntimeStore{client: client, prefix: prefix}
}

func (s *RedisRuntimeStore) key(parts ...string) string {
	return s.prefix + strings.Join(parts, ":")
}

func (s *RedisRuntimeStore) GetAffinity(ctx context.Context, sessionID string, ttl time.Duration) (string, bool, error) {
	if sessionID == "" {
		return "", false, nil
	}
	key := s.key("affinity", sessionID)
	credID, err := s.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if ttl > 0 {
		_ = s.client.Expire(ctx, key, ttl).Err()
	}
	return credID, credID != "", nil
}

func (s *RedisRuntimeStore) SetAffinity(ctx context.Context, sessionID, credID string, ttl time.Duration) error {
	if sessionID == "" || credID == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return s.client.Set(ctx, s.key("affinity", sessionID), credID, ttl).Err()
}

func (s *RedisRuntimeStore) SyncInFlight(ctx context.Context, creds []*Credential, model string) error {
	if len(creds) == 0 {
		return nil
	}
	keys := make([]string, 0, len(creds)*2)
	for _, c := range creds {
		keys = append(keys, s.key("inflight", c.ID))
		if model != "" {
			keys = append(keys, s.key("inflight", c.ID, "model", model))
		}
	}
	vals, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return err
	}
	idx := 0
	for _, c := range creds {
		accountInFlight := redisInt(vals[idx])
		idx++
		modelInFlight := int64(0)
		if model != "" {
			modelInFlight = redisInt(vals[idx])
			idx++
		}
		c.Mu.Lock()
		c.InFlight = accountInFlight
		if model != "" {
			if c.InFlightByModel == nil {
				c.InFlightByModel = make(map[string]int64)
			}
			if modelInFlight > 0 {
				c.InFlightByModel[model] = modelInFlight
			} else {
				delete(c.InFlightByModel, model)
			}
		}
		c.Mu.Unlock()
	}
	return nil
}

func redisInt(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case []byte:
		n, _ := strconv.ParseInt(string(x), 10, 64)
		return n
	case int64:
		return x
	default:
		return 0
	}
}

var reserveScript = redis.NewScript(`
local account_inflight = KEYS[1]
local model_inflight = KEYS[2]
local account_cooldown = KEYS[3]
local model_cooldown = KEYS[4]
local reservation = KEYS[5]

local max_inflight = tonumber(ARGV[1])
local ttl_ms = tonumber(ARGV[2])
local has_model = ARGV[3]
local reservation_value = ARGV[4]

if redis.call("EXISTS", account_cooldown) == 1 then
  return {0, "account_cooldown"}
end
if has_model == "1" and redis.call("EXISTS", model_cooldown) == 1 then
  return {0, "model_cooldown"}
end

local current = tonumber(redis.call("GET", account_inflight) or "0")
if max_inflight > 0 and current >= max_inflight then
  return {0, "max_inflight"}
end

redis.call("INCR", account_inflight)
redis.call("PEXPIRE", account_inflight, ttl_ms)
if has_model == "1" then
  redis.call("INCR", model_inflight)
  redis.call("PEXPIRE", model_inflight, ttl_ms)
end
redis.call("SET", reservation, reservation_value, "PX", ttl_ms)
return {1, "ok"}
`)

func (s *RedisRuntimeStore) TryReserve(ctx context.Context, cred *Credential, model string, leaseTTL time.Duration) (RuntimeReservation, bool, error) {
	if cred == nil {
		return RuntimeReservation{}, false, fmt.Errorf("nil credential")
	}
	if leaseTTL <= 0 {
		leaseTTL = 10 * time.Minute
	}
	res := RuntimeReservation{
		ID:     uuid.NewString(),
		CredID: cred.ID,
		Model:  model,
	}
	hasModel := "0"
	modelKey := s.key("inflight", cred.ID, "model", "_")
	modelCooldown := s.key("cooldown", cred.ID, "model", "_")
	if model != "" {
		hasModel = "1"
		modelKey = s.key("inflight", cred.ID, "model", model)
		modelCooldown = s.key("cooldown", cred.ID, "model", model)
	}
	keys := []string{
		s.key("inflight", cred.ID),
		modelKey,
		s.key("cooldown", cred.ID),
		modelCooldown,
		s.key("reservation", res.ID),
	}
	value := res.CredID + "\n" + res.Model
	raw, err := reserveScript.Run(ctx, s.client, keys,
		cred.MaxInFlight,
		leaseTTL.Milliseconds(),
		hasModel,
		value,
	).Result()
	if err != nil {
		return RuntimeReservation{}, false, err
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return RuntimeReservation{}, false, fmt.Errorf("unexpected redis reserve result %T", raw)
	}
	return res, redisInt(arr[0]) == 1, nil
}

var extendScript = redis.NewScript(`
local reservation = KEYS[1]
local account_inflight = KEYS[2]
local model_inflight = KEYS[3]
local has_model = ARGV[1]
local ttl_ms = tonumber(ARGV[2])

if redis.call("EXISTS", reservation) == 0 then
  return 0
end

redis.call("PEXPIRE", reservation, ttl_ms)
redis.call("PEXPIRE", account_inflight, ttl_ms)
if has_model == "1" then
  redis.call("PEXPIRE", model_inflight, ttl_ms)
end
return 1
`)

func (s *RedisRuntimeStore) Extend(ctx context.Context, res RuntimeReservation, leaseTTL time.Duration) (bool, error) {
	if res.ID == "" || res.CredID == "" {
		return false, nil
	}
	if leaseTTL <= 0 {
		leaseTTL = 10 * time.Minute
	}
	hasModel := "0"
	modelKey := s.key("inflight", res.CredID, "model", "_")
	if res.Model != "" {
		hasModel = "1"
		modelKey = s.key("inflight", res.CredID, "model", res.Model)
	}
	raw, err := extendScript.Run(ctx, s.client, []string{
		s.key("reservation", res.ID),
		s.key("inflight", res.CredID),
		modelKey,
	}, hasModel, leaseTTL.Milliseconds()).Result()
	if err != nil {
		return false, err
	}
	return redisInt(raw) == 1, nil
}

var releaseScript = redis.NewScript(`
local reservation = KEYS[1]
local account_inflight = KEYS[2]
local model_inflight = KEYS[3]
local has_model = ARGV[1]

if redis.call("DEL", reservation) == 0 then
  return 0
end

local account = tonumber(redis.call("GET", account_inflight) or "0")
if account > 1 then
  redis.call("DECR", account_inflight)
else
  redis.call("DEL", account_inflight)
end

if has_model == "1" then
  local model = tonumber(redis.call("GET", model_inflight) or "0")
  if model > 1 then
    redis.call("DECR", model_inflight)
  else
    redis.call("DEL", model_inflight)
  end
end
return 1
`)

func (s *RedisRuntimeStore) Release(ctx context.Context, res RuntimeReservation) error {
	if res.ID == "" || res.CredID == "" {
		return nil
	}
	hasModel := "0"
	modelKey := s.key("inflight", res.CredID, "model", "_")
	if res.Model != "" {
		hasModel = "1"
		modelKey = s.key("inflight", res.CredID, "model", res.Model)
	}
	_, err := releaseScript.Run(ctx, s.client, []string{
		s.key("reservation", res.ID),
		s.key("inflight", res.CredID),
		modelKey,
	}, hasModel).Result()
	return err
}

func (s *RedisRuntimeStore) SetCooldown(ctx context.Context, credID, model string, ttl time.Duration) error {
	if credID == "" || ttl <= 0 {
		return nil
	}
	key := s.key("cooldown", credID)
	if model != "" {
		key = s.key("cooldown", credID, "model", model)
	}
	return s.client.Set(ctx, key, "1", ttl).Err()
}

func (s *RedisRuntimeStore) ClearCooldown(ctx context.Context, credID, model string) error {
	if credID == "" {
		return nil
	}
	keys := []string{s.key("cooldown", credID)}
	if model != "" {
		keys = append(keys, s.key("cooldown", credID, "model", model))
	}
	return s.client.Del(ctx, keys...).Err()
}

type reservationTracker struct {
	mu    sync.Mutex
	items map[*Credential][]trackedReservation
}

type trackedReservation struct {
	res    RuntimeReservation
	cancel context.CancelFunc
}

func newReservationTracker() *reservationTracker {
	return &reservationTracker{items: make(map[*Credential][]trackedReservation)}
}

func (t *reservationTracker) push(c *Credential, res RuntimeReservation, cancel context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.items[c] = append(t.items[c], trackedReservation{res: res, cancel: cancel})
}

func (t *reservationTracker) pop(c *Credential, model string) (RuntimeReservation, context.CancelFunc, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.items[c]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].res.Model == model {
			item := list[i]
			list = append(list[:i], list[i+1:]...)
			if len(list) == 0 {
				delete(t.items, c)
			} else {
				t.items[c] = list
			}
			return item.res, item.cancel, true
		}
	}
	return RuntimeReservation{}, nil, false
}
