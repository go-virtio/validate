//go:build linux

package vtest

// Linux-only tests for the fd-passing transport and ring-memory helpers in
// venusring_linux.go. The fd-receive + mmap + full round-trip path needs a
// live virgl_test_server (exercised on the Debian VM, not in unit tests); here
// we cover the deterministic, server-independent pieces: RawConn's read/write
// loops over a socketpair, recvFD over a socketpair with a real SCM_RIGHTS
// message, the MappedRing control-word accessors, and writeCmd's wrap handling.

import (
	"bytes"
	"encoding/binary"
	"os"
	"syscall"
	"testing"
	"time"
)

// rawPair returns a connected RawConn plus the peer fd (an AF_UNIX socketpair),
// so tests can script server-side bytes/fds without a real server.
func rawPair(t *testing.T) (*RawConn, int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	rc := &RawConn{fd: fds[0]}
	t.Cleanup(func() { rc.Close(); syscall.Close(fds[1]) })
	return rc, fds[1]
}

func TestRawConnWriteRead(t *testing.T) {
	rc, peer := rawPair(t)
	msg := []byte("hello vtest")
	if n, err := rc.Write(msg); err != nil || n != len(msg) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	buf := make([]byte, len(msg))
	got := 0
	for got < len(buf) {
		n, err := syscall.Read(peer, buf[got:])
		if err != nil {
			t.Fatalf("peer read: %v", err)
		}
		got += n
	}
	if !bytes.Equal(buf, msg) {
		t.Fatalf("peer got %q, want %q", buf, msg)
	}
}

