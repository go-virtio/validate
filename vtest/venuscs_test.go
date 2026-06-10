package vtest

// Byte-derived tests for the Venus command-stream encoders in venuscs.go.
// Expected streams are hand-computed from the Mesa venus-protocol encoders
// (src/virtio/venus-protocol/vn_protocol_driver_{transport,instance}.h),
// quoted at each assertion. The vn_cs wire primitives used:
//
//	u32/i32/VkFlags/VkStructureType/VkCommandTypeEXT = 4 bytes LE
//	u64/size_t/handle/array_size/simple_pointer      = 8 bytes LE
//	char_array(s) = array_size(strlen+1) then the NUL-terminated bytes
//	                zero-padded up to a 4-byte boundary
//
// The VkRingCreateInfoMESA body is additionally cross-checked, byte for byte,
// against go-virtio/venus/ring.EncodeCreateInfo (the independently-proven
// encoder in the venus module).

import (
	"bytes"
	"testing"

	vring "github.com/go-virtio/venus/ring"
)

// le64 builds a little-endian 8-byte value for the hand-derived streams.
func le64(v uint64) []byte {
	return []byte{
		byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24),
		byte(v >> 32), byte(v >> 40), byte(v >> 48), byte(v >> 56),
	}
}

func TestCSBuilderPrimitives(t *testing.T) {
	// u32/i32 are 4-byte LE; u64 is 8-byte LE.
	var e csBuilder
	e.u32(0x11223344)
	e.i32(-1) // 0xFFFFFFFF
	e.u64(0x0102030405060708)
	want := concat(
		le32(0x11223344),
		le32(0xFFFFFFFF),
		le64(0x0102030405060708),
	)
	if !bytes.Equal(e.b, want) {
		t.Fatalf("csBuilder primitives\n got=%v\nwant=%v", e.b, want)
	}
}

func TestCSBuilderSimplePointer(t *testing.T) {
	// vn_encode_simple_pointer: array_size(1) when present, array_size(0) else.
	var present csBuilder
	if got := present.simplePointer(true); !got {
		t.Fatal("simplePointer(true) should return true")
	}
	if !bytes.Equal(present.b, le64(1)) {
		t.Fatalf("simplePointer(true) = %v, want %v", present.b, le64(1))
	}
	var absent csBuilder
	if got := absent.simplePointer(false); got {
		t.Fatal("simplePointer(false) should return false")
	}
	if !bytes.Equal(absent.b, le64(0)) {
		t.Fatalf("simplePointer(false) = %v, want %v", absent.b, le64(0))
	}
}

func TestCSBuilderStrOrEmpty(t *testing.T) {
	// Empty string → bare array_size(0), 8 bytes (vn_encode_array_size(enc, 0)).
	var empty csBuilder
	empty.strOrEmpty("")
	if !bytes.Equal(empty.b, le64(0)) {
		t.Fatalf("strOrEmpty(\"\") = %v, want %v", empty.b, le64(0))
	}

	// "vk" (len 2) → array_size(strlen+1=3) then "vk\0" padded to 4: "vk\0\0".
	var s csBuilder
	s.strOrEmpty("vk")
	want := concat(le64(3), []byte{'v', 'k', 0, 0})
	if !bytes.Equal(s.b, want) {
		t.Fatalf("strOrEmpty(\"vk\")\n got=%v\nwant=%v", s.b, want)
	}

	// "abc" (len 3) → array_size(4) then "abc\0" (already 4-aligned, no extra pad).
	var s2 csBuilder
	s2.strOrEmpty("abc")
	want2 := concat(le64(4), []byte{'a', 'b', 'c', 0})
	if !bytes.Equal(s2.b, want2) {
		t.Fatalf("strOrEmpty(\"abc\")\n got=%v\nwant=%v", s2.b, want2)
	}

	// "abcd" (len 4) → array_size(5) then "abcd\0" padded to 8: "abcd\0\0\0\0".
	var s3 csBuilder
	s3.strOrEmpty("abcd")
	want3 := concat(le64(5), []byte{'a', 'b', 'c', 'd', 0, 0, 0, 0})
	if !bytes.Equal(s3.b, want3) {
		t.Fatalf("strOrEmpty(\"abcd\")\n got=%v\nwant=%v", s3.b, want3)
	}
}

