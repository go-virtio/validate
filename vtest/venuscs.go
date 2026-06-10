package vtest

// Venus command-stream (vn_cs) encoders needed to drive a ring over the vtest
// socket. These reproduce, byte for byte, the Mesa-generated encoders in
// src/virtio/venus-protocol/vn_protocol_driver_transport.h and
// vn_protocol_driver_commands.h. We hand-encode here (rather than import
// go-virtio/venus) because that module's encoder runtime lives under an
// internal/ package and exposes no public Encoder constructor; the wire format
// is small and fully source-cited, so we transcribe it directly. The
// VkRingCreateInfoMESA *body* is identical to go-virtio/venus/ring.EncodeCreateInfo
// and is cross-checked against it in the tests.
//
// vn_cs wire primitives (vn_protocol_driver_cs.h, vn_cs.h):
//   - little-endian, every value padded to a 4-byte boundary;
//   - uint32/int32/VkFlags/VkStructureType/VkCommandTypeEXT = 4 bytes;
//   - uint64/size_t/VkDeviceSize/handle = 8 bytes;
//   - simple_pointer(p) = 8-byte array_size (1 if non-NULL else 0);
//   - string = 8-byte (strlen+1) array_size then the NUL-terminated bytes
//     padded to 4.

import "encoding/binary"

// VkCommandTypeEXT ids — vn_protocol_driver_defines.h.
const (
	cmdTypeVkCreateInstance            = 0   // VK_COMMAND_TYPE_vkCreateInstance_EXT
	cmdTypeVkSetReplyCommandStreamMESA = 178 // VK_COMMAND_TYPE_vkSetReplyCommandStreamMESA_EXT
	cmdTypeVkCreateRingMESA            = 188 // VK_COMMAND_TYPE_vkCreateRingMESA_EXT
)

// CmdGenerateReplyBit is VK_COMMAND_GENERATE_REPLY_BIT_EXT
// (vn_protocol_driver_defines.h: `VK_COMMAND_GENERATE_REPLY_BIT_EXT =
// 0x00000001`). Setting it in a command's cmd_flags makes the host renderer
// emit a reply into the reply command stream (see EncodeVkSetReplyCommandStreamMESACS
// and the venusclear driver's reply-shmem path).
const CmdGenerateReplyBit = 0x1

// VkStructureType ids for the MESA ring structs. VK_MESA_venus_protocol is
// extension number 385 (xmls/VK_MESA_venus_protocol.xml), so an offset-based
// sType is 1000000000 + (385-1)*1000 + offset:
//
//	RING_CREATE_INFO_MESA  offset 0 -> 1000384000
//	RING_MONITOR_INFO_MESA offset 6 -> 1000384006
const (
	styRingCreateInfoMESA = 1000384000
	// VkStructureType for a plain VkInstanceCreateInfo (Vulkan core).
	styInstanceCreateInfo = 1 // VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO
	styApplicationInfo    = 0 // VK_STRUCTURE_TYPE_APPLICATION_INFO
)

// csBuilder is a tiny little-endian, 4-byte-padded command-stream writer,
// equivalent to Mesa's vn_cs_encoder write path.
type csBuilder struct{ b []byte }

func (e *csBuilder) u32(v uint32) {
	e.b = binary.LittleEndian.AppendUint32(e.b, v)
}
func (e *csBuilder) i32(v int32) { e.u32(uint32(v)) }
func (e *csBuilder) u64(v uint64) {
	e.b = binary.LittleEndian.AppendUint64(e.b, v)
}

// simplePointer mirrors vn_encode_simple_pointer: array_size(1) if present
// else array_size(0); returns present so the caller can encode the pointee.
func (e *csBuilder) simplePointer(present bool) bool {
	if present {
		e.u64(1)
	} else {
		e.u64(0)
	}
	return present
}

