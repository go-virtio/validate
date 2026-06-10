package vtest

import (
	"bytes"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
)

// le32 builds a little-endian dword for the hand-derived expected byte streams,
// matching the style of go-virtio/venus/proof. The server reads every wire
// value as a 32-bit host-order (little-endian) integer.
func le32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestEncodeHeader(t *testing.T) {
	// [length, cmd] two LE dwords. VTEST_HDR_SIZE 2; CMD_LEN 0, CMD_ID 1.
	got := encodeHeader(10, VcmdResourceCreate)
	want := concat(le32(10), le32(2))
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeHeader\n got=%v\nwant=%v", got, want)
	}
	if len(got) != hdrBytes {
		t.Fatalf("header len = %d, want %d", len(got), hdrBytes)
	}
}

func TestEncodePingProtocolVersion(t *testing.T) {
	// VCMD_PING_PROTOCOL_VERSION_SIZE 0 → header only [0, 10].
	got := encodePingProtocolVersion()
	want := concat(le32(0), le32(VcmdPingProtocolVersion))
	if !bytes.Equal(got, want) {
		t.Fatalf("ping\n got=%v\nwant=%v", got, want)
	}
}

func TestEncodeProtocolVersion(t *testing.T) {
	// header [1, 11] + one dword version. VCMD_PROTOCOL_VERSION_SIZE 1.
	got := encodeProtocolVersion(VtestProtocolVersion)
	want := concat(le32(1), le32(VcmdProtocolVersion), le32(2))
	if !bytes.Equal(got, want) {
		t.Fatalf("protocol version\n got=%v\nwant=%v", got, want)
	}
}

func TestEncodeCreateRenderer(t *testing.T) {
	// header length is BYTE count of the name; payload is raw name bytes,
	// no NUL on the wire. name="go-virtio" → 9 bytes.
	name := "go-virtio"
	got := encodeCreateRenderer(name)
	want := concat(le32(uint32(len(name))), le32(VcmdCreateRenderer), []byte(name))
	if !bytes.Equal(got, want) {
		t.Fatalf("create renderer\n got=%v\nwant=%v", got, want)
	}
	// empty name → length 0, header only.
	if e := encodeCreateRenderer(""); !bytes.Equal(e, concat(le32(0), le32(VcmdCreateRenderer))) {
		t.Fatalf("create renderer empty = %v", e)
	}
}

func TestEncodeResourceCreate(t *testing.T) {
	// 10-dword payload in VCMD_RES_CREATE_* order. Values chosen to mirror a
	// B8G8R8A8 2D render target like go-virtio/gpu builds.
	a := ResourceCreateArgs{
		Handle: 1, Target: 2, Format: 1, Bind: 2,
		Width: 4, Height: 4, Depth: 1, ArraySize: 1,
		LastLevel: 0, NrSamples: 0,
	}
	got := encodeResourceCreate(a)
	want := concat(
		le32(vcmdResCreateSize), le32(VcmdResourceCreate),
		le32(1), le32(2), le32(1), le32(2),
		le32(4), le32(4), le32(1), le32(1), le32(0), le32(0),
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("resource create\n got=%v\nwant=%v", got, want)
	}
	// header says 10 dwords; total payload must be 10*4 bytes after the header.
	if len(got)-hdrBytes != vcmdResCreateSize*4 {
		t.Fatalf("resource create payload = %d bytes, want %d", len(got)-hdrBytes, vcmdResCreateSize*4)
	}
}

func TestEncodeSubmitCmd(t *testing.T) {
	// A tiny 2-dword "virgl command buffer" stand-in. Header length = dwords.
	cmdBuf := concat(le32(0xAABBCCDD), le32(0x11223344))
	got, err := encodeSubmitCmd(cmdBuf)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	want := concat(le32(2), le32(VcmdSubmitCmd), cmdBuf)
	if !bytes.Equal(got, want) {
		t.Fatalf("submit cmd\n got=%v\nwant=%v", got, want)
	}
}