func TestCSBuilderStr(t *testing.T) {
	// str(s) = array_size(strlen+1) then the bytes padded to 4 (vn_encode_char_array
	// without the empty-pointer special case).
	var e csBuilder
	e.str("hi")
	// "hi" len 2 → size 3, padded ((2+1+3)&^3)=4 bytes: "hi\0\0".
	want := concat(le64(3), []byte{'h', 'i', 0, 0})
	if !bytes.Equal(e.b, want) {
		t.Fatalf("str(\"hi\")\n got=%v\nwant=%v", e.b, want)
	}

	// Empty string still emits array_size(1) + 4 padded NULs (this is the
	// vn_encode_char_array shape, distinct from strOrEmpty's bare array_size(0)).
	var z csBuilder
	z.str("")
	wantz := concat(le64(1), []byte{0, 0, 0, 0})
	if !bytes.Equal(z.b, wantz) {
		t.Fatalf("str(\"\")\n got=%v\nwant=%v", z.b, wantz)
	}
}

func TestEncodeVkCreateRingMESACS(t *testing.T) {
	info := RingCreateInfoMESA{
		Flags: 0, ResourceID: 7,
		Offset: 0, Size: 0x40140, IdleTimeout: 0,
		HeadOffset: 0, TailOffset: 64, StatusOffset: 128,
		BufferOffset: 192, BufferSize: 0x40000,
		ExtraOffset: 0x40140, ExtraSize: 0,
	}
	ring := uint64(0x1000)
	got := EncodeVkCreateRingMESACS(ring, info)

	// vn_encode_vkCreateRingMESA: i32 cmd_type=188, u32 cmd_flags=0, u64 ring,
	// simple_pointer(pCreateInfo)=1; then VkRingCreateInfoMESA: i32 sType,
	// pnext simple_pointer(0), then _self body.
	want := concat(
		le32(cmdTypeVkCreateRingMESA), // 188
		le32(0),                       // cmd_flags
		le64(ring),
		le64(1),                     // simple_pointer(pCreateInfo)
		le32(styRingCreateInfoMESA), // sType=1000384000
		le64(0),                     // simple_pointer(pNext)=NULL
		// _self body: flags, resourceId, then 10 size_t/u64:
		le32(0),       // flags
		le32(7),       // resourceId
		le64(0),       // offset
		le64(0x40140), // size
		le64(0),       // idleTimeout
		le64(0),       // headOffset
		le64(64),      // tailOffset
		le64(128),     // statusOffset
		le64(192),     // bufferOffset
		le64(0x40000), // bufferSize
		le64(0x40140), // extraOffset
		le64(0),       // extraSize
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeVkCreateRingMESACS\n got=%v\nwant=%v", got, want)
	}
}

// TestEncodeVkSetReplyCommandStreamMESACS is byte-derived from Mesa
// vn_encode_vkSetReplyCommandStreamMESA + vn_encode_VkCommandStreamDescriptionMESA
// (src/virtio/venus-protocol/vn_protocol_driver_transport.h:558 / :27):
//
//	i32 cmd_type=178
//	u32 cmd_flags=0
//	u64 simple_pointer(pStream)=1
//	  u32 resourceId
//	  u64 offset   (size_t -> u64)
//	  u64 size     (size_t -> u64)
func TestEncodeVkSetReplyCommandStreamMESACS(t *testing.T) {
	got := EncodeVkSetReplyCommandStreamMESACS(42 /*resourceId*/, 0x80 /*offset*/, 0x1000 /*size*/)
	want := concat(
		le32(cmdTypeVkSetReplyCommandStreamMESA), // 178
		le32(0),                                  // cmd_flags
		le64(1),                                  // simple_pointer(pStream)=1
		le32(42),                                 // resourceId
		le64(0x80),                               // offset
		le64(0x1000),                             // size
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeVkSetReplyCommandStreamMESACS\n got=%v\nwant=%v", got, want)
	}
	// The stream is a whole number of dwords (4 + 4 + 8 + 4 + 8 + 8 = 36).
	if len(got)%4 != 0 {
		t.Fatalf("stream length %d is not dword-aligned", len(got))
	}
}

// TestCmdGenerateReplyBit pins the GENERATE_REPLY flag value to the Mesa
// definition (vn_protocol_driver_defines.h: VK_COMMAND_GENERATE_REPLY_BIT_EXT
// = 0x00000001).
func TestCmdGenerateReplyBit(t *testing.T) {
	if CmdGenerateReplyBit != 0x1 {
		t.Fatalf("CmdGenerateReplyBit = %#x, want 0x1", CmdGenerateReplyBit)
	}
}

