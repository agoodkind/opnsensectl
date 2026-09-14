package opnsense_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
	"goodkind.io/opnsensectl/internal/svc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// startInProcessExecServer wires an svc.Server behind a
// bufconn-backed gRPC server so the test can drive the real Exec
// handler over the generated client stub.
func startInProcessExecServer(t *testing.T) mwanv1.OpnsenseServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := svc.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir()+"/config.xml", t.TempDir())
	grpcServer := grpc.NewServer()
	mwanv1.RegisterOpnsenseServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return mwanv1.NewOpnsenseServiceClient(conn)
}

func TestExec_StreamRoundTrip(t *testing.T) {
	client := startInProcessExecServer(t)

	stream, err := client.Exec(context.Background())
	if err != nil {
		t.Fatalf("Exec open: %v", err)
	}
	if err := stream.Send(&mwanv1.ExecRequest{
		Body: &mwanv1.ExecRequest_Header{Header: &mwanv1.ExecHeader{
			Command:        "/bin/cat",
			Args:           []string{},
			Sudo:           false,
			TimeoutSeconds: 10,
		}},
	}); err != nil {
		t.Fatalf("send header: %v", err)
	}
	if err := stream.Send(&mwanv1.ExecRequest{
		Body: &mwanv1.ExecRequest_StdinChunk{StdinChunk: []byte("hello-from-stdin\n")},
	}); err != nil {
		t.Fatalf("send stdin chunk: %v", err)
	}
	if err := stream.Send(&mwanv1.ExecRequest{
		Body: &mwanv1.ExecRequest_StdinClose{StdinClose: true},
	}); err != nil {
		t.Fatalf("send stdin close: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}

	deadline := time.After(10 * time.Second)
	var stdout []byte
	var sawTerminal bool
	var exitCode int32 = -1
	for !sawTerminal {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for terminal; stdout=%q", stdout)
		default:
		}
		msg, recvErr := stream.Recv()
		if recvErr != nil {
			t.Fatalf("recv: %v", recvErr)
		}
		switch body := msg.GetBody().(type) {
		case *mwanv1.ExecResponse_StdoutChunk:
			stdout = append(stdout, body.StdoutChunk...)
		case *mwanv1.ExecResponse_StderrChunk:
			// /bin/cat is silent on stderr for the happy path.
			_ = body
		case *mwanv1.ExecResponse_Terminal:
			exitCode = body.Terminal.GetExitCode()
			sawTerminal = true
		}
	}
	if exitCode != 0 {
		t.Fatalf("exit_code=%d want 0; stdout=%q", exitCode, stdout)
	}
	if string(stdout) != "hello-from-stdin\n" {
		t.Fatalf("stdout=%q want %q", stdout, "hello-from-stdin\n")
	}
}