func TestRawConnReadFull(t *testing.T) {
	rc, peer := rawPair(t)
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if _, err := syscall.Write(peer, want); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	got := make([]byte, len(want))
	if err := rc.readFull(got); err != nil {
		t.Fatalf("readFull: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("readFull got %v want %v", got, want)
	}
}

func TestRawConnReadEOF(t *testing.T) {
	rc, peer := rawPair(t)
	syscall.Close(peer) // closing the peer makes our reads return EOF (n==0)
	buf := make([]byte, 4)
	if _, err := rc.Read(buf); err == nil {
		t.Fatal("want EOF error after peer close")
	}
}

func TestRawConnReadFullError(t *testing.T) {
	rc, peer := rawPair(t)
	// Only 2 bytes available then EOF → readFull of 4 must error.
	syscall.Write(peer, []byte{9, 9})
	syscall.Close(peer)
	if err := rc.readFull(make([]byte, 4)); err == nil {
		t.Fatal("want readFull error on short stream")
	}
}

func TestRawConnDialError(t *testing.T) {
	if _, err := DialRaw("/nonexistent/vtest.sock"); err == nil {
		t.Fatal("want DialRaw connect error")
	}
}

// TestRecvFD sends a real fd over SCM_RIGHTS from the peer and confirms recvFD
// receives a usable duplicate (same file contents).
func TestRecvFD(t *testing.T) {
	rc, peer := rawPair(t)

	// Make a temp file with known contents to pass by fd.
	f, err := os.CreateTemp(t.TempDir(), "scm")
	if err != nil {
		t.Fatalf("tempfile: %v", err)
	}
	const payload = "scm-rights-payload"
	if _, err := f.WriteString(payload); err != nil {
		t.Fatalf("write tmp: %v", err)
	}

	// Peer sends one dummy byte + the fd as SCM_RIGHTS (mirrors vtest_send_fd).
	rights := syscall.UnixRights(int(f.Fd()))
	if err := syscall.Sendmsg(peer, []byte{0}, rights, nil, 0); err != nil {
		t.Fatalf("sendmsg: %v", err)
	}

	gotFD, err := rc.recvFD()
	if err != nil {
		t.Fatalf("recvFD: %v", err)
	}
	defer syscall.Close(gotFD)

	// Read the received fd from offset 0 and compare.
	buf := make([]byte, len(payload))
	if _, err := syscall.Pread(gotFD, buf, 0); err != nil {
		t.Fatalf("pread received fd: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("received fd content %q, want %q", buf, payload)
	}
}

func TestRecvFDNoCmsg(t *testing.T) {
	rc, peer := rawPair(t)
	// Send a plain byte with no control message → recvFD must error (no SCM).
	syscall.Write(peer, []byte{0})
	if _, err := rc.recvFD(); err == nil {
		t.Fatal("want recvFD error when no SCM_RIGHTS present")
	}
}

func TestRecvFDClosedPeer(t *testing.T) {
	rc, peer := rawPair(t)
	syscall.Close(peer)
	if _, err := rc.recvFD(); err == nil {
		t.Fatal("want recvFD error on closed peer")
	}
}

// newTestRing builds a MappedRing backed by a plain byte slice (no mmap needed
// for the pure-logic accessors).
func newTestRing(bufferSize, extraSize int) *MappedRing {
	return &MappedRing{
		ResID:      1,
		mem:        make([]byte, RingShmemSize(bufferSize, extraSize)),
		bufferSize: bufferSize,
	}
}

func TestMappedRingControlWords(t *testing.T) {
	r := newTestRing(64, 0)
	binary.LittleEndian.PutUint32(r.mem[ringHeadOffset:], 0x11)
	binary.LittleEndian.PutUint32(r.mem[ringTailOffset:], 0x22)
	binary.LittleEndian.PutUint32(r.mem[ringStatusOffset:], 0x33)
	if r.Head() != 0x11 || r.Tail() != 0x22 || r.Status() != 0x33 {
		t.Fatalf("control words: head=%#x tail=%#x status=%#x", r.Head(), r.Tail(), r.Status())
	}
}

func TestMappedRingWriteCmdContiguous(t *testing.T) {
	r := newTestRing(64, 0)
	cmd := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	newCur := r.writeCmd(0, cmd)
	if newCur != 4 {
		t.Fatalf("newCur = %d, want 4", newCur)
	}
	if r.Tail() != 4 {
		t.Fatalf("tail = %d, want 4", r.Tail())
	}
	if !bytes.Equal(r.mem[ringBufferOffset:ringBufferOffset+4], cmd) {
		t.Fatalf("buffer = %v, want %v", r.mem[ringBufferOffset:ringBufferOffset+4], cmd)
	}
}

func TestMappedRingWriteCmdWrap(t *testing.T) {
	// bufferSize 8, start cursor near the end forces a wrap split.
	r := newTestRing(8, 0)
	cmd := []byte{1, 2, 3, 4, 5, 6}
	cur := uint32(6) // start at offset 6, 6 bytes → 2 bytes to end, 4 wrapped
	newCur := r.writeCmd(cur, cmd)
	if newCur != 12 {
		t.Fatalf("newCur = %d, want 12", newCur)
	}
	base := ringBufferOffset
	// first chunk (2 bytes) at offset 6,7
	if r.mem[base+6] != 1 || r.mem[base+7] != 2 {
		t.Fatalf("wrap tail bytes = %v %v", r.mem[base+6], r.mem[base+7])
	}
	// remainder (4 bytes) at offset 0..3
	if !bytes.Equal(r.mem[base:base+4], []byte{3, 4, 5, 6}) {
		t.Fatalf("wrap head bytes = %v", r.mem[base:base+4])
	}
	if r.Tail() != 12 {
		t.Fatalf("tail = %d, want 12", r.Tail())
	}
}

// TestVenusRoundTripNoServer confirms VenusRingRoundTrip fails cleanly (no
// panic) when no server is listening — the dial error path.
func TestVenusRoundTripDialError(t *testing.T) {
	_, err := VenusRingRoundTrip("/nonexistent/vtest.sock", 0x1000, 0, 10*time.Millisecond)
	if err == nil {
		t.Fatal("want dial error from VenusRingRoundTrip")
	}
}