// TestRingCreateInfoBodyMatchesVenusModule cross-checks the _self body our
// encoder emits against the independently-proven go-virtio/venus/ring encoder:
// strip the 24-byte command+info preamble (cmd_type, cmd_flags, ring,
// simple_pointer, sType, pnext) and the remaining 88 bytes must equal
// ring.EncodeCreateInfo for the equivalent CreateInfo.
func TestRingCreateInfoBodyMatchesVenusModule(t *testing.T) {
	info := RingCreateInfoMESA{
		Flags: 0, ResourceID: 42,
		Offset: 0, Size: 0x40140, IdleTimeout: 123,
		HeadOffset: 0, TailOffset: 64, StatusOffset: 128,
		BufferOffset: 192, BufferSize: 0x40000,
		ExtraOffset: 0x40140, ExtraSize: 0,
	}
	cs := EncodeVkCreateRingMESACS(0x1000, info)

	// preamble before the _self body:
	//   i32 cmd_type (4) + u32 cmd_flags (4) + u64 ring (8)
	// + u64 simple_pointer (8) + i32 sType (4) + u64 simple_pointer pnext (8) = 36
	const preamble = 4 + 4 + 8 + 8 + 4 + 8
	body := cs[preamble:]

	ref := vring.EncodeCreateInfo(vring.CreateInfo{
		Flags: info.Flags, ResourceID: info.ResourceID,
		Offset: info.Offset, Size: info.Size, IdleTimeout: info.IdleTimeout,
		HeadOffset: info.HeadOffset, TailOffset: info.TailOffset,
		StatusOffset: info.StatusOffset, BufferOffset: info.BufferOffset,
		BufferSize: info.BufferSize, ExtraOffset: info.ExtraOffset,
		ExtraSize: info.ExtraSize,
	})
	if len(ref) != vring.CreateInfoBodySize {
		t.Fatalf("venus ring body size = %d, want %d", len(ref), vring.CreateInfoBodySize)
	}
	if !bytes.Equal(body, ref) {
		t.Fatalf("ring info body mismatch vs go-virtio/venus/ring\n got=%v\nref=%v", body, ref)
	}
}

func TestEncodeMinimalVkCreateInstanceCS_NoHandle(t *testing.T) {
	// cmd_flags=0, empty app/engine names, apiVersion=0, no returned handle.
	got := EncodeMinimalVkCreateInstanceCS(0, "", "", 0, 0)
	want := concat(
		le32(cmdTypeVkCreateInstance), // 0
		le32(0),                       // cmd_flags
		le64(1),                       // simple_pointer(pCreateInfo)
		le32(styInstanceCreateInfo),   // sType=1
		le64(0),                       // simple_pointer(pNext)=0
		le32(0),                       // flags
		le64(1),                       // simple_pointer(pApplicationInfo)
		le32(styApplicationInfo),      // sType=0
		le64(0),                       // simple_pointer(pNext)=0
		le64(0),                       // pApplicationName: array_size(0)
		le32(0),                       // applicationVersion
		le64(0),                       // pEngineName: array_size(0)
		le32(0),                       // engineVersion
		le32(0),                       // apiVersion
		le32(0),                       // enabledLayerCount
		le64(0),                       // ppEnabledLayerNames array_size(0)
		le32(0),                       // enabledExtensionCount
		le64(0),                       // ppEnabledExtensionNames array_size(0)
		le64(0),                       // simple_pointer(pAllocator)=0
		le64(0),                       // simple_pointer(pInstance)=0 (no handle)
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeMinimalVkCreateInstanceCS no-handle\n got=%v\nwant=%v", got, want)
	}
}

func TestEncodeMinimalVkCreateInstanceCS_WithNamesAndHandle(t *testing.T) {
	// Non-empty names exercise the char_array path; a non-zero instance handle
	// exercises simple_pointer(pInstance)=1 + the 8-byte handle id.
	got := EncodeMinimalVkCreateInstanceCS(1 /*GENERATE_REPLY*/, "app", "engx", 0x00401000, 0xABCD)
	want := concat(
		le32(cmdTypeVkCreateInstance),
		le32(1), // cmd_flags = GENERATE_REPLY
		le64(1), // simple_pointer(pCreateInfo)
		le32(styInstanceCreateInfo),
		le64(0), // pNext
		le32(0), // flags
		le64(1), // simple_pointer(pApplicationInfo)
		le32(styApplicationInfo),
		le64(0), // pNext
		concat(le64(4), []byte{'a', 'p', 'p', 0}), // "app" → size 4, "app\0"
		le32(0), // applicationVersion
		concat(le64(5), []byte{'e', 'n', 'g', 'x', 0, 0, 0, 0}), // "engx" → size 5, padded to 8
		le32(0),          // engineVersion
		le32(0x00401000), // apiVersion
		le32(0),          // enabledLayerCount
		le64(0),          // layer names array_size(0)
		le32(0),          // enabledExtensionCount
		le64(0),          // ext names array_size(0)
		le64(0),          // pAllocator
		le64(1),          // simple_pointer(pInstance)=1
		le64(0xABCD),     // instance handle id
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeMinimalVkCreateInstanceCS with names+handle\n got=%v\nwant=%v", got, want)
	}
}
