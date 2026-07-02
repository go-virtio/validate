<p align="center"><img src="https://raw.githubusercontent.com/go-virtio/brand/main/social/go-virtio.png" alt="go-virtio/validate" width="720"></p>

# go-virtio/validate

[![Go Reference](https://pkg.go.dev/badge/github.com/go-virtio/validate.svg)](https://pkg.go.dev/github.com/go-virtio/validate)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-virtio/validate/actions/workflows/ci.yml/badge.svg)](https://github.com/go-virtio/validate/actions/workflows/ci.yml)

A **real-hardware validation harness** for the [`go-virtio`](https://github.com/go-virtio)
guest drivers: it boots a bare-metal [tamago](https://github.com/usbarmory/tamago)
guest under QEMU, drives a real virtio-gpu device with the pure-Go go-virtio
drivers, and asserts the result — beyond unit tests with a fake device.

## What it proves (validated, end-to-end)

**`go-virtio/gpu` 2D framebuffer + `go-virtio/gpu/soft3d`**: a tamago guest
enumerates PCI, binds a `common.Transport` to a QEMU `virtio-gpu-pci` device,
runs `OpenVirtioGPU → DisplayInfo → SetupFramebuffer → soft3d.RenderCube →
Flush`, and the host `screendump` shows a shaded 3D cube. Run `./run.sh`:

```
VALIDATE: GPU=0x1af4:0x1050 scanouts=1
VALIDATE: fb 320x240 resource=1 pixbytes=307200
VALIDATE: checksum=0xdcd3247a nonzero_pixels=18168 distinct_colors=5
RESULT: PASS (non-uniform frame consistent with a rendered cube)
```

The 2D virtio-gpu path needs **no virglrenderer** — QEMU's stock device model
serves it. So this validates the 2D driver + the CPU software 3D rasterizer on
a real device model. (This is literally the weft microVM guest stack.)

Two real bugs were found here that a fake-device unit test cannot catch:
- the PCI config cap-walk needs dword-granular byte extraction (a real device
  reads config in 32-bit words; the fake returns bytes directly);
- `screendump` must target the virtio-gpu, not q35's default VGA.

## Layout / arch notes

- **x86_64 under TCG.** tamago has no arm64 QEMU-`virt` board; HVF only
  accelerates arm64 guests, so x86_64 tamago runs under `qemu-system-x86_64`
  TCG emulation (slow but correct for a correctness check).
- **`board/board.go`** — a local q35/amd64 board (modeled on tamago's
  `cloud_hypervisor/vm`) that, crucially, **masks the legacy 8259 PIC**
  (`outb(0x21,0xff)`) after switching IRQ routing to the I/O APIC. Without
  this, q35's legacy 8254 PIT ticks IRQ0 onto IDT vector 8 (the Double-Fault
  slot) and the guest reports a spurious `exception: vector 8` after a fixed
  wall-clock budget. (Upstream note: tamago's own `cloud_hypervisor/vm` board
  has the same missing mask and would fault identically on q35; its
  `board/firecracker/microvm` correctly does `reg.Out8(0x21, 0xff)`.)
- **`transport.go`** — `tamagoTransport` implementing `common.Transport`:
  port-mapped PCI config (0xcf8/0xcfc), BAR-window MMIO, `dma.Reserve`-backed
  page allocation.

## vtest/ — virgl (3D) validation against a real virglrenderer

`vtest/` is a pure-Go (CGO=0) client for virglrenderer's **vtest** protocol
(`virgl_test_server` over a Unix socket — Mesa's CI method, software-rendered
with llvmpipe, **no GPU**). `vtest/cmd/validator` feeds go-virtio/gpu's actual
virgl command buffers (the bytes `buildClearVirglBuffer` / `buildDrawVirglBuffer`
emit) to a real `virgl_test_server`, reads the framebuffer back, and asserts the
pixels. The protocol client is 100%-unit-tested offline.

**Validated, end-to-end, against a live virglrenderer:**

- **CLEAR** — go-virtio/gpu's clear-to-red command stream is accepted and the
  16×16 render target reads back uniformly red (`BGRA 00 00 FF FF`).
- **DrawTriangle** — accepted and rasterizes: a non-uniform readback (corners
  the background, centre a triangle fragment), with no renderer error.
- **DrawTexturedTriangle** — accepted; a 2×2 red/green/blue/white texture is
  sampled and perspective-correctly interpolated across the triangle (24
  distinct interior colours in the readback), no renderer error.

This is where three real go-virtio/gpu bugs were caught that the offline path
could not:

- `VIRGL_OBJECT_SURFACE` was 7 (must be 8) — gpu v0.3.1.
- `VIRGL_CCMD_BIND_SHADER` was 32 (= `SET_TESS_STATE`; must be 31) — the live
  renderer rejected the draw with *"Illegal command buffer"*, dispatching
  command 32 as `SET_TESS_STATE` — gpu v0.5.1.
- the textured fragment shader's texcoord input defaulted to `CONSTANT` (flat)
  interpolation — the triangle sampled a single texel until the TGSI input was
  declared `PERSPECTIVE` — gpu v0.5.3.

How it was run (the host setup that works on an Apple-Silicon Mac with no GPU
passthrough): a **Debian arm64 cloud image** under `qemu-system-aarch64 -accel
hvf`, `apt-get install virgl-server` (the package that ships `virgl_test_server`)
straight onto the disk, started with `EGL_PLATFORM=surfaceless virgl_test_server
--use-egl-surfaceless` + `LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe`. Guest
networking needs `nameserver 10.0.2.3` (SLIRP host-side resolver) and
`Acquire::ForceIPv4=true` (SLIRP has no IPv6 route). Any Linux host running
`virgl_test_server` works just as well.

## Running

Needs the tamago Go compiler (`GOOS=tamago` support). `run.sh` builds the
guest, boots it headless under QEMU, waits for the serial `DONE`, captures a
host `screendump`, and asserts the PPM is a non-uniform rendered frame.

## License

BSD-3-Clause.
