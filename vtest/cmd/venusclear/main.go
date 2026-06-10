//go:build linux

// Command venusclear drives the FULL Venus clear-image command sequence
// against a live `virgl_test_server --venus` (backed by lavapipe) over the
// vtest socket, then reports the host's acceptance of every command.
//
// It submits, in order, through the proven ring transport:
//
//	vkCreateInstance -> vkEnumeratePhysicalDevices -> GetPhysicalDeviceMemoryProperties
//	-> GetPhysicalDeviceQueueFamilyProperties -> vkCreateDevice -> vkGetDeviceQueue
//	-> vkCreateImage(LINEAR) -> vkGetImageMemoryRequirements -> vkAllocateMemory
//	-> vkBindImageMemory -> vkCreateCommandPool -> vkAllocateCommandBuffers
//	-> vkBeginCommandBuffer -> vkCmdPipelineBarrier(UNDEFINED->GENERAL)
//	-> vkCmdClearColorImage(red) -> vkEndCommandBuffer -> vkQueueSubmit
//	-> vkQueueWaitIdle
//
// Each reply-bearing command uses the Mesa reply-shmem path (a dedicated
// reply blob + vkSetReplyCommandStreamMESA + GENERATE_REPLY bit), so the
// returned handles and VkResults are decoded for real. The host head advance
// per command is the proof the renderer consumed each batch.
//
// GUEST-VISIBLE READBACK (the last Venus frontier — now closed). After the
// clear completes, the driver creates the renderer BO that backs the
// VkDeviceMemory (vn_device_memory_bo_init: RESOURCE_CREATE_BLOB HOST3D|MAPPABLE
// with blob_id = the VkDeviceMemory's vn_object_id), mmaps the exported blob fd
// guest-side (vtest_bo_map = plain mmap; vkMapMemory is no host round-trip for
// the bytes, vn_UnmapMemory2 is a no-op), and reads the cleared texels. This is
// the vtest path — NOT the guest_vram VkImportMemoryResourceInfoMESA pNext path,
// which is gated on dev->renderer->info.has_guest_vram that vtest never sets.
// The host backs the device memory with that blob via vkr
// vkr_context_create_resource_from_device_memory (blob_id -> existing
// VkDeviceMemory), so the mapped bytes ARE the image's storage. The verdict now
// requires the read-back texels to equal the clear colour (red).
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-virtio/validate/vtest"
)

