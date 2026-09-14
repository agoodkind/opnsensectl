package svc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
)

// TestDeployStatusMarkHealthyClearsPending covers the RPC path the new
// `mwan opnsense daemon mark-healthy` verb drives: DeployStatus with
// Mark=MARK_HEALTHY routes to MarkHealthy, which clears the pending-verify
// marker and stamps health=ok so the rc.d preflight does not revert the
// deploy on a later respawn.
func TestDeployStatusMarkHealthyClearsPending(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	writeBinary(t, filepath.Join(binDir, BinaryCurrent), []byte("active-binary"))
	if err := os.WriteFile(dm.cfg.PendingPath, []byte(MarkerFreshDeploy), PendingFileMode); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	if err := dm.writeState(deployState{Active: "abc", Previous: "def", Health: HealthPending}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	srv := NewServerWithDeploy(nil, "", "", dm)

	resp, err := srv.DeployStatus(context.Background(), &mwanv1.DeployStatusRequest{
		Mark: mwanv1.DeployStatusRequest_MARK_HEALTHY,
	})
	if err != nil {
		t.Fatalf("DeployStatus MARK_HEALTHY: %v", err)
	}
	if resp.GetHealth() != HealthOK {
		t.Errorf("health = %q, want %q", resp.GetHealth(), HealthOK)
	}
	if _, statErr := os.Stat(dm.cfg.PendingPath); !os.IsNotExist(statErr) {
		t.Errorf("pending marker should be cleared, statErr=%v", statErr)
	}

	// A follow-up plain Status must still report health=ok (durable).
	statusResp, err := srv.DeployStatus(context.Background(), &mwanv1.DeployStatusRequest{})
	if err != nil {
		t.Fatalf("DeployStatus: %v", err)
	}
	if statusResp.GetHealth() != HealthOK {
		t.Errorf("follow-up health = %q, want %q", statusResp.GetHealth(), HealthOK)
	}
}
