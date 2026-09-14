package svc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
	"goodkind.io/opnsensectl/internal/version"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Server hosts the OpnsenseService and TransferService gRPC handlers.
// All mutating operations on /conf/config.xml take a snapshot first
// via backupConfigWithLog and return the backup path so the caller can
// immediately revert by issuing a write of the snapshot bytes.
type Server struct {
	mwanv1.UnimplementedOpnsenseServiceServer
	mwanv1.UnimplementedTransferServiceServer

	configPath string
	backupDir  string
	log        *slog.Logger
	clock      Clock
	deploy     *DeployManager
	transfer   *TransferManager
	onRestart  func()
	mu         sync.Mutex
}

// NewServer constructs a Server with package-default paths.
func NewServer(log *slog.Logger, configPath, backupDir string) *Server {
	return NewServerWithDeploy(log, configPath, backupDir, NewDeployManager(log, DeployConfig{
		BinaryDir:      "",
		StatePath:      "",
		PendingPath:    "",
		WriteStateFile: nil,
		NowFn:          nil,
	}))
}

// NewServerWithDeploy is the explicit form used by tests to inject a
// tmpdir-rooted DeployManager.
func NewServerWithDeploy(log *slog.Logger, configPath, backupDir string, deploy *DeployManager) *Server {
	if configPath == "" {
		configPath = ConfigPath
	}
	if backupDir == "" {
		backupDir = BackupDir
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		UnimplementedOpnsenseServiceServer: mwanv1.UnimplementedOpnsenseServiceServer{},
		UnimplementedTransferServiceServer: mwanv1.UnimplementedTransferServiceServer{},
		configPath:                         configPath,
		backupDir:                          backupDir,
		log:                                log,
		clock:                              realClock{},
		deploy:                             deploy,
		transfer:                           nil,
		onRestart:                          nil,
		mu:                                 sync.Mutex{},
	}
}

// SetRestartHook wires a shutdown function that RestartDaemon will
// invoke (in a goroutine after a short grace period so the gRPC
// response can flush). The daemon main typically wires this to
// context cancel so the serve loop exits and rc.d respawns the
// process from the staged binary.
func (s *Server) SetRestartHook(fn func()) {
	s.onRestart = fn
}

// AttachTransferManager wires a TransferManager onto the server.
func (s *Server) AttachTransferManager(tm *TransferManager) {
	s.transfer = tm
}

// CleanStaleDeployTemps removes orphaned deploy scratch files left by a
// prior crash. The daemon main calls this once at startup, before the
// serial port opens, so an interrupted deploy or upload cannot leave a
// stale .staged/.staged.tmp/upload temp wedged in the binary dir.
func (s *Server) CleanStaleDeployTemps(ctx context.Context) (int, error) {
	if s.deploy == nil {
		return 0, nil
	}
	return s.deploy.CleanStaleTemps(ctx)
}

// clampToInt32 saturates n to the int32 range for proto fields that
// carry small counts but originate from int-typed callees.
func clampToInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}

// Version returns the build metadata of the running binary.
func (s *Server) Version(_ context.Context, _ *mwanv1.VersionRequest) (*mwanv1.VersionResponse, error) {
	return &mwanv1.VersionResponse{
		Version:      version.BuildVersionString(),
		BuildCommit:  version.GitCommit(),
		BuildDirty:   version.GitDirty() == "dirty",
		BuildBinhash: version.BinaryHash(),
	}, nil
}

