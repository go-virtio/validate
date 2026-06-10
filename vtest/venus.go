package vtest

// This file extends the vtest client with the protocol-version-3 / Venus
// (Vulkan-over-virtio) opcodes, resolved from source against
// virglrenderer's vtest/vtest_protocol.h, vtest/vtest_renderer.c and
// vtest/vtest_server.c (gitlab.freedesktop.org/virgl/virglrenderer, project
// 166) and Mesa's src/virtio/vulkan/vn_renderer_vtest.c + vn_ring.c (project
// 176). Every wire constant and layout is cited at its use site.
//
// HOW VENUS RIDES THE VTEST SOCKET (the resolved framing)
// --------------------------------------------------------
// Unlike the DRM-ioctl path (CONTEXT_INIT capset=VENUS, NUM_RINGS, ring blob
// via EXECBUFFER+RING_IDX), `virgl_test_server --venus` consumes Venus over
// the SAME plain vtest socket using the v3 opcodes. The init/submit flow,
// transcribed from vn_renderer_vtest.c:vtest_init + vtest_submit, is:
//
//  1. CREATE_RENDERER, PING_PROTOCOL_VERSION, PROTOCOL_VERSION(>=3).
//     (vtest_init_protocol_version: min_protocol_version = 3.)
//  2. GET_PARAM(VCMD_PARAM_MAX_TIMELINE_COUNT) -> must be non-zero.
//  3. GET_CAPSET(id=VIRTGPU_DRM_CAPSET_VENUS=4, version=0) -> capset blob.
//  4. CONTEXT_INIT(capset_id=4). Server side
//     (vtest_renderer.c:vtest_context_init -> vtest_lazy_init_context) calls
//     virgl_renderer_context_create_with_flags(capset=VENUS), spinning up the
//     host vkr (Venus) context.
//  5. RESOURCE_CREATE_BLOB(type=HOST3D, flags=MAPPABLE, size=ringShmemSize,
//     blob_id) -> reply res_id + an mmap'able fd over SCM_RIGHTS
//     (vtest_vcmd_resource_create_blob / vtest_renderer.c:
//     vtest_resource_create_blob, which exports the blob and vtest_send_fd's
//     it). mmap that fd: this is the ring shared memory both guest and host
//     read/write.
//  6. Register the ring: SUBMIT_CMD2 (ring_idx=0, the CPU ring) carrying the
//     raw command stream `vkCreateRingMESA(ring_id, VkRingCreateInfoMESA{
//     resourceId=res_id, ...vn_ring.c offsets})`. This is exactly Mesa's
//     vn_ring_create -> vn_renderer_submit_simple (ring_idx 0). The host now
//     spawns a vkr_ring consumer that polls the ring's tail word.
//  7. Submit a Vulkan command (e.g. vkCreateInstance) by writing its cs into
//     the ring buffer at (cur & mask), storing the new tail
//     (vn_ring.c:vn_ring_store_tail), then busy-polling the head word until
//     the host consumer advances it past our tail
//     (vn_ring.c:vn_ring_wait_seqno / vn_ring_get_seqno_status: head >= seqno).
//
// The ROUND TRIP we can actually prove is step 7's head advance: the host
// renderer consuming the ring bytes and writing head. That is the genuine
// "host advances the ring head" signal the venus README calls the real
// unknown.

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ---- protocol version 3 ---------------------------------------------------

// VtestProtocolVersionVenus is the minimum protocol version Venus needs.
// vn_renderer_vtest.c:vtest_init_protocol_version: min_protocol_version = 3;
// the v3 opcodes (GET_PARAM/GET_CAPSET/CONTEXT_INIT/RESOURCE_CREATE_BLOB/
// SUBMIT_CMD2) are compiled only under VIRGL_RENDERER_UNSTABLE_APIS.
const VtestProtocolVersionVenus = 3

// v3 command ids — vtest_protocol.h (VIRGL_RENDERER_UNSTABLE_APIS block).
const (
	VcmdGetParam           = 15
	VcmdGetCapset          = 16
	VcmdContextInit        = 17
	VcmdResourceCreateBlob = 18
	VcmdSyncCreate         = 19
	VcmdSyncUnref          = 20
	VcmdSyncRead           = 21
	VcmdSyncWrite          = 22
	VcmdSyncWait           = 23
	VcmdSubmitCmd2         = 24
)

