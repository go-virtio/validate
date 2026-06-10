package vtest

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// Client is a vtest protocol client speaking to a virgl_test_server over a
// connected stream (a *net.UnixConn in production; any io.ReadWriter in tests).
//
// The encoders are split out as package-level functions (encode*) so they can
// be unit-tested offline against hand-derived expected byte streams without a
// live server, mirroring the go-virtio/venus "proof" tests.
type Client struct {
	rw io.ReadWriter
}

// New wraps an already-connected stream.
func New(rw io.ReadWriter) *Client { return &Client{rw: rw} }

// Dial connects to a virgl_test_server listening on the given Unix socket path
// (use DefaultSocketName for the server's default).
func Dial(socketPath string) (*Client, net.Conn, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("vtest: dial %s: %w", socketPath, err)
	}
	return &Client{rw: conn}, conn, nil
}

// ---- header framing ------------------------------------------------------

// encodeHeader builds the 2-dword command header
// (vtest_protocol.h: VTEST_HDR_SIZE 2; dword[0]=length, dword[1]=cmd id).
// The meaning of length is command-specific; callers pass the value the
// server's read site expects (see encode* helpers below).
func encodeHeader(lengthDwords, cmdID uint32) []byte {
	b := make([]byte, hdrBytes)
	binary.LittleEndian.PutUint32(b[0:4], lengthDwords)
	binary.LittleEndian.PutUint32(b[4:8], cmdID)
	return b
}

// ---- handshake ------------------------------------------------------------

// encodePingProtocolVersion builds VCMD_PING_PROTOCOL_VERSION (cmd 10).
//
// vtest_protocol.h: VCMD_PING_PROTOCOL_VERSION_SIZE 0 — a header-only command
// with zero payload. The server (vtest_renderer.c:vtest_ping_protocol_version)
// replies with a bare header [0, 10].
func encodePingProtocolVersion() []byte {
	return encodeHeader(0, VcmdPingProtocolVersion)
}

// encodeProtocolVersion builds VCMD_PROTOCOL_VERSION (cmd 11).
//
// vtest_protocol.h: VCMD_PROTOCOL_VERSION_SIZE 1, payload dword[0]=version.
// Server (vtest_renderer.c:vtest_protocol_version) reads one dword, then
// replies header [1, 11] followed by one dword = MIN(requested, server max).
func encodeProtocolVersion(version uint32) []byte {
	b := encodeHeader(vcmdProtocolVersionSize, VcmdProtocolVersion)
	return binary.LittleEndian.AppendUint32(b, version)
}

// encodeCreateRenderer builds VCMD_CREATE_RENDERER (cmd 8).
//
// This is the special first command (vtest_server.c:vtest_client_dispatch_
// commands requires header[1]==VCMD_CREATE_RENDERER before any context). Unlike
// every other command the header length here is a BYTE count, not a dword
// count: vtest_create_context does `input->read(name, length)` then allocates
// length+1 and stores the bytes as the debug name. The name is NOT NUL-padded
// on the wire; the server appends its own NUL.
func encodeCreateRenderer(name string) []byte {
	raw := []byte(name)
	b := encodeHeader(uint32(len(raw)), VcmdCreateRenderer)
	return append(b, raw...)
}

// ---- resource create ------------------------------------------------------

// ResourceCreateArgs mirrors the 10-dword VCMD_RESOURCE_CREATE payload read by
// vtest_renderer.c:vtest_create_resource_decode_args, field order from
// vtest_protocol.h (VCMD_RES_CREATE_* indices).
type ResourceCreateArgs struct {
	Handle    uint32 // [0] RES_HANDLE — client-chosen resource id
	Target    uint32 // [1] TARGET     — pipe_texture_target (2 = 2D)
	Format    uint32 // [2] FORMAT     — virgl_formats
	Bind      uint32 // [3] BIND       — VIRGL_BIND_* (RENDER_TARGET etc.)
	Width     uint32 // [4] WIDTH
	Height    uint32 // [5] HEIGHT
	Depth     uint32 // [6] DEPTH
	ArraySize uint32 // [7] ARRAY_SIZE
	LastLevel uint32 // [8] LAST_LEVEL
	NrSamples uint32 // [9] NR_SAMPLES
}

