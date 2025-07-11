package main

// This file tests the compiler by running Go files in testdata/*.go and
// comparing their output with the expected output in testdata/*.txt.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aykevl/go-wasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
	"github.com/tinygo-org/tinygo/builder"
	"github.com/tinygo-org/tinygo/compileopts"
	"github.com/tinygo-org/tinygo/diagnostics"
	"github.com/tinygo-org/tinygo/goenv"
)

const TESTDATA = "testdata"

var testTarget = flag.String("target", "", "override test target")

var testOnlyCurrentOS = flag.Bool("only-current-os", false, "")

var supportedLinuxArches = map[string]string{
	"AMD64Linux": "linux/amd64",
	"X86Linux":   "linux/386",
	"ARMLinux":   "linux/arm/6",
	"ARM64Linux": "linux/arm64",
	"MIPSLinux":  "linux/mips/hardfloat",
	"WASIp1":     "wasip1/wasm",
}

func init() {
	major, _, _ := goenv.GetGorootVersion()
	if major < 21 {
		// Go 1.20 backwards compatibility.
		// Should be removed once we drop support for Go 1.20.
		delete(supportedLinuxArches, "WASIp1")
	}
}

var sema = make(chan struct{}, runtime.NumCPU())

func TestBuild(t *testing.T) {
	t.Parallel()

	tests := []string{
		"alias.go",
		"atomic.go",
		"binop.go",
		"calls.go",
		"cgo/",
		"channel.go",
		"embed/",
		"float.go",
		"gc.go",
		"generics.go",
		"goroutines.go",
		"init.go",
		"init_multi.go",
		"interface.go",
		"json.go",
		"map.go",
		"math.go",
		"oldgo/",
		"print.go",
		"reflect.go",
		"signal.go",
		"slice.go",
		"sort.go",
		"stdlib.go",
		"string.go",
		"structs.go",
		"testing.go",
		"timers.go",
		"zeroalloc.go",
	}

	// Go 1.21 made some changes to the language, which we can only test when
	// we're actually on Go 1.21.
	_, minor, err := goenv.GetGorootVersion()
	if err != nil {
		t.Fatal("could not get version:", minor)
	}
	if minor >= 21 {
		tests = append(tests, "go1.21.go")
	}
	if minor >= 22 {
		tests = append(tests, "go1.22/")
	}
	if minor >= 23 {
		tests = append(tests, "go1.23/")
	}

	if *testTarget != "" {
		// This makes it possible to run one specific test (instead of all),
		// which is especially useful to quickly check whether some changes
		// affect a particular target architecture.
		runPlatTests(optionsFromTarget(*testTarget, sema), tests, t)
		return
	}

	t.Run("Host", func(t *testing.T) {
		t.Parallel()
		runPlatTests(optionsFromTarget("", sema), tests, t)
	})

	// Test a few build options.
	t.Run("build-options", func(t *testing.T) {
		t.Parallel()

		// Test with few optimizations enabled (no inlining, etc).
		t.Run("opt=1", func(t *testing.T) {
			t.Parallel()
			opts := optionsFromTarget("", sema)
			opts.Opt = "1"
			runTestWithConfig("stdlib.go", t, opts, nil, nil)
		})

		// Test with only the bare minimum of optimizations enabled.
		// TODO: fix this for stdlib.go, which currently fails.
		t.Run("opt=0", func(t *testing.T) {
			t.Parallel()
			opts := optionsFromTarget("", sema)
			opts.Opt = "0"
			runTestWithConfig("print.go", t, opts, nil, nil)
		})

		t.Run("ldflags", func(t *testing.T) {
			t.Parallel()
			opts := optionsFromTarget("", sema)
			opts.GlobalValues = map[string]map[string]string{
				"main": {
					"someGlobal": "foobar",
				},
			}
			runTestWithConfig("ldflags.go", t, opts, nil, nil)
		})
	})

	if testing.Short() {
		// Don't test other targets when the -short flag is used. Only test the
		// host system.
		return
	}

	if !*testOnlyCurrentOS {
		t.Run("EmulatedCortexM3", func(t *testing.T) {
			t.Parallel()
			runPlatTests(optionsFromTarget("cortex-m-qemu", sema), tests, t)
		})

		t.Run("EmulatedRISCV", func(t *testing.T) {
			t.Parallel()
			runPlatTests(optionsFromTarget("riscv-qemu", sema), tests, t)
		})

		t.Run("AVR", func(t *testing.T) {
			t.Parallel()
			runPlatTests(optionsFromTarget("simavr", sema), tests, t)
		})

		t.Run("WebAssembly", func(t *testing.T) {
			t.Parallel()

			runPlatTests(optionsFromTarget("wasm", sema), tests, t)
			// Test with -gc=boehm.
			t.Run("gc.go-boehm", func(t *testing.T) {
				t.Parallel()
				optionsBoehm := optionsFromTarget("wasm", sema)
				optionsBoehm.GC = "boehm"
				runTest("gc.go", optionsBoehm, t, nil, nil)
			})
		})
		t.Run("WASIp1", func(t *testing.T) {
			t.Parallel()
			runPlatTests(optionsFromTarget("wasip1", sema), tests, t)

			// Test with -gc=boehm.
			t.Run("gc.go-boehm", func(t *testing.T) {
				t.Parallel()
				optionsBoehm := optionsFromTarget("wasip1", sema)
				optionsBoehm.GC = "boehm"
				runTest("gc.go", optionsBoehm, t, nil, nil)
			})
		})
		t.Run("WASIp2", func(t *testing.T) {
			t.Parallel()
			runPlatTests(optionsFromTarget("wasip2", sema), tests, t)
		})
	}

	if runtime.GOOS == "linux" {
		for name, osArch := range supportedLinuxArches {
			options := optionsFromOSARCH(osArch, sema)
			if options.GOARCH != runtime.GOARCH { // Native architecture already run above.
				t.Run(name, func(t *testing.T) {
					runPlatTests(options, tests, t)
				})
			}
		}
		t.Run("MIPS little-endian", func(t *testing.T) {
			// Run a single test for GOARCH=mipsle to see whether it works at
			// all. It is already mostly tested because GOARCH=mips and
			// GOARCH=mipsle are so similar, but it's good to have an extra test
			// to be sure.
			t.Parallel()
			options := optionsFromOSARCH("linux/mipsle/softfloat", sema)
			runTest("cgo/", options, t, nil, nil)
		})
	} else if runtime.GOOS == "windows" {
		if runtime.GOARCH != "386" {
			t.Run("Windows386", func(t *testing.T) {
				t.Parallel()
				runPlatTests(optionsFromOSARCH("windows/386", sema), tests, t)
			})
		}
	}
}

