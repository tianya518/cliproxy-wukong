package sentinel

import (
	"fmt"
	"time"
)

// MaybeClearStaleImageAsyncPending 有图但长期未收到 complete 时，解除 pending 避免永久卡住。
func (result *ChatResult) MaybeClearStaleImageAsyncPending() bool {
	if result == nil || result.imageAsyncTaskPending <= 0 || result.imageGenAsyncCompleteSeen {
		return false
	}
	if !result.HasDalleGeneratedOutput() {
		return false
	}
	since := time.Since(time.Unix(0, result.lastImageGenActivityAt))
	if since < 20*time.Second {
		return false
	}
	result.imageAsyncTaskPending = 0
	result.imageAsyncTaskActive = false
	return true
}

// ImageGenExitBlockReason 当前为何还不能结束 WS（用于诊断日志）。
func (result *ChatResult) ImageGenExitBlockReason() string {
	if result == nil {
		return "blocking=nil"
	}
	if !result.HasDalleGeneratedOutput() {
		return "blocking=no_dalle_image_yet"
	}
	if result.lastImageGenActivityAt == 0 {
		return "blocking=no_image_activity_ts"
	}
	since := time.Since(time.Unix(0, result.lastImageGenActivityAt))
	if result.imageAsyncTaskPending > 0 {
		return fmt.Sprintf("blocking=async_pending(%d,active=%v) idleSinceImg=%.1fs", result.imageAsyncTaskPending, result.imageAsyncTaskActive, since.Seconds())
	}
	need := ImageGenIdleDuration(result)
	if result.imageGenAsyncCompleteSeen || result.imageGenConvAsyncStatusDone {
		need = 3 * time.Second
		if since < need {
			return fmt.Sprintf("blocking=post_complete_idle(%.1fs/%.0fs convStatus=%v)",
				since.Seconds(), need.Seconds(), result.imageGenConvAsyncStatusDone)
		}
		return "ok"
	}
	if result.imageGenTurnDone {
		return fmt.Sprintf("blocking=turn_done_but_async_may_continue idleSinceImg=%.1fs", since.Seconds())
	}
	if since < need {
		return fmt.Sprintf("blocking=idle_wait(%.1fs/%.0fs)", since.Seconds(), need.Seconds())
	}
	return "ok"
}
