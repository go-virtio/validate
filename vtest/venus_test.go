package vtest

// Tests for the Venus protocol-version-3 framing in venus.go. Wire layouts are
// quoted from virglrenderer vtest/vtest_protocol.h + vtest_renderer.c and Mesa
// src/virtio/vulkan/vn_renderer_vtest.c at each assertion. Live-method tests
// drive the Client against the in-memory fakeServer / error helpers defined in
// protocol_test.go.

import (
	"bytes"
	"testing"
)

func TestHandshakeVenus(t *testing.T) {
	// HandshakeVenus negotiates v3: ping reply [0,10] then version reply
	// [1,11]+[3]. Verifies the request is the create/ping/protocol-version
	// triple asking for v3.
	reply := concat(
		le32(0), le32(VcmdPingProtocolVersion),
		le32(1), le32(VcmdProtocolVersion), le32(3),
	)
	f := newFake(reply)
	c := New(f)
	neg, err := c.HandshakeVenus("go-virtio-venus")
	if err != nil {
		t.Fatalf("handshake venus: %v", err)
	}
	if neg != 3 {
		t.Fatalf("negotiated = %d, want 3", neg)
	}
	want := concat(
		encodeCreateRenderer("go-virtio-venus"),
		encodePingProtocolVersion(),
		encodeProtocolVersion(VtestProtocolVersionVenus),
	)
	if !bytes.Equal(f.sent.Bytes(), want) {
		t.Fatalf("handshake venus bytes\n got=%v\nwant=%v", f.sent.Bytes(), want)
	}
}

func TestEncodeGetParam(t *testing.T) {
	// VCMD_GET_PARAM_SIZE 1; header [1,15] + one dword param.
	got := encodeGetParam(VcmdParamMaxTimelineCount)
	want := concat(le32(vcmdGetParamSize), le32(VcmdGetParam), le32(VcmdParamMaxTimelineCount))
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeGetParam\n got=%v\nwant=%v", got, want)
	}
}

func TestGetParam(t *testing.T) {
	// Reply: header [2,15] + {valid=1, value=4} (VTEST_MAX_TIMELINE_COUNT).
	reply := concat(le32(2), le32(VcmdGetParam), le32(1), le32(4))
	c := New(newFake(reply))
	v, err := c.GetParam(VcmdParamMaxTimelineCount)
	if err != nil {
		t.Fatalf("get_param: %v", err)
	}
	if v != 4 {
		t.Fatalf("get_param value = %d, want 4", v)
	}
}

func TestGetParamInvalid(t *testing.T) {
	// valid=0 → returns 0 regardless of value (mirrors resp[0]?resp[1]:0).
	reply := concat(le32(2), le32(VcmdGetParam), le32(0), le32(99))
	c := New(newFake(reply))
	v, err := c.GetParam(VcmdParamMaxTimelineCount)
	if err != nil {
		t.Fatalf("get_param: %v", err)
	}
	if v != 0 {
		t.Fatalf("get_param invalid value = %d, want 0", v)
	}
}

func TestGetParamWriteError(t *testing.T) {
	if _, err := New(&errRW{failWrite: true}).GetParam(1); err == nil {
		t.Fatal("want get_param write error")
	}
}

func TestGetParamReplyHeaderError(t *testing.T) {
	// no reply bytes → header read fails.
	if _, err := New(newFake(nil)).GetParam(1); err == nil {
		t.Fatal("want get_param reply header error")
	}
}

func TestGetParamReplyMismatch(t *testing.T) {
	// wrong cmd id in reply header.
	reply := concat(le32(2), le32(VcmdGetCapset), le32(1), le32(4))
	if _, err := New(newFake(reply)).GetParam(1); err == nil {
		t.Fatal("want get_param reply mismatch error")
	}
}

func TestGetParamReplyBodyError(t *testing.T) {
	// header ok but body dwords missing.
	reply := concat(le32(2), le32(VcmdGetParam)) // header only, no {valid,value}
	if _, err := New(newFake(reply)).GetParam(1); err == nil {
		t.Fatal("want get_param reply body error")
	}
}

func TestEncodeGetCapset(t *testing.T) {
	// VCMD_GET_CAPSET_SIZE 2; header [2,16] + {id, version}.
	got := encodeGetCapset(CapsetVenus, 0)
	want := concat(le32(vcmdGetCapsetSize), le32(VcmdGetCapset), le32(CapsetVenus), le32(0))
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeGetCapset\n got=%v\nwant=%v", got, want)
	}
}

func TestGetCapset(t *testing.T) {
	// Reply: header [1+caps/4, 16] + {valid=1} + caps bytes. 8 bytes of caps →
	// length = 1 + 2 = 3 dwords.
	caps := concat(le32(0xDEADBEEF), le32(0x00000001))
	reply := concat(le32(3), le32(VcmdGetCapset), le32(1), caps)
	c := New(newFake(reply))
	got, valid, err := c.GetCapset(CapsetVenus, 0)
	if err != nil {
		t.Fatalf("get_capset: %v", err)
	}
	if !valid {
		t.Fatal("expected valid capset")
	}
	if !bytes.Equal(got, caps) {
		t.Fatalf("capset body\n got=%v\nwant=%v", got, caps)
	}
}