func runPlatTests(options compileopts.Options, tests []string, t *testing.T) {
	emuCheck(t, options)

	spec, err := compileopts.LoadTarget(&options)
	if err != nil {
		t.Fatal("failed to load target spec:", err)
	}

	// FIXME: this should really be:
	// isWebAssembly := strings.HasPrefix(spec.Triple, "wasm")
	isWASI := strings.HasPrefix(options.Target, "wasi")
	isWebAssembly := isWASI || strings.HasPrefix(options.Target, "wasm") || (options.Target == "" && strings.HasPrefix(options.GOARCH, "wasm"))
	isBaremetal := options.Target == "simavr" || options.Target == "cortex-m-qemu" || options.Target == "riscv-qemu"

	for _, name := range tests {
		if options.GOOS == "linux" && (options.GOARCH == "arm" || options.GOARCH == "386") {
			switch name {
			case "timers.go":
				// Timer tests do not work because syscall.seek is implemented
				// as Assembly in mainline Go and causes linker failure
				continue
			}
		}
		if options.GOOS == "linux" && (options.GOARCH == "mips" || options.GOARCH == "mipsle") {
			if name == "atomic.go" || name == "timers.go" {
				// 64-bit atomic operations aren't currently supported on MIPS.
				continue
			}
		}
		if options.GOOS == "linux" && options.GOARCH == "mips" {
			if name == "cgo/" {
				// CGo isn't supported yet on big-endian systems (needs updates
				// to bitfield access methods).
				continue
			}
		}
		if options.Target == "simavr" {
			// Not all tests are currently supported on AVR.
			// Skip the ones that aren't.
			switch name {
			case "reflect.go":
				// Reflect tests do not run correctly, probably because of the
				// limited amount of memory.
				continue

			case "gc.go":
				// Does not pass due to high mark false positive rate.
				continue

			case "json.go", "stdlib.go", "testing.go":
				// Too big for AVR. Doesn't fit in flash/RAM.
				continue

			case "math.go":
				// Needs newer picolibc version (for sqrt).
				continue

			case "cgo/":
				// CGo function pointers don't work on AVR (needs LLVM 16 and
				// some compiler changes).
				continue

			case "timers.go":
				// Crashes starting with Go 1.23.
				// Bug: https://github.com/llvm/llvm-project/issues/104032
				continue

			default:
			}
		}
		if options.Target == "wasip2" {
			switch name {
			case "cgo/":
				// waisp2 use our own libc; cgo tests fail
				continue
			}
		}
		if isWebAssembly || isBaremetal || options.GOOS == "windows" {
			switch name {
			case "signal.go":
				// Signals only work on POSIX-like systems.
				continue
			}
		}

		name := name // redefine to avoid race condition
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runTest(name, options, t, nil, nil)
		})
	}
	if !strings.HasPrefix(spec.Emulator, "simavr ") {
		t.Run("env.go", func(t *testing.T) {
			t.Parallel()
			runTest("env.go", options, t, []string{"first", "second"}, []string{"ENV1=VALUE1", "ENV2=VALUE2"})
		})
	}
	if isWebAssembly {
		t.Run("alias.go-scheduler-none", func(t *testing.T) {
			t.Parallel()
			options := compileopts.Options(options)
			options.Scheduler = "none"
			runTest("alias.go", options, t, nil, nil)
		})
	}
	if options.Target == "" || isWASI {
		t.Run("filesystem.go", func(t *testing.T) {
			t.Parallel()
			runTest("filesystem.go", options, t, nil, nil)
		})
	}
	if options.Target == "" || options.Target == "wasm" || isWASI {
		t.Run("rand.go", func(t *testing.T) {
			t.Parallel()
			runTest("rand.go", options, t, nil, nil)
		})
	}
	if !isWebAssembly {
		// The recover() builtin isn't supported yet on WebAssembly and Windows.
		t.Run("recover.go", func(t *testing.T) {
			t.Parallel()
			runTest("recover.go", options, t, nil, nil)
		})
	}
}

