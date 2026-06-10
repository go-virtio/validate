//go:build linux

package vtest

// Venus CLEAR-IMAGE driver over the vtest socket.
//
// This drives the FULL Vulkan clear-image command closure through the same
// proven ring transport the round-trip used (RawConn + RESOURCE_CREATE_BLOB +
// MappedRing + vkCreateRingMESA), then confirms the host cleared the image.
// The per-command Vulkan encoders and reply decoders are the offline
// byte-verified generated closure in go-virtio/venus, exposed through the
// importable clearcs façade (NOT re-hand-encoded here); the only command
// streams hand-encoded in this package are the MESA ring plumbing
// (vkCreateRingMESA + vkSetReplyCommandStreamMESA in venuscs.go), which the
// generated set does not cover.
//
// THE REPLY MECHANISM (source-cited, the crux of reading handles back).
// A reply-bearing Venus command does NOT return its reply over the socket and
// does NOT land in the ring's extra region. Instead, per Mesa
// src/virtio/vulkan/vn_ring.c:vn_ring_submit_command:
//
//  1. the guest allocates a SEPARATE reply shmem resource (its own
//     RESOURCE_CREATE_BLOB HOST3D|MAPPABLE blob), mmap'd;
//  2. it submits vkSetReplyCommandStreamMESA(VkCommandStreamDescriptionMESA{
//     resourceId=reply_blob, offset, size}) into the ring
//     (vn_ring_set_reply_shmem_locked) to tell the renderer WHERE to write the
//     reply;
//  3. it submits the actual command with cmd_flags |=
//     VK_COMMAND_GENERATE_REPLY_BIT_EXT;
//  4. it waits for head >= seqno (vn_ring_wait_seqno);
//  5. it decodes the reply from reply_blob.mmap_ptr + offset
//     (VN_CS_DECODER_INITIALIZER(reply_ptr, reply_size)).
//
// We reproduce exactly this. One dedicated reply blob is created up front and
// reused (zeroed before each reply); offset 0 within that blob.

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"time"

	"github.com/go-virtio/venus/clearcs"
)

// VkCommandTypeEXT ids echoed back as the first int32 of each reply
// (vn_protocol_driver_defines.h). Used to PROVE the host actually wrote a reply
// (a CS-errored command writes none, so the reply slot keeps our sentinel and
// the echo will not match).
const (
	ctCreateInstance         = 0
	ctEnumeratePhysDevices   = 2
	ctGetPhysDevMemoryProps  = 8
	ctGetPhysDevQueueFamily  = 7
	ctCreateDevice           = 11
	ctGetDeviceQueue         = 17
	ctGetDeviceQueue2        = 155
	ctCreateImage            = 54
	ctGetImageMemoryReqs     = 31
	ctAllocateMemory         = 21
	ctBindImageMemory        = 29
	ctCreateCommandPool      = 85
	ctAllocateCommandBuffers = 88
	ctBeginCommandBuffer     = 90
	ctEndCommandBuffer       = 91
	ctQueueSubmit            = 18
	ctCreateFence            = 35
	ctWaitForFences          = 39
)

// Vulkan enum/flag constants used by the clear-image sequence, transcribed
// from vulkan_core.h (cited at each use). Only the handful the sequence needs.
const (
	vkFormatR8G8B8A8UNORM        = 37     // VK_FORMAT_R8G8B8A8_UNORM
	vkImageType2D                = 1      // VK_IMAGE_TYPE_2D
	vkImageTilingLinear          = 1      // VK_IMAGE_TILING_LINEAR
	vkSharingModeExclusive       = 0      // VK_SHARING_MODE_EXCLUSIVE
	vkImageLayoutUndefined       = 0      // VK_IMAGE_LAYOUT_UNDEFINED
	vkImageLayoutGeneral         = 1      // VK_IMAGE_LAYOUT_GENERAL
	vkImageUsageTransferDstBit   = 0x2    // VK_IMAGE_USAGE_TRANSFER_DST_BIT
	vkImageAspectColorBit        = 0x1    // VK_IMAGE_ASPECT_COLOR_BIT
	vkSampleCount1Bit            = 0x1    // VK_SAMPLE_COUNT_1_BIT
	vkAccessTransferWriteBit     = 0x1000 // VK_ACCESS_TRANSFER_WRITE_BIT
	vkPipelineStageTopOfPipe     = 0x1    // VK_PIPELINE_STAGE_TOP_OF_PIPE_BIT
	vkPipelineStageTransfer      = 0x1000 // VK_PIPELINE_STAGE_TRANSFER_BIT
	vkMemoryPropertyHostVisible  = 0x2    // VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT
	vkMemoryPropertyHostCoherent = 0x4    // VK_MEMORY_PROPERTY_HOST_COHERENT_BIT
	vkCommandBufferUsageOneTime  = 0x1    // VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT
	vkQueueFamilyIgnored         = 0xFFFFFFFF
)

