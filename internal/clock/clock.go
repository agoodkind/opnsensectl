// Package clock is the OPNsense packages' own wall-clock source. The daemon
// service and the upgrade runner inject it wherever they stamp or measure
// time, so their tests can substitute a fixed clock.
package clock

import "time"

// Clock reads wall time for OPNsense code that needs testable timestamps.
type Clock interface {
	Now() time.Time
}

// Real is the Clock the OPNsense packages use outside tests.
type Real struct{}

// Now returns the current wall-clock time.
func (Real) Now() time.Time {
	return time.Now()
}
