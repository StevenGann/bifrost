package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const clientKeyPrefix = "bk_"

// clientKey is one tenant credential. The secret itself is never stored: only
// its SHA-256, so a stolen keys file yields nothing that can be presented.
type clientKey struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Hash      string  `json:"hash"`
	Created   string  `json:"created"`
	LastUsed  string  `json:"last_used,omitempty"`
	Revoked   string  `json:"revoked,omitempty"`
	Budget    float64 `json:"budget,omitempty"`
	RateLimit int     `json:"rate_limit,omitempty"`
	Static    bool    `json:"static,omitempty"` // from APP_KEYS, not minted here
}

func (k *clientKey) live() bool { return k.Revoked == "" }

// public strips the secret material for API responses.
func (k *clientKey) public() map[string]any {
	return map[string]any{
		"id":         k.ID,
		"name":       k.Name,
		"created":    k.Created,
		"last_used":  k.LastUsed,
		"revoked":    k.Revoked,
		"budget":     k.Budget,
		"rate_limit": k.RateLimit,
		"static":     k.Static,
	}
}

// keyStore is the tenant registry. Every key here is either minted by Bifrost
// (the secret appears exactly once, in the create response) or imported from
// the legacy APP_KEYS env var.
type keyStore struct {
	mu       sync.RWMutex
	path     string
	byID     map[string]*clientKey
	byName   map[string]*clientKey
	byHash   map[string]*clientKey
	dirty    bool
	lastSave time.Time
}

// keySaveInterval throttles last-used writes; a request should not cost a disk
// write, but attribution should not be a day stale either.
const keySaveInterval = 30 * time.Second

func newKeyStore(path string) (*keyStore, error) {
	s := &keyStore{
		path:   path,
		byID:   map[string]*clientKey{},
		byName: map[string]*clientKey{},
		byHash: map[string]*clientKey{},
	}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	var list []clientKey
	if err := json.Unmarshal(b, &list); err != nil {
		// A corrupt key file must not silently downgrade to "everyone is
		// anonymous"; refuse to start instead.
		return nil, fmt.Errorf("keys file %s: %w", path, err)
	}
	for i := range list {
		k := &list[i]
		s.byID[k.ID] = k
		s.byName[k.Name] = k
		s.byHash[k.Hash] = k
	}
	return s, nil
}

func (s *keyStore) addStatic(name, secret string) {
	if secret == "" || name == "" {
		return
	}
	h := hashSecret(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byHash[h]; ok {
		return
	}
	id := hex.EncodeToString(hashBytes(secret)[:4])
	k := &clientKey{
		ID:      id,
		Name:    name,
		Hash:    h,
		Created: time.Now().UTC().Format(time.RFC3339),
		Static:  true,
	}
	s.byID[id] = k
	s.byName[name] = k
	s.byHash[h] = k
}

func hashBytes(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func hashSecret(s string) string { return hex.EncodeToString(hashBytes(s)) }

// mint returns a new key. The plaintext token is returned once and never kept.
func (s *keyStore) mint(name string, budget float64, rateLimit int) (clientKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return clientKey{}, "", errors.New("name is required")
	}
	if len(name) > 64 {
		return clientKey{}, "", errors.New("name too long (max 64)")
	}
	id, err := randomHex(4)
	if err != nil {
		return clientKey{}, "", err
	}
	secret, err := randomHex(32)
	if err != nil {
		return clientKey{}, "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.byName[name]; ok && prev.live() {
		return clientKey{}, "", fmt.Errorf("a live key named %q already exists", name)
	}
	k := clientKey{
		ID:        id,
		Name:      name,
		Hash:      hashSecret(secret),
		Created:   time.Now().UTC().Format(time.RFC3339),
		Budget:    budget,
		RateLimit: rateLimit,
	}
	s.byID[id] = &k
	s.byName[name] = &k
	s.byHash[k.Hash] = &k
	s.dirty = true
	if err := s.saveLocked(); err != nil {
		delete(s.byID, id)
		delete(s.byName, name)
		delete(s.byHash, k.Hash)
		return clientKey{}, "", err
	}
	return k, clientKeyPrefix + id + "_" + secret, nil
}

// lookup resolves a presented token. Bifrost-minted tokens carry their id so
// the common path is a map hit; anything else (legacy APP_KEYS) is matched by
// hash over the live keys.
func (s *keyStore) lookup(token string) (*clientKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var k *clientKey
	if rest, ok := strings.CutPrefix(token, clientKeyPrefix); ok {
		if id, secret, ok := strings.Cut(rest, "_"); ok {
			if cand, found := s.byID[id]; found {
				if !cand.live() {
					return nil, errors.New("key revoked")
				}
				if subtle.ConstantTimeCompare([]byte(cand.Hash), []byte(hashSecret(secret))) == 1 {
					k = cand
				}
			}
		}
	}
	if k == nil {
		h := hashSecret(token)
		if cand, found := s.byHash[h]; found && cand.live() {
			k = cand
		}
	}
	if k == nil {
		return nil, errors.New("unknown key")
	}
	return k, nil
}

// touch records last use. Writes are throttled so a busy gateway is not doing
// a disk write per request.
func (s *keyStore) touch(id string) {
	if s.path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok {
		return
	}
	k.LastUsed = time.Now().UTC().Format(time.RFC3339)
	s.dirty = true
	if time.Since(s.lastSave) >= keySaveInterval {
		if err := s.saveLocked(); err != nil {
			log.Printf("keys: save failed: %v", err)
		}
	}
}

func (s *keyStore) revoke(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || !k.live() {
		return false
	}
	k.Revoked = time.Now().UTC().Format(time.RFC3339)
	delete(s.byHash, k.Hash)
	s.dirty = true
	if err := s.saveLocked(); err != nil {
		log.Printf("keys: save failed: %v", err)
	}
	return true
}

func (s *keyStore) list() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.byID))
	for _, k := range s.byID {
		out = append(out, k.public())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["created"].(string) < out[j]["created"].(string)
	})
	return out
}