// ClearImageResult records every host-observable value of the clear-image walk
// so the caller can report exact acceptance per command.
type ClearImageResult struct {
	NegotiatedVersion uint32
	MaxTimelineCount  uint32
	CapsetLen         int
	RingBlobResID     uint32
	ReplyBlobResID    uint32

	// Decoded handles / indices from each reply.
	Instance       uint64
	PhysDevCount   uint32
	PhysDev        uint64
	MemTypeCount   uint32
	MemTypeIndex   uint32 // chosen HOST_VISIBLE|COHERENT type
	MemTypeFlags   uint32
	QueueFamCount  uint32
	QueueFamilyIdx uint32
	Device         uint64
	Queue          uint64
	Image          uint64
	MemReqSize     uint64
	MemReqAlign    uint64
	MemReqBits     uint32
	Memory         uint64
	CmdPool        uint64
	CmdBuf         uint64
	Fence          uint64

	// VkResult of each result-bearing reply (0 == VK_SUCCESS).
	ResCreateInstance int32
	ResCreateDevice   int32
	ResCreateImage    int32
	ResAllocMemory    int32
	ResBindImage      int32
	ResCreatePool     int32
	ResAllocCmdBuf    int32
	ResBeginCmdBuf    int32
	ResEndCmdBuf      int32
	ResCreateFence    int32
	ResQueueSubmit    int32
	ResWaitForFences  int32

	// Step-by-step trace lines (each command's seqno + head-after + decode).
	Trace []string

	// ClearColor is the colour we asked CmdClearColorImage to write.
	ClearColor [4]float32

	// ---- guest-visible readback (the last Venus frontier) ----
	// MemBlobResID is the res_id of the HOST3D|MAPPABLE blob whose blob_id ties
	// to the VkDeviceMemory (vn_device_memory_bo_init: blob_id = mem->base.id);
	// the host backs the device memory with this blob, so its mmap IS the
	// cleared image's storage. Zero until the readback path runs.
	MemBlobResID uint32
	// ReadbackDone is true once the mapped device-memory blob was read.
	ReadbackDone bool
	// FirstTexel is the RGBA bytes of pixel (0,0) read from the mapped blob.
	FirstTexel [4]byte
	// ExpectTexel is the expected clear texel (the float clear encoded to the
	// image format, R8G8B8A8_UNORM here).
	ExpectTexel [4]byte
	// TexelsChecked / TexelsRed: how many texels of the first row(s) were read
	// back and how many matched the clear colour.
	TexelsChecked int
	TexelsRed     int
	// ReadbackPass is true iff every checked texel equalled the clear colour.
	ReadbackPass bool
}

// clearDriver carries the live transport state through the sequence.
type clearDriver struct {
	ring      *MappedRing
	replyMem  []byte
	replySize int
	res       *ClearImageResult
	timeout   time.Duration
}

// submitWithReply performs the full reply round-trip for one reply-bearing
// command stream `cmd` (the caller must have set cmd_flags |=
// CmdGenerateReplyBit). It returns the reply bytes (a copy of the reply blob
// window), or an error if the host did not advance head to the seqno within
// the timeout. The sequence mirrors vn_ring_submit_command: SetReply, then the
// command, then wait the seqno, then read the reply blob.
func (d *clearDriver) submitWithReply(name string, wantCmdType int32, cmd []byte) ([]byte, error) {
	// Sentinel-fill the reply window so we can PROVE the host wrote a reply: if
	// the command CS-errors host-side, the renderer writes NOTHING and the slot
	// keeps the sentinel. A plain zero-fill could not distinguish "no reply" from
	// a genuine vkCreateInstance reply (whose echoed cmd_type is 0); the sentinel
	// can.
	for i := range d.replyMem {
		d.replyMem[i] = 0xCC
	}
	setReply := EncodeVkSetReplyCommandStreamMESACS(d.res.ReplyBlobResID, 0, uint64(d.replySize))
	d.ring.Submit(padDword(setReply))
	seqno := d.ring.Submit(padDword(cmd))
	head, ok := d.ring.WaitHead(seqno, d.timeout)
	// The echoed VkCommandTypeEXT is the first int32 of every reply
	// (vn_decode_<cmd>_reply: vn_decode_VkCommandTypeEXT first). If it does not
	// match wantCmdType, the host did NOT write our reply (CS error / stale slot)
	// — a head advance alone is NOT acceptance.
	echoed := int32(binary.LittleEndian.Uint32(d.replyMem[0:4]))
	wrote := echoed == wantCmdType
	d.res.Trace = append(d.res.Trace,
		fmt.Sprintf("%-38s seqno=%-7d head=%-7d reached=%v reply_cmd=%d(want %d)%s",
			name, seqno, head, ok, echoed, wantCmdType, map[bool]string{true: " OK", false: " NO-REPLY"}[wrote]))
	if !ok {
		return nil, fmt.Errorf("%s: host head=%d did not reach seqno=%d within %s (consumer stalled or rejected)", name, head, seqno, d.timeout)
	}
	if !wrote {
		return nil, fmt.Errorf("%s: host advanced head but wrote NO valid reply (reply cmd_type=%d, want %d) — the command CS-errored or the reply slot is stale", name, echoed, wantCmdType)
	}
	return append([]byte(nil), d.replyMem[:d.replySize]...), nil
}

// submitNoReply submits a fire-and-forget command (no GENERATE_REPLY) and waits
// for head to reach its seqno so ordering holds and we observe consumption.
// Used for the recorded void Cmd* commands (vkCmdPipelineBarrier,
// vkCmdClearColorImage): they buffer into the command buffer host-side, and
// the head advance is the proof the host consumed them.
func (d *clearDriver) submitNoReply(name string, cmd []byte) error {
	seqno := d.ring.Submit(padDword(cmd))
	head, ok := d.ring.WaitHead(seqno, d.timeout)
	d.res.Trace = append(d.res.Trace,
		fmt.Sprintf("%-38s seqno=%-7d head=%-7d reached=%v (void)", name, seqno, head, ok))
	if !ok {
		return fmt.Errorf("%s: host head=%d did not reach seqno=%d within %s", name, head, seqno, d.timeout)
	}
	return nil
}

