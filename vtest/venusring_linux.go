//go:build linux

package vtest

// Linux fd-passing + mmap + ring round-trip for Venus over the vtest socket.
//
// To reliably receive the HOST3D blob fd the server sends over SCM_RIGHTS
// (vtest_renderer.c:vtest_send_fd: one dummy data byte + an SCM_RIGHTS cmsg),
// we drive the socket with raw recvmsg/sendmsg on the underlying fd rather than
// through net.Conn, whose buffered reader could swallow the dummy byte (and the
// attached cmsg) ahead of our recvmsg. RawConn therefore implements the SAME
// vtest wire the rest of the package speaks, but on a bare socket fd.

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"time"
)

// RawConn is a raw-fd vtest transport: a connected AF_UNIX/SOCK_STREAM socket
// we read/write with plain syscalls so we can recvmsg the blob fd.
type RawConn struct {
	fd int
}

// DialRaw connects to the vtest server at socketPath on a bare socket fd.
func DialRaw(socketPath string) (*RawConn, error) {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vtest: socket: %w", err)
	}
	sa := &syscall.SockaddrUnix{Name: socketPath}
	if err := syscall.Connect(fd, sa); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("vtest: connect %s: %w", socketPath, err)
	}
	return &RawConn{fd: fd}, nil
}

// Close closes the socket.
func (r *RawConn) Close() error { return syscall.Close(r.fd) }

// Write writes the whole buffer (handles short writes), matching
// vn_renderer_vtest.c:vtest_write.
func (r *RawConn) Write(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := syscall.Write(r.fd, p[total:])
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return total, err
		}
		if n == 0 {
			return total, fmt.Errorf("vtest: raw write returned 0")
		}
		total += n
	}
	return total, nil
}

// Read reads up to len(p) bytes (single recv; callers loop for full reads via
// readFull). It satisfies io.Reader so RawConn can back a Client for the plain
// (non-fd) commands.
func (r *RawConn) Read(p []byte) (int, error) {
	for {
		n, err := syscall.Read(r.fd, p)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return n, err
		}
		if n == 0 {
			return 0, fmt.Errorf("vtest: raw read EOF (server closed connection)")
		}
		return n, nil
	}
}

// readFull reads exactly len(p) bytes.
func (r *RawConn) readFull(p []byte) error {
	got := 0
	for got < len(p) {
		n, err := r.Read(p[got:])
		if err != nil {
			return err
		}
		got += n
	}
	return nil
}

// recvFD performs a recvmsg expecting one dummy data byte plus an SCM_RIGHTS
// control message carrying a single fd, mirroring Mesa vtest_receive_fd.
func (r *RawConn) recvFD() (int, error) {
	oob := make([]byte, syscall.CmsgSpace(4)) // room for one int fd
	dummy := make([]byte, 1)
	for {
		n, oobn, _, _, err := syscall.Recvmsg(r.fd, dummy, oob, 0)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return -1, fmt.Errorf("vtest: recvmsg blob fd: %w", err)
		}
		if n == 0 && oobn == 0 {
			return -1, fmt.Errorf("vtest: recvmsg blob fd: server closed (no fd)")
		}
		scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return -1, fmt.Errorf("vtest: parse cmsg: %w", err)
		}
		if len(scms) == 0 {
			return -1, fmt.Errorf("vtest: no SCM_RIGHTS cmsg in blob reply")
		}
		fds, err := syscall.ParseUnixRights(&scms[0])
		if err != nil || len(fds) == 0 {
			return -1, fmt.Errorf("vtest: parse SCM_RIGHTS: %v", err)
		}
		return fds[0], nil
	}
}

// ResourceCreateBlob sends VCMD_RESOURCE_CREATE_BLOB, reads the [len=1,id=18] +
// res_id reply, then recvmsg's the exported blob fd over SCM_RIGHTS. Mirrors
// vn_renderer_vtest.c:vtest_vcmd_resource_create_blob.
func (r *RawConn) ResourceCreateBlob(blobType, flags uint32, size, blobID uint64) (resID uint32, fd int, err error) {
	if _, err = r.Write(encodeResourceCreateBlob(blobType, flags, size, blobID)); err != nil {
		return 0, -1, fmt.Errorf("vtest: resource_create_blob write: %w", err)
	}
	var hdr [hdrBytes]byte
	if err = r.readFull(hdr[:]); err != nil {
		return 0, -1, fmt.Errorf("vtest: resource_create_blob reply header: %w", err)
	}
	length := binary.LittleEndian.Uint32(hdr[0:4])
	cmd := binary.LittleEndian.Uint32(hdr[4:8])
	if cmd != VcmdResourceCreateBlob || length != 1 {
		return 0, -1, fmt.Errorf("vtest: resource_create_blob reply mismatch len=%d cmd=%d (want len=1 cmd=%d)", length, cmd, VcmdResourceCreateBlob)
	}
	var idBuf [4]byte
	if err = r.readFull(idBuf[:]); err != nil {
		return 0, -1, fmt.Errorf("vtest: resource_create_blob res_id: %w", err)
	}
	resID = binary.LittleEndian.Uint32(idBuf[:])
	fd, err = r.recvFD()
	if err != nil {
		return 0, -1, err
	}
	return resID, fd, nil
}

