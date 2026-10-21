package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"github.com/yukkcat/chatgpt2api/go-backend/internal/repository"
	"strings"
	"sync"
	"time"
)

type Auth struct {
	db                 *repository.Database
	bootstrap          string
	now                func() time.Time
	mu                 sync.Mutex
	refresh            sync.Mutex
	snapshot           repository.Snapshot
	attempted, success time.Time
	flushed            map[string]time.Time
	epoch              uint64
}

func NewAuth(db *repository.Database, bootstrap string, now func() time.Time) *Auth {
	if now == nil {
		now = time.Now
	}
	return &Auth{db: db, bootstrap: strings.TrimSpace(bootstrap), now: now, flushed: map[string]time.Time{}}
}
func validKey(m Map) bool {
	r := strings.ToLower(text(m["role"]))
	return (r == "user" || r == "admin") && text(m["id"]) != "" && text(m["key_hash"]) != ""
}
func publicKey(m Map) Map {
	role := strings.ToLower(text(m["role"]))
	name := text(m["name"])
	if name == "" {
		name = "普通用户"
		if role == "admin" {
			name = "管理员密钥"
		}
	}
	return Map{"id": m["id"], "name": name, "role": role, "enabled": truth(defaultValue(m["enabled"], true)), "created_at": m["created_at"], "last_used_at": defaultValue(m["last_used_at"], nil)}
}
func (a *Auth) reload(ctx context.Context, force bool) bool {
	a.refresh.Lock()
	defer a.refresh.Unlock()
	for {
		a.mu.Lock()
		epoch := a.epoch
		now := a.now()
		usable := !a.success.IsZero() && now.Sub(a.success) <= 30*time.Second
		if !force && !a.attempted.IsZero() && now.Sub(a.attempted) < 5*time.Second {
			a.mu.Unlock()
			return usable
		}
		a.attempted = now
		a.mu.Unlock()
		snap, e := a.db.Snapshot(ctx, "auth_keys")
		a.mu.Lock()
		if epoch != a.epoch {
			a.mu.Unlock()
			force = true
			continue
		}
		defer a.mu.Unlock()
		if e != nil {
			return !a.success.IsZero() && a.now().Sub(a.success) <= 30*time.Second
		}
		a.snapshot = snap
		a.success = a.now()
		return true
	}
}
func equalSecret(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func (a *Auth) Identity(ctx context.Context, authorization string) (Map, error) {
	scheme, value, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(value) == "" {
		return nil, problem(401, "密钥无效或已失效，请重新登录")
	}
	candidate := strings.TrimSpace(value)
	if a.bootstrap != "" && equalSecret(candidate, a.bootstrap) {
		return Map{"id": "admin", "name": "管理员", "role": "admin"}, nil
	}
	if !a.reload(ctx, false) {
		return nil, problem(401, "密钥无效或已失效，请重新登录")
	}
	hash := digest(candidate)
	for attempt := 0; attempt < 4; attempt++ {
		a.mu.Lock()
		var found Map
		for _, m := range a.snapshot.Items {
			if validKey(m) && truth(defaultValue(m["enabled"], true)) && equalSecret(text(m["key_hash"]), hash) {
				found = copyMap(m)
				break
			}
		}
		if found == nil {
			a.mu.Unlock()
			break
		}
		now := a.now()
		found["last_used_at"] = iso(now)
		keyID := text(found["id"])
		revision := a.snapshot.Revision
		epoch := a.epoch
		last := a.flushed[keyID]
		a.mu.Unlock()
		if !last.IsZero() && now.Sub(last) < 60*time.Second {
			return publicKey(found), nil
		}
		res, e := a.db.Mutate(ctx, "auth_keys", repository.Mutation{Upserts: []Map{found}, ExpectedRevision: &revision})
		if errors.Is(e, repository.ErrRevision) {
			if !a.reload(ctx, true) {
				break
			}
			continue
		}
		if e == nil {
			a.mu.Lock()
			if a.epoch == epoch && a.snapshot.Revision == revision {
				a.snapshot.Revision = res.Revision
				for i, m := range a.snapshot.Items {
					if text(m["id"]) == keyID {
						a.snapshot.Items[i] = found
					}
				}
			} else {
				a.attempted = time.Time{}
				a.success = time.Time{}
				a.epoch++
			}
			a.flushed[keyID] = now
			a.mu.Unlock()
		}
		return publicKey(found), nil
	}
	return nil, problem(401, "密钥无效或已失效，请重新登录")
}
func AuthView(version string, identity Map) Map {
	cap := Map{"admin_console": false, "studio": false}
	out := Map{"schema_version": 1, "authenticated": false, "version": version, "subject": nil, "capabilities": cap, "home_route": "/login"}
	if identity == nil {
		return out
	}
	role := strings.ToLower(text(identity["role"]))
	if role != "admin" && role != "user" {
		role = "unknown"
	}
	who := text(identity["id"])
	if who == "" {
		who = "authenticated"
	}
	name := text(identity["name"])
	if name == "" {
		name = who
	}
	out["authenticated"] = true
	out["subject"] = Map{"id": who, "name": name, "role": role}
	cap["admin_console"] = role == "admin"
	cap["studio"] = true
	out["home_route"] = "/studio"
	if role == "admin" {
		out["home_route"] = "/"
	}
	return out
}
func (a *Auth) List(ctx context.Context) ([]Map, error) {
	snap, e := a.db.Snapshot(ctx, "auth_keys")
	if e != nil {
		return nil, e
	}
	out := []Map{}
	for _, m := range snap.Items {
		if validKey(m) && strings.ToLower(text(m["role"])) == "user" {
			out = append(out, publicKey(m))
		}
	}
	return out, nil
}
func keyName(items []Map, name, exclude string) (string, error) {
	candidate := strings.TrimSpace(name)
	used := func(s string) bool {
		for _, m := range items {
			if text(m["id"]) != exclude && m["role"] == "user" && text(m["name"]) == s {
				return true
			}
		}
		return false
	}
	if candidate != "" {
		if used(candidate) {
			return "", problem(400, "这个名称已经在使用中了，换一个更容易区分的名称吧")
		}
		return candidate, nil
	}
	candidate = "普通用户"
	for n := 2; used(candidate); n++ {
		candidate = "普通用户 " + text(n)
	}
	return candidate, nil
}
func (a *Auth) invalidate() {
	a.mu.Lock()
	a.epoch++
	a.attempted = time.Time{}
	a.success = time.Time{}
	a.mu.Unlock()
}
func (a *Auth) Create(ctx context.Context, name string) (Map, string, error) {
	random := make([]byte, 24)
	if _, e := rand.Read(random); e != nil {
		return nil, "", e
	}
	raw := "sk-" + base64.RawURLEncoding.EncodeToString(random)
	var created Map
	_, e := a.db.Modify(ctx, "auth_keys", nil, func(items []Map) ([]Map, error) {
		name, e := keyName(items, name, "")
		if e != nil {
			return nil, e
		}
		created = Map{"id": id(6), "name": name, "role": "user", "key_hash": digest(raw), "enabled": true, "created_at": iso(a.now()), "last_used_at": nil}
		return append(items, created), nil
	})
	if e != nil {
		return nil, "", e
	}
	a.invalidate()
	return publicKey(created), raw, nil
}
func (a *Auth) Update(ctx context.Context, keyID string, patch Map) (Map, error) {
	var result Map
	_, e := a.db.Modify(ctx, "auth_keys", nil, func(items []Map) ([]Map, error) {
		index := -1
		for i, m := range items {
			if m["id"] == keyID && m["role"] == "user" {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, problem(404, "这条用户密钥不存在，可能已经被删除")
		}
		m := copyMap(items[index])
		if v, ok := patch["name"]; ok && v != nil {
			name, e := keyName(items, text(v), keyID)
			if e != nil {
				return nil, e
			}
			m["name"] = name
		}
		if v, ok := patch["enabled"]; ok && v != nil {
			m["enabled"] = truth(v)
		}
		if v, ok := patch["key"]; ok && v != nil {
			raw := text(v)
			if raw == "" {
				return nil, problem(400, "请输入新的专用密钥")
			}
			if equalSecret(raw, a.bootstrap) {
				return nil, problem(400, "这个密钥和管理员密钥冲突了，请换一个新的密钥")
			}
			hash := digest(raw)
			for _, other := range items {
				if other["id"] != keyID && equalSecret(text(other["key_hash"]), hash) {
					return nil, problem(400, "这个专用密钥已经存在，请换一个新的密钥")
				}
			}
			m["key_hash"] = hash
		}
		items[index] = m
		result = publicKey(m)
		return items, nil
	})
	if e == nil {
		a.invalidate()
	}
	return result, e
}
func (a *Auth) Delete(ctx context.Context, keyID string) error {
	_, e := a.db.Modify(ctx, "auth_keys", nil, func(items []Map) ([]Map, error) {
		out := []Map{}
		found := false
		for _, m := range items {
			if m["id"] == keyID && m["role"] == "user" {
				found = true
			} else {
				out = append(out, m)
			}
		}
		if !found {
			return nil, problem(404, "这条用户密钥不存在，可能已经被删除")
		}
		return out, nil
	})
	if e == nil {
		a.invalidate()
	}
	return e
}
