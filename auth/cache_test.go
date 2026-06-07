package auth

import (
	"strconv"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestCacheRoundTrip(t *testing.T) {
	c := newJWTCache(4)
	c.put("a", map[string]any{"role": "x"})
	got, ok := c.get("a")
	if !ok || got["role"] != "x" {
		t.Fatalf("get a = %v, %v", got, ok)
	}
	if _, ok := c.get("missing"); ok {
		t.Error("missing key must not hit")
	}
}

func TestCacheEvictsWhenFull(t *testing.T) {
	c := newJWTCache(2)
	c.put("a", map[string]any{})
	c.put("b", map[string]any{})
	c.put("c", map[string]any{}) // evicts the unvisited oldest, "a"
	if c.len() != 2 {
		t.Fatalf("len = %d, want 2", c.len())
	}
	if _, ok := c.get("a"); ok {
		t.Error("a should have been evicted")
	}
	if _, ok := c.get("c"); !ok {
		t.Error("c should be present")
	}
}

func TestCacheSieveSparesVisited(t *testing.T) {
	c := newJWTCache(2)
	c.put("a", map[string]any{})
	c.put("b", map[string]any{})
	// touch "a" so its visited bit is set; the next insert should pass over it
	// and evict "b" instead.
	c.get("a")
	c.put("c", map[string]any{})
	if _, ok := c.get("a"); !ok {
		t.Error("a was visited and must survive eviction")
	}
	if _, ok := c.get("b"); ok {
		t.Error("b was not visited and should have been evicted")
	}
}

func TestCacheDisabledIsNoOp(t *testing.T) {
	c := newJWTCache(0)
	c.put("a", map[string]any{})
	if c.len() != 0 {
		t.Fatalf("a zero-capacity cache must stay empty, len = %d", c.len())
	}
}

func TestCacheHitSkipsVerifyButRechecksExpiry(t *testing.T) {
	v := hmacVerifier(t, Config{CacheMaxEntries: 8})
	tok := signHS(t, jwt.MapClaims{
		"role": "web_user",
		"exp":  clockNow.Add(time.Minute).Unix(),
	})
	if _, err := v.Authenticate("Bearer " + tok); err != nil {
		t.Fatalf("first auth: %v", err)
	}
	if _, hit := v.cache.get(tok); !hit {
		t.Fatal("the token should be cached after first verification")
	}
	// Advance the clock past exp: the cached entry must not extend its life.
	v.now = fixedClock(clockNow.Add(2 * time.Minute))
	_, err := v.Authenticate("Bearer " + tok)
	if err == nil || err.Code != "PGRST301" {
		t.Fatalf("a cached but now-expired token must be PGRST301, got %v", err)
	}
}

func BenchmarkAuthenticateCached(b *testing.B) {
	v, err := NewVerifier(Config{Secret: hmacKey, AnonRole: anonRole, CacheMaxEntries: 1000})
	if err != nil {
		b.Fatalf("NewVerifier: %v", err)
	}
	v.now = fixedClock(clockNow)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"role": "web_user",
		"exp":  clockNow.Add(time.Hour).Unix(),
	})
	signed, _ := tok.SignedString(hmacKey)
	header := "Bearer " + signed
	if _, aerr := v.Authenticate(header); aerr != nil {
		b.Fatalf("warm: %v", aerr)
	}
	for b.Loop() {
		if _, aerr := v.Authenticate(header); aerr != nil {
			b.Fatalf("auth: %v", aerr)
		}
	}
}

func BenchmarkAuthenticateUncached(b *testing.B) {
	v, err := NewVerifier(Config{Secret: hmacKey, AnonRole: anonRole})
	if err != nil {
		b.Fatalf("NewVerifier: %v", err)
	}
	v.now = fixedClock(clockNow)
	headers := make([]string, 256)
	for i := range headers {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"role": "web_user",
			"sub":  strconv.Itoa(i),
			"exp":  clockNow.Add(time.Hour).Unix(),
		})
		signed, _ := tok.SignedString(hmacKey)
		headers[i] = "Bearer " + signed
	}
	i := 0
	for b.Loop() {
		if _, aerr := v.Authenticate(headers[i%len(headers)]); aerr != nil {
			b.Fatalf("auth: %v", aerr)
		}
		i++
	}
}
