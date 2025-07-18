package compileopts

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	validBuildModeOptions     = []string{"default", "c-shared", "wasi-legacy"}
	validGCOptions            = []string{"none", "leaking", "conservative", "custom", "precise", "boehm"}
	validSchedulerOptions     = []string{"none", "tasks", "asyncify", "threads", "cores"}
	validSerialOptions        = []string{"none", "uart", "usb", "rtt"}
	validPrintSizeOptions     = []string{"none", "short", "full", "html"}
	validPanicStrategyOptions = []string{"print", "trap"}
	validOptOptions           = []string{"none", "0", "1", "2", "s", "z"}
)

// Options contains extra options to give to the compiler. These options are
// usually passed from the command line, but can also be passed in environment
// variables for example.
type Options struct {
	GOOS            string // environment variable
	GOARCH          string // environment variable
	GOARM           string // environment variable (only used with GOARCH=arm)
	GOMIPS          string // environment variable (only used with GOARCH=mips and GOARCH=mipsle)
	Directory       string // working dir, leave it unset to use the current working dir
	Target          string
	BuildMode       string // -buildmode flag
	Opt             string
	GC              string
	PanicStrategy   string
	Scheduler       string
	StackSize       uint64 // goroutine stack size (if none could be automatically determined)
	Serial          string
	Work            bool // -work flag to print temporary build directory
	InterpTimeout   time.Duration
	PrintIR         bool
	DumpSSA         bool
	VerifyIR        bool
	SkipDWARF       bool
	PrintCommands   func(cmd string, args ...string) `json:"-"`
	Semaphore       chan struct{}                    `json:"-"` // -p flag controls cap
	Debug           bool
	Nobounds        bool
	PrintSizes      string
	PrintAllocs     *regexp.Regexp // regexp string
	PrintStacks     bool
	Tags            []string
	GlobalValues    map[string]map[string]string // map[pkgpath]map[varname]value
	TestConfig      TestConfig
	Programmer      string
	OpenOCDCommands []string
	LLVMFeatures    string
	Monitor         bool
	BaudRate        int
	Timeout         time.Duration
	WITPackage      string // pass through to wasm-tools component embed invocation
	WITWorld        string // pass through to wasm-tools component embed -w option
	ExtLDFlags      []string
}

// Verify performs a validation on the given options, raising an error if options are not valid.
func (o *Options) Verify() error {
	if o.BuildMode != "" {
		valid := isInArray(validBuildModeOptions, o.BuildMode)
		if !valid {
			return fmt.Errorf(`invalid buildmode option '%s': valid values are %s`,
				o.BuildMode,
				strings.Join(validBuildModeOptions, ", "))
		}
	}
	if o.GC != "" {
		valid := isInArray(validGCOptions, o.GC)
		if !valid {
			return fmt.Errorf(`invalid gc option '%s': valid values are %s`,
				o.GC,
				strings.Join(validGCOptions, ", "))
		}
	}

	if o.Scheduler != "" {
		valid := isInArray(validSchedulerOptions, o.Scheduler)
		if !valid {
			return fmt.Errorf(`invalid scheduler option '%s': valid values are %s`,
				o.Scheduler,
				strings.Join(validSchedulerOptions, ", "))
		}
	}

	if o.Serial != "" {
		valid := isInArray(validSerialOptions, o.Serial)
		if !valid {
			return fmt.Errorf(`invalid serial option '%s': valid values are %s`,
				o.Serial,
				strings.Join(validSerialOptions, ", "))
		}
	}

	if o.PrintSizes != "" {
		valid := isInArray(validPrintSizeOptions, o.PrintSizes)
		if !valid {
			return fmt.Errorf(`invalid size option '%s': valid values are %s`,
				o.PrintSizes,
				strings.Join(validPrintSizeOptions, ", "))
		}
	}

	if o.PanicStrategy != "" {
		valid := isInArray(validPanicStrategyOptions, o.PanicStrategy)
		if !valid {
			return fmt.Errorf(`invalid panic option '%s': valid values are %s`,
				o.PanicStrategy,
				strings.Join(validPanicStrategyOptions, ", "))
		}
	}

	if o.Opt != "" {
		if !isInArray(validOptOptions, o.Opt) {
			return fmt.Errorf("invalid -opt=%s: valid values are %s", o.Opt, strings.Join(validOptOptions, ", "))
		}
	}

	return nil
}

func isInArray(arr []string, item string) bool {
	for _, i := range arr {
		if i == item {
			return true
		}
	}
	return false
}
