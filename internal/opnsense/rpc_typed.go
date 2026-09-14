package opnsense

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
)

// RPC exposes typed methods for the mwan-opnsense service. Each
// method is a thin wrapper around a generated gRPC client stub plus,
// for streaming RPCs, a synchronous drive loop.
type RPC struct {
	c *Client
}

// RPC returns the typed service wrapper for this client.
func (c *Client) RPC() *RPC { return &RPC{c: c} }

// ExecResult is the synchronous return shape from RPC.Exec.
type ExecResult struct {
	Stdout          []byte
	Stderr          []byte
	ExitCode        int32
	DurationMs      int64
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
}

// Version calls the Version RPC.
func (r *RPC) Version(ctx context.Context, req *mwanv1.VersionRequest) (*mwanv1.VersionResponse, error) {
	resp, err := r.c.opnsense.Version(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "Version", err)
	}
	return resp, nil
}

// Exec drives the bidi Exec stream synchronously: send the header,
// send all stdin in one chunk, close send, then drain stdout, stderr,
// and the terminal message.
func (r *RPC) Exec(ctx context.Context, command string, args []string, sudo bool, timeoutSeconds int32, stdin []byte) (*ExecResult, error) {
	stream, err := r.c.opnsense.Exec(ctx)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "Exec stream", err)
	}
	if sendErr := stream.Send(&mwanv1.ExecRequest{
		Body: &mwanv1.ExecRequest_Header{Header: &mwanv1.ExecHeader{
			Command:        command,
			Args:           args,
			Sudo:           sudo,
			TimeoutSeconds: timeoutSeconds,
		}},
	}); sendErr != nil {
		return nil, logWrap(ctx, r.c.log, "Exec send header", sendErr)
	}
	if len(stdin) > 0 {
		if sendErr := stream.Send(&mwanv1.ExecRequest{
			Body: &mwanv1.ExecRequest_StdinChunk{StdinChunk: stdin},
		}); sendErr != nil {
			return nil, logWrap(ctx, r.c.log, "Exec send stdin", sendErr)
		}
	}
	if sendErr := stream.Send(&mwanv1.ExecRequest{
		Body: &mwanv1.ExecRequest_StdinClose{StdinClose: true},
	}); sendErr != nil {
		return nil, logWrap(ctx, r.c.log, "Exec send stdin close", sendErr)
	}
	if closeErr := stream.CloseSend(); closeErr != nil {
		return nil, logWrap(ctx, r.c.log, "Exec close send", closeErr)
	}
	return drainExecResult(ctx, r.c, stream)
}

func drainExecResult(ctx context.Context, cli *Client, stream mwanv1.OpnsenseService_ExecClient) (*ExecResult, error) {
	res := &ExecResult{
		Stdout:          nil,
		Stderr:          nil,
		ExitCode:        0,
		DurationMs:      0,
		StdoutTruncated: false,
		StderrTruncated: false,
		TimedOut:        false,
	}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return res, logWrap(ctx, cli.log, "Exec recv", err)
		}
		switch body := msg.GetBody().(type) {
		case *mwanv1.ExecResponse_StdoutChunk:
			res.Stdout = append(res.Stdout, body.StdoutChunk...)
		case *mwanv1.ExecResponse_StderrChunk:
			res.Stderr = append(res.Stderr, body.StderrChunk...)
		case *mwanv1.ExecResponse_Terminal:
			res.ExitCode = body.Terminal.GetExitCode()
			res.DurationMs = body.Terminal.GetDurationMs()
			res.StdoutTruncated = body.Terminal.GetStdoutTruncated()
			res.StderrTruncated = body.Terminal.GetStderrTruncated()
			res.TimedOut = body.Terminal.GetTimedOut()
			return res, nil
		}
	}
}

// BackupConfigXML calls the BackupConfigXML RPC.
func (r *RPC) BackupConfigXML(ctx context.Context, req *mwanv1.BackupConfigXMLRequest) (*mwanv1.BackupConfigXMLResponse, error) {
	resp, err := r.c.opnsense.BackupConfigXML(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "BackupConfigXML", err)
	}
	return resp, nil
}

// XPathGet calls the server-streaming XPathGet RPC and collects every
// match into a slice.
func (r *RPC) XPathGet(ctx context.Context, req *mwanv1.XPathGetRequest) ([]string, error) {
	stream, err := r.c.opnsense.XPathGet(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "XPathGet open", err)
	}
	var matches []string
	for {
		m, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return matches, nil
		}
		if recvErr != nil {
			return matches, logWrap(ctx, r.c.log, "XPathGet recv", recvErr)
		}
		matches = append(matches, m.GetMatch())
	}
}