func emuCheck(t *testing.T, options compileopts.Options) {
	// Check if the emulator is installed.
	spec, err := compileopts.LoadTarget(&options)
	if err != nil {
		t.Fatal("failed to load target spec:", err)
	}
	if spec.Emulator != "" {
		emulatorCommand := strings.SplitN(spec.Emulator, " ", 2)[0]
		_, err := exec.LookPath(emulatorCommand)
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				t.Skipf("emulator not installed: %q", emulatorCommand)
			}

			t.Errorf("searching for emulator: %v", err)
			return
		}
	}
}

func optionsFromTarget(target string, sema chan struct{}) compileopts.Options {
	separators := strings.Count(target, "/")
	if (separators == 1 || separators == 2) && !strings.HasSuffix(target, ".json") {
		return optionsFromOSARCH(target, sema)
	}
	return compileopts.Options{
		// GOOS/GOARCH are only used if target == ""
		GOOS:          goenv.Get("GOOS"),
		GOARCH:        goenv.Get("GOARCH"),
		GOARM:         goenv.Get("GOARM"),
		GOMIPS:        goenv.Get("GOMIPS"),
		Target:        target,
		Semaphore:     sema,
		InterpTimeout: 180 * time.Second,
		Debug:         true,
		VerifyIR:      true,
		Opt:           "z",
	}
}

// optionsFromOSARCH returns a set of options based on the "osarch" string. This
// string is in the form of "os/arch/subarch", with the subarch only sometimes
// being necessary. Examples are "darwin/amd64" or "linux/arm/7".
func optionsFromOSARCH(osarch string, sema chan struct{}) compileopts.Options {
	parts := strings.Split(osarch, "/")
	options := compileopts.Options{
		GOOS:          parts[0],
		GOARCH:        parts[1],
		Semaphore:     sema,
		InterpTimeout: 180 * time.Second,
		Debug:         true,
		VerifyIR:      true,
		Opt:           "z",
	}
	switch options.GOARCH {
	case "arm":
		options.GOARM = parts[2]
	case "mips", "mipsle":
		options.GOMIPS = parts[2]
	}
	return options
}

