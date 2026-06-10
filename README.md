# go-virtio/validate

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

## vtest/ — virgl (3D) validation tooling (ready, not yet run end-to-end)

`vtest/` is a pure-Go (CGO=0) client for virglrenderer's **vtest** protocol
(`virgl_test_server` over a Unix socket — Mesa's CI method, software-rendered
with llvmpipe, **no GPU**). `vtest/cmd/validator` feeds go-virtio/gpu's actual
virgl command buffers (the bytes from `buildClearVirglBuffer` etc.) to a real
`virgl_test_server`, reads the framebuffer back, and asserts the pixels.

The vtest protocol client is 100%-unit-tested offline. The live half is **not
yet validated** because it needs a Linux host running `virgl_test_server`, and
the QEMU/HVF setup used here could not (a) resolve DNS in the guest (SLIRP) to
`apk add virglrenderer`, nor (b) move the ~79 MB virglrenderer+mesa+llvm
payload into the guest (large 9p/virtio-blk reads hang). It runs cleanly on any
host with working guest networking, or boot a prebuilt Alpine rootfs that
already contains `virglrenderer` (Alpine packages it: `virglrenderer-1.1.0`).

## Running

Needs the tamago Go compiler (`GOOS=tamago` support). `run.sh` builds the
guest, boots it headless under QEMU, waits for the serial `DONE`, captures a
host `screendump`, and asserts the PPM is a non-uniform rendered frame.

## License

BSD-3-Clause.
