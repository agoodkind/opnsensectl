package mwanv1_test

// The OPNsense proto moved from gen/mwan/v1 to this package while keeping
// proto package mwan.v1, so a daemon built before the move and a client built
// after it must still agree on every byte. The testdata files were recorded
// from the types generated at the old location (origin/main f3ed44aa). They
// cannot be produced by linking both packages in one binary, because the
// protobuf registry panics when two files register the same full names.

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	pb "goodkind.io/opnsensectl/gen/opnsense/v1"
)

// TestRecordedWireBytesDecodeIntoMovedTypes decodes every request and
// response recorded from the old types and requires the moved types to
// produce the same value and re-encode to the same bytes.
func TestRecordedWireBytesDecodeIntoMovedTypes(t *testing.T) {
	recorded := readRecordedWire(t)
	cases := wireCases()
	if len(recorded) != len(cases) {
		t.Fatalf("recorded %d wire cases, fixture has %d", len(recorded), len(cases))
	}
	for name, want := range cases {
		encoded, ok := recorded[name]
		if !ok {
			t.Errorf("%s: no recorded bytes", name)
			continue
		}
		got := want.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(encoded, got); err != nil {
			t.Errorf("%s: unmarshal recorded bytes: %v", name, err)
			continue
		}
		if !proto.Equal(got, want) {
			t.Errorf("%s: decoded %v, want %v", name, got, want)
		}
		reencoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(want)
		if err != nil {
			t.Errorf("%s: marshal: %v", name, err)
			continue
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Errorf("%s: encoded %x, recorded %x", name, reencoded, encoded)
		}
	}
}