// VCMD_PARAM_MAX_TIMELINE_COUNT — vtest_protocol.h enum vcmd_param.
const VcmdParamMaxTimelineCount = 1

// CapsetVenus is VIRTGPU_DRM_CAPSET_VENUS (drm-uapi/virtgpu_drm.h:
// #define VIRTGPU_DRM_CAPSET_VENUS 4), the capset id vtest_init_capset sends.
const CapsetVenus = 4

// Blob types — vtest_protocol.h enum vcmd_blob_type.
const (
	BlobTypeGuest       = 1
	BlobTypeHost3D      = 2
	BlobTypeHost3DGuest = 3
)

// Blob flags — vtest_protocol.h enum vcmd_blob_flag.
const (
	BlobFlagMappable    = 1 << 0
	BlobFlagShareable   = 1 << 1
	BlobFlagCrossDevice = 1 << 2
)

// SUBMIT_CMD2 batch flags — vtest_protocol.h enum vcmd_submit_cmd2_flag.
const SubmitCmd2FlagRingIdx = 1 << 0

// payload sizes (in dwords) — vtest_protocol.h.
const (
	vcmdGetParamSize      = 1 // [0]=param
	vcmdGetCapsetSize     = 2 // [0]=id [1]=version
	vcmdContextInitSize   = 1 // [0]=capset_id
	vcmdResCreateBlobSize = 6 // [0]=type [1]=flags [2..3]=size [4..5]=blob_id
)

// ---- handshake (v3) -------------------------------------------------------

// HandshakeVenus performs the vtest startup handshake negotiating protocol
// version >= 3 (required for Venus). Same ordering as Handshake but asks for
// v3. It returns the version the server negotiated; the caller MUST verify it
// is >= 3 (an older virglrenderer built WITHOUT VIRGL_RENDERER_UNSTABLE_APIS
// caps the negotiation at 2 and every Venus opcode below is unreachable —
// that is the first wall to map).
func (c *Client) HandshakeVenus(name string) (negotiated uint32, err error) {
	return c.Handshake(name, VtestProtocolVersionVenus)
}

// ---- GET_PARAM ------------------------------------------------------------

// encodeGetParam builds VCMD_GET_PARAM (cmd 15). vtest_vcmd_get_param writes
// header[len=1,id=15] + one dword param; server replies header[len=2,id=15] +
// {valid, value}.
func encodeGetParam(param uint32) []byte {
	b := encodeHeader(vcmdGetParamSize, VcmdGetParam)
	return binary.LittleEndian.AppendUint32(b, param)
}

// GetParam sends VCMD_GET_PARAM and returns the value (0 when the server
// reports the param invalid, mirroring vtest_vcmd_get_param's
// `resp[0] ? resp[1] : 0`).
func (c *Client) GetParam(param uint32) (uint32, error) {
	if _, err := c.rw.Write(encodeGetParam(param)); err != nil {
		return 0, fmt.Errorf("vtest: get_param write: %w", err)
	}
	length, cmd, err := c.readHeader()
	if err != nil {
		return 0, fmt.Errorf("vtest: get_param reply header: %w", err)
	}
	if cmd != VcmdGetParam || length != 2 {
		return 0, fmt.Errorf("vtest: get_param reply mismatch: len=%d cmd=%d (want len=2 cmd=%d)", length, cmd, VcmdGetParam)
	}
	var resp [2]uint32
	if err := readDwords(c.rw, resp[:]); err != nil {
		return 0, fmt.Errorf("vtest: get_param reply body: %w", err)
	}
	if resp[0] == 0 {
		return 0, nil
	}
	return resp[1], nil
}

// ---- GET_CAPSET -----------------------------------------------------------

// encodeGetCapset builds VCMD_GET_CAPSET (cmd 16). vtest_vcmd_get_capset
// writes header[len=2,id=16] + {id, version}; server replies header[len,id=16]
// + {valid} + (len-1)*4 bytes of capset data.
func encodeGetCapset(id, version uint32) []byte {
	b := encodeHeader(vcmdGetCapsetSize, VcmdGetCapset)
	b = binary.LittleEndian.AppendUint32(b, id)
	return binary.LittleEndian.AppendUint32(b, version)
}

