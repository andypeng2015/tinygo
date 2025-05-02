//go:build wasip2

package runtime

import (
	"unsafe"

	"internal/wasi/cli/v0.2.0/environment"
	wasiclirun "internal/wasi/cli/v0.2.0/run"
	monotonicclock "internal/wasi/clocks/v0.2.0/monotonic-clock"

	"internal/cm"
)

func init() {
	wasiclirun.Exports.Run = func() cm.BoolResult {
		callMain()
		return false
	}
}

var args []string

//go:linkname os_runtime_args os.runtime_args
func os_runtime_args() []string {
	if args == nil {
		args = environment.GetArguments().Slice()
	}
	return args
}

//export cabi_realloc
func cabi_realloc(ptr, oldsize, align, newsize unsafe.Pointer) unsafe.Pointer {
	return realloc(ptr, uintptr(newsize))
}

func ticksToNanoseconds(ticks timeUnit) int64 {
	return int64(ticks)
}

func nanosecondsToTicks(ns int64) timeUnit {
	return timeUnit(ns)
}

func sleepTicks(d timeUnit) {
	p := monotonicclock.SubscribeDuration(monotonicclock.Duration(d))
	p.Block()
}

func ticks() timeUnit {
	return timeUnit(monotonicclock.Now())
}

// Need to stub these for `os/signal`, but wasi doesn't support signals.

//go:linkname signal_disable os/signal.signal_disable
func signal_disable(uint32) {}

//go:linkname signal_enable os/signal.signal_enable
func signal_enable(uint32) {}

//go:linkname signal_ignore os/signal.signal_ignore
func signal_ignore(uint32) {}

//go:linkname signal_ignored os/signal.signal_ignored
func signal_ignored(uint32) bool { return false }

//go:linkname signal_recv os/signal.signal_recv
func signal_recv() uint32 { return ^uint32(0) }