func TestEncodeSubmitCmdMisaligned(t *testing.T) {
	// 3 bytes is not a whole number of dwords → error, no packet.
	if _, err := encodeSubmitCmd([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for misaligned cmd buffer")
	}
}

func TestEncodeTransferGet(t *testing.T) {
	// 11-dword payload in VCMD_TRANSFER_* order. 4x4 BGRA → 64 bytes readback.
	a := TransferGetArgs{
		Handle: 1, Level: 0, Stride: 16, LayerStride: 64,
		X: 0, Y: 0, Z: 0, Width: 4, Height: 4, Depth: 1,
		DataSize: 64,
	}
	got := encodeTransferGet(a)
	want := concat(
		le32(vcmdTransferHdrSize), le32(VcmdTransferGet),
		le32(1), le32(0), le32(16), le32(64),
		le32(0), le32(0), le32(0), le32(4), le32(4), le32(1),
		le32(64),
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("transfer get\n got=%v\nwant=%v", got, want)
	}
}

func TestEncodeTransferPut(t *testing.T) {
	// 11-dword transfer header in VCMD_TRANSFER_* order, then DataSize bytes of
	// payload. Header length = VCMD_TRANSFER_HDR_SIZE (11) + len(data)/4 dwords.
	// 12 bytes of data = 3 dwords of a vertex buffer upload (handle 2).
	data := concat(le32(0x3f800000), le32(0x00000000), le32(0xbf800000))
	a := TransferGetArgs{
		Handle: 2, Level: 0, Stride: 12, LayerStride: 12,
		X: 0, Y: 0, Z: 0, Width: 3, Height: 1, Depth: 1,
		DataSize: uint32(len(data)),
	}
	got := encodeTransferPut(a, data)
	want := concat(
		le32(vcmdTransferHdrSize+uint32(len(data))/4), le32(VcmdTransferPut),
		le32(2), le32(0), le32(12), le32(12),
		le32(0), le32(0), le32(0), le32(3), le32(1), le32(1),
		le32(uint32(len(data))),
		data,
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("transfer put\n got=%v\nwant=%v", got, want)
	}
	// header advertises 11 + data dwords; total wire bytes after the 8-byte
	// header must be (11*4 + len(data)).
	if len(got)-hdrBytes != vcmdTransferHdrSize*4+len(data) {
		t.Fatalf("transfer put payload = %d bytes, want %d", len(got)-hdrBytes, vcmdTransferHdrSize*4+len(data))
	}
}

// ---- live-method tests against a scripted in-memory server ---------------

// fakeServer is an io.ReadWriter: Write captures what the client sends; Read
// hands back a pre-scripted reply stream. This exercises the Client methods'
// framing/reply-parsing without a real virgl_test_server.
type fakeServer struct {
	sent  bytes.Buffer // what the client wrote
	reply bytes.Reader // what the server "replies"
}

func (f *fakeServer) Write(p []byte) (int, error) { return f.sent.Write(p) }
func (f *fakeServer) Read(p []byte) (int, error)  { return f.reply.Read(p) }

func newFake(reply []byte) *fakeServer {
	f := &fakeServer{}
	f.reply.Reset(reply)
	return f
}

func TestHandshake(t *testing.T) {
	// Scripted server replies: ping reply [0,10], then version reply
	// [1,11]+[2].
	reply := concat(
		le32(0), le32(VcmdPingProtocolVersion),
		le32(1), le32(VcmdProtocolVersion), le32(2),
	)
	f := newFake(reply)
	c := New(f)

	neg, err := c.Handshake("go-virtio", VtestProtocolVersion)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if neg != 2 {
		t.Fatalf("negotiated = %d, want 2", neg)
	}
	// Verify the client emitted create-renderer, ping, protocol-version in order.
	want := concat(
		encodeCreateRenderer("go-virtio"),
		encodePingProtocolVersion(),
		encodeProtocolVersion(VtestProtocolVersion),
	)
	if !bytes.Equal(f.sent.Bytes(), want) {
		t.Fatalf("handshake bytes\n got=%v\nwant=%v", f.sent.Bytes(), want)
	}
}

func TestCreateResource(t *testing.T) {
	f := newFake(nil)
	c := New(f)
	a := ResourceCreateArgs{Handle: 1, Target: 2, Format: 1, Bind: 2, Width: 4, Height: 4, Depth: 1, ArraySize: 1}
	if err := c.CreateResource(a); err != nil {
		t.Fatalf("create resource: %v", err)
	}
	if !bytes.Equal(f.sent.Bytes(), encodeResourceCreate(a)) {
		t.Fatal("create resource bytes mismatch")
	}
}

func TestSubmitCmd(t *testing.T) {
	f := newFake(nil)
	c := New(f)
	cmdBuf := concat(le32(0xDEADBEEF), le32(0xFEEDFACE))
	if err := c.SubmitCmd(cmdBuf); err != nil {
		t.Fatalf("submit: %v", err)
	}
	pkt, _ := encodeSubmitCmd(cmdBuf)
	if !bytes.Equal(f.sent.Bytes(), pkt) {
		t.Fatal("submit bytes mismatch")
	}
	// misaligned buffer surfaces the encoder error through the method.
	if err := c.SubmitCmd([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected misalignment error")
	}
}

func TestTransferGet(t *testing.T) {
	// Server replies with 8 bytes of "pixels": two red BGRA pixels
	// (B=0,G=0,R=0xFF,A=0xFF) → 0x00,0x00,0xFF,0xFF repeated.
	pixels := []byte{0x00, 0x00, 0xFF, 0xFF, 0x00, 0x00, 0xFF, 0xFF}
	f := newFake(pixels)
	c := New(f)
	a := TransferGetArgs{Handle: 1, Width: 2, Height: 1, Depth: 1, DataSize: uint32(len(pixels))}
	got, err := c.TransferGet(a)
	if err != nil {
		t.Fatalf("transfer get: %v", err)
	}
	if !bytes.Equal(got, pixels) {
		t.Fatalf("readback\n got=%v\nwant=%v", got, pixels)
	}
	if !bytes.Equal(f.sent.Bytes(), encodeTransferGet(a)) {
		t.Fatal("transfer get request bytes mismatch")
	}
}

func TestTransferPut(t *testing.T) {
	// No reply body. Confirm the client writes exactly encodeTransferPut's bytes.
	f := newFake(nil)
	c := New(f)
	data := concat(le32(0xCAFEBABE), le32(0x0BADF00D))
	a := TransferGetArgs{Handle: 2, Width: 2, Height: 1, Depth: 1, DataSize: uint32(len(data))}
	if err := c.TransferPut(a, data); err != nil {
		t.Fatalf("transfer put: %v", err)
	}
	if !bytes.Equal(f.sent.Bytes(), encodeTransferPut(a, data)) {
		t.Fatal("transfer put request bytes mismatch")
	}
}

func TestTransferPutWriteError(t *testing.T) {
	if err := New(&errRW{failWrite: true}).TransferPut(TransferGetArgs{DataSize: 4}, []byte{1, 2, 3, 4}); err == nil {
		t.Fatal("want transfer put write error")
	}
}

func TestDial(t *testing.T) {
	// Success path: stand up a raw Unix listener (no virgl_test_server needed)
	// and confirm Dial connects and the returned Client can write a frame.
	dir := t.TempDir()
	sock := filepath.Join(dir, "vtest.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan []byte, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			done <- nil
			return
		}
		defer conn.Close()
		buf := make([]byte, hdrBytes)
		_, _ = io.ReadFull(conn, buf)
		done <- buf
	}()

	c, conn, err := Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := c.CreateResource(ResourceCreateArgs{Handle: 7}); err != nil {
		t.Fatalf("write after dial: %v", err)
	}
	got := <-done
	if got == nil {
		t.Fatal("server did not accept/read")
	}

	// Error path: a socket path that cannot be dialed.
	if _, _, err := Dial(filepath.Join(dir, "does-not-exist.sock")); err == nil {
		t.Fatal("want dial error for missing socket")
	}
}

// ---- error-path coverage --------------------------------------------------

// errRW returns an error on Read and/or Write to exercise the wrapped error
// paths in the Client methods.
type errRW struct {
	failWrite bool
	rd        io.Reader // for partial-read scenarios
}

var errBoom = errors.New("boom")

func (e *errRW) Write(p []byte) (int, error) {
	if e.failWrite {
		return 0, errBoom
	}
	return len(p), nil
}
func (e *errRW) Read(p []byte) (int, error) {
	if e.rd != nil {
		return e.rd.Read(p)
	}
	return 0, errBoom
}

func TestHandshakeWriteErrors(t *testing.T) {
	if _, err := New(&errRW{failWrite: true}).Handshake("x", 2); err == nil {
		t.Fatal("want create-renderer write error")
	}
}

func TestHandshakePingWriteError(t *testing.T) {
	// create-renderer write succeeds, ping write fails: a writer that fails on
	// the SECOND write.
	w := &nthFailWriter{failOn: 2}
	if _, err := New(w).Handshake("x", 2); err == nil {
		t.Fatal("want ping write error")
	}
}

func TestHandshakePingReplyError(t *testing.T) {
	// writes ok, but no reply bytes → ping reply read fails (EOF).
	f := &errRW{rd: bytes.NewReader(nil)}
	if _, err := New(f).Handshake("x", 2); err == nil {
		t.Fatal("want ping reply error")
	}
}

func TestHandshakeProtocolWriteError(t *testing.T) {
	// create + ping writes ok and ping reply readable, but protocol-version
	// write (3rd write) fails.
	w := &scriptedRW{
		writer: &nthFailWriter{failOn: 3},
		reader: bytes.NewReader(concat(le32(0), le32(VcmdPingProtocolVersion))),
	}
	if _, err := New(w).Handshake("x", 2); err == nil {
		t.Fatal("want protocol-version write error")
	}
}

func TestHandshakeProtocolReplyHeaderError(t *testing.T) {
	// ping reply present, but protocol-version reply header truncated.
	f := newFake(concat(le32(0), le32(VcmdPingProtocolVersion))) // only ping reply
	if _, err := New(f).Handshake("x", 2); err == nil {
		t.Fatal("want protocol reply header error")
	}
}

func TestHandshakeProtocolReplyBodyError(t *testing.T) {
	// ping reply + protocol header, but missing the version dword body.
	f := newFake(concat(
		le32(0), le32(VcmdPingProtocolVersion),
		le32(1), le32(VcmdProtocolVersion), // header but no body dword
	))
	if _, err := New(f).Handshake("x", 2); err == nil {
		t.Fatal("want protocol reply body error")
	}
}

func TestCreateResourceWriteError(t *testing.T) {
	if err := New(&errRW{failWrite: true}).CreateResource(ResourceCreateArgs{}); err == nil {
		t.Fatal("want create resource write error")
	}
}

func TestSubmitCmdWriteError(t *testing.T) {
	if err := New(&errRW{failWrite: true}).SubmitCmd(concat(le32(1))); err == nil {
		t.Fatal("want submit write error")
	}
}

func TestTransferGetWriteError(t *testing.T) {
	if _, err := New(&errRW{failWrite: true}).TransferGet(TransferGetArgs{DataSize: 4}); err == nil {
		t.Fatal("want transfer get write error")
	}
}

func TestTransferGetReadbackError(t *testing.T) {
	// write ok, but fewer readback bytes than DataSize → ReadFull errors.
	f := &scriptedRW{writer: &nthFailWriter{failOn: 0}, reader: bytes.NewReader([]byte{1, 2})}
	if _, err := New(f).TransferGet(TransferGetArgs{DataSize: 4}); err == nil {
		t.Fatal("want transfer get readback error")
	}
}

func TestReadHeaderError(t *testing.T) {
	c := New(&errRW{rd: bytes.NewReader([]byte{1, 2, 3})}) // <8 bytes
	if _, _, err := c.readHeader(); err == nil {
		t.Fatal("want short header error")
	}
}

// nthFailWriter fails on the Nth Write call (1-based); failOn<=0 never fails.
type nthFailWriter struct {
	failOn int
	n      int
}

func (w *nthFailWriter) Write(p []byte) (int, error) {
	w.n++
	if w.failOn > 0 && w.n == w.failOn {
		return 0, errBoom
	}
	return len(p), nil
}
func (w *nthFailWriter) Read(p []byte) (int, error) { return 0, io.EOF }

// scriptedRW pairs a custom writer with a custom reader.
type scriptedRW struct {
	writer io.Writer
	reader io.Reader
}

func (s *scriptedRW) Write(p []byte) (int, error) { return s.writer.Write(p) }
func (s *scriptedRW) Read(p []byte) (int, error)  { return s.reader.Read(p) }