// GetCapset sends VCMD_GET_CAPSET(id, version). It returns the raw capset blob
// (struct virgl_renderer_capset_venus on the wire) and whether the server
// reported it valid. Layout per vtest_vcmd_get_capset: reply header length
// counts {valid}+capset in dwords, so capset is (length-1)*4 bytes.
func (c *Client) GetCapset(id, version uint32) (capset []byte, valid bool, err error) {
	if _, err = c.rw.Write(encodeGetCapset(id, version)); err != nil {
		return nil, false, fmt.Errorf("vtest: get_capset write: %w", err)
	}
	length, cmd, err := c.readHeader()
	if err != nil {
		return nil, false, fmt.Errorf("vtest: get_capset reply header: %w", err)
	}
	if cmd != VcmdGetCapset {
		return nil, false, fmt.Errorf("vtest: get_capset reply cmd=%d want %d", cmd, VcmdGetCapset)
	}
	var validBuf [4]byte
	if _, err = io.ReadFull(c.rw, validBuf[:]); err != nil {
		return nil, false, fmt.Errorf("vtest: get_capset valid: %w", err)
	}
	valid = binary.LittleEndian.Uint32(validBuf[:]) != 0
	if !valid {
		return nil, false, nil
	}
	n := int(length-1) * 4
	capset = make([]byte, n)
	if _, err = io.ReadFull(c.rw, capset); err != nil {
		return nil, false, fmt.Errorf("vtest: get_capset body: %w", err)
	}
	return capset, true, nil
}

// ---- CONTEXT_INIT ---------------------------------------------------------

// encodeContextInit builds VCMD_CONTEXT_INIT (cmd 17). vtest_vcmd_context_init
// writes header[len=1,id=17] + capset_id. No reply (the server's
// vtest_context_init defers actual context creation to vtest_lazy_init_context
// on the next init_context command).
func encodeContextInit(capsetID uint32) []byte {
	b := encodeHeader(vcmdContextInitSize, VcmdContextInit)
	return binary.LittleEndian.AppendUint32(b, capsetID)
}

// ContextInit sends VCMD_CONTEXT_INIT(capset_id). No reply body.
func (c *Client) ContextInit(capsetID uint32) error {
	if _, err := c.rw.Write(encodeContextInit(capsetID)); err != nil {
		return fmt.Errorf("vtest: context_init: %w", err)
	}
	return nil
}

// ---- RESOURCE_CREATE_BLOB -------------------------------------------------

// encodeResourceCreateBlob builds VCMD_RESOURCE_CREATE_BLOB (cmd 18).
// vtest_vcmd_resource_create_blob writes header[len=6,id=18] + {type, flags,
// size_lo, size_hi, blob_id_lo, blob_id_hi}.
func encodeResourceCreateBlob(blobType, flags uint32, size, blobID uint64) []byte {
	b := encodeHeader(vcmdResCreateBlobSize, VcmdResourceCreateBlob)
	b = binary.LittleEndian.AppendUint32(b, blobType)
	b = binary.LittleEndian.AppendUint32(b, flags)
	b = binary.LittleEndian.AppendUint32(b, uint32(size))
	b = binary.LittleEndian.AppendUint32(b, uint32(size>>32))
	b = binary.LittleEndian.AppendUint32(b, uint32(blobID))
	b = binary.LittleEndian.AppendUint32(b, uint32(blobID>>32))
	return b
}

// ---- SUBMIT_CMD2 ----------------------------------------------------------

// SubmitCmd2Batch mirrors struct vcmd_submit_cmd2_batch (vtest_protocol.h),
// which is EIGHT dwords on the wire:
//
//	{flags, cmd_offset, cmd_size, sync_offset, sync_count, ring_idx,
//	 num_in_syncobj, num_out_syncobj}
//
// where the offsets/sizes are in DWORDS relative to the start of the submit
// payload (after the batch_count + the batch headers). The two trailing
// syncobj-count dwords are only *read* by the server at protocol_version >= 4
// (vtest_renderer.c:vtest_submit_cmd2), but Mesa always writes the whole
// sizeof(struct vcmd_submit_cmd2_batch) = 32 bytes = 8 dwords
// (vn_renderer_vtest.c:vtest_vcmd_submit_cmd2 `vtest_write(vtest, &dst,
// sizeof(dst))`), so the batch stride is 8 dwords regardless of version. We
// zero num_in/num_out_syncobj.
type SubmitCmd2Batch struct {
	Flags   uint32
	RingIdx uint32
	Syncs   []SubmitCmd2Sync
	CmdData []byte // the raw command stream (must be a whole number of dwords)
}

