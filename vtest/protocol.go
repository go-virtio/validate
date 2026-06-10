// Package vtest implements a pure-Go (CGO=0) client for the virglrenderer
// "vtest" wire protocol — the same protocol Mesa's virgl driver speaks to a
// running `virgl_test_server` over a Unix-domain socket WITHOUT a virtual
// machine. Mesa CI uses this path with llvmpipe (software GL, no GPU) to
// validate the virgl command stream headlessly; go-virtio uses it to feed the
// command buffers produced by go-virtio/gpu (buildClearVirglBuffer /
// buildDrawVirglBuffer) to a real renderer and read back the rendered pixels.
//
// All wire constants and layouts below are quoted from virglrenderer's
// vtest/vtest_protocol.h and the read/write sites in vtest/vtest_renderer.c
// and vtest/vtest_server.c (utmapp/virglrenderer mirror of
// gitlab.freedesktop.org/virgl/virglrenderer).
//
// Wire conventions (from vtest_server.c:vtest_client_dispatch_commands and
// vtest_renderer.c:vtest_block_write):
//
//   - Every value on the wire is a 32-bit unsigned integer in the host byte
//     order of the server. vtest is a local-socket, same-machine protocol, so
//     this is little-endian on all platforms go-virtio targets (x86-64,
//     aarch64). We encode little-endian.
//
//   - Each command is framed by a 2-dword header:
//     dword[0] = length   (VTEST_CMD_LEN, # of payload dwords OR bytes,
//     command-specific — see notes per command)
//     dword[1] = cmd id   (VTEST_CMD_ID)
//     (vtest_protocol.h: VTEST_HDR_SIZE 2, VTEST_CMD_LEN 0, VTEST_CMD_ID 1.)
package vtest

// Header framing — vtest_protocol.h.
//
//	#define VTEST_HDR_SIZE 2
//	#define VTEST_CMD_LEN 0
//	#define VTEST_CMD_ID  1
const (
	hdrSize  = 2 // dwords
	hdrBytes = hdrSize * 4
)

// Command ids — vtest_protocol.h "vtest cmds".
const (
	VcmdGetCaps             = 1
	VcmdResourceCreate      = 2
	VcmdResourceUnref       = 3
	VcmdTransferGet         = 4
	VcmdTransferPut         = 5
	VcmdSubmitCmd           = 6
	VcmdResourceBusyWait    = 7
	VcmdCreateRenderer      = 8
	VcmdGetCaps2            = 9
	VcmdPingProtocolVersion = 10
	VcmdProtocolVersion     = 11
	VcmdResourceCreate2     = 12
	VcmdTransferGet2        = 13
	VcmdTransferPut2        = 14
)

// VtestProtocolVersion is the version this client negotiates.
// vtest_protocol.h: #define VTEST_PROTOCOL_VERSION 2.
const VtestProtocolVersion = 2

// DefaultSocketName — vtest_protocol.h:
//
//	#define VTEST_DEFAULT_SOCKET_NAME "/tmp/.virgl_test"
const DefaultSocketName = "/tmp/.virgl_test"

// VCMD_RES_CREATE layout — vtest_protocol.h.
//
//	#define VCMD_RES_CREATE_SIZE 10
//	  [0]=RES_HANDLE [1]=TARGET [2]=FORMAT [3]=BIND [4]=WIDTH [5]=HEIGHT
//	  [6]=DEPTH [7]=ARRAY_SIZE [8]=LAST_LEVEL [9]=NR_SAMPLES
//
// Read back by vtest_renderer.c:vtest_create_resource_decode_args.
const vcmdResCreateSize = 10

// VCMD_TRANSFER_HDR layout — vtest_protocol.h.
//
//	#define VCMD_TRANSFER_HDR_SIZE 11
//	  [0]=RES_HANDLE [1]=LEVEL [2]=STRIDE [3]=LAYER_STRIDE [4]=X [5]=Y
//	  [6]=Z [7]=WIDTH [8]=HEIGHT [9]=DEPTH [10]=DATA_SIZE
//
// Read back by vtest_renderer.c:vtest_transfer_decode_args.
const vcmdTransferHdrSize = 11

// VCMD_PROTOCOL_VERSION payload — vtest_protocol.h.
//
//	#define VCMD_PROTOCOL_VERSION_SIZE 1
//	#define VCMD_PROTOCOL_VERSION_VERSION 0
const vcmdProtocolVersionSize = 1