// encodeResourceCreate builds VCMD_RESOURCE_CREATE (cmd 2).
//
// Header length = VCMD_RES_CREATE_SIZE (10) dwords; payload is the 10 fields in
// the order above. The server creates the resource and (per
// vtest_server.c:dispatch) fences it; there is no reply body to read.
func encodeResourceCreate(a ResourceCreateArgs) []byte {
	b := encodeHeader(vcmdResCreateSize, VcmdResourceCreate)
	b = binary.LittleEndian.AppendUint32(b, a.Handle)
	b = binary.LittleEndian.AppendUint32(b, a.Target)
	b = binary.LittleEndian.AppendUint32(b, a.Format)
	b = binary.LittleEndian.AppendUint32(b, a.Bind)
	b = binary.LittleEndian.AppendUint32(b, a.Width)
	b = binary.LittleEndian.AppendUint32(b, a.Height)
	b = binary.LittleEndian.AppendUint32(b, a.Depth)
	b = binary.LittleEndian.AppendUint32(b, a.ArraySize)
	b = binary.LittleEndian.AppendUint32(b, a.LastLevel)
	b = binary.LittleEndian.AppendUint32(b, a.NrSamples)
	return b
}

// ---- submit cmd -----------------------------------------------------------

// encodeSubmitCmd builds VCMD_SUBMIT_CMD (cmd 6), carrying the exact virgl
// command buffer produced by go-virtio/gpu (e.g. buildClearVirglBuffer).
//
// vtest_renderer.c:vtest_submit_cmd reads `length_dw * 4` bytes and passes them
// straight to virgl_renderer_submit_cmd. So the header length is the command
// buffer's length in DWORDS, and the payload is the buffer verbatim. The buffer
// MUST be a whole number of dwords (virgl command buffers always are).
func encodeSubmitCmd(cmdBuf []byte) ([]byte, error) {
	if len(cmdBuf)%4 != 0 {
		return nil, fmt.Errorf("vtest: submit cmd buffer length %d not a multiple of 4", len(cmdBuf))
	}
	b := encodeHeader(uint32(len(cmdBuf)/4), VcmdSubmitCmd)
	return append(b, cmdBuf...), nil
}

// ---- transfer get ---------------------------------------------------------

// TransferGetArgs mirrors the 11-dword VCMD_TRANSFER_GET payload read by
// vtest_renderer.c:vtest_transfer_decode_args (VCMD_TRANSFER_* indices). The
// box (x,y,z,w,h,d) is a virgl_box; DataSize is how many bytes the server will
// write back (vtest_transfer_get_internal mallocs DataSize and block_writes it
// after the read-back, with no response header).
type TransferGetArgs struct {
	Handle      uint32 // [0]  RES_HANDLE
	Level       uint32 // [1]  LEVEL
	Stride      uint32 // [2]  STRIDE
	LayerStride uint32 // [3]  LAYER_STRIDE
	X           uint32 // [4]  X
	Y           uint32 // [5]  Y
	Z           uint32 // [6]  Z
	Width       uint32 // [7]  WIDTH
	Height      uint32 // [8]  HEIGHT
	Depth       uint32 // [9]  DEPTH
	DataSize    uint32 // [10] DATA_SIZE — bytes the server writes back
}

// encodeTransferGet builds VCMD_TRANSFER_GET (cmd 4). Header length =
// VCMD_TRANSFER_HDR_SIZE (11) dwords; payload is the 11 fields in order.
func encodeTransferGet(a TransferGetArgs) []byte {
	b := encodeHeader(vcmdTransferHdrSize, VcmdTransferGet)
	b = binary.LittleEndian.AppendUint32(b, a.Handle)
	b = binary.LittleEndian.AppendUint32(b, a.Level)
	b = binary.LittleEndian.AppendUint32(b, a.Stride)
	b = binary.LittleEndian.AppendUint32(b, a.LayerStride)
	b = binary.LittleEndian.AppendUint32(b, a.X)
	b = binary.LittleEndian.AppendUint32(b, a.Y)
	b = binary.LittleEndian.AppendUint32(b, a.Z)
	b = binary.LittleEndian.AppendUint32(b, a.Width)
	b = binary.LittleEndian.AppendUint32(b, a.Height)
	b = binary.LittleEndian.AppendUint32(b, a.Depth)
	b = binary.LittleEndian.AppendUint32(b, a.DataSize)
	return b
}

