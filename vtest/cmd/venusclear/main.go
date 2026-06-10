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
// CONFIRMATION (Task 2). Pixel readback over a host-visible mapping requires
// host-defined plumbing the proven encoder closure does not derive
// (VkImportMemoryResourceInfoMESA as the VkMemoryAllocateInfo pNext + a guest
// renderer BO whose blob_id matches the device-memory id + RESOURCE_MAP_BLOB;
// see Mesa vn_device_memory.c:vn_device_memory_import_dma_buf /
// vn_device_memory_alloc_guest_vram). So venusclear's verdict rests on the
// HOST-BEHAVIOUR evidence: vkCreateImage returned VK_SUCCESS + a handle, the
// barrier + CmdClearColorImage were consumed (head advanced), and vkQueueSubmit
// + vkQueueWaitIdle returned VK_SUCCESS with NO CS error. The harness pairs
// this with the server stderr (vkr logs) for the renderer's own confirmation.
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
		fmt.Println("  --- per-command ring trace (seqno = tail after write; head = host consumed) ---")
		for _, t := range res.Trace {
			fmt.Printf("    %s\n", t)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "VENUS-CLEAR-WALL: %v\n", err)
		os.Exit(1)
	}

	// Host-behaviour confirmation (Task 2): the image was created, the clear was
	// recorded + submitted, and the submit's FENCE was signalled — i.e. the host
	// actually EXECUTED the command buffer (including CmdClearColorImage) to
	// completion. WaitForFences returning VK_SUCCESS is the host's own "the clear
	// finished" confirmation.
	if res.ResCreateImage == 0 && res.Image != 0 &&
		res.ResQueueSubmit == 0 && res.ResWaitForFences == 0 {
		fmt.Printf("PASS: VENUS clear-image — host created the image (%#x), consumed the barrier + "+
			"CmdClearColorImage(red), and EXECUTED QueueSubmit to completion (fence %#x signalled, "+
			"WaitForFences=VK_SUCCESS, no CS error)\n",
			res.Image, res.Fence)
		os.Exit(0)
	}
	fmt.Println("FAIL: VENUS clear-image — sequence completed but a VkResult was non-success or a handle was zero")
	os.Exit(2)
}