// padDword zero-pads a command stream up to a 4-byte boundary (vn_cs writes are
// always dword-multiples; our encoders already emit dword sizes — defensive).
func padDword(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// --- vkGetDeviceQueue2 command stream (hand-encoded) ---------------------
//
// virglrenderer's vkr DELIBERATELY refuses plain vkGetDeviceQueue:
//
//	vkr_dispatch_vkGetDeviceQueue(...) {
//	    /* Must use vkGetDeviceQueue2 for proper device queue initialization. */
//	    vkr_context_set_fatal(ctx);   // unconditional CS error
//	}
//
// (src/venus/vkr_queue.c). The guest MUST use vkGetDeviceQueue2, whose
// VkDeviceQueueInfo2 carries a VkDeviceQueueTimelineInfoMESA pNext binding the
// queue to a sync ring index (vkr_queue_assign_ring_idx requires a non-zero,
// in-range, currently-unbound ring_idx in ctx->sync_queues[64]). The wire shape
// (Mesa vn_encode_*, mirrored by the host vn_decode_* in
// vn_protocol_renderer_device.h) is:
//
//	int32   cmd_type = 155 (VK_COMMAND_TYPE_vkGetDeviceQueue2_EXT)
//	uint32  cmd_flags (GENERATE_REPLY)
//	handle  device                                          (uint64)
//	simple_pointer(pQueueInfo present)                      (uint64 = 1)
//	  int32  sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_INFO_2 (1000145003)
//	  // pNext: simple_pointer(present)                     (uint64 = 1)
//	  int32  pNext sType = DEVICE_QUEUE_TIMELINE_INFO_MESA (1000384005)
//	  simple_pointer(pNext->pNext = NULL)                   (uint64 = 0)
//	  uint32 ringIdx
//	  // VkDeviceQueueInfo2 self:
//	  uint32 flags
//	  uint32 queueFamilyIndex
//	  uint32 queueIndex
//	simple_pointer(pQueue present)                          (uint64 = 1)
//	handle  pQueue object id                                (uint64)
//
// Reply (vn_encode_vkGetDeviceQueue2_reply):
//
//	int32  cmd_type = 155
//	simple_pointer(present)                                 (uint64 = 1)
//	handle VkQueue                                          (uint64)
const (
	stDeviceQueueInfo2        = 1000145003
	stDeviceQueueTimelineMESA = 1000384005
	queueSyncRingIdx          = 1 // non-zero, unused (CPU ring is idx 0)
)

func putU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }
func putU64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }
func putSimplePtr(b []byte, p bool) []byte {
	if p {
		return putU64(b, 1)
	}
	return putU64(b, 0)
}

// encodeGetDeviceQueue2CS builds the vkGetDeviceQueue2 command stream.
func encodeGetDeviceQueue2CS(cmdFlags uint32, device uint64, family, index uint32, ringIdx uint32, pQueue uint64) []byte {
	var b []byte
	b = putU32(b, ctGetDeviceQueue2) // cmd_type = 155
	b = putU32(b, cmdFlags)
	b = putU64(b, device) // handle
	b = putSimplePtr(b, true)
	// VkDeviceQueueInfo2.sType
	b = putU32(b, stDeviceQueueInfo2)
	// VkDeviceQueueInfo2.pNext = simple_pointer(present) -> timeline info
	b = putSimplePtr(b, true)
	b = putU32(b, stDeviceQueueTimelineMESA) // timeline sType
	b = putSimplePtr(b, false)               // timeline.pNext = NULL
	b = putU32(b, ringIdx)                   // timeline.ringIdx
	// VkDeviceQueueInfo2 self
	b = putU32(b, 0)      // flags (matches queue create flags = 0)
	b = putU32(b, family) // queueFamilyIndex
	b = putU32(b, index)  // queueIndex
	// pQueue
	b = putSimplePtr(b, true)
	b = putU64(b, pQueue) // queue object id
	return b
}

// decodeGetDeviceQueue2Reply reads the VkQueue handle out of a reply blob.
func decodeGetDeviceQueue2Reply(reply []byte) (queue uint64, ok bool) {
	if len(reply) < 4+8+8 {
		return 0, false
	}
	if int32(binary.LittleEndian.Uint32(reply[0:4])) != ctGetDeviceQueue2 {
		return 0, false
	}
	if binary.LittleEndian.Uint64(reply[4:12]) != 1 { // simple_pointer present
		return 0, false
	}
	queue = binary.LittleEndian.Uint64(reply[12:20])
	return queue, true
}