// SubmitCmd2Sync is one (sync_id, value) pair appended after the command
// streams (vtest_vcmd_submit_cmd2 writes {id, val_lo, val_hi}).
type SubmitCmd2Sync struct {
	ID    uint32
	Value uint64
}

// submitCmd2BatchDwords is the wire size, in dwords, of one
// struct vcmd_submit_cmd2_batch — 8 dwords (32 bytes):
//
//	flags, cmd_offset, cmd_size, sync_offset, sync_count, ring_idx,
//	num_in_syncobj, num_out_syncobj
//
// (vtest_protocol.h; the VCMD_SUBMIT_CMD2_BATCH_*(n) macros stride by 8*n, and
// Mesa writes sizeof(struct vcmd_submit_cmd2_batch)=32 bytes per batch).
const submitCmd2BatchDwords = 8

// encodeSubmitCmd2 builds VCMD_SUBMIT_CMD2 (cmd 24) for a single batch,
// matching the layout produced by vn_renderer_vtest.c:vtest_vcmd_submit_cmd2:
//
//	[batch_count]
//	per-batch: struct vcmd_submit_cmd2_batch (8 dwords)
//	all cmd streams concatenated (dword-aligned)
//	all (id, val_lo, val_hi) sync triples
//
// cmd_offset/sync_offset/cmd_size are DWORD indices/counts into the payload
// (Mesa: cmd_offset = cs_offset/4, cmd_size = cs_size/4). With one batch the
// header occupies 1 (batch_count) + 8 (batch) = 9 dwords, so cmd_offset = 9.
func encodeSubmitCmd2(batch SubmitCmd2Batch) ([]byte, error) {
	if len(batch.CmdData)%4 != 0 {
		return nil, fmt.Errorf("vtest: submit_cmd2 cmd data length %d not dword-aligned", len(batch.CmdData))
	}
	headerDwords := uint32(1 + submitCmd2BatchDwords) // batch_count + one batch
	cmdDwords := uint32(len(batch.CmdData) / 4)
	syncDwords := uint32(len(batch.Syncs) * 3)
	totalDwords := headerDwords + cmdDwords + syncDwords

	cmdOffset := headerDwords
	syncOffset := cmdOffset + cmdDwords

	b := encodeHeader(totalDwords, VcmdSubmitCmd2)
	b = binary.LittleEndian.AppendUint32(b, 1) // batch_count
	// batch header (8 dwords)
	b = binary.LittleEndian.AppendUint32(b, batch.Flags)
	b = binary.LittleEndian.AppendUint32(b, cmdOffset)
	b = binary.LittleEndian.AppendUint32(b, cmdDwords)
	b = binary.LittleEndian.AppendUint32(b, syncOffset)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(batch.Syncs)))
	b = binary.LittleEndian.AppendUint32(b, batch.RingIdx)
	b = binary.LittleEndian.AppendUint32(b, 0) // num_in_syncobj  (v4-only read)
	b = binary.LittleEndian.AppendUint32(b, 0) // num_out_syncobj (v4-only read)
	// cmd stream
	b = append(b, batch.CmdData...)
	// syncs
	for _, s := range batch.Syncs {
		b = binary.LittleEndian.AppendUint32(b, s.ID)
		b = binary.LittleEndian.AppendUint32(b, uint32(s.Value))
		b = binary.LittleEndian.AppendUint32(b, uint32(s.Value>>32))
	}
	return b, nil
}

// SubmitCmd2 sends a single-batch VCMD_SUBMIT_CMD2. No reply body
// (vtest_submit_cmd2 dispatches into virgl_renderer_submit_cmd and returns).
func (c *Client) SubmitCmd2(batch SubmitCmd2Batch) error {
	pkt, err := encodeSubmitCmd2(batch)
	if err != nil {
		return err
	}
	if _, err := c.rw.Write(pkt); err != nil {
		return fmt.Errorf("vtest: submit_cmd2: %w", err)
	}
	return nil
}

// ---- helpers --------------------------------------------------------------

// readDwords reads len(out) little-endian uint32s from r.
func readDwords(r io.Reader, out []uint32) error {
	buf := make([]byte, len(out)*4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(buf[i*4 : i*4+4])
	}
	return nil
}