// TestServiceNamesAndMethodPathsMatchRecorded requires the gRPC service
// names, method paths, and streaming shapes to equal the old ones.
func TestServiceNamesAndMethodPathsMatchRecorded(t *testing.T) {
	recorded, err := os.ReadFile(filepath.Join("testdata", "methods.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := renderMethods(); got != string(recorded) {
		t.Fatalf("method set changed\ngot:\n%s\nrecorded:\n%s", got, recorded)
	}
}

// TestSchemaMatchesRecorded compares the whole file descriptor, minus the
// file path and Go package, with the one recorded from the old types.
func TestSchemaMatchesRecorded(t *testing.T) {
	recorded, err := os.ReadFile(filepath.Join("testdata", "descriptor.binpb"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, recorded) {
		return
	}
	t.Fatalf("schema changed\ngot:\n%s\nrecorded:\n%s", describe(t, got), describe(t, recorded))
}

func describe(t *testing.T, encoded []byte) string {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{}
	if err := proto.Unmarshal(encoded, file); err != nil {
		t.Fatal(err)
	}
	return prototext.MarshalOptions{Multiline: true}.Format(file)
}

func readRecordedWire(t *testing.T) map[string][]byte {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "wire.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	recorded := map[string][]byte{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, encodedHex, ok := strings.Cut(scanner.Text(), " ")
		if !ok {
			t.Fatalf("malformed wire line %q", scanner.Text())
		}
		encoded, err := hex.DecodeString(encodedHex)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		recorded[name] = encoded
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// renderMethods lists every server handler and client method path.
func renderMethods() string {
	var lines []string
	for _, desc := range []grpc.ServiceDesc{pb.OpnsenseService_ServiceDesc, pb.TransferService_ServiceDesc} {
		for _, method := range desc.Methods {
			lines = append(lines, fmt.Sprintf("server /%s/%s unary", desc.ServiceName, method.MethodName))
		}
		for _, stream := range desc.Streams {
			lines = append(lines, fmt.Sprintf("server /%s/%s client_streams=%t server_streams=%t",
				desc.ServiceName, stream.StreamName, stream.ClientStreams, stream.ServerStreams))
		}
	}
	for _, path := range clientMethodPaths() {
		lines = append(lines, "client "+path)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n") + "\n"
}

// renderDescriptor serializes the file descriptor with the fields that name
// its location cleared, so only the wire-visible schema remains.
func renderDescriptor() ([]byte, error) {
	file := protodesc.ToFileDescriptorProto(pb.File_opnsense_v1_mwan_opnsense_proto)
	file.Name = nil
	file.SourceCodeInfo = nil
	if file.Options != nil {
		file.Options.GoPackage = nil
		if proto.Size(file.Options) == 0 {
			file.Options = nil
		}
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(file)
}

// clientMethodPaths lists the method paths the generated clients dial.
func clientMethodPaths() []string {
	return []string{
		pb.OpnsenseService_Version_FullMethodName,
		pb.OpnsenseService_Exec_FullMethodName,
		pb.OpnsenseService_XPathGet_FullMethodName,
		pb.OpnsenseService_XPathSet_FullMethodName,
		pb.OpnsenseService_XPathDelete_FullMethodName,
		pb.OpnsenseService_BackupConfigXML_FullMethodName,
		pb.OpnsenseService_StripGatewayV6_FullMethodName,
		pb.OpnsenseService_InjectGatewayV6_FullMethodName,
		pb.OpnsenseService_DeployStatus_FullMethodName,
		pb.OpnsenseService_Revert_FullMethodName,
		pb.OpnsenseService_StageBinary_FullMethodName,
		pb.OpnsenseService_RestartDaemon_FullMethodName,
		pb.TransferService_Upload_FullMethodName,
		pb.TransferService_Status_FullMethodName,
		pb.TransferService_Cancel_FullMethodName,
	}
}

// wireCases returns one populated value per request and response shape the
// OPNsense services carry, with every oneof variant, enum, repeated field,
// and negative integer represented.
func wireCases() map[string]proto.Message {
	return map[string]proto.Message{
		"VersionRequest": &pb.VersionRequest{},
		"VersionResponse": &pb.VersionResponse{
			Version:      "v1.2.3",
			BuildCommit:  "0123456789abcdef",
			BuildDirty:   true,
			BuildBinhash: "sha256:feedface",
		},
		"ExecRequest_Header": &pb.ExecRequest{Body: &pb.ExecRequest_Header{Header: &pb.ExecHeader{
			Command:        "/usr/local/sbin/configctl",
			Args:           []string{"filter", "reload", ""},
			Sudo:           true,
			TimeoutSeconds: 30,
		}}},
		"ExecRequest_StdinChunk":   &pb.ExecRequest{Body: &pb.ExecRequest_StdinChunk{StdinChunk: []byte{0x00, 0x01, 0xff, '\n'}}},
		"ExecRequest_StdinClose":   &pb.ExecRequest{Body: &pb.ExecRequest_StdinClose{StdinClose: true}},
		"ExecRequest_Cancel":       &pb.ExecRequest{Body: &pb.ExecRequest_Cancel{Cancel: &pb.ExecCancel{}}},
		"ExecResponse_StdoutChunk": &pb.ExecResponse{Body: &pb.ExecResponse_StdoutChunk{StdoutChunk: []byte("stdout\n")}},
		"ExecResponse_StderrChunk": &pb.ExecResponse{Body: &pb.ExecResponse_StderrChunk{StderrChunk: []byte("stderr\n")}},
		"ExecResponse_Terminal": &pb.ExecResponse{Body: &pb.ExecResponse_Terminal{Terminal: &pb.ExecTerminal{
			ExitCode:        -1,
			DurationMs:      1234567890123,
			StdoutTruncated: true,
			StderrTruncated: true,
			TimedOut:        true,
		}}},
		"XPathMatch":              &pb.XPathMatch{Match: "<gateway>WAN_GW</gateway>"},
		"XPathGetRequest":         &pb.XPathGetRequest{Expression: "//gateways/gateway_item"},
		"XPathSetRequest":         &pb.XPathSetRequest{Expression: "//system/hostname", NewValue: "router"},
		"XPathSetResponse":        &pb.XPathSetResponse{BackupPath: "/conf/backup/config-1.xml", ChangedCount: 2},
		"XPathDeleteRequest":      &pb.XPathDeleteRequest{Expression: "//staticroutes/route"},
		"XPathDeleteResponse":     &pb.XPathDeleteResponse{BackupPath: "/conf/backup/config-2.xml", DeletedCount: 3},
		"BackupConfigXMLRequest":  &pb.BackupConfigXMLRequest{Label: "pre-upgrade"},
		"BackupConfigXMLResponse": &pb.BackupConfigXMLResponse{BackupPath: "/conf/backup/config-3.xml", SizeBytes: 987654},
		"StripGatewayV6Request":   &pb.StripGatewayV6Request{},
		"StripGatewayV6Response":  &pb.StripGatewayV6Response{BackupPath: "/conf/backup/config-4.xml", Changed: true},
		"InjectGatewayV6Request":  &pb.InjectGatewayV6Request{GatewayName: "WAN_GW6"},
		"InjectGatewayV6Response": &pb.InjectGatewayV6Response{BackupPath: "/conf/backup/config-5.xml", Changed: true},
		"DeployStatusRequest":     &pb.DeployStatusRequest{Mark: pb.DeployStatusRequest_MARK_HEALTHY},
		"DeployStatusResponse": &pb.DeployStatusResponse{
			ActiveSha256:   "aa",
			PreviousSha256: "bb",
			Health:         "healthy",
			DeployedAt:     1789371623,
		},
		"RevertRequest":         &pb.RevertRequest{},
		"RevertResponse":        &pb.RevertResponse{RevertedToSha256: "cc"},
		"StageBinaryRequest":    &pb.StageBinaryRequest{StagedSha256: "dd", VersionStr: "v9"},
		"StageBinaryResponse":   &pb.StageBinaryResponse{PreviousPath: "/usr/local/bin/mwan.prev", ActiveSha256: "ee"},
		"RestartDaemonRequest":  &pb.RestartDaemonRequest{},
		"RestartDaemonResponse": &pb.RestartDaemonResponse{},
		"UploadRequest_Header": &pb.UploadRequest{Body: &pb.UploadRequest_Header{Header: &pb.TransferHeader{
			Path:             "/usr/local/bin/mwan",
			Direction:        pb.TransferDirection_TRANSFER_DIRECTION_WRITE,
			FinishStep:       pb.FinishStep_FINISH_STEP_STAGE,
			ResumeTransferId: "transfer-1",
			ResumeFromOffset: 4096,
			Label:            "deploy",
			TotalSize:        1 << 33,
		}}},
		"UploadRequest_Data": &pb.UploadRequest{Body: &pb.UploadRequest_Data{Data: &pb.TransferDataChunk{
			Offset: 8192,
			Data:   []byte{0xde, 0xad, 0xbe, 0xef},
		}}},
		"UploadRequest_Final":  &pb.UploadRequest{Body: &pb.UploadRequest_Final{Final: &pb.TransferFinal{Sha256Hex: "ff"}}},
		"UploadRequest_Cancel": &pb.UploadRequest{Body: &pb.UploadRequest_Cancel{Cancel: &pb.TransferCancel{}}},
		"UploadResponse_Ack": &pb.UploadResponse{Body: &pb.UploadResponse_Ack{Ack: &pb.TransferAck{
			TransferId:      "transfer-2",
			TotalBytes:      65536,
			ResumeAccepted:  true,
			CommittedOffset: 32768,
		}}},
		"UploadResponse_Data": &pb.UploadResponse{Body: &pb.UploadResponse_Data{Data: &pb.TransferDataChunk{
			Offset: 1,
			Data:   []byte("chunk"),
		}}},
		"UploadResponse_Terminal": &pb.UploadResponse{Body: &pb.UploadResponse_Terminal{Terminal: &pb.TransferTerminal{
			Sha256Hex:     "0f",
			TotalBytes:    77,
			BackupPath:    "/var/backups/mwan",
			StatusCode:    -2,
			StatusMessage: "staged",
			StagedPath:    "/usr/local/bin/mwan.staged",
		}}},
		"UploadResponse_DataAck": &pb.UploadResponse{Body: &pb.UploadResponse_DataAck{DataAck: &pb.TransferDataAck{CommittedOffset: 9}}},
		"StatusRequest":          &pb.StatusRequest{TransferId: "transfer-3"},
		"StatusResponse": &pb.StatusResponse{
			TransferId:      "transfer-3",
			Exists:          true,
			CommittedOffset: 10,
			TotalBytes:      20,
			Path:            "/tmp/upload",
		},
		"CancelRequest":  &pb.CancelRequest{TransferId: "transfer-4"},
		"CancelResponse": &pb.CancelResponse{WasPresent: true},
	}
}