// Exec implements the bidi Exec stream. The first inbound message
// must be an ExecHeader; stdin chunks follow until stdin_close, then
// the server emits stdout, stderr, and an ExecTerminal.
func (s *Server) Exec(stream mwanv1.OpnsenseService_ExecServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "exec: recv header: %v", err)
	}
	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "exec: first message must be header")
	}
	s.log.InfoContext(ctx, "opnsensesvc: Exec",
		"exec_command", header.GetCommand(),
		"args_len", len(header.GetArgs()),
		"sudo", header.GetSudo(),
		"timeout_seconds", header.GetTimeoutSeconds())

	stdin := make([]byte, 0, 4096)
	for {
		msg, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return status.Errorf(codes.Canceled, "exec: recv: %v", recvErr)
		}
		switch body := msg.GetBody().(type) {
		case *mwanv1.ExecRequest_StdinChunk:
			stdin = append(stdin, body.StdinChunk...)
		case *mwanv1.ExecRequest_StdinClose:
			if body.StdinClose {
				break
			}
		case *mwanv1.ExecRequest_Cancel:
			return status.Error(codes.Canceled, "exec: client cancel")
		default:
			return status.Error(codes.InvalidArgument, "exec: unexpected body in stream")
		}
		if _, closed := msg.GetBody().(*mwanv1.ExecRequest_StdinClose); closed {
			break
		}
	}

	res, runErr := runExec(ctx, ExecArgs{
		Command:        header.GetCommand(),
		Args:           header.GetArgs(),
		Sudo:           header.GetSudo(),
		TimeoutSeconds: header.GetTimeoutSeconds(),
		StdinBytes:     stdin,
		Clock:          s.clock,
		Log:            s.log,
	})
	if runErr != nil {
		s.log.ErrorContext(ctx, "opnsensesvc: Exec failed", "err", runErr,
			"exec_command", header.GetCommand())
		return status.Errorf(codes.Internal, "exec: %v", runErr)
	}

	if sendErr := sendExecChunks(stream, res); sendErr != nil {
		return sendErr
	}
	if err := stream.Send(&mwanv1.ExecResponse{
		Body: &mwanv1.ExecResponse_Terminal{
			Terminal: &mwanv1.ExecTerminal{
				ExitCode:        res.ExitCode,
				DurationMs:      res.DurationMS,
				StdoutTruncated: res.StdoutTruncated,
				StderrTruncated: res.StderrTruncated,
				TimedOut:        res.TimedOut,
			},
		},
	}); err != nil {
		return fmt.Errorf("exec: send terminal: %w", err)
	}
	return nil
}

func sendExecChunks(stream mwanv1.OpnsenseService_ExecServer, res *ExecResult) error {
	ctx := stream.Context()
	if len(res.Stdout) > 0 {
		if err := stream.Send(&mwanv1.ExecResponse{
			Body: &mwanv1.ExecResponse_StdoutChunk{StdoutChunk: res.Stdout},
		}); err != nil {
			slog.ErrorContext(ctx, "exec: send stdout", "err", err)
			return fmt.Errorf("exec: send stdout: %w", err)
		}
	}
	if len(res.Stderr) > 0 {
		if err := stream.Send(&mwanv1.ExecResponse{
			Body: &mwanv1.ExecResponse_StderrChunk{StderrChunk: res.Stderr},
		}); err != nil {
			slog.ErrorContext(ctx, "exec: send stderr", "err", err)
			return fmt.Errorf("exec: send stderr: %w", err)
		}
	}
	return nil
}

// XPathGet streams one XPathMatch per result node.
func (s *Server) XPathGet(req *mwanv1.XPathGetRequest, stream mwanv1.OpnsenseService_XPathGetServer) error {
	ctx := stream.Context()
	content, err := readConfigWithLog(ctx, s.log, s.configPath)
	if err != nil {
		return status.Errorf(codes.Internal, "read: %v", err)
	}
	matches, err := xpathGetWithLog(ctx, s.log, content, req.GetExpression())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "xpath: %v", err)
	}
	s.log.InfoContext(ctx, "opnsensesvc: XPathGet",
		"expression", req.GetExpression(), "matches", len(matches))
	for _, m := range matches {
		if sendErr := stream.Send(&mwanv1.XPathMatch{Match: m}); sendErr != nil {
			s.log.ErrorContext(ctx, "xpath: send match", "err", sendErr)
			return fmt.Errorf("xpath: send match: %w", sendErr)
		}
	}
	return nil
}

