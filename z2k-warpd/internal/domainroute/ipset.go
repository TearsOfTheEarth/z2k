package domainroute

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

type PairSet struct{ Binary, Name string }

func (s PairSet) Apply(changes []Change, now time.Time) error {
	if len(changes) == 0 {
		return nil
	}
	if s.Name != "z2k_warp_dns" {
		return errors.New("unexpected WARP DNS set name")
	}
	bin := s.Binary
	if bin == "" {
		bin = "ipset"
	}
	var stream bytes.Buffer
	for _, ch := range changes {
		if !ch.Client.Is4() || !EligibleDestination(ch.Dest) {
			return errors.New("invalid WARP DNS pair")
		}
		entry := ch.Client.String() + "/32," + ch.Dest.String() + "/32"
		if ch.Delete {
			fmt.Fprintf(&stream, "del %s %s\n", s.Name, entry)
			continue
		}
		seconds := int(ch.Expiry.Sub(now).Seconds())
		if ch.Expiry.After(now.Add(time.Duration(seconds) * time.Second)) {
			seconds++
		}
		if seconds < 1 {
			seconds = 1
		}
		if seconds > 3600 {
			seconds = 3600
		}
		fmt.Fprintf(&stream, "add %s %s timeout %d -exist\n", s.Name, entry, seconds)
	}
	cmd := exec.Command(bin, "restore", "-exist")
	cmd.Stdin = &stream
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ipset restore: %w: %s", err, out)
	}
	return nil
}
