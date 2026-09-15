package upgrade

import (
	"context"
	"reflect"
	"testing"
)

// upgradeSnapshotFixture is the rollback target the listing cases share.
const upgradeSnapshotFixture = "pre-upgrade-26x-1700000000"

// snapshotListingCase pairs a qm listsnapshot tree with the names
// snapshotsAfter must return for it. The want values were recorded from
// the watchdog's parser this one was copied from, so the table pins
// parity with it, including nil for listings with nothing to delete.
type snapshotListingCase struct {
	name    string
	listing string
	target  string
	want    []string
}

func snapshotListingCases() []snapshotListingCase {
	return []snapshotListingCase{
		{
			name: "nested watchdog children below the upgrade snapshot",
			listing: "`-> known-good-1699990000                  2023-11-14 19:26:40     watchdog\n" +
				"    `-> pre-upgrade-26x-1700000000         2023-11-14 22:13:20     opnsense-upgrade\n" +
				"        `-> pre-deploy-1700000100          2023-11-14 22:15:00     watchdog\n" +
				"            `-> known-good-1700000200      2023-11-14 22:16:40     watchdog\n" +
				"                `-> current                                        You are here!\n",
			target: upgradeSnapshotFixture,
			want:   []string{"pre-deploy-1700000100", "known-good-1700000200"},
		},
		{
			name: "operator and upgrade-prefix snapshots below the target are kept",
			listing: "`-> pre-upgrade-26x-1700000000             2023-11-14 22:13:20     opnsense-upgrade\n" +
				"    `-> manual-before-vlan-change          2023-11-14 22:14:00     operator\n" +
				"        `-> pre-upgrade-26x-1700000500     2023-11-14 22:21:40     opnsense-upgrade\n" +
				"            `-> known-good-1700000600      2023-11-14 22:23:20     watchdog\n" +
				"                `-> current                                        You are here!\n",
			target: upgradeSnapshotFixture,
			want:   []string{"known-good-1700000600"},
		},
		{
			name: "branched tree with both patterns and an embedded match",
			listing: "`-> pre-upgrade-26x-1700000000             2023-11-14 22:13:20     opnsense-upgrade\n" +
				"    |-> pre-deploy-1700000100              2023-11-14 22:15:00     watchdog\n" +
				"    |   `-> copy-of-known-good-1700000150  2023-11-14 22:15:50     operator\n" +
				"    `-> known-good-1700000200              2023-11-14 22:16:40     watchdog\n" +
				"        `-> current                                                You are here!\n",
			target: upgradeSnapshotFixture,
			want: []string{
				"pre-deploy-1700000100",
				"copy-of-known-good-1700000150",
				"known-good-1700000200",
			},
		},
		{
			name: "target missing from the listing",
			listing: "`-> pre-deploy-1700000100                  2023-11-14 22:15:00     watchdog\n" +
				"    `-> known-good-1700000200              2023-11-14 22:16:40     watchdog\n" +
				"        `-> current                                                You are here!\n",
			target: upgradeSnapshotFixture,
			want:   nil,
		},
		{
			name: "target is the newest snapshot",
			listing: "`-> known-good-1699990000                  2023-11-14 19:26:40     watchdog\n" +
				"    `-> pre-upgrade-26x-1700000000         2023-11-14 22:13:20     opnsense-upgrade\n" +
				"        `-> current                                                You are here!\n",
			target: upgradeSnapshotFixture,
			want:   nil,
		},
		{
			name:    "empty listing",
			listing: "",
			target:  upgradeSnapshotFixture,
			want:    nil,
		},
	}
}

func TestSnapshotsAfterMatchesRecordedWatchdogParserOutput(t *testing.T) {
	t.Parallel()
	for _, tc := range snapshotListingCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := snapshotsAfter([]byte(tc.listing), tc.target)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("snapshotsAfter = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestRollbackDeletesWatchdogChildrenNewestFirst drives the public
// Rollback and checks that only watchdog snapshots below the upgrade
// snapshot are deleted, newest first, before qm rollback runs.
func TestRollbackDeletesWatchdogChildrenNewestFirst(t *testing.T) {
	t.Parallel()
	deps, _, s, _, v := newDeps(t)
	v.result = AggregateChecks([]CheckResult{{Name: "qga_responsive", Pass: false}})
	s.running = true
	opts := newOpts(t, "101")

	prepared, err := Prepare(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := Execute(context.Background(), deps, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, _, err := Validate(context.Background(), deps, opts); err != nil {
		t.Fatalf("validate: %v", err)
	}

	s.mu.Lock()
	s.deletes = nil
	s.listing = []byte("`-> known-good-1699990000                  2023-11-14 19:26:40     watchdog\n" +
		"    `-> " + prepared.Snapshot + "          2023-11-14 22:13:20     opnsense-upgrade\n" +
		"        `-> pre-deploy-1700000100          2023-11-14 22:15:00     watchdog\n" +
		"            `-> manual-before-vlan-change  2023-11-14 22:15:50     operator\n" +
		"                `-> known-good-1700000200  2023-11-14 22:16:40     watchdog\n" +
		"                    `-> current                                    You are here!\n")
	s.mu.Unlock()

	if _, err := Rollback(context.Background(), deps, opts); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	wantDeletes := []snapshotCall{
		{VMID: "101", Snap: "known-good-1700000200"},
		{VMID: "101", Snap: "pre-deploy-1700000100"},
	}
	if !reflect.DeepEqual(s.deletes, wantDeletes) {
		t.Fatalf("deletes = %+v, want %+v", s.deletes, wantDeletes)
	}
	wantRollbacks := []snapshotCall{{VMID: "101", Snap: prepared.Snapshot}}
	if !reflect.DeepEqual(s.rollbacks, wantRollbacks) {
		t.Fatalf("rollbacks = %+v, want %+v", s.rollbacks, wantRollbacks)
	}
}