// XPathSet applies a snapshot-then-set sequence to config.xml.
func (s *Server) XPathSet(ctx context.Context, req *mwanv1.XPathSetRequest) (*mwanv1.XPathSetResponse, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()

	content, err := readConfigWithLog(ctx, s.log, s.configPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read: %v", err)
	}
	updated, n, err := xpathSetWithLog(ctx, s.log, content, req.GetExpression(), req.GetNewValue())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "xpath: %v", err)
	}
	if n == 0 {
		return &mwanv1.XPathSetResponse{BackupPath: "", ChangedCount: 0}, nil
	}
	backupPath, err := backupConfigWithLog(ctx, s.log, s.clock, s.configPath, s.backupDir, "xpath-set")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "backup: %v", err)
	}
	if writeErr := writeConfigWithLog(ctx, s.log, s.configPath, updated); writeErr != nil {
		return nil, status.Errorf(codes.Internal, "write: %v", writeErr)
	}
	s.log.InfoContext(ctx, "opnsensesvc: XPathSet",
		"expression", req.GetExpression(), "changed_count", n, "backup_path", backupPath)
	return &mwanv1.XPathSetResponse{
		BackupPath:   backupPath,
		ChangedCount: clampToInt32(n),
	}, nil
}

// XPathDelete applies a snapshot-then-delete sequence to config.xml.
func (s *Server) XPathDelete(ctx context.Context, req *mwanv1.XPathDeleteRequest) (*mwanv1.XPathDeleteResponse, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()

	content, err := readConfigWithLog(ctx, s.log, s.configPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read: %v", err)
	}
	updated, n, err := xpathDeleteWithLog(ctx, s.log, content, req.GetExpression())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "xpath: %v", err)
	}
	if n == 0 {
		return &mwanv1.XPathDeleteResponse{BackupPath: "", DeletedCount: 0}, nil
	}
	backupPath, err := backupConfigWithLog(ctx, s.log, s.clock, s.configPath, s.backupDir, "xpath-delete")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "backup: %v", err)
	}
	if writeErr := writeConfigWithLog(ctx, s.log, s.configPath, updated); writeErr != nil {
		return nil, status.Errorf(codes.Internal, "write: %v", writeErr)
	}
	s.log.InfoContext(ctx, "opnsensesvc: XPathDelete",
		"expression", req.GetExpression(), "deleted_count", n, "backup_path", backupPath)
	return &mwanv1.XPathDeleteResponse{
		BackupPath:   backupPath,
		DeletedCount: clampToInt32(n),
	}, nil
}

// BackupConfigXML takes an explicit snapshot of /conf/config.xml.
func (s *Server) BackupConfigXML(ctx context.Context, req *mwanv1.BackupConfigXMLRequest) (*mwanv1.BackupConfigXMLResponse, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	backupPath, err := backupConfigWithLog(ctx, s.log, s.clock, s.configPath, s.backupDir, req.GetLabel())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "backup: %v", err)
	}
	content, err := readConfigWithLog(ctx, s.log, backupPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stat: %v", err)
	}
	s.log.InfoContext(ctx, "opnsensesvc: BackupConfigXML",
		"backup_path", backupPath, "size_bytes", len(content))
	return &mwanv1.BackupConfigXMLResponse{
		BackupPath: backupPath,
		SizeBytes:  int64(len(content)),
	}, nil
}