func runTest(name string, options compileopts.Options, t *testing.T, cmdArgs, environmentVars []string) {
	runTestWithConfig(name, t, options, cmdArgs, environmentVars)
}

func runTestWithConfig(name string, t *testing.T, options compileopts.Options, cmdArgs, environmentVars []string) {
	// Get the expected output for this test.
	// Note: not using filepath.Join as it strips the path separator at the end
	// of the path.
	path := TESTDATA + "/" + name
	// Get the expected output for this test.
	expectedOutputPath := path[:len(path)-3] + ".txt"
	pkgName := "./" + path
	if path[len(path)-1] == '/' {
		expectedOutputPath = path + "out.txt"
		options.Directory = path
		pkgName = "."
	}

	config, err := builder.NewConfig(&options)
	if err != nil {
		t.Fatal(err)
	}

	// Build the test binary.
	stdout := &bytes.Buffer{}
	_, err = buildAndRun(pkgName, config, stdout, cmdArgs, environmentVars, time.Minute, func(cmd *exec.Cmd, result builder.BuildResult) error {
		return cmd.Run()
	})
	if err != nil {
		w := &bytes.Buffer{}
		diagnostics.CreateDiagnostics(err).WriteTo(w, "")
		for _, line := range strings.Split(strings.TrimRight(w.String(), "\n"), "\n") {
			t.Log(line)
		}
		if stdout.Len() != 0 {
			t.Logf("output:\n%s", stdout.String())
		}
		t.Fail()
		return
	}

	actual := stdout.Bytes()
	if config.EmulatorName() == "simavr" {
		// Strip simavr log formatting.
		actual = bytes.Replace(actual, []byte{0x1b, '[', '3', '2', 'm'}, nil, -1)
		actual = bytes.Replace(actual, []byte{0x1b, '[', '0', 'm'}, nil, -1)
		actual = bytes.Replace(actual, []byte{'.', '.', '\n'}, []byte{'\n'}, -1)
		actual = bytes.Replace(actual, []byte{'\n', '.', '\n'}, []byte{'\n', '\n'}, -1)
	}
	if name == "testing.go" {
		// Strip actual time.
		re := regexp.MustCompile(`\([0-9]\.[0-9][0-9]s\)`)
		actual = re.ReplaceAllLiteral(actual, []byte{'(', '0', '.', '0', '0', 's', ')'})
	}

	// Check whether the command ran successfully.
	if err != nil {
		t.Error("failed to run:", err)
	}
	checkOutput(t, expectedOutputPath, actual)

	if t.Failed() {
		r := bufio.NewReader(bytes.NewReader(actual))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			t.Log("stdout:", line[:len(line)-1])
		}
		t.Fail()
	}
}

// Test WebAssembly files for certain properties.
func TestWebAssembly(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name          string
		target        string
		panicStrategy string
		imports       []string
	}
	for _, tc := range []testCase{
		// Test whether there really are no imports when using -panic=trap. This
		// tests the bugfix for https://github.com/tinygo-org/tinygo/issues/4161.
		{name: "panic-default", target: "wasip1", imports: []string{"wasi_snapshot_preview1.fd_write", "wasi_snapshot_preview1.random_get"}},
		{name: "panic-trap", target: "wasm-unknown", panicStrategy: "trap", imports: []string{}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpdir := t.TempDir()
			options := optionsFromTarget(tc.target, sema)
			options.PanicStrategy = tc.panicStrategy
			config, err := builder.NewConfig(&options)
			if err != nil {
				t.Fatal(err)
			}

			result, err := builder.Build("testdata/trivialpanic.go", ".wasm", tmpdir, config)
			if err != nil {
				t.Fatal("failed to build binary:", err)
			}
			f, err := os.Open(result.Binary)
			if err != nil {
				t.Fatal("could not open output binary:", err)
			}
			defer f.Close()
			module, err := wasm.Parse(f)
			if err != nil {
				t.Fatal("could not parse output binary:", err)
			}

			// Test the list of imports.
			if tc.imports != nil {
				var imports []string
				for _, section := range module.Sections {
					switch section := section.(type) {
					case *wasm.SectionImport:
						for _, symbol := range section.Entries {
							imports = append(imports, symbol.Module+"."+symbol.Field)
						}
					}
				}
				if !stringSlicesEqual(imports, tc.imports) {
					t.Errorf("import list not as expected!\nexpected: %v\nactual:   %v", tc.imports, imports)
				}
			}
		})
	}
}