// VenusClearImage runs the full clear-image walk against a live
// virgl_test_server --venus and returns the observed result. width/height are
// the LINEAR image dimensions; clear is the RGBA colour (0..1 floats).
//
// bufferSize must be a power of two and large enough to hold every command
// stream written across the whole sequence without wrapping (the validator
// picks a large default). On any host rejection the returned error quotes the
// failing command; the caller should additionally capture the server stderr
// (the next wall).
func VenusClearImage(socketPath string, bufferSize, width, height int, clear [4]float32, timeout time.Duration) (*ClearImageResult, error) {
	res := &ClearImageResult{ClearColor: clear}

	rc, err := DialRaw(socketPath)
	if err != nil {
		return res, err
	}
	defer rc.Close()
	c := New(rc)

	// ---- bring-up (identical to the proven round-trip) ----
	neg, err := c.HandshakeVenus("go-virtio-venus-clear")
	if err != nil {
		return res, fmt.Errorf("handshake: %w", err)
	}
	res.NegotiatedVersion = neg
	if neg < VtestProtocolVersionVenus {
		return res, fmt.Errorf("server negotiated v%d < 3: Venus unreachable", neg)
	}
	tl, err := c.GetParam(VcmdParamMaxTimelineCount)
	if err != nil {
		return res, fmt.Errorf("get_param(max_timeline_count): %w", err)
	}
	res.MaxTimelineCount = tl
	if tl == 0 {
		return res, fmt.Errorf("max_timeline_count=0: no timeline support (VIRGL_DISABLE_MT set?)")
	}
	capset, valid, err := c.GetCapset(CapsetVenus, 0)
	if err != nil {
		return res, fmt.Errorf("get_capset(venus): %w", err)
	}
	if !valid {
		return res, fmt.Errorf("get_capset(venus) invalid: no Venus capset")
	}
	res.CapsetLen = len(capset)
	if err := c.ContextInit(CapsetVenus); err != nil {
		return res, fmt.Errorf("context_init(venus): %w", err)
	}

	// ---- ring shmem blob (no extra region; replies use a SEPARATE blob) ----
	const extraSize = 0
	shmemSize := RingShmemSize(bufferSize, extraSize)
	ringResID, ringFD, err := rc.ResourceCreateBlob(BlobTypeHost3D, BlobFlagMappable, uint64(shmemSize), 0)
	if err != nil {
		return res, fmt.Errorf("resource_create_blob (ring): %w", err)
	}
	res.RingBlobResID = ringResID
	ringMem, err := syscall.Mmap(ringFD, 0, shmemSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	syscall.Close(ringFD)
	if err != nil {
		return res, fmt.Errorf("mmap ring shmem: %w", err)
	}
	for i := range ringMem {
		ringMem[i] = 0
	}
	ring := &MappedRing{ResID: ringResID, mem: ringMem, bufferSize: bufferSize}
	defer ring.Unmap()

	// ---- register the ring (vkCreateRingMESA, ring_idx 0) ----
	const ringIdleTimeoutNS = uint64(1_000_000_000)
	info := ringCreateInfoForBlob(ringResID, bufferSize, extraSize, ringIdleTimeoutNS)
	if err := c.SubmitCmd2(SubmitCmd2Batch{
		Flags:   SubmitCmd2FlagRingIdx,
		RingIdx: 0,
		CmdData: EncodeVkCreateRingMESACS(0x1000, info),
	}); err != nil {
		return res, fmt.Errorf("submit_cmd2(vkCreateRingMESA): %w", err)
	}

	// ---- reply shmem blob (dedicated; reused per reply-bearing command) ----
	const replySize = 4096 // >= the largest vn_sizeof_<cmd>_reply in this set
	replyResID, replyFD, err := rc.ResourceCreateBlob(BlobTypeHost3D, BlobFlagMappable, uint64(replySize), 0)
	if err != nil {
		return res, fmt.Errorf("resource_create_blob (reply): %w", err)
	}
	res.ReplyBlobResID = replyResID
	replyMem, err := syscall.Mmap(replyFD, 0, replySize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	syscall.Close(replyFD)
	if err != nil {
		return res, fmt.Errorf("mmap reply shmem: %w", err)
	}
	defer syscall.Munmap(replyMem)

	d := &clearDriver{ring: ring, replyMem: replyMem, replySize: replySize, res: res, timeout: timeout}

	// Client-allocated Venus object ids (vn_object_id). The host registers each
	// created object under the id we supply (vkr_context_alloc_object). They
	// must be non-zero and distinct.
	const (
		idInstance = 0x1001
		idDevice   = 0x1002
		idImage    = 0x1003
		idMemory   = 0x1004
		idCmdPool  = 0x1005
		idCmdBuf   = 0x1006
		idQueue    = 0x1007
	)
	const rep = CmdGenerateReplyBit

	// 1. vkCreateInstance --------------------------------------------------
	{
		ci := &clearcs.VkInstanceCreateInfo{PApplicationInfo: &clearcs.VkApplicationInfo{ApiVersion: 0}}
		reply, err := d.submitWithReply("vkCreateInstance", ctCreateInstance, clearcs.EncodeCreateInstance(rep, ci, idInstance))
		if err != nil {
			return res, err
		}
		result, inst, ok := clearcs.DecodeCreateInstanceReply(reply)
		res.ResCreateInstance = result
		if result != 0 || !ok {
			return res, fmt.Errorf("vkCreateInstance reply: result=%d ok=%v", result, ok)
		}
		res.Instance = inst
	}

	// 2. vkEnumeratePhysicalDevices (count, then fetch device 0) ----------
	{
		reply, err := d.submitWithReply("vkEnumeratePhysicalDevices(count)", ctEnumeratePhysDevices,
			clearcs.EncodeEnumeratePhysicalDevices(rep, res.Instance, 0, nil))
		if err != nil {
			return res, err
		}
		_, count, countOK, _ := clearcs.DecodeEnumeratePhysicalDevicesReply(reply)
		if !countOK || count == 0 {
			return res, fmt.Errorf("vkEnumeratePhysicalDevices count: countOK=%v count=%d", countOK, count)
		}
		res.PhysDevCount = count
		// The fetch call must carry a client-allocated, NON-ZERO vn_object_id for
		// each VkPhysicalDevice slot: the host decoder registers each physical
		// device under the id we send and rejects id 0 with a CS error
		// ("vkr: invalid object id 0"). Mesa does the same — it fills the request
		// array with the ids of pre-allocated vn_physical_device objects
		// (vn_physical_device.c:vn_instance_enumerate_physical_devices:
		// handles[i] = vn_physical_device_to_handle(physical_dev)). We allocate a
		// distinct id per slot starting at a fixed base.
		const idPhysDevBase = 0x2000
		phys := make([]uint64, count)
		for i := range phys {
			phys[i] = idPhysDevBase + uint64(i)
		}
		reply2, err := d.submitWithReply("vkEnumeratePhysicalDevices(fetch)", ctEnumeratePhysDevices,
			clearcs.EncodeEnumeratePhysicalDevices(rep, res.Instance, count, phys))
		if err != nil {
			return res, err
		}
		_, _, _, devs := clearcs.DecodeEnumeratePhysicalDevicesReply(reply2)
		if len(devs) == 0 {
			return res, fmt.Errorf("vkEnumeratePhysicalDevices fetch: empty device array")
		}
		res.PhysDev = devs[0] // device 0
	}

	// 3. vkGetPhysicalDeviceMemoryProperties (find HOST_VISIBLE|COHERENT) --
	{
		reply, err := d.submitWithReply("vkGetPhysicalDeviceMemoryProperties", ctGetPhysDevMemoryProps,
			clearcs.EncodeGetPhysicalDeviceMemoryProperties(rep, res.PhysDev))
		if err != nil {
			return res, err
		}
		mp, ok := clearcs.DecodeGetPhysicalDeviceMemoryPropertiesReply(reply)
		if !ok {
			return res, fmt.Errorf("vkGetPhysicalDeviceMemoryProperties: decode failed")
		}
		res.MemTypeCount = mp.MemoryTypeCount
		const want = vkMemoryPropertyHostVisible | vkMemoryPropertyHostCoherent
		found := -1
		for i := uint32(0); i < mp.MemoryTypeCount; i++ {
			if mp.MemoryTypes[i].PropertyFlags&want == want {
				found = int(i)
				res.MemTypeFlags = mp.MemoryTypes[i].PropertyFlags
				break
			}
		}
		if found < 0 {
			return res, fmt.Errorf("no HOST_VISIBLE|COHERENT memory type among %d types", mp.MemoryTypeCount)
		}
		res.MemTypeIndex = uint32(found)
	}

	// 4. vkGetPhysicalDeviceQueueFamilyProperties (pick family 0) ---------
	{
		reply, err := d.submitWithReply("vkGetPhysicalDeviceQueueFamilyProperties", ctGetPhysDevQueueFamily,
			clearcs.EncodeGetPhysicalDeviceQueueFamilyProperties(rep, res.PhysDev, 0, nil))
		if err != nil {
			return res, err
		}
		count, countOK, _ := clearcs.DecodeGetPhysicalDeviceQueueFamilyPropertiesReply(reply)
		if !countOK || count == 0 {
			return res, fmt.Errorf("queue family count: countOK=%v count=%d", countOK, count)
		}
		res.QueueFamCount = count
		res.QueueFamilyIdx = 0 // family 0 (lavapipe exposes a single universal family)
	}

	// 5. vkCreateDevice (one queue from family 0) -------------------------
	{
		dci := &clearcs.VkDeviceCreateInfo{
			QueueCreateInfoCount: 1,
			PQueueCreateInfos: []clearcs.VkDeviceQueueCreateInfo{{
				QueueFamilyIndex: res.QueueFamilyIdx,
				QueueCount:       1,
				PQueuePriorities: []float32{1.0},
			}},
		}
		reply, err := d.submitWithReply("vkCreateDevice", ctCreateDevice, clearcs.EncodeCreateDevice(rep, res.PhysDev, dci, idDevice))
		if err != nil {
			return res, err
		}
		result, dev, ok := clearcs.DecodeCreateDeviceReply(reply)
		res.ResCreateDevice = result
		if result != 0 || !ok {
			return res, fmt.Errorf("vkCreateDevice reply: result=%d ok=%v", result, ok)
		}
		res.Device = dev
	}

	// 6. vkGetDeviceQueue2 ------------------------------------------------
	// NOTE: virglrenderer's vkr unconditionally CS-errors plain
	// vkGetDeviceQueue ("Must use vkGetDeviceQueue2 for proper device queue
	// initialization."). We use vkGetDeviceQueue2 with a
	// VkDeviceQueueTimelineInfoMESA pNext binding the queue to sync ring
	// index 1 (non-zero, unused; the CPU ring is index 0).
	{
		reply, err := d.submitWithReply("vkGetDeviceQueue2", ctGetDeviceQueue2,
			encodeGetDeviceQueue2CS(rep, res.Device, res.QueueFamilyIdx, 0, queueSyncRingIdx, idQueue))
		if err != nil {
			return res, err
		}
		queue, ok := decodeGetDeviceQueue2Reply(reply)
		if !ok {
			return res, fmt.Errorf("vkGetDeviceQueue2: absent queue")
		}
		res.Queue = queue
	}

	// 7. vkCreateImage (small LINEAR, TRANSFER_DST) -----------------------
	{
		ici := &clearcs.VkImageCreateInfo{
			ImageType:     vkImageType2D,
			Format:        vkFormatR8G8B8A8UNORM,
			Extent:        clearcs.VkExtent3D{Width: uint32(width), Height: uint32(height), Depth: 1},
			MipLevels:     1,
			ArrayLayers:   1,
			Samples:       vkSampleCount1Bit,
			Tiling:        vkImageTilingLinear,
			Usage:         vkImageUsageTransferDstBit,
			SharingMode:   vkSharingModeExclusive,
			InitialLayout: vkImageLayoutUndefined,
		}
		reply, err := d.submitWithReply("vkCreateImage", ctCreateImage, clearcs.EncodeCreateImage(rep, res.Device, ici, idImage))
		if err != nil {
			return res, err
		}
		result, img, ok := clearcs.DecodeCreateImageReply(reply)
		res.ResCreateImage = result
		if result != 0 || !ok {
			return res, fmt.Errorf("vkCreateImage reply: result=%d ok=%v", result, ok)
		}
		res.Image = img
	}

	// 8. vkGetImageMemoryRequirements -------------------------------------
	{
		reply, err := d.submitWithReply("vkGetImageMemoryRequirements", ctGetImageMemoryReqs,
			clearcs.EncodeGetImageMemoryRequirements(rep, res.Device, res.Image))
		if err != nil {
			return res, err
		}
		mr, ok := clearcs.DecodeGetImageMemoryRequirementsReply(reply)
		if !ok {
			return res, fmt.Errorf("vkGetImageMemoryRequirements: decode failed")
		}
		res.MemReqSize, res.MemReqAlign, res.MemReqBits = mr.Size, mr.Alignment, mr.MemoryTypeBits
		if mr.MemoryTypeBits&(1<<res.MemTypeIndex) == 0 {
			// The image disallows our chosen type; re-pick a HOST_VISIBLE|COHERENT
			// type that IS allowed. (Recorded so the report shows the adjustment.)
			res.Trace = append(res.Trace,
				fmt.Sprintf("memTypeBits=%#x disallows type %d; re-picking", mr.MemoryTypeBits, res.MemTypeIndex))
			return res, fmt.Errorf("chosen mem type %d not in image memoryTypeBits %#x (need re-pick from memprops)", res.MemTypeIndex, mr.MemoryTypeBits)
		}
	}

	// 9. vkAllocateMemory -------------------------------------------------
	{
		allocSize := res.MemReqSize
		if allocSize == 0 {
			allocSize = uint64(width * height * 4)
		}
		mai := &clearcs.VkMemoryAllocateInfo{AllocationSize: allocSize, MemoryTypeIndex: res.MemTypeIndex}
		reply, err := d.submitWithReply("vkAllocateMemory", ctAllocateMemory, clearcs.EncodeAllocateMemory(rep, res.Device, mai, idMemory))
		if err != nil {
			return res, err
		}
		result, mem, ok := clearcs.DecodeAllocateMemoryReply(reply)
		res.ResAllocMemory = result
		if result != 0 || !ok {
			return res, fmt.Errorf("vkAllocateMemory reply: result=%d ok=%v", result, ok)
		}
		res.Memory = mem
	}

	// 10. vkBindImageMemory -----------------------------------------------
	{
		reply, err := d.submitWithReply("vkBindImageMemory", ctBindImageMemory,
			clearcs.EncodeBindImageMemory(rep, res.Device, res.Image, res.Memory, 0))
		if err != nil {
			return res, err
		}
		result := clearcs.DecodeBindImageMemoryReply(reply)
		res.ResBindImage = result
		if result != 0 {
			return res, fmt.Errorf("vkBindImageMemory reply: result=%d", result)
		}
	}

	// 11. vkCreateCommandPool ---------------------------------------------
	{
		cpi := &clearcs.VkCommandPoolCreateInfo{QueueFamilyIndex: res.QueueFamilyIdx}
		reply, err := d.submitWithReply("vkCreateCommandPool", ctCreateCommandPool, clearcs.EncodeCreateCommandPool(rep, res.Device, cpi, idCmdPool))
		if err != nil {
			return res, err
		}
		result, pool, ok := clearcs.DecodeCreateCommandPoolReply(reply)
		res.ResCreatePool = result
		if result != 0 || !ok {
			return res, fmt.Errorf("vkCreateCommandPool reply: result=%d ok=%v", result, ok)
		}
		res.CmdPool = pool
	}

	// 12. vkAllocateCommandBuffers ----------------------------------------
	{
		cbi := &clearcs.VkCommandBufferAllocateInfo{CommandPool: res.CmdPool, Level: 0, CommandBufferCount: 1}
		reply, err := d.submitWithReply("vkAllocateCommandBuffers", ctAllocateCommandBuffers,
			clearcs.EncodeAllocateCommandBuffers(rep, res.Device, cbi, []uint64{idCmdBuf}))
		if err != nil {
			return res, err
		}
		result, bufs := clearcs.DecodeAllocateCommandBuffersReply(reply, 1)
		res.ResAllocCmdBuf = result
		if result != 0 || len(bufs) == 0 {
			return res, fmt.Errorf("vkAllocateCommandBuffers reply: result=%d bufs=%d", result, len(bufs))
		}
		res.CmdBuf = bufs[0]
	}

	// 13. vkBeginCommandBuffer --------------------------------------------
	{
		bi := &clearcs.VkCommandBufferBeginInfo{Flags: vkCommandBufferUsageOneTime}
		reply, err := d.submitWithReply("vkBeginCommandBuffer", ctBeginCommandBuffer, clearcs.EncodeBeginCommandBuffer(rep, res.CmdBuf, bi))
		if err != nil {
			return res, err
		}
		result := clearcs.DecodeBeginCommandBufferReply(reply)
		res.ResBeginCmdBuf = result
		if result != 0 {
			return res, fmt.Errorf("vkBeginCommandBuffer reply: result=%d", result)
		}
	}

	// 14. vkCmdPipelineBarrier (UNDEFINED -> GENERAL) — void, recorded ----
	{
		barrier := clearcs.VkImageMemoryBarrier{
			SrcAccessMask:       0,
			DstAccessMask:       vkAccessTransferWriteBit,
			OldLayout:           vkImageLayoutUndefined,
			NewLayout:           vkImageLayoutGeneral,
			SrcQueueFamilyIndex: vkQueueFamilyIgnored,
			DstQueueFamilyIndex: vkQueueFamilyIgnored,
			Image:               res.Image,
			SubresourceRange: clearcs.VkImageSubresourceRange{
				AspectMask: vkImageAspectColorBit, BaseMipLevel: 0, LevelCount: 1,
				BaseArrayLayer: 0, LayerCount: 1,
			},
		}
		cmd := clearcs.EncodeCmdPipelineBarrier(0, res.CmdBuf,
			vkPipelineStageTopOfPipe, vkPipelineStageTransfer, 0,
			[]clearcs.VkImageMemoryBarrier{barrier})
		if err := d.submitNoReply("vkCmdPipelineBarrier", cmd); err != nil {
			return res, err
		}
	}

	// 15. vkCmdClearColorImage (the clear!) — void, recorded --------------
	{
		color := &clearcs.VkClearColorValue{Tag: 0, Float32: clear}
		rng := clearcs.VkImageSubresourceRange{
			AspectMask: vkImageAspectColorBit, BaseMipLevel: 0, LevelCount: 1,
			BaseArrayLayer: 0, LayerCount: 1,
		}
		cmd := clearcs.EncodeCmdClearColorImage(0, res.CmdBuf, res.Image,
			vkImageLayoutGeneral, color, []clearcs.VkImageSubresourceRange{rng})
		if err := d.submitNoReply("vkCmdClearColorImage", cmd); err != nil {
			return res, err
		}
	}

	// 16. vkEndCommandBuffer ----------------------------------------------
	{
		reply, err := d.submitWithReply("vkEndCommandBuffer", ctEndCommandBuffer, clearcs.EncodeEndCommandBuffer(rep, res.CmdBuf))
		if err != nil {
			return res, err
		}
		result := clearcs.DecodeEndCommandBufferReply(reply)
		res.ResEndCmdBuf = result
		if result != 0 {
			return res, fmt.Errorf("vkEndCommandBuffer reply: result=%d", result)
		}
	}

	// 17. vkCreateFence (the completion signal) ---------------------------
	// Venus does NOT implement queue-idle as a raw vkQueueWaitIdle command — vkr
	// has no host handler for it, so a raw vkQueueWaitIdle CS-errors ("vkr:
	// vkQueueWaitIdle resulted in CS error"). Mesa's vn_QueueWaitIdle (vn_queue.c)
	// instead submits the batch with a FENCE and waits the fence. We follow that
	// exact idiom: create a fence, submit with it, then vkWaitForFences.
	const idFence = 0x1008
	{
		reply, err := d.submitWithReply("vkCreateFence", ctCreateFence,
			clearcs.EncodeCreateFence(rep, res.Device, &clearcs.VkFenceCreateInfo{}, idFence))
		if err != nil {
			return res, err
		}
		result, fence, ok := clearcs.DecodeCreateFenceReply(reply)
		res.ResCreateFence = result
		if result != 0 || !ok {
			return res, fmt.Errorf("vkCreateFence reply: result=%d ok=%v", result, ok)
		}
		res.Fence = fence
	}

	// 18. vkQueueSubmit (signalling the fence) ----------------------------
	{
		si := clearcs.VkSubmitInfo{CommandBufferCount: 1, PCommandBuffers: []uint64{res.CmdBuf}}
		reply, err := d.submitWithReply("vkQueueSubmit", ctQueueSubmit,
			clearcs.EncodeQueueSubmit(rep, res.Queue, []clearcs.VkSubmitInfo{si}, res.Fence))
		if err != nil {
			return res, err
		}
		result, cmdOK := clearcs.DecodeQueueSubmitReply(reply)
		res.ResQueueSubmit = result
		if !cmdOK {
			return res, fmt.Errorf("vkQueueSubmit reply: unexpected cmd_type echo")
		}
		if result != 0 {
			return res, fmt.Errorf("vkQueueSubmit reply: result=%d", result)
		}
	}

	// 19. vkWaitForFences (block until the clear actually completed) -------
	{
		reply, err := d.submitWithReply("vkWaitForFences", ctWaitForFences,
			clearcs.EncodeWaitForFences(rep, res.Device, []uint64{res.Fence}, true, 0xFFFFFFFFFFFFFFFF))
		if err != nil {
			return res, err
		}
		result := clearcs.DecodeWaitForFencesReply(reply)
		res.ResWaitForFences = result
		if result != 0 {
			return res, fmt.Errorf("vkWaitForFences reply: result=%d (host did not signal the submit's fence)", result)
		}
	}

	// 20. GUEST-VISIBLE READBACK — read the cleared image's pixels back. ------
	//
	// This closes the last Venus frontier: not just "the host confirmed the
	// clear", but the GUEST reads the actual cleared texels and they are red.
	//
	// THE PATH (source-cited, Mesa src/virtio/vulkan/vn_device_memory.c +
	// vn_renderer_vtest.c + virglrenderer 1.3.0 vkr).  Venus over vtest does NOT
	// use the guest_vram path (vn_device_memory_alloc_guest_vram, which prepends
	// VkImportMemoryResourceInfoMESA to the VkMemoryAllocateInfo pNext) — that
	// path is gated on dev->renderer->info.has_guest_vram, a virtgpu/crosvm
	// feature the vtest renderer never advertises (vn_renderer_vtest.c sets no
	// guest_vram).  Instead vtest takes the DEFERRED-bo path: the device memory
	// is allocated plainly (our step 9, no pNext — vn_device_memory_alloc_simple
	// / vn_AllocateMemory), and the renderer bo that backs it is created LATER,
	// at map time, by vn_device_memory_bo_init:
	//
	//	vn_renderer_bo_create_from_device_memory(dev->renderer, batch,
	//	    mem_vk->size, mem->base.id,            // blob_id = VkDeviceMemory id
	//	    mem_type->propertyFlags, export_handle_types, &mem->base_bo);
	//
	// which in vn_renderer_vtest.c:vtest_bo_create_from_device_memory issues
	//
	//	VCMD_RESOURCE_CREATE_BLOB(VCMD_BLOB_TYPE_HOST3D,
	//	    VCMD_BLOB_FLAG_MAPPABLE, size, blob_id=mem_id)
	//
	// (HOST_VISIBLE -> MAPPABLE per vtest_bo_blob_flags).  Host-side, the vtest
	// server routes a HOST3D blob whose blob_id != 0 to
	// virglrenderer vkr's vkr_context_create_resource ->
	// vkr_context_create_resource_from_device_memory(ctx, res_id, blob_id, ...),
	// which looks up the EXISTING VkDeviceMemory by blob_id
	// (vkr_context_get_object(ctx, blob_id)) and exports its storage as the
	// resource's blob (vkr_device_memory_export_blob).  Because our plain
	// HOST_VISIBLE allocation has no res_info/export_info, vkr's vkAllocateMemory
	// dispatch FORCES an exportable handle type host-side (the
	// `(property_flags & HOST_VISIBLE) && !res_info` branch sets local_export_info
	// to opaque-fd / dma_buf), so the export succeeds — the mapped blob therefore
	// aliases lavapipe's real host-visible device memory.
	//
	// vkMapMemory is NOT a host round-trip for the bytes: vtest_bo_map just
	// mmap(res_fd) the exported blob (vn_UnmapMemory2 is a literal no-op,
	// bo_flush/bo_invalidate are nops), so the guest mmap of this blob IS the
	// device memory.  We reproduce exactly the create-blob + mmap, then read.
	//
	// The image is LINEAR R8G8B8A8_UNORM bound at memory offset 0 (step 10,
	// bindOffset 0), dedicated allocation, so texel (0,0) is at blob offset 0 and
	// the first row is rowPitch-packed (rowPitch >= width*4).  We read the first
	// row's worth of texels and assert each equals the clear colour.
	{
		allocSize := res.MemReqSize
		if allocSize == 0 {
			allocSize = uint64(width * height * 4)
		}
		// blob_id = idMemory ties the blob to the VkDeviceMemory (the crux).
		memBlobResID, memFD, err := rc.ResourceCreateBlob(BlobTypeHost3D, BlobFlagMappable, allocSize, idMemory)
		if err != nil {
			return res, fmt.Errorf("readback: resource_create_blob(device-memory blob_id=%#x): %w "+
				"(host could not back/export the VkDeviceMemory as a mappable blob)", idMemory, err)
		}
		res.MemBlobResID = memBlobResID
		memMem, err := syscall.Mmap(memFD, 0, int(allocSize), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		syscall.Close(memFD)
		if err != nil {
			return res, fmt.Errorf("readback: mmap device-memory blob res_id=%d size=%d: %w", memBlobResID, allocSize, err)
		}
		defer syscall.Munmap(memMem)

		// Expected clear texel: R8G8B8A8_UNORM encodes float c in [0,1] as
		// round(c*255) per component, in R,G,B,A byte order (Vulkan 1.3 §VkFormat:
		// R8G8B8A8_UNORM = byte0=R … byte3=A).
		enc := func(f float32) byte {
			if f <= 0 {
				return 0
			}
			if f >= 1 {
				return 255
			}
			return byte(f*255 + 0.5)
		}
		res.ExpectTexel = [4]byte{enc(clear[0]), enc(clear[1]), enc(clear[2]), enc(clear[3])}

		// Read the first row of texels (width texels, 4 bytes each, packed at the
		// row start). A dedicated LINEAR R8G8B8A8 image has rowPitch >= width*4;
		// texel i of row 0 is at byte 4*i.
		res.ReadbackDone = true
		copy(res.FirstTexel[:], memMem[0:4])
		nTexels := width
		if nTexels*4 > len(memMem) {
			nTexels = len(memMem) / 4
		}
		allRed := nTexels > 0
		for i := 0; i < nTexels; i++ {
			off := i * 4
			var t [4]byte
			copy(t[:], memMem[off:off+4])
			res.TexelsChecked++
			if t == res.ExpectTexel {
				res.TexelsRed++
			} else {
				allRed = false
			}
		}
		res.ReadbackPass = allRed
		res.Trace = append(res.Trace, fmt.Sprintf(
			"readback device-memory blob res_id=%d size=%d: texel0=%v expect=%v texels_red=%d/%d pass=%v",
			memBlobResID, allocSize, res.FirstTexel, res.ExpectTexel, res.TexelsRed, res.TexelsChecked, res.ReadbackPass))
		if !res.ReadbackPass {
			return res, fmt.Errorf("readback: mapped device-memory texels are NOT the clear colour "+
				"(texel0=%v expect=%v, %d/%d red) — the clear did not land in the guest-visible mapping",
				res.FirstTexel, res.ExpectTexel, res.TexelsRed, res.TexelsChecked)
		}
	}

	return res, nil
}