// StripGatewayV6 removes the IPv6 gateway from the WAN interface config.
func (s *Server) StripGatewayV6(ctx context.Context, _ *mwanv1.StripGatewayV6Request) (*mwanv1.StripGatewayV6Response, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()

	content, err := readConfigWithLog(ctx, s.log, s.configPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read: %v", err)
	}
	updated, changed, err := stripGatewayV6WithLog(ctx, s.log, content)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "strip: %v", err)
	}
	if !changed {
		return &mwanv1.StripGatewayV6Response{BackupPath: "", Changed: false}, nil
	}
	backupPath, err := backupConfigWithLog(ctx, s.log, s.clock, s.configPath, s.backupDir, "strip-gatewayv6")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "backup: %v", err)
	}
	if writeErr := writeConfigWithLog(ctx, s.log, s.configPath, updated); writeErr != nil {
		return nil, status.Errorf(codes.Internal, "write: %v", writeErr)
	}
	s.log.InfoContext(ctx, "opnsensesvc: StripGatewayV6", "backup_path", backupPath)
	return &mwanv1.StripGatewayV6Response{BackupPath: backupPath, Changed: true}, nil
}

// InjectGatewayV6 inserts <gatewayv6>NAME</gatewayv6> before </wan>.
func (s *Server) InjectGatewayV6(ctx context.Context, req *mwanv1.InjectGatewayV6Request) (*mwanv1.InjectGatewayV6Response, error) {
	if req.GetGatewayName() == "" {
		return nil, status.Error(codes.InvalidArgument, "gateway_name required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	content, err := readConfigWithLog(ctx, s.log, s.configPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read: %v", err)
	}
	updated, changed, err := injectGatewayV6WithLog(ctx, s.log, content, req.GetGatewayName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "inject: %v", err)
	}
	if !changed {
		return &mwanv1.InjectGatewayV6Response{BackupPath: "", Changed: false}, nil
	}
	backupPath, err := backupConfigWithLog(ctx, s.log, s.clock, s.configPath, s.backupDir, "inject-gatewayv6")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "backup: %v", err)
	}
	if writeErr := writeConfigWithLog(ctx, s.log, s.configPath, updated); writeErr != nil {
		return nil, status.Errorf(codes.Internal, "write: %v", writeErr)
	}
	return &mwanv1.InjectGatewayV6Response{BackupPath: backupPath, Changed: true}, nil
}

// DeployStatus returns current deploy state. With Mark=MARK_HEALTHY,
// it also clears the pending-verify marker and stamps health=ok.
func (s *Server) DeployStatus(ctx context.Context, req *mwanv1.DeployStatusRequest) (*mwanv1.DeployStatusResponse, error) {
	if s.deploy == nil {
		return nil, status.Error(codes.FailedPrecondition, "DeployStatus: DeployManager not configured")
	}
	if req.GetMark() == mwanv1.DeployStatusRequest_MARK_HEALTHY {
		active, previous, deployedAt, err := s.deploy.MarkHealthy(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "mark healthy: %v", err)
		}
		return &mwanv1.DeployStatusResponse{
			ActiveSha256:   active,
			PreviousSha256: previous,
			Health:         HealthOK,
			DeployedAt:     deployedAt,
		}, nil
	}
	active, previous, health, deployedAt, err := s.deploy.Status(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "status: %v", err)
	}
	return &mwanv1.DeployStatusResponse{
		ActiveSha256:   active,
		PreviousSha256: previous,
		Health:         health,
		DeployedAt:     deployedAt,
	}, nil
}

// Revert copies .previous to .current, clears pending-verify, re-execs.
func (s *Server) Revert(ctx context.Context, _ *mwanv1.RevertRequest) (*mwanv1.RevertResponse, error) {
	if s.deploy == nil {
		return nil, status.Error(codes.FailedPrecondition, "Revert: DeployManager not configured")
	}
	revertedTo, err := s.deploy.Revert(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "revert: %v", err)
	}
	return &mwanv1.RevertResponse{RevertedToSha256: revertedTo}, nil
}