func stringSlicesEqual(s1, s2 []string) bool {
	// We can use slices.Equal once we drop support for Go 1.20 (it was added in
	// Go 1.21).
	if len(s1) != len(s2) {
		return false
	}
	for i, s := range s1 {
		if s != s2[i] {
			return false
		}
	}
	return true
}

func TestWasmExport(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name      string
		target    string
		buildMode string
		scheduler string
		file      string
		noOutput  bool
		command   bool // call _start (command mode) instead of _initialize
	}

	tests := []testCase{
		// "command mode" WASI
		{
			name:    "WASIp1-command",
			target:  "wasip1",
			command: true,
		},
		// "reactor mode" WASI (with -buildmode=c-shared)
		{
			name:      "WASIp1-reactor",
			target:    "wasip1",
			buildMode: "c-shared",
		},
		// Make sure reactor mode also works without a scheduler.
		{
			name:      "WASIp1-reactor-noscheduler",
			target:    "wasip1",
			buildMode: "c-shared",
			scheduler: "none",
			file:      "wasmexport-noscheduler.go",
		},
		// Test -target=wasm-unknown with the default build mode (which is
		// c-shared).
		{
			name:     "wasm-unknown-reactor",
			target:   "wasm-unknown",
			file:     "wasmexport-noscheduler.go",
			noOutput: true, // wasm-unknown cannot produce output
		},
		// Test -target=wasm-unknown with -buildmode=default, which makes it run
		// in command mode.
		{
			name:      "wasm-unknown-command",
			target:    "wasm-unknown",
			buildMode: "default",
			file:      "wasmexport-noscheduler.go",
			noOutput:  true, // wasm-unknown cannot produce output
			command:   true,
		},
		// Test buildmode=wasi-legacy with WASI.
		{
			name:      "WASIp1-legacy",
			target:    "wasip1",
			buildMode: "wasi-legacy",
			scheduler: "none",
			file:      "wasmexport-noscheduler.go",
			command:   true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Build the wasm binary.
			tmpdir := t.TempDir()
			options := optionsFromTarget(tc.target, sema)
			options.BuildMode = tc.buildMode
			options.Scheduler = tc.scheduler
			buildConfig, err := builder.NewConfig(&options)
			if err != nil {
				t.Fatal(err)
			}
			filename := "wasmexport.go"
			if tc.file != "" {
				filename = tc.file
			}
			result, err := builder.Build("testdata/"+filename, ".wasm", tmpdir, buildConfig)
			if err != nil {
				t.Fatal("failed to build binary:", err)
			}

			// Read the wasm binary back into memory.
			data, err := os.ReadFile(result.Binary)
			if err != nil {
				t.Fatal("could not read wasm binary: ", err)
			}

			// Set up the wazero runtime.
			output := &bytes.Buffer{}
			ctx := context.Background()
			r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
			defer r.Close(ctx)
			config := wazero.NewModuleConfig().
				WithStdout(output).WithStderr(output).
				WithStartFunctions()

			// Prepare for testing.
			var mod api.Module
			mustCall := func(results []uint64, err error) []uint64 {
				if err != nil {
					t.Error("failed to run function:", err)
				}
				return results
			}
			checkResult := func(name string, results []uint64, expected []uint64) {
				if len(results) != len(expected) {
					t.Errorf("%s: expected %v but got %v", name, expected, results)
				}
				for i, result := range results {
					if result != expected[i] {
						t.Errorf("%s: expected %v but got %v", name, expected, results)
						break
					}
				}
			}
			runTests := func() {
				// Test an exported function without params or return value.
				checkResult("hello()", mustCall(mod.ExportedFunction("hello").Call(ctx)), nil)

				// Test that we can call an exported function more than once.
				checkResult("add(3, 5)", mustCall(mod.ExportedFunction("add").Call(ctx, 3, 5)), []uint64{8})
				checkResult("add(7, 9)", mustCall(mod.ExportedFunction("add").Call(ctx, 7, 9)), []uint64{16})
				checkResult("add(6, 1)", mustCall(mod.ExportedFunction("add").Call(ctx, 6, 1)), []uint64{7})

				// Test that imported functions can call exported functions
				// again.
				checkResult("reentrantCall(2, 3)", mustCall(mod.ExportedFunction("reentrantCall").Call(ctx, 2, 3)), []uint64{5})
				checkResult("reentrantCall(1, 8)", mustCall(mod.ExportedFunction("reentrantCall").Call(ctx, 1, 8)), []uint64{9})

				// Check that goroutines started inside //go:wasmexport don't
				// block the called function from returning.
				checkResult("goroutineExit()", mustCall(mod.ExportedFunction("goroutineExit").Call(ctx)), nil)
			}

			// Add wasip1 module.
			wasi_snapshot_preview1.MustInstantiate(ctx, r)

			// Add custom "tester" module.
			callOutside := func(a, b int32) int32 {
				results, err := mod.ExportedFunction("add").Call(ctx, uint64(a), uint64(b))
				if err != nil {
					t.Error("could not call exported add function:", err)
				}
				return int32(results[0])
			}
			callTestMain := func() {
				runTests()
			}
			builder := r.NewHostModuleBuilder("tester")
			builder.NewFunctionBuilder().WithFunc(callOutside).Export("callOutside")
			builder.NewFunctionBuilder().WithFunc(callTestMain).Export("callTestMain")
			_, err = builder.Instantiate(ctx)
			if err != nil {
				t.Fatal(err)
			}

			// Parse and instantiate the wasm.
			mod, err = r.InstantiateWithConfig(ctx, data, config)
			if err != nil {
				t.Fatal("could not instantiate wasm module:", err)
			}

			// Initialize the module and run the tests.
			if tc.command {
				// Call _start (the entry point), which calls
				// tester.callTestMain, which then runs all the tests.
				_, err := mod.ExportedFunction("_start").Call(ctx)
				if err != nil {
					if exitErr, ok := err.(*sys.ExitError); ok && exitErr.ExitCode() == 0 {
						// Exited with code 0. Nothing to worry about.
					} else {
						t.Error("failed to run _start:", err)
					}
				}
			} else {
				// Run the _initialize call, because this is reactor mode wasm.
				mustCall(mod.ExportedFunction("_initialize").Call(ctx))
				runTests()
			}

			// Check that the output matches the expected output.
			// (Skip this for wasm-unknown because it can't produce output).
			if !tc.noOutput {
				checkOutput(t, "testdata/wasmexport.txt", output.Bytes())
			}
		})
	}
}