// strOrEmpty mirrors Mesa's encode of a char* member (e.g.
// VkApplicationInfo.pApplicationName): a non-empty string is array_size(strlen+1)
// then the NUL-terminated bytes zero-padded to a 4-byte boundary; an empty/NULL
// string is a bare array_size(0). It is NOT wrapped in a simple_pointer.
func (e *csBuilder) strOrEmpty(s string) {
	if s == "" {
		e.u64(0)
		return
	}
	e.u64(uint64(len(s) + 1)) // array_size = strlen + 1 (counts the NUL)
	e.b = append(e.b, s...)
	e.b = append(e.b, 0) // NUL terminator
	for len(e.b)%4 != 0 {
		e.b = append(e.b, 0) // pad to 4-byte boundary
	}
}

// str mirrors vn_encode_char_array for a const char*: array_size(strlen+1)
// then the NUL-terminated bytes padded to 4.
func (e *csBuilder) str(s string) {
	size := uint64(len(s) + 1)
	e.u64(size)
	withNUL := make([]byte, (len(s)+1+3)&^3)
	copy(withNUL, s)
	e.b = append(e.b, withNUL...)
}

// EncodeVkCreateRingMESACS produces the raw command stream for
// vkCreateRingMESA(ring, &VkRingCreateInfoMESA{...}), the cs Mesa passes to
// vn_renderer_submit_simple in vn_ring_create. pNext is encoded as NULL (the
// optional VkRingMonitorInfoMESA is omitted; vn_encode_VkRingCreateInfoMESA_pnext
// accepts a NULL chain as simple_pointer(0)).
//
// Field order, from vn_encode_vkCreateRingMESA + vn_encode_VkRingCreateInfoMESA
// + _self:
//
//	i32 cmd_type=188
//	u32 cmd_flags=0
//	u64 ring (handle)
//	u64 simple_pointer(pCreateInfo)=1
//	  i32 sType=RING_CREATE_INFO_MESA
//	  u64 simple_pointer(pNext)=0          (no monitor info)
//	  u32 flags
//	  u32 resourceId
//	  u64 offset u64 size u64 idleTimeout
//	  u64 headOffset u64 tailOffset u64 statusOffset
//	  u64 bufferOffset u64 bufferSize u64 extraOffset u64 extraSize
func EncodeVkCreateRingMESACS(ring uint64, info RingCreateInfoMESA) []byte {
	var e csBuilder
	e.i32(cmdTypeVkCreateRingMESA)
	e.u32(0) // cmd_flags
	e.u64(ring)
	if e.simplePointer(true) { // pCreateInfo present
		e.i32(styRingCreateInfoMESA)
		e.simplePointer(false) // pNext = NULL
		e.u32(info.Flags)
		e.u32(info.ResourceID)
		e.u64(info.Offset)
		e.u64(info.Size)
		e.u64(info.IdleTimeout)
		e.u64(info.HeadOffset)
		e.u64(info.TailOffset)
		e.u64(info.StatusOffset)
		e.u64(info.BufferOffset)
		e.u64(info.BufferSize)
		e.u64(info.ExtraOffset)
		e.u64(info.ExtraSize)
	}
	return e.b
}

// RingCreateInfoMESA mirrors the fields of VkRingCreateInfoMESA that
// vn_encode_VkRingCreateInfoMESA_self serialises (sType/pNext skipped). It is
// the same shape as go-virtio/venus/ring.CreateInfo; we keep a local copy so
// this package does not need the venus module just to name the fields.
type RingCreateInfoMESA struct {
	Flags        uint32
	ResourceID   uint32
	Offset       uint64
	Size         uint64
	IdleTimeout  uint64
	HeadOffset   uint64
	TailOffset   uint64
	StatusOffset uint64
	BufferOffset uint64
	BufferSize   uint64
	ExtraOffset  uint64
	ExtraSize    uint64
}