func TestGetCapsetInvalid(t *testing.T) {
	// valid=0 → no body, returns (nil, false).
	reply := concat(le32(1), le32(VcmdGetCapset), le32(0))
	c := New(newFake(reply))
	got, valid, err := c.GetCapset(CapsetVenus, 0)
	if err != nil {
		t.Fatalf("get_capset: %v", err)
	}
	if valid || got != nil {
		t.Fatalf("expected invalid/nil, got valid=%v body=%v", valid, got)
	}
}

func TestGetCapsetWriteError(t *testing.T) {
	if _, _, err := New(&errRW{failWrite: true}).GetCapset(CapsetVenus, 0); err == nil {
		t.Fatal("want get_capset write error")
	}
}

func TestGetCapsetReplyHeaderError(t *testing.T) {
	if _, _, err := New(newFake(nil)).GetCapset(CapsetVenus, 0); err == nil {
		t.Fatal("want get_capset reply header error")
	}
}

func TestGetCapsetReplyCmdMismatch(t *testing.T) {
	reply := concat(le32(1), le32(VcmdGetParam), le32(1))
	if _, _, err := New(newFake(reply)).GetCapset(CapsetVenus, 0); err == nil {
		t.Fatal("want get_capset cmd mismatch error")
	}
}

func TestGetCapsetValidReadError(t *testing.T) {
	// header ok but the {valid} dword is missing.
	reply := concat(le32(3), le32(VcmdGetCapset))
	if _, _, err := New(newFake(reply)).GetCapset(CapsetVenus, 0); err == nil {
		t.Fatal("want get_capset valid read error")
	}
}

func TestGetCapsetBodyError(t *testing.T) {
	// valid=1, length says 2 dwords of caps follow, but body is truncated.
	reply := concat(le32(3), le32(VcmdGetCapset), le32(1), le32(0xAA)) // only 4 of 8 caps bytes
	if _, _, err := New(newFake(reply)).GetCapset(CapsetVenus, 0); err == nil {
		t.Fatal("want get_capset body read error")
	}
}

func TestEncodeContextInit(t *testing.T) {
	// VCMD_CONTEXT_INIT_SIZE 1; header [1,17] + capset_id.
	got := encodeContextInit(CapsetVenus)
	want := concat(le32(vcmdContextInitSize), le32(VcmdContextInit), le32(CapsetVenus))
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeContextInit\n got=%v\nwant=%v", got, want)
	}
}

func TestContextInit(t *testing.T) {
	f := newFake(nil)
	c := New(f)
	if err := c.ContextInit(CapsetVenus); err != nil {
		t.Fatalf("context_init: %v", err)
	}
	if !bytes.Equal(f.sent.Bytes(), encodeContextInit(CapsetVenus)) {
		t.Fatal("context_init bytes mismatch")
	}
}

func TestContextInitWriteError(t *testing.T) {
	if err := New(&errRW{failWrite: true}).ContextInit(CapsetVenus); err == nil {
		t.Fatal("want context_init write error")
	}
}

func TestEncodeResourceCreateBlob(t *testing.T) {
	// VCMD_RES_CREATE_BLOB_SIZE 6; header [6,18] + {type,flags,size_lo,size_hi,
	// blob_id_lo,blob_id_hi}.
	got := encodeResourceCreateBlob(BlobTypeHost3D, BlobFlagMappable, 0x1_0000_2000, 0xAB)
	want := concat(
		le32(vcmdResCreateBlobSize), le32(VcmdResourceCreateBlob),
		le32(BlobTypeHost3D), le32(BlobFlagMappable),
		le32(0x0000_2000), le32(0x1), // size lo/hi
		le32(0xAB), le32(0x0), // blob_id lo/hi
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeResourceCreateBlob\n got=%v\nwant=%v", got, want)
	}
}

func TestEncodeSubmitCmd2(t *testing.T) {
	// One batch carrying an 8-byte (2-dword) cmd stream, no syncs.
	// Layout (vtest_protocol.h + Mesa vtest_vcmd_submit_cmd2):
	//   header[len, 24]
	//   batch_count = 1
	//   batch (8 dwords): flags, cmd_offset, cmd_size, sync_offset, sync_count,
	//                     ring_idx, num_in_syncobj, num_out_syncobj
	//   cmd stream
	// With one batch: header occupies 1+8 = 9 dwords → cmd_offset = 9.
	// cmd_size = 2 dwords. sync_offset = 9+2 = 11. total = 9 + 2 = 11 dwords.
	cmd := concat(le32(0x11111111), le32(0x22222222))
	got, err := encodeSubmitCmd2(SubmitCmd2Batch{
		Flags:   SubmitCmd2FlagRingIdx,
		RingIdx: 0,
		CmdData: cmd,
	})
	if err != nil {
		t.Fatalf("encodeSubmitCmd2: %v", err)
	}
	want := concat(
		le32(11), le32(VcmdSubmitCmd2), // header: 11 dwords payload, cmd 24
		le32(1),                     // batch_count
		le32(SubmitCmd2FlagRingIdx), // flags
		le32(9),                     // cmd_offset (dwords)
		le32(2),                     // cmd_size (dwords)
		le32(11),                    // sync_offset (dwords)
		le32(0),                     // sync_count
		le32(0),                     // ring_idx
		le32(0),                     // num_in_syncobj
		le32(0),                     // num_out_syncobj
		cmd,
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeSubmitCmd2\n got=%v\nwant=%v", got, want)
	}
}