// MappedRing is an mmap'd ring shared-memory region plus its res_id.
type MappedRing struct {
	ResID      uint32
	mem        []byte
	bufferSize int
}

// Head/Tail/Status read the consumer/producer/status control words from the
// mmap (vn_ring.c offsets). Head and Status are written by the HOST renderer.
func (m *MappedRing) Head() uint32 {
	return binary.LittleEndian.Uint32(m.mem[ringHeadOffset : ringHeadOffset+ringControlWord])
}
func (m *MappedRing) Tail() uint32 {
	return binary.LittleEndian.Uint32(m.mem[ringTailOffset : ringTailOffset+ringControlWord])
}
func (m *MappedRing) Status() uint32 {
	return binary.LittleEndian.Uint32(m.mem[ringStatusOffset : ringStatusOffset+ringControlWord])
}

// writeCmd writes cmd into the buffer region at the masked cursor (handling
// wrap), then release-stores the new tail (= cur). Mirrors vn_ring.c
// vn_ring_store_tail + the buffer write.
func (m *MappedRing) writeCmd(cur uint32, cmd []byte) uint32 {
	bufSize := uint32(m.bufferSize)
	mask := bufSize - 1
	start := cur & mask
	n := uint32(len(cmd))
	base := ringBufferOffset
	if start+n <= bufSize {
		copy(m.mem[base+int(start):], cmd)
	} else {
		first := bufSize - start
		copy(m.mem[base+int(start):base+m.bufferSize], cmd[:first])
		copy(m.mem[base:], cmd[first:])
	}
	newCur := cur + n
	binary.LittleEndian.PutUint32(m.mem[ringTailOffset:ringTailOffset+ringControlWord], newCur)
	return newCur
}

// Unmap releases the mapping.
func (m *MappedRing) Unmap() error { return syscall.Munmap(m.mem) }

// RoundTripResult captures the observed host response of a Venus ring attempt.
type RoundTripResult struct {
	NegotiatedVersion uint32
	MaxTimelineCount  uint32
	CapsetLen         int
	BlobResID         uint32
	RingTailAfter     uint32
	HeadStart         uint32
	HeadAfter         uint32
	StatusAfter       uint32
	HeadAdvanced      bool
}