// XPathSet calls the XPathSet RPC.
func (r *RPC) XPathSet(ctx context.Context, req *mwanv1.XPathSetRequest) (*mwanv1.XPathSetResponse, error) {
	resp, err := r.c.opnsense.XPathSet(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "XPathSet", err)
	}
	return resp, nil
}

// XPathDelete calls the XPathDelete RPC.
func (r *RPC) XPathDelete(ctx context.Context, req *mwanv1.XPathDeleteRequest) (*mwanv1.XPathDeleteResponse, error) {
	resp, err := r.c.opnsense.XPathDelete(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "XPathDelete", err)
	}
	return resp, nil
}

// StripGatewayV6 calls the StripGatewayV6 RPC.
func (r *RPC) StripGatewayV6(ctx context.Context, req *mwanv1.StripGatewayV6Request) (*mwanv1.StripGatewayV6Response, error) {
	resp, err := r.c.opnsense.StripGatewayV6(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "StripGatewayV6", err)
	}
	return resp, nil
}

// InjectGatewayV6 calls the InjectGatewayV6 RPC.
func (r *RPC) InjectGatewayV6(ctx context.Context, req *mwanv1.InjectGatewayV6Request) (*mwanv1.InjectGatewayV6Response, error) {
	resp, err := r.c.opnsense.InjectGatewayV6(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "InjectGatewayV6", err)
	}
	return resp, nil
}

// DeployStatus calls the DeployStatus RPC.
func (r *RPC) DeployStatus(ctx context.Context, req *mwanv1.DeployStatusRequest) (*mwanv1.DeployStatusResponse, error) {
	resp, err := r.c.opnsense.DeployStatus(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "DeployStatus", err)
	}
	return resp, nil
}

// Revert calls the Revert RPC.
func (r *RPC) Revert(ctx context.Context, req *mwanv1.RevertRequest) (*mwanv1.RevertResponse, error) {
	resp, err := r.c.opnsense.Revert(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "Revert", err)
	}
	return resp, nil
}

// StageBinary calls the StageBinary RPC. The daemon verifies the
// previously staged file's sha256 and swaps it into the active slot
// without restarting itself.
func (r *RPC) StageBinary(ctx context.Context, req *mwanv1.StageBinaryRequest) (*mwanv1.StageBinaryResponse, error) {
	resp, err := r.c.opnsense.StageBinary(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "StageBinary", err)
	}
	return resp, nil
}

// RestartDaemon calls the RestartDaemon RPC. The daemon shuts down
// cleanly and rc.d respawns it from the active binary slot. The
// caller's connection will see the yamux session close shortly after
// this returns and must reconnect for further work.
func (r *RPC) RestartDaemon(ctx context.Context, req *mwanv1.RestartDaemonRequest) (*mwanv1.RestartDaemonResponse, error) {
	resp, err := r.c.opnsense.RestartDaemon(ctx, req)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "RestartDaemon", err)
	}
	return resp, nil
}

// ConfigXMLResult is the buffered result of a config.xml read.
type ConfigXMLResult struct {
	Content   []byte
	SizeBytes int64
	Sha256    string
}

// ReadConfigXML drives a TransferService.Upload(READ) call against
// /conf/config.xml and returns the buffered bytes plus their hex
// sha256.
func (r *RPC) ReadConfigXML(ctx context.Context) (*ConfigXMLResult, error) {
	stream, err := r.c.transfer.Upload(ctx)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "ReadConfigXML open", err)
	}
	header := &mwanv1.UploadRequest{
		Body: &mwanv1.UploadRequest_Header{Header: &mwanv1.TransferHeader{
			Path:             "/conf/config.xml",
			Direction:        mwanv1.TransferDirection_TRANSFER_DIRECTION_READ,
			FinishStep:       mwanv1.FinishStep_FINISH_STEP_UNSPECIFIED,
			ResumeTransferId: "",
			ResumeFromOffset: 0,
			Label:            "",
			TotalSize:        0,
		}},
	}
	if sendErr := stream.Send(header); sendErr != nil {
		return nil, logWrap(ctx, r.c.log, "ReadConfigXML send header", sendErr)
	}
	if closeErr := stream.CloseSend(); closeErr != nil {
		return nil, logWrap(ctx, r.c.log, "ReadConfigXML close send", closeErr)
	}
	var content []byte
	var terminalSHA string
	for {
		msg, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return nil, logWrap(ctx, r.c.log, "ReadConfigXML recv", recvErr)
		}
		switch body := msg.GetBody().(type) {
		case *mwanv1.UploadResponse_Ack:
			// Initial ack carries no payload for READ.
			_ = body
		case *mwanv1.UploadResponse_Data:
			content = append(content, body.Data.GetData()...)
		case *mwanv1.UploadResponse_Terminal:
			terminalSHA = body.Terminal.GetSha256Hex()
		}
	}
	sum := sha256.Sum256(content)
	gotHex := hex.EncodeToString(sum[:])
	if terminalSHA != "" && gotHex != terminalSHA {
		return nil, logWrap(ctx, r.c.log, "ReadConfigXML sha256 mismatch",
			errors.New("got "+gotHex+" want "+terminalSHA))
	}
	return &ConfigXMLResult{Content: content, SizeBytes: int64(len(content)), Sha256: gotHex}, nil
}