// Test js.FuncOf (for syscall/js).
// This test might be extended in the future to cover more cases in syscall/js.
func TestWasmFuncOf(t *testing.T) {
	// Build the wasm binary.
	tmpdir := t.TempDir()
	options := optionsFromTarget("wasm", sema)
	buildConfig, err := builder.NewConfig(&options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := builder.Build("testdata/wasmfunc.go", ".wasm", tmpdir, buildConfig)
	if err != nil {
		t.Fatal("failed to build binary:", err)
	}

	// Test the resulting binary using NodeJS.
	output := &bytes.Buffer{}
	cmd := exec.Command("node", "testdata/wasmfunc.js", result.Binary, buildConfig.BuildMode())
	cmd.Stdout = output
	cmd.Stderr = output
	err = cmd.Run()
	if err != nil {
		t.Error("failed to run node:", err)
	}
	checkOutput(t, "testdata/wasmfunc.txt", output.Bytes())
}

// Test //go:wasmexport in JavaScript (using NodeJS).
func TestWasmExportJS(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name      string
		buildMode string
	}

	tests := []testCase{
		{name: "default"},
		{name: "c-shared", buildMode: "c-shared"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Build the wasm binary.
			tmpdir := t.TempDir()
			options := optionsFromTarget("wasm", sema)
			options.BuildMode = tc.buildMode
			buildConfig, err := builder.NewConfig(&options)
			if err != nil {
				t.Fatal(err)
			}
			result, err := builder.Build("testdata/wasmexport-noscheduler.go", ".wasm", tmpdir, buildConfig)
			if err != nil {
				t.Fatal("failed to build binary:", err)
			}

			// Test the resulting binary using NodeJS.
			output := &bytes.Buffer{}
			cmd := exec.Command("node", "testdata/wasmexport.js", result.Binary, buildConfig.BuildMode())
			cmd.Stdout = output
			cmd.Stderr = output
			err = cmd.Run()
			if err != nil {
				t.Error("failed to run node:", err)
			}
			checkOutput(t, "testdata/wasmexport.txt", output.Bytes())
		})
	}
}