// StageBinary verifies the sha256 of a previously staged binary file,
// atomically swaps it into the active slot, drops the pending-verify
// marker, and returns. It does not restart the daemon; the caller
// invokes RestartDaemon when ready to take the new binary live.
func (s *Server) StageBinary(ctx context.Context, req *mwanv1.StageBinaryRequest) (*mwanv1.StageBinaryResponse, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	if s.deploy == nil {
		return nil, status.Error(codes.FailedPrecondition, "StageBinary: DeployManager not configured")
	}
	if req.GetStagedSha256() == "" {
		return nil, status.Error(codes.InvalidArgument, "StageBinary: staged_sha256 required")
	}
	stagedPath := s.deploy.pathOf(BinaryName + ".staged")
	previousPath, activeSHA, err := commitStagedBinary(ctx, s.log, s.deploy, stagedPath, req.GetStagedSha256(), req.GetVersionStr())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stage: %v", err)
	}
	s.log.InfoContext(ctx, "opnsensesvc: StageBinary audit",
		"staged_sha256", req.GetStagedSha256(),
		"active_sha256", activeSHA,
		"previous_path", previousPath,
		"version_str", req.GetVersionStr())
	return &mwanv1.StageBinaryResponse{
		PreviousPath: previousPath,
		ActiveSha256: activeSHA,
	}, nil
}

// RestartDaemon initiates a clean shutdown of the running daemon.
// The actual shutdown runs asynchronously after a short grace so the
// gRPC response can flush before the yamux session goes away. The
// rc.d supervisor respawns the process from the active binary slot.
func (s *Server) RestartDaemon(ctx context.Context, _ *mwanv1.RestartDaemonRequest) (*mwanv1.RestartDaemonResponse, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	if s.onRestart == nil {
		return nil, status.Error(codes.FailedPrecondition, "RestartDaemon: shutdown hook not configured")
	}
	s.log.InfoContext(ctx, "opnsensesvc: RestartDaemon requested")
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "opnsensesvc: RestartDaemon hook panic",
					"panic", r, "err", fmt.Errorf("panic: %v", r))
			}
		}()
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		s.onRestart()
	}()
	return &mwanv1.RestartDaemonResponse{}, nil
}

// commitStagedBinary reads the staged file, verifies sha256, and
// hands it to the deploy manager's path-based promote routine.
func commitStagedBinary(ctx context.Context, log *slog.Logger, deploy *DeployManager, stagedPath, expectedSHA, versionStr string) (string, string, error) {
	content, err := ReadStaged(ctx, stagedPath)
	if err != nil {
		return "", "", logWrappedErrorContext(ctx, log,
			"opnsensesvc: commit read staged",
			"read staged "+stagedPath, err,
			slog.String("staged_path", stagedPath))
	}
	sum := sha256.Sum256(content)
	gotHex := hex.EncodeToString(sum[:])
	if gotHex != expectedSHA {
		return "", "", fmt.Errorf("sha256 mismatch: got %s want %s", gotHex, expectedSHA)
	}
	log.InfoContext(ctx, "opnsensesvc: CommitDeploy verify ok",
		"staged_path", stagedPath, "sha256", gotHex, "bytes", len(content))
	previousPath, activeSHA, err := deploy.DeployFromPath(ctx, stagedPath, gotHex, versionStr)
	if err != nil {
		return "", "", logWrappedErrorContext(ctx, log,
			"opnsensesvc: commit promote",
			"promote", err,
			slog.String("staged_path", stagedPath))
	}
	return previousPath, activeSHA, nil
}

// ServeOpts controls the daemon's listener wiring.
type ServeOpts struct {
	SerialPath   string
	OpenSerial   func(path string) (io.ReadWriteCloser, error)
	Server       *Server
	Log          *slog.Logger
	OnSerialOpen func(path string)
	OnGRPCAccept func()
	Transfer     *TransferManager
	StopTimeout  time.Duration
}

// Compile-time guards.
var (
	_ mwanv1.OpnsenseServiceServer = (*Server)(nil)
	_ mwanv1.TransferServiceServer = (*Server)(nil)
)

// ReadStaged reads the entire content of a staged-binary path.
func ReadStaged(ctx context.Context, path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		slog.ErrorContext(ctx, "opnsensesvc: read staged", "path", path, "err", err)
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return content, nil
}