// WriteConfigXMLResult is the buffered result of a config.xml write.
type WriteConfigXMLResult struct {
	BackupPath   string
	BytesWritten int64
}

// WriteConfigXML drives a TransferService.Upload(WRITE,SNAPSHOT_THEN_REPLACE)
// call against /conf/config.xml.
func (r *RPC) WriteConfigXML(ctx context.Context, content []byte, label string) (*WriteConfigXMLResult, error) {
	if len(content) == 0 {
		return nil, errors.New("opnsense: WriteConfigXML empty content")
	}
	stream, err := r.c.transfer.Upload(ctx)
	if err != nil {
		return nil, logWrap(ctx, r.c.log, "WriteConfigXML open", err)
	}
	if sendErr := stream.Send(&mwanv1.UploadRequest{
		Body: &mwanv1.UploadRequest_Header{Header: &mwanv1.TransferHeader{
			Path:             "/conf/config.xml",
			Direction:        mwanv1.TransferDirection_TRANSFER_DIRECTION_WRITE,
			FinishStep:       mwanv1.FinishStep_FINISH_STEP_SNAPSHOT_THEN_REPLACE,
			ResumeTransferId: "",
			ResumeFromOffset: 0,
			Label:            label,
			TotalSize:        int64(len(content)),
		}},
	}); sendErr != nil {
		return nil, logWrap(ctx, r.c.log, "WriteConfigXML send header", sendErr)
	}
	ack, recvErr := stream.Recv()
	if recvErr != nil {
		return nil, logWrap(ctx, r.c.log, "WriteConfigXML recv ack", recvErr)
	}
	_ = ack
	const chunkSize = 16 << 10
	offset := int64(0)
	for offset < int64(len(content)) {
		end := min(offset+chunkSize, int64(len(content)))
		if sendErr := stream.Send(&mwanv1.UploadRequest{
			Body: &mwanv1.UploadRequest_Data{Data: &mwanv1.TransferDataChunk{
				Offset: offset,
				Data:   content[offset:end],
			}},
		}); sendErr != nil {
			return nil, logWrap(ctx, r.c.log, "WriteConfigXML send data", sendErr)
		}
		ackMsg, recvAckErr := stream.Recv()
		if recvAckErr != nil {
			return nil, logWrap(ctx, r.c.log, "WriteConfigXML recv data ack", recvAckErr)
		}
		dataAck, ok := ackMsg.GetBody().(*mwanv1.UploadResponse_DataAck)
		if !ok {
			return nil, logWrap(ctx, r.c.log, "WriteConfigXML expected data ack",
				fmt.Errorf("got %T", ackMsg.GetBody()))
		}
		if dataAck.DataAck.GetCommittedOffset() != end {
			return nil, logWrap(ctx, r.c.log, "WriteConfigXML data ack offset",
				fmt.Errorf("got %d want %d", dataAck.DataAck.GetCommittedOffset(), end))
		}
		offset = end
	}
	sum := sha256.Sum256(content)
	finalHex := hex.EncodeToString(sum[:])
	if sendErr := stream.Send(&mwanv1.UploadRequest{
		Body: &mwanv1.UploadRequest_Final{Final: &mwanv1.TransferFinal{Sha256Hex: finalHex}},
	}); sendErr != nil {
		return nil, logWrap(ctx, r.c.log, "WriteConfigXML send final", sendErr)
	}
	if closeErr := stream.CloseSend(); closeErr != nil {
		return nil, logWrap(ctx, r.c.log, "WriteConfigXML close send", closeErr)
	}
	for {
		msg, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return nil, errors.New("opnsense: WriteConfigXML stream ended before terminal")
		}
		if recvErr != nil {
			return nil, logWrap(ctx, r.c.log, "WriteConfigXML recv", recvErr)
		}
		if term := msg.GetTerminal(); term != nil {
			return &WriteConfigXMLResult{
				BackupPath:   term.GetBackupPath(),
				BytesWritten: term.GetTotalBytes(),
			}, nil
		}
	}
}