// Test whether Go.run() (in wasm_exec.js) normally returns and returns the
// right exit code.
func TestWasmExit(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name   string
		output string
	}

	tests := []testCase{
		{name: "normal", output: "exit code: 0\n"},
		{name: "exit-0", output: "exit code: 0\n"},
		{name: "exit-0-sleep", output: "slept\nexit code: 0\n"},
		{name: "exit-1", output: "exit code: 1\n"},
		{name: "exit-1-sleep", output: "slept\nexit code: 1\n"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := optionsFromTarget("wasm", sema)
			buildConfig, err := builder.NewConfig(&options)
			if err != nil {
				t.Fatal(err)
			}
			buildConfig.Target.Emulator = "node testdata/wasmexit.js {}"
			output := &bytes.Buffer{}
			_, err = buildAndRun("testdata/wasmexit.go", buildConfig, output, []string{tc.name}, nil, time.Minute, func(cmd *exec.Cmd, result builder.BuildResult) error {
				return cmd.Run()
			})
			if err != nil {
				t.Error(err)
			}
			expected := "wasmexit test: " + tc.name + "\n" + tc.output
			checkOutputData(t, []byte(expected), output.Bytes())
		})
	}
}

// Check whether the output of a test equals the expected output.
func checkOutput(t *testing.T, filename string, actual []byte) {
	expectedOutput, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal("could not read output file:", err)
	}
	checkOutputData(t, expectedOutput, actual)
}

func checkOutputData(t *testing.T, expectedOutput, actual []byte) {
	expectedOutput = bytes.ReplaceAll(expectedOutput, []byte("\r\n"), []byte("\n"))
	actual = bytes.ReplaceAll(actual, []byte("\r\n"), []byte("\n"))

	if !bytes.Equal(actual, expectedOutput) {
		t.Errorf("output did not match (expected %d bytes, got %d bytes):", len(expectedOutput), len(actual))
		t.Error(string(Diff("expected", expectedOutput, "actual", actual)))
	}
}

