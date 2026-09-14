package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"goodkind.io/send-email/mailer"

	opnsensecfg "goodkind.io/opnsensectl/internal/config"
	"goodkind.io/opnsensectl/internal/notify"
)

const (
	alertMailService   = "mwan-opnsense-upgrade"
	alertMailRecipient = "opnsense-alerts@example.test"
	alertMailSender    = "opnsense-upgrade@example.test"
	alertMailEnvAPIKey = "test-placeholder"
)

// alertMailConfig is the tooling config the Proxmox host deploy renders: the
// [email] table beside the upgrade target. The file key is replaced by the
// SMTP2GO_API_KEY environment variable in the test.
const alertMailConfig = `
[email]
smtp2go_api_key = "key-from-file"
alert_email = "opnsense-alerts@example.test"
from = "opnsense-upgrade@example.test"
subject_prefix = "[OPNsense-test]"
bind_iface = ""
min_level = "ERROR"

[opnsense.upgrade]
vmid = 101
`

type recordedMail struct {
	config  mailer.Config
	message mailer.Message
}

type mailRecorder struct {
	mu    sync.Mutex
	mails []recordedMail
}

func (r *mailRecorder) newMailer(mailerConfig mailer.Config) notify.Mailer {
	return &recordingMailer{recorder: r, config: mailerConfig}
}

func (r *mailRecorder) recorded() []recordedMail {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedMail, len(r.mails))
	copy(out, r.mails)
	return out
}

// recordingMailer stands in for the SMTP2GO HTTP API call inside send-email,
// which a test can neither reach nor redirect. It records what would have been
// sent and reports delivery.
type recordingMailer struct {
	recorder *mailRecorder
	config   mailer.Config
}

func (m *recordingMailer) Send(_ context.Context, message mailer.Message) error {
	m.recorder.mu.Lock()
	defer m.recorder.mu.Unlock()
	m.recorder.mails = append(m.recorder.mails, recordedMail{config: m.config, message: message})
	return nil
}

func expectedAlertMail(subject, body string) recordedMail {
	return recordedMail{
		config: mailer.Config{
			SMTP2GOAPIKey:     alertMailEnvAPIKey,
			MsmtprcPath:       "",
			DefaultFromDomain: "goodkind.io",
			BindInterface:     "",
			Transport:         mailer.MethodAuto,
			Now:               nil,
		},
		message: mailer.Message{
			To:      alertMailRecipient,
			Subject: subject,
			Body:    body,
			From:    alertMailSender,
			Name:    "",
			Caller:  alertMailService,
		},
	}
}

