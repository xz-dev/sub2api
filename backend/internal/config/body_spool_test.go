package config

import (
	"strings"
	"testing"
)

// TestValidateBodySpoolThreshold: T below 32 KiB is refused before any
// spool or cgroup check; 32 KiB and the default pass the threshold check.
func TestValidateBodySpoolThreshold(t *testing.T) {
	for _, tc := range []struct {
		threshold int
		refused   bool
	}{{1, true}, {MinBodySpoolThreshold - 1, true}, {MinBodySpoolThreshold, false}, {0, false}} {
		err := validateBodySpool(GatewayBodySpoolConfig{Enabled: true, Threshold: tc.threshold, Dir: ""})
		refused := err != nil && strings.Contains(err.Error(), "threshold")
		if refused != tc.refused {
			t.Fatalf("threshold %d: err %v, want refused %v", tc.threshold, err, tc.refused)
		}
	}
}
