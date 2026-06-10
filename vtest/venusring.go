package vtest

// Venus ring round-trip driver (platform-independent parts).
//
// The genuine "host responded" proof for Venus over the vtest socket is the
// host renderer ADVANCING THE RING HEAD after consuming a command we wrote into
// the ring's shared memory. That shared memory is a HOST3D blob the server
// exports and hands us as an fd over SCM_RIGHTS (vtest_renderer.c:
// vtest_resource_create_blob -> vtest_send_fd); we mmap it, write a
// vkCreateInstance command stream into the buffer region, store the new tail,
// then busy-poll the head word.
//
// The fd-receive + mmap + poll are OS-specific and live in
// venusring_linux.go (the validator binary runs on the Debian arm64 guest).
// This file holds the OS-independent pieces: the ring layout constants
// (transcribed from go-virtio/venus/ring, itself from Mesa vn_ring.c) and the
// VkRingCreateInfoMESA the guest sends.

// Ring shared-memory layout — Mesa vn_ring.c:vn_ring_get_layout. Each control
// word sits on its own 64-byte cache line (alignas(64)); buffer follows at 192.
const (
	ringCacheLine    = 64
	ringHeadOffset   = 0
	ringTailOffset   = ringCacheLine     // 64
	ringStatusOffset = 2 * ringCacheLine // 128
	ringBufferOffset = 3 * ringCacheLine // 192
	ringControlWord  = 4                 // uint32_t head/tail/status width
)

// RingShmemSize returns the total ring shared-memory size for a command buffer
// of bufferSize bytes plus an extra (reply) region of extraSize bytes:
// extra_offset = buffer_offset + buffer_size; shmem_size = extra_offset +
// extra_size (vn_ring.c).
func RingShmemSize(bufferSize, extraSize int) int {
	return ringBufferOffset + bufferSize + extraSize
}

// ringCreateInfoForBlob builds the VkRingCreateInfoMESA body for a ring backed
// by HOST3D blob res_id whose shmem is laid out per vn_ring.c at blob offset 0,
// with a power-of-two command buffer and an extra region. Mirrors
// vn_ring_create's `struct VkRingCreateInfoMESA info = {...}`.
func ringCreateInfoForBlob(resID uint32, bufferSize, extraSize int, idleTimeout uint64) RingCreateInfoMESA {
	return RingCreateInfoMESA{
		Flags:        0,
		ResourceID:   resID,
		Offset:       0,
		Size:         uint64(RingShmemSize(bufferSize, extraSize)),
		IdleTimeout:  idleTimeout,
		HeadOffset:   ringHeadOffset,
		TailOffset:   ringTailOffset,
		StatusOffset: ringStatusOffset,
		BufferOffset: ringBufferOffset,
		BufferSize:   uint64(bufferSize),
		ExtraOffset:  uint64(ringBufferOffset + bufferSize),
		ExtraSize:    uint64(extraSize),
	}
}
