package domainroute

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPairSetWritesExactClientDestinationAndTimeout(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	stub := filepath.Join(dir, "ipset")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat > \"$Z2K_IPSET_TEST_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("Z2K_IPSET_TEST_LOG", log)
	now := time.Unix(100, 0)
	s := PairSet{Binary: stub, Name: "z2k_warp_dns"}
	if err := s.Apply([]Change{{Client: netip.MustParseAddr("192.168.1.10"), Dest: netip.MustParseAddr("8.8.8.8"), Expiry: now.Add(20 * time.Second)}}, now); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "add z2k_warp_dns 192.168.1.10/32,8.8.8.8/32 timeout 20 -exist") {
		t.Fatalf("wrong pair command: %s", b)
	}
	if err := s.Apply([]Change{{Client: netip.MustParseAddr("192.168.1.10"), Dest: netip.MustParseAddr("8.8.8.8"), Delete: true}}, now); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(log)
	if !strings.Contains(string(b), "del z2k_warp_dns 192.168.1.10/32,8.8.8.8/32") {
		t.Fatalf("wrong delete: %s", b)
	}
}
