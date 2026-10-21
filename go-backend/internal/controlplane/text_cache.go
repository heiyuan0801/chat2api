package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/redis/go-redis/v9"
)

// TextEventCache stores sanitized successful public text events. It is disposable;
// the caller retains ownership of upstream execution and process-local singleflight.
type TextEventCache interface {
	Get(context.Context, string) ([]Map, bool, error)
	Put(context.Context, string, []Map, time.Duration, int) error
}

type RedisTextCache struct {
	client *redis.Client
	keys   []string
}

// The hash and expiry index share one Cluster slot. Redis time, rather than
// application clocks, owns expiration. Both keys expire after the final entry.
const cacheCleanup = `
local t = redis.call('TIME')
local now = tonumber(t[1])*1000 + math.floor(tonumber(t[2])/1000)
local expired = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now)
for _, k in ipairs(expired) do
 redis.call('HDEL', KEYS[1], k)
 redis.call('ZREM', KEYS[2], k)
end
`

var redisCacheGet = redis.NewScript(cacheCleanup + `
local expiry = redis.call('ZSCORE', KEYS[2], ARGV[1])
if not expiry then return false end
return redis.call('HGET', KEYS[1], ARGV[1])
`)
var redisCachePut = redis.NewScript(cacheCleanup + `
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('ZADD', KEYS[2], now+tonumber(ARGV[3]), ARGV[1])
local excess = redis.call('ZCARD', KEYS[2])-tonumber(ARGV[4])
if excess > 0 then
 local evict = redis.call('ZRANGE', KEYS[2], 0, excess-1)
 for _, k in ipairs(evict) do
  redis.call('HDEL', KEYS[1], k)
  redis.call('ZREM', KEYS[2], k)
 end
end
local last = redis.call('ZRANGE', KEYS[2], -1, -1, 'WITHSCORES')
if #last > 0 then
 redis.call('PEXPIREAT', KEYS[1], math.ceil(tonumber(last[2])))
 redis.call('PEXPIREAT', KEYS[2], math.ceil(tonumber(last[2])))
end
return 1
`)

// NewRedisTextCache deliberately hides URL/transport errors, which can contain
// credentials. A configured but unavailable cache is a startup configuration error.
func NewRedisTextCache(ctx context.Context, rawURL, namespace string) (*RedisTextCache, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9:_-]{1,128}$`).MatchString(namespace) {
		return nil, errors.New("invalid Redis cache namespace")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("invalid Redis URL")
	}
	options.Protocol = 2
	options.MaxRetries = -1
	options.PoolSize = 8
	options.DialTimeout = time.Second
	options.ReadTimeout = 750 * time.Millisecond
	options.WriteTimeout = 750 * time.Millisecond
	options.PoolTimeout = 750 * time.Millisecond
	options.ContextTimeoutEnabled = true
	client := redis.NewClient(options)
	check, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if client.Ping(check).Err() != nil {
		_ = client.Close()
		return nil, errors.New("Redis cache startup check failed")
	}
	prefix := "{" + namespace + "}:text:v1:"
	return &RedisTextCache{client: client, keys: []string{prefix + "events", prefix + "expiry"}}, nil
}
func (c *RedisTextCache) Close() error { return c.client.Close() }
func (c *RedisTextCache) Get(ctx context.Context, key string) ([]Map, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	value, err := redisCacheGet.Run(ctx, c.client, c.keys, key).Text()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.New("Redis cache read failed")
	}
	var events []Map
	if len(value) > 32<<20 || json.Unmarshal([]byte(value), &events) != nil || events == nil {
		return nil, false, errors.New("invalid Redis cache entry")
	}
	return events, true, nil
}
func (c *RedisTextCache) Put(ctx context.Context, key string, events []Map, ttl time.Duration, capacity int) error {
	if ttl <= 0 || capacity <= 0 {
		return nil
	}
	value, err := json.Marshal(events)
	if err != nil || len(value) > 32<<20 {
		return errors.New("invalid Redis cache entry")
	}
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	if redisCachePut.Run(ctx, c.client, c.keys, key, value, max(1, ttl.Milliseconds()), capacity).Err() != nil {
		return errors.New("Redis cache write failed")
	}
	return nil
}
