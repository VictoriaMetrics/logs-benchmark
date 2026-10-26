package writer

import (
	"testing"
	"time"
)

func TestConfigTargetRate(t *testing.T) {
	// Fixed mode (no ramp): the rate is constant regardless of elapsed time.
	fixed := Config{Rate: 10000}
	for _, elapsed := range []time.Duration{0, time.Minute, time.Hour} {
		if got := fixed.targetRate(elapsed); got != 10000 {
			t.Errorf("fixed targetRate(%s) = %d; want 10000", elapsed, got)
		}
	}

	// Ramp: start 5000, +5000 every 5m, capped at 20000.
	ramp := Config{Rate: 5000, RampStep: 5000, RampInterval: 5 * time.Minute, RampMax: 20000}
	cases := []struct {
		elapsed time.Duration
		want    int
	}{
		{0, 5000},                 // step 0
		{4 * time.Minute, 5000},   // still step 0
		{5 * time.Minute, 10000},  // step 1
		{12 * time.Minute, 15000}, // step 2
		{60 * time.Minute, 20000}, // would be 65000, capped at 20000
	}
	for _, c := range cases {
		if got := ramp.targetRate(c.elapsed); got != c.want {
			t.Errorf("ramp targetRate(%s) = %d; want %d", c.elapsed, got, c.want)
		}
	}

	// Uncapped ramp keeps climbing.
	uncapped := Config{Rate: 5000, RampStep: 5000, RampInterval: time.Minute}
	if got := uncapped.targetRate(10 * time.Minute); got != 55000 {
		t.Errorf("uncapped targetRate(10m) = %d; want 55000", got)
	}
}