func (s *keyStore) get(id string) *clientKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byID[id]
}

func (s *keyStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}

// idForName lets the ledger tag spend with the key that produced it without
// threading an identity through every handler.
func (s *keyStore) idForName(name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if k, ok := s.byName[name]; ok {
		return k.ID
	}
	return ""
}

func (s *keyStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	list := make([]clientKey, 0, len(s.byID))
	for _, k := range s.byID {
		list = append(list, *k)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created < list[j].Created })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	s.lastSave = time.Now()
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// clientIP buckets anonymous traffic per caller; one abuser should not be able
// to starve every other anonymous client.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- admin API -------------------------------------------------------------

func (g *Governor) adminAllowed(r *http.Request) bool {
	if g.adminKey == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(g.adminKey)) == 1
}

func (g *Governor) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	if !g.adminAllowed(r) {
		writeJSON(w, 401, map[string]string{"error": "admin authorization required"})
		return
	}
	var req struct {
		Name      string  `json:"name"`
		Budget    float64 `json:"budget"`
		RateLimit int     `json:"rate_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON body"})
		return
	}
	k, token, err := g.store.mint(req.Name, req.Budget, req.RateLimit)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("keys: minted %s for %q", k.ID, k.Name)
	out := k.public()
	out["key"] = token
	out["notice"] = "this is the only time the key is shown"
	writeJSON(w, 201, out)
}

func (g *Governor) handleAdminList(w http.ResponseWriter, r *http.Request) {
	if !g.adminAllowed(r) {
		writeJSON(w, 401, map[string]string{"error": "admin authorization required"})
		return
	}
	keys := g.store.list()
	g.mu.Lock()
	g.rolloverLocked()
	spend := make(map[string]float64, len(g.monthSpend))
	for k, v := range g.monthSpend {
		spend[k] = v
	}
	g.mu.Unlock()
	for _, k := range keys {
		if name, ok := k["name"].(string); ok {
			k["spend_month"] = spend[name]
		}
	}
	writeJSON(w, 200, map[string]any{"keys": keys})
}

func (g *Governor) handleAdminRevoke(w http.ResponseWriter, r *http.Request) {
	if !g.adminAllowed(r) {
		writeJSON(w, 401, map[string]string{"error": "admin authorization required"})
		return
	}
	id := r.PathValue("id")
	if !g.store.revoke(id) {
		writeJSON(w, 404, map[string]string{"error": "unknown or already revoked key"})
		return
	}
	log.Printf("keys: revoked %s", id)
	w.WriteHeader(204)
}