func TestTest(t *testing.T) {
	t.Parallel()

	type targ struct {
		name string
		opts compileopts.Options
	}
	targs := []targ{
		// Host
		{"Host", optionsFromTarget("", sema)},
	}
	if !testing.Short() {
		if runtime.GOOS == "linux" {
			for name, osArch := range supportedLinuxArches {
				options := optionsFromOSARCH(osArch, sema)
				if options.GOARCH != runtime.GOARCH { // Native architecture already run above.
					targs = append(targs, targ{name, options})
				}
			}
		}

		targs = append(targs,
			// QEMU microcontrollers
			targ{"EmulatedCortexM3", optionsFromTarget("cortex-m-qemu", sema)},
			targ{"EmulatedRISCV", optionsFromTarget("riscv-qemu", sema)},

			// Node/Wasmtime
			targ{"WASM", optionsFromTarget("wasm", sema)},
			targ{"WASI", optionsFromTarget("wasip1", sema)},
		)
	}
	for _, targ := range targs {
		targ := targ
		t.Run(targ.name, func(t *testing.T) {
			t.Parallel()

			emuCheck(t, targ.opts)

			t.Run("Pass", func(t *testing.T) {
				t.Parallel()

				// Test a package which builds and passes normally.

				var wg sync.WaitGroup
				defer wg.Wait()

				out := ioLogger(t, &wg)
				defer out.Close()

				opts := targ.opts
				passed, err := Test("github.com/tinygo-org/tinygo/tests/testing/pass", out, out, &opts, "")
				if err != nil {
					t.Errorf("test error: %v", err)
				}
				if !passed {
					t.Error("test failed")
				}
			})

			t.Run("Fail", func(t *testing.T) {
				t.Parallel()

				// Test a package which builds fine but fails.

				var wg sync.WaitGroup
				defer wg.Wait()

				out := ioLogger(t, &wg)
				defer out.Close()

				opts := targ.opts
				passed, err := Test("github.com/tinygo-org/tinygo/tests/testing/fail", out, out, &opts, "")
				if err != nil {
					t.Errorf("test error: %v", err)
				}
				if passed {
					t.Error("test passed")
				}
			})

			if targ.name != "Host" {
				// Emulated tests are somewhat slow, and these do not need to be run across every platform.
				return
			}

			t.Run("Nothing", func(t *testing.T) {
				t.Parallel()

				// Test a package with no test files.

				var wg sync.WaitGroup
				defer wg.Wait()

				out := ioLogger(t, &wg)
				defer out.Close()

				var output bytes.Buffer
				opts := targ.opts
				passed, err := Test("github.com/tinygo-org/tinygo/tests/testing/nothing", io.MultiWriter(&output, out), out, &opts, "")
				if err != nil {
					t.Errorf("test error: %v", err)
				}
				if !passed {
					t.Error("test failed")
				}
				if !strings.Contains(output.String(), "[no test files]") {
					t.Error("missing [no test files] in output")
				}
			})

			t.Run("BuildErr", func(t *testing.T) {
				t.Parallel()

				// Test a package which fails to build.

				var wg sync.WaitGroup
				defer wg.Wait()

				out := ioLogger(t, &wg)
				defer out.Close()

				opts := targ.opts
				passed, err := Test("github.com/tinygo-org/tinygo/tests/testing/builderr", out, out, &opts, "")
				if err == nil {
					t.Error("test did not error")
				}
				if passed {
					t.Error("test passed")
				}
			})
		})
	}
}

func ioLogger(t *testing.T, wg *sync.WaitGroup) io.WriteCloser {
	r, w := io.Pipe()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer r.Close()

		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			t.Log(scanner.Text())
		}
	}()

	return w
}

func TestGetListOfPackages(t *testing.T) {
	opts := optionsFromTarget("", sema)
	tests := []struct {
		pkgs          []string
		expectedPkgs  []string
		expectesError bool
	}{
		{
			pkgs: []string{"./tests/testing/recurse/..."},
			expectedPkgs: []string{
				"github.com/tinygo-org/tinygo/tests/testing/recurse",
				"github.com/tinygo-org/tinygo/tests/testing/recurse/subdir",
			},
		},
		{
			pkgs: []string{"./tests/testing/pass"},
			expectedPkgs: []string{
				"github.com/tinygo-org/tinygo/tests/testing/pass",
			},
		},
		{
			pkgs:          []string{"./tests/testing"},
			expectesError: true,
		},
	}

	for _, test := range tests {
		actualPkgs, err := getListOfPackages(test.pkgs, &opts)
		if err != nil && !test.expectesError {
			t.Errorf("unexpected error: %v", err)
		} else if err == nil && test.expectesError {
			t.Error("expected error, but got none")
		}

		if !reflect.DeepEqual(test.expectedPkgs, actualPkgs) {
			t.Errorf("expected two slices to be equal, expected %v got %v", test.expectedPkgs, actualPkgs)
		}
	}
}

// This TestMain is necessary because TinyGo may also be invoked to run certain
// LLVM tools in a separate process. Not capturing these invocations would lead
// to recursive tests.
func TestMain(m *testing.M) {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "clang", "ld.lld", "wasm-ld":
			// Invoke a specific tool.
			err := builder.RunTool(os.Args[1], os.Args[2:]...)
			if err != nil {
				// The tool should have printed an error message already.
				// Don't print another error message here.
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	// Run normal tests.
	os.Exit(m.Run())
}