func TestEncodeSubmitCmd2WithSyncs(t *testing.T) {
	// One batch + one sync triple (id, val_lo, val_hi). cmd 4 bytes (1 dword).
	// header 9 dwords, cmd 1 dword → sync_offset 10, sync 3 dwords, total 13.
	cmd := le32(0xABCDABCD)
	got, err := encodeSubmitCmd2(SubmitCmd2Batch{
		Flags:   SubmitCmd2FlagRingIdx,
		RingIdx: 0,
		CmdData: cmd,
		Syncs:   []SubmitCmd2Sync{{ID: 7, Value: 0x1_0000_0002}},
	})
	if err != nil {
		t.Fatalf("encodeSubmitCmd2: %v", err)
	}
	want := concat(
		le32(13), le32(VcmdSubmitCmd2),
		le32(1),                     // batch_count
		le32(SubmitCmd2FlagRingIdx), // flags
		le32(9),                     // cmd_offset
		le32(1),                     // cmd_size
		le32(10),                    // sync_offset
		le32(1),                     // sync_count
		le32(0),                     // ring_idx
		le32(0), le32(0),            // num_in/out_syncobj
		cmd,
		le32(7), le32(2), le32(1), // sync (id, lo, hi) for value 0x1_0000_0002
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeSubmitCmd2WithSyncs\n got=%v\nwant=%v", got, want)
	}
}

func TestEncodeSubmitCmd2Misaligned(t *testing.T) {
	if _, err := encodeSubmitCmd2(SubmitCmd2Batch{CmdData: []byte{1, 2, 3}}); err == nil {
		t.Fatal("want misaligned cmd data error")
	}
}

func TestSubmitCmd2Method(t *testing.T) {
	f := newFake(nil)
	c := New(f)
	batch := SubmitCmd2Batch{Flags: SubmitCmd2FlagRingIdx, CmdData: le32(0xCAFE0000)}
	if err := c.SubmitCmd2(batch); err != nil {
		t.Fatalf("submit_cmd2: %v", err)
	}
	pkt, _ := encodeSubmitCmd2(batch)
	if !bytes.Equal(f.sent.Bytes(), pkt) {
		t.Fatal("submit_cmd2 bytes mismatch")
	}
}

func TestSubmitCmd2EncodeError(t *testing.T) {
	// misaligned data surfaces through the method.
	if err := New(newFake(nil)).SubmitCmd2(SubmitCmd2Batch{CmdData: []byte{1}}); err == nil {
		t.Fatal("want submit_cmd2 encode error")
	}
}

func TestSubmitCmd2WriteError(t *testing.T) {
	if err := New(&errRW{failWrite: true}).SubmitCmd2(SubmitCmd2Batch{CmdData: le32(1)}); err == nil {
		t.Fatal("want submit_cmd2 write error")
	}
}

func TestReadDwordsError(t *testing.T) {
	// Fewer bytes than requested → io.ReadFull error surfaces.
	r := bytes.NewReader([]byte{1, 2, 3}) // 3 < 4 bytes for one dword
	out := make([]uint32, 1)
	if err := readDwords(r, out); err == nil {
		t.Fatal("want readDwords short-read error")
	}
}

func TestRingShmemSizeAndCreateInfo(t *testing.T) {
	// RingShmemSize = buffer_offset(192) + bufferSize + extraSize.
	if got := RingShmemSize(0x40000, 0); got != 192+0x40000 {
		t.Fatalf("RingShmemSize = %d, want %d", got, 192+0x40000)
	}
	if got := RingShmemSize(1024, 256); got != 192+1024+256 {
		t.Fatalf("RingShmemSize(extra) = %d, want %d", got, 192+1024+256)
	}
	// ringCreateInfoForBlob fills the vn_ring.c fixed offsets.
	info := ringCreateInfoForBlob(9, 0x40000, 0, 123)
	if info.ResourceID != 9 || info.HeadOffset != 0 || info.TailOffset != 64 ||
		info.StatusOffset != 128 || info.BufferOffset != 192 ||
		info.BufferSize != 0x40000 || info.ExtraOffset != 192+0x40000 ||
		info.ExtraSize != 0 || info.IdleTimeout != 123 ||
		info.Size != uint64(RingShmemSize(0x40000, 0)) {
		t.Fatalf("ringCreateInfoForBlob unexpected: %+v", info)
	}
}