// ---- live client methods --------------------------------------------------

// readHeader reads a 2-dword response header [length, cmd] from the server.
func (c *Client) readHeader() (length, cmd uint32, err error) {
	var hdr [hdrBytes]byte
	if _, err = io.ReadFull(c.rw, hdr[:]); err != nil {
		return 0, 0, fmt.Errorf("vtest: read header: %w", err)
	}
	length = binary.LittleEndian.Uint32(hdr[0:4])
	cmd = binary.LittleEndian.Uint32(hdr[4:8])
	return length, cmd, nil
}

// Handshake performs the full vtest startup handshake against a live server:
//
//  1. VCMD_PING_PROTOCOL_VERSION  -> server replies header [0,10]
//  2. VCMD_PROTOCOL_VERSION(v)    -> server replies header [1,11] + [negotiated]
//  3. VCMD_CREATE_RENDERER(name)  -> no reply
//
// It returns the protocol version the server negotiated. Per
// vtest_server.c:vtest_client_dispatch_commands, CREATE_RENDERER must be the
// FIRST command that creates the context — but PING/PROTOCOL_VERSION are
// dispatched against the current context, so Mesa sends CREATE_RENDERER first
// in practice. We follow Mesa's util/virgl_vtest_socket ordering: ping,
// negotiate, then create.
//
// NOTE on ordering: vtest_server requires the very first header to be
// CREATE_RENDERER before a context exists. Real Mesa sends CREATE_RENDERER
// first, THEN ping/negotiate. We therefore send create first here.
func (c *Client) Handshake(name string, version uint32) (negotiated uint32, err error) {
	if _, err = c.rw.Write(encodeCreateRenderer(name)); err != nil {
		return 0, fmt.Errorf("vtest: create renderer: %w", err)
	}
	if _, err = c.rw.Write(encodePingProtocolVersion()); err != nil {
		return 0, fmt.Errorf("vtest: ping: %w", err)
	}
	// ping reply: bare header [0, VCMD_PING_PROTOCOL_VERSION]
	if _, _, err = c.readHeader(); err != nil {
		return 0, fmt.Errorf("vtest: ping reply: %w", err)
	}
	if _, err = c.rw.Write(encodeProtocolVersion(version)); err != nil {
		return 0, fmt.Errorf("vtest: protocol version: %w", err)
	}
	// reply: header [1, VCMD_PROTOCOL_VERSION] + one dword negotiated version
	if _, _, err = c.readHeader(); err != nil {
		return 0, fmt.Errorf("vtest: protocol version reply header: %w", err)
	}
	var verBuf [4]byte
	if _, err = io.ReadFull(c.rw, verBuf[:]); err != nil {
		return 0, fmt.Errorf("vtest: protocol version reply body: %w", err)
	}
	return binary.LittleEndian.Uint32(verBuf[:]), nil
}

// CreateResource sends VCMD_RESOURCE_CREATE. No reply body.
func (c *Client) CreateResource(a ResourceCreateArgs) error {
	if _, err := c.rw.Write(encodeResourceCreate(a)); err != nil {
		return fmt.Errorf("vtest: resource create: %w", err)
	}
	return nil
}

// SubmitCmd sends VCMD_SUBMIT_CMD carrying a virgl command buffer. No reply.
func (c *Client) SubmitCmd(cmdBuf []byte) error {
	pkt, err := encodeSubmitCmd(cmdBuf)
	if err != nil {
		return err
	}
	if _, err := c.rw.Write(pkt); err != nil {
		return fmt.Errorf("vtest: submit cmd: %w", err)
	}
	return nil
}

// TransferGet sends VCMD_TRANSFER_GET and reads back exactly args.DataSize
// bytes of pixel data (the server writes the read-back IOV with no response
// header, per vtest_transfer_get_internal).
func (c *Client) TransferGet(a TransferGetArgs) ([]byte, error) {
	if _, err := c.rw.Write(encodeTransferGet(a)); err != nil {
		return nil, fmt.Errorf("vtest: transfer get: %w", err)
	}
	out := make([]byte, a.DataSize)
	if _, err := io.ReadFull(c.rw, out); err != nil {
		return nil, fmt.Errorf("vtest: transfer get readback: %w", err)
	}
	return out, nil
}
