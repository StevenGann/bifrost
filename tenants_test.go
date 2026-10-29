package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyStoreMintAndLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := newKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	k, token, err := s.mint("hermes", 5, 60)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, clientKeyPrefix) {
		t.Fatalf("token %q lacks prefix", token)
	}
	if strings.Contains(token, k.Hash) {
		t.Fatal("token must not contain the stored hash")
	}
	got, err := s.lookup(token)
	if err != nil || got.ID != k.ID || got.Name != "hermes" {
		t.Fatalf("lookup: got %+v err=%v", got, err)
	}

	// The file must not contain the secret.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := token[strings.Index(token, "_")+1:]
	if strings.Contains(string(b), secret) {
		t.Fatal("keys file contains the plaintext secret")
	}
	if !strings.Contains(string(b), k.Hash) {
		t.Fatal("keys file should contain the hash")
	}
}

func TestKeyStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, _ := newKeyStore(path)
	k, token, err := s.mint("jeeves", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := newKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.lookup(token)
	if err != nil || got.ID != k.ID {
		t.Fatalf("after reload: got %+v err=%v", got, err)
	}
}

func TestKeyStoreRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, _ := newKeyStore(path)
	k, token, _ := s.mint("temp", 0, 0)
	if !s.revoke(k.ID) {
		t.Fatal("revoke returned false")
	}
	if _, err := s.lookup(token); err == nil {
		t.Fatal("revoked key still authenticates")
	}
	if s.revoke(k.ID) {
		t.Fatal("second revoke should be a no-op")
	}
	// A revoked name can be reused.
	if _, _, err := s.mint("temp", 0, 0); err != nil {
		t.Fatalf("re-mint after revoke failed: %v", err)
	}
}

func TestKeyStoreRejectsDuplicateLiveName(t *testing.T) {
	s, _ := newKeyStore("")
	if _, _, err := s.mint("dup", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.mint("dup", 0, 0); err == nil {
		t.Fatal("expected duplicate-name refusal")
	}
}

func TestKeyStoreCorruptFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newKeyStore(path); err == nil {
		t.Fatal("corrupt registry must not load silently")
	}
	g := newGovernor(Config{KeysFile: path, Anon: "open"})
	if g.storeErr == nil {
		t.Fatal("governor should surface the store error")
	}
}

func TestGovernorKeyAuthAndAttribution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	ledger := filepath.Join(t.TempDir(), "spend.jsonl")
	g := newGovernor(Config{KeysFile: path, LedgerFile: ledger, AdminKey: "adm"})
	k, token, err := g.store.mint("hermes", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	id, err := g.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if id.KeyID != k.ID || id.Name != "hermes" || id.Anon {
		t.Fatalf("identity = %+v", id)
	}

	// A presented-but-wrong key is an error, never anonymous.
	bad := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	bad.Header.Set("Authorization", "Bearer bk_dead_beef")
	if _, err := g.Authenticate(bad); err == nil {
		t.Fatal("invalid token must not fall through to anonymous")
	}

	// With keys configured, anonymous is closed by default.
	if _, err := g.Authenticate(httptest.NewRequest("POST", "/v1/chat/completions", nil)); err == nil {
		t.Fatal("expected 401 with anonymous closed")
	}

	g.Record("hermes", "deepseek-v4-flash", "deepseek-v4-flash", 0, 0, 0.01)
	b, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	var line struct {
		App string `json:"app"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(b, &line); err != nil {
		t.Fatal(err)
	}
	if line.Key != k.ID || line.App != "hermes" {
		t.Fatalf("ledger line = %+v, want key %s", line, k.ID)
	}
}

func TestGovernorAnonymousPolicies(t *testing.T) {
	req := func() *http.Request {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.RemoteAddr = "192.0.2.10:5555"
		return r
	}

	open := newGovernor(Config{Anon: "open"})
	id, err := open.Authenticate(req())
	if err != nil || !id.Anon || id.Name != "anonymous" || id.Addr != "192.0.2.10" {
		t.Fatalf("open policy: id=%+v err=%v", id, err)
	}

	closed := newGovernor(Config{Anon: "closed"})
	if _, err := closed.Authenticate(req()); err == nil {
		t.Fatal("closed policy must refuse")
	}

	limited := newGovernor(Config{Anon: "limited", AnonRate: 1})
	id, err = limited.Authenticate(req())
	if err != nil {
		t.Fatal(err)
	}
	if err := limited.Check(id); err != nil {
		t.Fatalf("first anonymous request should pass: %v", err)
	}
	if err := limited.Check(id); err == nil {
		t.Fatal("second anonymous request should be rate limited")
	}
}

func TestGovernorPerKeyLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	g := newGovernor(Config{KeysFile: path})
	k, _, err := g.store.mint("tight", 0.01, 1)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{Name: k.Name, KeyID: k.ID}
	if err := g.Check(id); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := g.Check(id); err == nil {
		t.Fatal("per-key rate limit not applied")
	}

	// Independent budget: spend it, then refuse.
	g2 := newGovernor(Config{KeysFile: filepath.Join(t.TempDir(), "k2.json")})
	k2, _, _ := g2.store.mint("budgeted", 0.05, 0)
	id2 := Identity{Name: k2.Name, KeyID: k2.ID}
	g2.Record("budgeted", "deepseek-v4-flash", "deepseek-v4-flash", 0, 0, 0.05)
	if err := g2.Check(id2); err == nil {
		t.Fatal("per-key budget not applied")
	}
}

func TestAdminKeyAPI(t *testing.T) {
	g := newGovernor(Config{KeysFile: filepath.Join(t.TempDir(), "keys.json"), AdminKey: "adm"})

	// Wrong admin token.
	if rr := doAdmin(g.handleAdminCreate, "POST", "", "wrong", `{"name":"x"}`); rr.Code != 401 {
		t.Fatalf("bad admin token: got %d", rr.Code)
	}

	rr := doAdmin(g.handleAdminCreate, "POST", "", "adm", `{"name":"symbio","budget":2.5,"rate_limit":30}`)
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Key == "" || created.ID == "" {
		t.Fatalf("create response missing fields: %s", rr.Body.String())
	}

	// The minted key authenticates.
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+created.Key)
	if id, err := g.Authenticate(r); err != nil || id.Name != "symbio" || id.KeyID != created.ID {
		t.Fatalf("minted key auth: id=%+v err=%v", id, err)
	}

	// Listing never leaks key material.
	lr := doAdmin(g.handleAdminList, "GET", "", "adm", "")
	if lr.Code != 200 {
		t.Fatalf("list: %d", lr.Code)
	}
	if strings.Contains(lr.Body.String(), created.Key) || strings.Contains(lr.Body.String(), "hash") {
		t.Fatalf("list leaked secret material: %s", lr.Body.String())
	}

	// Revoke, then the key stops working.
	dr := doAdmin(g.handleAdminRevoke, "DELETE", created.ID, "adm", "")
	if dr.Code != 204 {
		t.Fatalf("revoke: %d", dr.Code)
	}
	if _, err := g.Authenticate(r); err == nil {
		t.Fatal("revoked key still authenticates")
	}
}

func doAdmin(h func(http.ResponseWriter, *http.Request), method, id, token, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "/admin/keys", strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "/admin/keys", nil)
	}
	if id != "" {
		r.SetPathValue("id", id)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h(rr, r)
	return rr
}
