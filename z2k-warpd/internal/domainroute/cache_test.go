package domainroute

import (
	"net/netip"
	"testing"
	"time"
)

func TestCacheScopesPairsToClientAndKeepsSharedJustification(t *testing.T) {
	r, err := ParseRules([]byte("v1\na.example\nb.example\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := NewCache(r)
	now := time.Unix(100, 0)
	a := netip.MustParseAddr("192.168.1.10")
	b := netip.MustParseAddr("192.168.1.11")
	ip := netip.MustParseAddr("8.8.8.8")
	if got := c.Observe(a, "a.example", []Answer{{ip, 20 * time.Second}}, now); len(got) != 1 || got[0].Delete {
		t.Fatalf("first pair: %+v", got)
	}
	if got := c.Observe(b, "a.example", []Answer{{ip, 20 * time.Second}}, now); len(got) != 1 || got[0].Client != b {
		t.Fatalf("second client: %+v", got)
	}
	if got := c.Observe(a, "b.example", []Answer{{ip, 30 * time.Second}}, now); len(got) != 1 || got[0].Delete {
		t.Fatalf("longer shared expiry: %+v", got)
	}
	r, _ = ParseRules([]byte("v1\nb.example\n"))
	changes := c.ReplaceRules(r, now)
	for _, ch := range changes {
		if ch.Client == a && ch.Delete {
			t.Fatal("removed shared pair")
		}
	}
	if c.Len() != 1 {
		t.Fatalf("wanted one retained pair, got %d", c.Len())
	}
	r, _ = ParseRules([]byte("v1\n"))
	changes = c.ReplaceRules(r, now)
	if len(changes) != 1 || !changes[0].Delete || c.Len() != 0 {
		t.Fatalf("not deleted after both names removed: %+v", changes)
	}
}

func TestCacheExpiryLimitsAndZeroTTL(t *testing.T) {
	r, _ := ParseRules([]byte("v1\na.example\n"))
	c := NewCache(r)
	now := time.Unix(100, 0)
	client := netip.MustParseAddr("192.168.1.10")
	ip := netip.MustParseAddr("8.8.8.8")
	if got := c.Observe(client, "a.example", []Answer{{ip, 0}}, now); len(got) != 0 {
		t.Fatalf("zero TTL: %+v", got)
	}
	got := c.Observe(client, "a.example", []Answer{{ip, 2 * time.Hour}}, now)
	if len(got) != 1 || !got[0].Expiry.Equal(now.Add(time.Hour)) {
		t.Fatalf("TTL cap: %+v", got)
	}
	if got := c.Expire(now.Add(59 * time.Minute)); len(got) != 0 {
		t.Fatalf("expired early: %+v", got)
	}
	if got := c.Expire(now.Add(time.Hour)); len(got) != 1 || !got[0].Delete {
		t.Fatalf("not expired: %+v", got)
	}
}

func TestSnapshotRestoresOnlyCurrentlySelectedAndUnexpiredNames(t *testing.T) {
	r, _ := ParseRules([]byte("v1\na.example\nb.example\n"))
	c := NewCache(r)
	now := time.Unix(100, 0)
	client := netip.MustParseAddr("192.168.1.10")
	c.Observe(client, "a.example", []Answer{{netip.MustParseAddr("8.8.8.8"), 20 * time.Second}}, now)
	c.Observe(client, "b.example", []Answer{{netip.MustParseAddr("1.1.1.1"), 10 * time.Second}}, now)
	path := t.TempDir() + "/pairs.v1"
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	r, _ = ParseRules([]byte("v1\na.example\n"))
	restored := NewCache(r)
	if err := restored.Load(path, now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if restored.Len() != 1 {
		t.Fatalf("restored %d pairs, wanted one", restored.Len())
	}
	if err := restored.Load(path, now.Add(21*time.Second)); err != nil {
		t.Fatal(err)
	}
	if restored.Len() != 0 {
		t.Fatalf("restored expired pair: %d", restored.Len())
	}
}