func main() {
	var (
		sock    = flag.String("sock", vtest.DefaultSocketName, "vtest server socket")
		bufSize = flag.Int("buf", 1<<20, "ring command-buffer size (power of two)")
		width   = flag.Int("w", 16, "clear image width")
		height  = flag.Int("h", 16, "clear image height")
		timeout = flag.Duration("timeout", 5*time.Second, "per-command head-advance poll timeout")
	)
	flag.Parse()

	// Red, opaque. R8G8B8A8_UNORM float clear -> (1,0,0,1).
	clear := [4]float32{1.0, 0.0, 0.0, 1.0}

	fmt.Printf("VENUS clear-image: sock=%s buf=%d image=%dx%d clear=%v timeout=%s\n",
		*sock, *bufSize, *width, *height, clear, *timeout)

	res, err := vtest.VenusClearImage(*sock, *bufSize, *width, *height, clear, *timeout)
	if res != nil {
		fmt.Printf("  negotiated protocol version = %d\n", res.NegotiatedVersion)
		fmt.Printf("  max_timeline_count          = %d\n", res.MaxTimelineCount)
		fmt.Printf("  venus capset length         = %d bytes\n", res.CapsetLen)
		fmt.Printf("  ring blob res_id            = %d\n", res.RingBlobResID)
		fmt.Printf("  reply blob res_id           = %d\n", res.ReplyBlobResID)
		fmt.Println("  --- decoded handles / indices ---")
		fmt.Printf("  VkInstance                  = %#x\n", res.Instance)
		fmt.Printf("  physical device count       = %d (using device 0 = %#x)\n", res.PhysDevCount, res.PhysDev)
		fmt.Printf("  memory type count           = %d (chose type %d flags=%#x HOST_VISIBLE|COHERENT)\n",
			res.MemTypeCount, res.MemTypeIndex, res.MemTypeFlags)
		fmt.Printf("  queue family count          = %d (using family %d)\n", res.QueueFamCount, res.QueueFamilyIdx)
		fmt.Printf("  VkDevice                    = %#x\n", res.Device)
		fmt.Printf("  VkQueue                     = %#x\n", res.Queue)
		fmt.Printf("  VkImage                     = %#x\n", res.Image)
		fmt.Printf("  image mem requirements      = size=%d align=%d typeBits=%#x\n",
			res.MemReqSize, res.MemReqAlign, res.MemReqBits)
		fmt.Printf("  VkDeviceMemory              = %#x\n", res.Memory)
		fmt.Printf("  VkCommandPool               = %#x\n", res.CmdPool)
		fmt.Printf("  VkCommandBuffer             = %#x\n", res.CmdBuf)
		fmt.Printf("  VkFence                     = %#x\n", res.Fence)
		fmt.Println("  --- VkResults (0 == VK_SUCCESS) ---")
		fmt.Printf("  CreateInstance=%d CreateDevice=%d CreateImage=%d AllocMemory=%d BindImage=%d\n",
			res.ResCreateInstance, res.ResCreateDevice, res.ResCreateImage, res.ResAllocMemory, res.ResBindImage)
		fmt.Printf("  CreatePool=%d AllocCmdBuf=%d BeginCmdBuf=%d EndCmdBuf=%d\n",
			res.ResCreatePool, res.ResAllocCmdBuf, res.ResBeginCmdBuf, res.ResEndCmdBuf)
		fmt.Printf("  CreateFence=%d QueueSubmit=%d WaitForFences=%d\n",
			res.ResCreateFence, res.ResQueueSubmit, res.ResWaitForFences)
		fmt.Println("  --- guest-visible readback (the cleared image's pixels) ---")
		fmt.Printf("  device-memory blob res_id   = %d (blob_id tied to VkDeviceMemory %#x)\n",
			res.MemBlobResID, res.Memory)
		fmt.Printf("  readback performed          = %v\n", res.ReadbackDone)
		fmt.Printf("  texel(0,0) read back        = %#02x %#02x %#02x %#02x (RGBA)\n",
			res.FirstTexel[0], res.FirstTexel[1], res.FirstTexel[2], res.FirstTexel[3])
		fmt.Printf("  expected clear texel        = %#02x %#02x %#02x %#02x (RGBA)\n",
			res.ExpectTexel[0], res.ExpectTexel[1], res.ExpectTexel[2], res.ExpectTexel[3])
		fmt.Printf("  texels matching clear       = %d/%d  readback_pass=%v\n",
			res.TexelsRed, res.TexelsChecked, res.ReadbackPass)
		fmt.Println("  --- per-command ring trace (seqno = tail after write; head = host consumed) ---")
		for _, t := range res.Trace {
			fmt.Printf("    %s\n", t)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "VENUS-CLEAR-WALL: %v\n", err)
		os.Exit(1)
	}

	// GUEST-VISIBLE verdict: the guest read the actual cleared texels back from
	// the device-memory blob mapping and they ARE the clear colour. This is the
	// real result — not just host-behaviour confirmation, but the bytes in hand.
	if res.ResCreateImage == 0 && res.Image != 0 &&
		res.ResQueueSubmit == 0 && res.ResWaitForFences == 0 &&
		res.ReadbackDone && res.ReadbackPass {
		fmt.Printf("PASS: VENUS clear-image — GUEST READ BACK the cleared pixels: texel(0,0)=%#02x%#02x%#02x%#02x "+
			"(RGBA), %d/%d texels == clear colour red. The host created the image (%#x), executed "+
			"CmdClearColorImage(red) to fence completion, and backed the VkDeviceMemory (%#x) with a "+
			"HOST3D|MAPPABLE blob (res_id=%d) the guest mmap'd and read.\n",
			res.FirstTexel[0], res.FirstTexel[1], res.FirstTexel[2], res.FirstTexel[3],
			res.TexelsRed, res.TexelsChecked, res.Image, res.Memory, res.MemBlobResID)
		os.Exit(0)
	}
	fmt.Println("FAIL: VENUS clear-image — sequence completed but readback did not confirm red " +
		"(a VkResult was non-success, a handle was zero, or the mapped texels were not the clear colour)")
	os.Exit(2)
}