// EncodeMinimalVkCreateInstanceCS produces the command stream for a minimal
// vkCreateInstance with an application info (no layers, no extensions) and a
// returned-handle slot. This is the command we write INTO the ring to force
// the host consumer to advance head. Field order from
// vn_encode_vkCreateInstance + vn_encode_VkInstanceCreateInfo(_self) +
// vn_encode_VkApplicationInfo(_self):
//
//	vkCreateInstance:
//	  i32 cmd_type=0
//	  u32 cmd_flags
//	  u64 simple_pointer(pCreateInfo)=1
//	    VkInstanceCreateInfo:
//	      i32 sType=INSTANCE_CREATE_INFO
//	      u64 simple_pointer(pNext)=0
//	      u32 flags=0
//	      u64 simple_pointer(pApplicationInfo)=1
//	        VkApplicationInfo:
//	          i32 sType=APPLICATION_INFO
//	          u64 simple_pointer(pNext)=0
//	          str pApplicationName    (array_size(0) when "")
//	          u32 applicationVersion
//	          str pEngineName         (array_size(0) when "")
//	          u32 engineVersion
//	          u32 apiVersion
//	      u32 enabledLayerCount=0
//	      (pp layer names) array_size(0)
//	      u32 enabledExtensionCount=0
//	      (pp ext names) array_size(0)
//	  u64 simple_pointer(pAllocator)=0
//	  u64 simple_pointer(pInstance)=1
//	    u64 handle id (0 placeholder; the renderer fills the reply)
//
// cmdFlags carries the vn_cs cmd flags; Mesa's vn_submit_vkCreateInstance uses
// VK_COMMAND_GENERATE_REPLY_BIT_EXT (= 1) to request a reply, but for the bare
// "did the host consume the ring" proof a reply is not required, so the caller
// chooses (0 = fire-and-forget, 1 = ask for a reply).
func EncodeMinimalVkCreateInstanceCS(cmdFlags uint32, appName, engineName string, apiVersion uint64, instanceHandle uint64) []byte {
	var e csBuilder
	e.i32(cmdTypeVkCreateInstance)
	e.u32(cmdFlags)
	if e.simplePointer(true) { // pCreateInfo
		e.i32(styInstanceCreateInfo)
		e.simplePointer(false) // pNext
		e.u32(0)               // flags
		if e.simplePointer(true) {
			e.i32(styApplicationInfo)
			e.simplePointer(false) // pNext
			// vn_encode_VkApplicationInfo_self: pApplicationName is a char*
			// encoded as a string (array_size(strlen+1)+bytes) when non-empty,
			// else a bare array_size(0). NOT wrapped in a simple_pointer.
			e.strOrEmpty(appName)
			e.u32(0) // applicationVersion
			e.strOrEmpty(engineName)
			e.u32(0)                  // engineVersion
			e.u32(uint32(apiVersion)) // apiVersion
		}
		e.u32(0) // enabledLayerCount
		e.u64(0) // ppEnabledLayerNames array_size(0)
		e.u32(0) // enabledExtensionCount
		e.u64(0) // ppEnabledExtensionNames array_size(0)
	}
	e.simplePointer(false) // pAllocator = NULL
	if e.simplePointer(instanceHandle != 0) {
		e.u64(instanceHandle)
	}
	return e.b
}

// EncodeVkSetReplyCommandStreamMESACS produces the command stream for
// vkSetReplyCommandStreamMESA(&VkCommandStreamDescriptionMESA{resourceId,
// offset, size}), the command Mesa submits into the ring BEFORE a
// reply-bearing command to tell the host renderer where to write that reply
// (vn_ring.c:vn_ring_set_reply_shmem_locked). The reply lands in a SEPARATE
// reply shmem resource (resourceId), not in the ring's extra region.
//
// Field order, from vn_encode_vkSetReplyCommandStreamMESA +
// vn_encode_VkCommandStreamDescriptionMESA
// (vn_protocol_driver_transport.h:558 / :27):
//
//	i32 cmd_type=178
//	u32 cmd_flags=0
//	u64 simple_pointer(pStream)=1
//	  u32 resourceId
//	  u64 offset   (size_t -> u64; vn_encode_size_t)
//	  u64 size     (size_t -> u64)
func EncodeVkSetReplyCommandStreamMESACS(resourceID uint32, offset, size uint64) []byte {
	var e csBuilder
	e.i32(cmdTypeVkSetReplyCommandStreamMESA)
	e.u32(0) // cmd_flags
	if e.simplePointer(true) {
		e.u32(resourceID)
		e.u64(offset)
		e.u64(size)
	}
	return e.b
}