// TestUpgradePhasesMailAlertsThroughSendEmail runs an upgrade that fails to
// snapshot twice, then prepares, dry-run executes, fails validation, rolls
// back, passes validation, and commits. The notifier is built from the loaded
// tooling config, and only send-email's HTTP call is replaced. The operator
// receives one mail for the repeated snapshot failure, one for the failed
// validation, and one recovery mail at the failure's level. The rollback and
// the INFO phase events stay below min_level and are not mailed.
func TestUpgradePhasesMailAlertsThroughSendEmail(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(alertMailConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(opnsensecfg.PathEnv, configPath)
	t.Setenv(opnsensecfg.SMTP2GOEnv, alertMailEnvAPIKey)
	cfg, err := opnsensecfg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	deps, _, snap, exec, validator := newDeps(t)
	recorder := &mailRecorder{}
	notifier, err := notify.NewWithMailer(cfg.Email, deps.Log, alertMailService, recorder.newMailer)
	if err != nil {
		t.Fatalf("NewWithMailer: %v", err)
	}
	deps.Notifier = notifier
	exec.byCommand["true"] = GuestExecResult{ExitCode: 0, Stdout: "", Stderr: ""}
	snap.running = true
	opts := newOpts(t, "101")
	opts.DryRunExecute = true
	ctx := context.Background()

	snap.snapErr = errors.New("qm snapshot 101: exit status 2")
	for range 2 {
		if _, prepareErr := Prepare(ctx, deps, opts); prepareErr == nil {
			t.Fatal("Prepare succeeded while the snapshot fails")
		}
	}
	snap.snapErr = nil
	if st, prepareErr := Prepare(ctx, deps, opts); prepareErr != nil || st.Phase != PhasePrepared {
		t.Fatalf("Prepare = %q, %v; want %q", st.Phase, prepareErr, PhasePrepared)
	}
	if st, executeErr := Execute(ctx, deps, opts); executeErr != nil || st.Phase != PhaseExecuted {
		t.Fatalf("Execute = %q, %v; want %q", st.Phase, executeErr, PhaseExecuted)
	}

	validator.result = AggregateChecks([]CheckResult{{Name: "bgp_established", Pass: false, Note: "0 of 2 peers"}})
	if st, _, validateErr := Validate(ctx, deps, opts); validateErr != nil || st.Phase != PhaseValidatedFail {
		t.Fatalf("Validate = %q, %v; want %q", st.Phase, validateErr, PhaseValidatedFail)
	}
	if st, rollbackErr := Rollback(ctx, deps, opts); rollbackErr != nil || st.Phase != PhaseRolledBack {
		t.Fatalf("Rollback = %q, %v; want %q", st.Phase, rollbackErr, PhaseRolledBack)
	}
	validator.result = AggregateChecks([]CheckResult{{Name: "bgp_established", Pass: true, Note: "2 of 2 peers"}})
	if st, _, validateErr := Validate(ctx, deps, opts); validateErr != nil || st.Phase != PhaseValidatedPass {
		t.Fatalf("Validate = %q, %v; want %q", st.Phase, validateErr, PhaseValidatedPass)
	}
	if st, commitErr := Commit(ctx, deps, opts); commitErr != nil || st.Phase != PhaseCommitted {
		t.Fatalf("Commit = %q, %v; want %q", st.Phase, commitErr, PhaseCommitted)
	}

	snapshotName := SnapshotName(time.Unix(1_700_000_000, 0))
	want := []recordedMail{
		expectedAlertMail(
			"[OPNsense-test] ERROR: opnsense-upgrade prepare: snapshot creation failed",
			"opnsense-upgrade prepare: snapshot creation failed\n\n"+
				"What:    qm snapshot 101: exit status 2\n\n"+
				"Alert_key: 101\n"+
				"Alert_kind: opnsense-upgrade-prepare\n"+
				"Snapshot: "+snapshotName+"\n"+
				"Transition: true\n"+
				"Vmid: 101",
		),
		expectedAlertMail(
			"[OPNsense-test] ERROR: opnsense-upgrade validate: 1 check(s) failed",
			"opnsense-upgrade validate: 1 check(s) failed\n\n"+
				"Where:   phase=validated_fail\n\n"+
				"Alert_key: 101\n"+
				"Alert_kind: opnsense-upgrade-validate\n"+
				"Failing_count: 1\n"+
				"Transition: true\n"+
				"Vmid: 101",
		),
		expectedAlertMail(
			"[OPNsense-test] ERROR: RECOVERED: opnsense-upgrade validate: all checks passed",
			"RECOVERED: opnsense-upgrade validate: all checks passed\n\n"+
				"Alert_key: 101\n"+
				"Alert_kind: opnsense-upgrade-validate\n"+
				"Resolved: true",
		),
	}
	got := recorder.recorded()
	if len(got) != len(want) {
		for i, mail := range got {
			t.Logf("mail %d: subject %q", i, mail.message.Subject)
		}
		t.Fatalf("recorded %d mails, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i].config, want[i].config) {
			t.Errorf("mail %d send-email config = %+v, want %+v", i, got[i].config, want[i].config)
		}
		if got[i].message.To != want[i].message.To || got[i].message.From != want[i].message.From ||
			got[i].message.Caller != want[i].message.Caller || got[i].message.Name != want[i].message.Name {
			t.Errorf("mail %d envelope = to %q from %q caller %q name %q, want to %q from %q caller %q name %q",
				i, got[i].message.To, got[i].message.From, got[i].message.Caller, got[i].message.Name,
				want[i].message.To, want[i].message.From, want[i].message.Caller, want[i].message.Name)
		}
		if got[i].message.Subject != want[i].message.Subject {
			t.Errorf("mail %d subject = %q, want %q", i, got[i].message.Subject, want[i].message.Subject)
		}
		if got[i].message.Body != want[i].message.Body {
			t.Errorf("mail %d body:\n--- got ---\n%s\n--- want ---\n%s", i, got[i].message.Body, want[i].message.Body)
		}
	}
}