// VenusRingRoundTrip performs the full Venus bring-up over the vtest socket and
// attempts the genuine round-trip: it creates the ring's backing HOST3D blob,
// mmaps it, registers the ring (vkCreateRingMESA via SUBMIT_CMD2 ring_idx 0),
// writes a vkCreateInstance command stream into the ring buffer, stores the new
// tail, then busy-polls the HOST-owned head word for up to timeout. A head
// advance past 0 is the host actually consuming our ring bytes — the real
// "host responded" proof.
//
// bufferSize must be a power of two. The returned result records every
// host-observable value (negotiated version, timeline count, capset length,
// blob res_id, and the head/status the host wrote) so the caller can report the
// exact outcome or the precise wall.
func VenusRingRoundTrip(socketPath string, bufferSize, extraSize int, timeout time.Duration) (*RoundTripResult, error) {
	rc, err := DialRaw(socketPath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	c := New(rc)
	res := &RoundTripResult{}

	// 1. handshake -> protocol version >= 3
	neg, err := c.HandshakeVenus("go-virtio-venus")
	if err != nil {
		return res, fmt.Errorf("handshake: %w", err)
	}
	res.NegotiatedVersion = neg
	if neg < VtestProtocolVersionVenus {
		return res, fmt.Errorf("server negotiated protocol v%d < 3 (built without VIRGL_RENDERER_UNSTABLE_APIS): Venus opcodes unreachable", neg)
	}

	// 2. GET_PARAM(MAX_TIMELINE_COUNT) must be non-zero
	tl, err := c.GetParam(VcmdParamMaxTimelineCount)
	if err != nil {
		return res, fmt.Errorf("get_param(max_timeline_count): %w", err)
	}
	res.MaxTimelineCount = tl
	if tl == 0 {
		return res, fmt.Errorf("max_timeline_count=0: server has no timeline support (VIRGL_DISABLE_MT set or no eventfd) — Venus needs it")
	}

	// 3. GET_CAPSET(VENUS, 0) must be valid
	capset, valid, err := c.GetCapset(CapsetVenus, 0)
	if err != nil {
		return res, fmt.Errorf("get_capset(venus): %w", err)
	}
	if !valid {
		return res, fmt.Errorf("get_capset(venus) invalid: renderer has no Venus capset (Vulkan backend not enabled/lavapipe missing)")
	}
	res.CapsetLen = len(capset)

	// 4. CONTEXT_INIT(VENUS) — server creates the host vkr context NOW
	// (vtest_context_init -> vtest_lazy_init_context ->
	// virgl_renderer_context_create_with_flags). A failure here closes the
	// connection with no reply; the next read will see EOF.
	if err := c.ContextInit(CapsetVenus); err != nil {
		return res, fmt.Errorf("context_init(venus) write: %w", err)
	}

	// 5. RESOURCE_CREATE_BLOB(HOST3D, MAPPABLE, shmem_size) -> res_id + fd
	shmemSize := RingShmemSize(bufferSize, extraSize)
	resID, fd, err := rc.ResourceCreateBlob(BlobTypeHost3D, BlobFlagMappable, uint64(shmemSize), 0)
	if err != nil {
		return res, fmt.Errorf("resource_create_blob (ring shmem): %w", err)
	}
	res.BlobResID = resID

	mem, err := syscall.Mmap(fd, 0, shmemSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	syscall.Close(fd)
	if err != nil {
		return res, fmt.Errorf("mmap ring shmem (res_id=%d, %d bytes): %w", resID, shmemSize, err)
	}
	ring := &MappedRing{ResID: resID, mem: mem, bufferSize: bufferSize}
	defer ring.Unmap()
	// zero the shmem (Mesa memsets it before use)
	for i := range mem {
		mem[i] = 0
	}

	// 6. Register the ring: SUBMIT_CMD2(ring_idx=0) carrying vkCreateRingMESA.
	// idleTimeout MUST be non-zero: the host consumer (virglrenderer
	// vkr_ring.c:vkr_ring_thread) busy-polls the tail for idle_timeout
	// nanoseconds after the last submit, and only then goes idle and cond-waits
	// for a vkNotifyRingMESA wakeup. With idleTimeout=0 it idles on the very
	// first loop iteration and would never see our write without a notify.
	// We pick 1s so the consumer is still polling when we store the tail below.
	const ringIdleTimeoutNS = uint64(1_000_000_000)
	ringID := uint64(0x1000) // arbitrary non-zero ring handle
	info := ringCreateInfoForBlob(resID, bufferSize, extraSize, ringIdleTimeoutNS)
	createRingCS := EncodeVkCreateRingMESACS(ringID, info)
	if err := c.SubmitCmd2(SubmitCmd2Batch{
		Flags:   SubmitCmd2FlagRingIdx, // Mesa always sets RING_IDX
		RingIdx: 0,
		CmdData: createRingCS,
	}); err != nil {
		return res, fmt.Errorf("submit_cmd2(vkCreateRingMESA): %w", err)
	}

	// 7. Write a vkCreateInstance command stream INTO the ring buffer, store the
	// new tail, and busy-poll the host-owned head word.
	//
	// pInstance carries a client-allocated Venus object id (vn_object_id): the
	// host renderer registers the new VkInstance under this id
	// (vkr_context_alloc_object(ctx, ..., args->pInstance) in
	// virglrenderer vkr_instance.c). A zero/absent id makes the decoder flag a
	// CS error, so we pass a non-zero id. apiVersion 0 is fine — the renderer
	// patches it up to VK_API_VERSION_1_1.
	res.HeadStart = ring.Head()
	const instanceObjectID = 0x1 // client-allocated VkInstance vn_object_id
	instanceCS := EncodeMinimalVkCreateInstanceCS(0, "", "", 0, instanceObjectID)
	// pad to dword (ring writes are byte-granular but commands are dword sized)
	for len(instanceCS)%4 != 0 {
		instanceCS = append(instanceCS, 0)
	}
	newTail := ring.writeCmd(0, instanceCS)
	res.RingTailAfter = newTail

	deadline := time.Now().Add(timeout)
	for {
		h := ring.Head()
		if h != res.HeadStart {
			res.HeadAfter = h
			res.StatusAfter = ring.Status()
			res.HeadAdvanced = true
			return res, nil
		}
		if time.Now().After(deadline) {
			res.HeadAfter = h
			res.StatusAfter = ring.Status()
			return res, nil
		}
		time.Sleep(2 * time.Millisecond)
	}
}
