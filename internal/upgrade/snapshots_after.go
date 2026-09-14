package upgrade

import (
	"regexp"
	"strings"
)

// preDeploySnapRE and knownGoodSnapRE match the snapshot names the
// watchdog takes around a gateway deploy. An upgrade rollback must still
// remove those snapshots when they sit below the upgrade snapshot, so
// the patterns stay identical to the watchdog's.
var (
	preDeploySnapRE = regexp.MustCompile(`pre-deploy-[^\s]+`)
	knownGoodSnapRE = regexp.MustCompile(`known-good-[^\s]+`)
)

// snapshotsAfter returns snapshot names that appear AFTER targetSnap in
// qm listsnapshot output (they are children/descendants of targetSnap and
// must be deleted before rolling back to it).
// It returns them in the order they appear, which is oldest-to-newest;
// callers should delete in reverse order (newest first).
func snapshotsAfter(qmOutput []byte, targetSnap string) []string {
	lines := strings.Split(string(qmOutput), "\n")
	var result []string
	past := false
	for _, line := range lines {
		// qm listsnapshot lines look like: ` `-> snapname   timestamp   desc`
		// The name is the first non-space/arrow token after whitespace.
		trimmed := strings.TrimLeft(line, " `->|")
		if trimmed == "" {
			continue
		}
		// Extract just the snapshot name (first field).
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if name == "current" {
			continue
		}
		if !past {
			if name == targetSnap {
				past = true
			}
			continue
		}
		// Only collect watchdog-managed snapshots; never touch user snapshots.
		if preDeploySnapRE.MatchString(name) || knownGoodSnapRE.MatchString(name) {
			result = append(result, name)
		}
	}
	return result
}
