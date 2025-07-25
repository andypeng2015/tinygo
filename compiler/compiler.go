package compiler

import (
	"debug/dwarf"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"math/bits"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tinygo-org/tinygo/compiler/llvmutil"
	"github.com/tinygo-org/tinygo/loader"
	"github.com/tinygo-org/tinygo/src/tinygo"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
	"tinygo.org/x/go-llvm"
)

func init() {
	llvm.InitializeAllTargets()
	llvm.InitializeAllTargetMCs()
	llvm.InitializeAllTargetInfos()
	llvm.InitializeAllAsmParsers()
	llvm.InitializeAllAsmPrinters()
}

// Config is the configuration for the compiler. Most settings should be copied
// directly from compileopts.Config, it recreated here to decouple the compiler
// package a bit and because it makes caching easier.
//
// This struct can be used for caching: if one of the flags here changes the
// code must be recompiled.
type Config struct {
	// Target and output information.
	Triple          string
	CPU             string
	Features        string
	ABI             string
	GOOS            string
	GOARCH          string
	BuildMode       string
	CodeModel       string
	RelocationModel string
	SizeLevel       int
	TinyGoVersion   string // for llvm.ident

	// Various compiler options that determine how code is generated.
	Scheduler          string
	AutomaticStackSize bool
	DefaultStackSize   uint64
	MaxStackAlloc      uint64
	NeedsStackObjects  bool
	Debug              bool // Whether to emit debug information in the LLVM module.
	Nobounds           bool // Whether to skip bounds checks
	PanicStrategy      string
}

// compilerContext contains function-independent data that should still be
// available while compiling every function. It is not strictly read-only, but
// must not contain function-dependent data such as an IR builder.
type compilerContext struct {
	*Config
	DumpSSA          bool
	mod              llvm.Module
	ctx              llvm.Context
	builder          llvm.Builder // only used for constant operations
	dibuilder        *llvm.DIBuilder
	cu               llvm.Metadata
	difiles          map[string]llvm.Metadata
	ditypes          map[types.Type]llvm.Metadata
	llvmTypes        typeutil.Map
	interfaceTypes   typeutil.Map
	machine          llvm.TargetMachine
	targetData       llvm.TargetData
	intType          llvm.Type
	dataPtrType      llvm.Type // pointer in address space 0
	funcPtrType      llvm.Type // pointer in function address space (1 for AVR, 0 elsewhere)
	funcPtrAddrSpace int
	uintptrType      llvm.Type
	program          *ssa.Program
	diagnostics      []error
	functionInfos    map[*ssa.Function]functionInfo
	astComments      map[string]*ast.CommentGroup
	embedGlobals     map[string][]*loader.EmbedFile
	pkg              *types.Package
	packageDir       string // directory for this package
	runtimePkg       *types.Package
}

// newCompilerContext returns a new compiler context ready for use, most
// importantly with a newly created LLVM context and module.
func newCompilerContext(moduleName string, machine llvm.TargetMachine, config *Config, dumpSSA bool) *compilerContext {
	c := &compilerContext{
		Config:        config,
		DumpSSA:       dumpSSA,
		difiles:       make(map[string]llvm.Metadata),
		ditypes:       make(map[types.Type]llvm.Metadata),
		machine:       machine,
		targetData:    machine.CreateTargetData(),
		functionInfos: map[*ssa.Function]functionInfo{},
		astComments:   map[string]*ast.CommentGroup{},
	}

	c.ctx = llvm.NewContext()
	c.builder = c.ctx.NewBuilder()
	c.mod = c.ctx.NewModule(moduleName)
	c.mod.SetTarget(config.Triple)
	c.mod.SetDataLayout(c.targetData.String())
	if c.Debug {
		c.dibuilder = llvm.NewDIBuilder(c.mod)
	}

	c.uintptrType = c.ctx.IntType(c.targetData.PointerSize() * 8)
	if c.targetData.PointerSize() <= 4 {
		// 8, 16, 32 bits targets
		c.intType = c.ctx.Int32Type()
	} else if c.targetData.PointerSize() == 8 {
		// 64 bits target
		c.intType = c.ctx.Int64Type()
	} else {
		panic("unknown pointer size")
	}
	c.dataPtrType = llvm.PointerType(c.ctx.Int8Type(), 0)

	dummyFuncType := llvm.FunctionType(c.ctx.VoidType(), nil, false)
	dummyFunc := llvm.AddFunction(c.mod, "tinygo.dummy", dummyFuncType)
	c.funcPtrAddrSpace = dummyFunc.Type().PointerAddressSpace()
	c.funcPtrType = dummyFunc.Type()
	dummyFunc.EraseFromParentAsFunction()

	return c
}

// Dispose everything related to the context, _except_ for the IR module (and
// the associated context).
func (c *compilerContext) dispose() {
	c.builder.Dispose()
}

// builder contains all information relevant to build a single function.
type builder struct {
	*compilerContext
	llvm.Builder
	fn                *ssa.Function
	llvmFnType        llvm.Type
	llvmFn            llvm.Value
	info              functionInfo
	locals            map[ssa.Value]llvm.Value            // local variables
	blockEntries      map[*ssa.BasicBlock]llvm.BasicBlock // a *ssa.BasicBlock may be split up
	blockExits        map[*ssa.BasicBlock]llvm.BasicBlock // these are the exit blocks
	currentBlock      *ssa.BasicBlock
	phis              []phiNode
	deferPtr          llvm.Value
	deferFrame        llvm.Value
	stackChainAlloca  llvm.Value
	landingpad        llvm.BasicBlock
	difunc            llvm.Metadata
	dilocals          map[*types.Var]llvm.Metadata
	initInlinedAt     llvm.Metadata            // fake inlinedAt position
	initPseudoFuncs   map[string]llvm.Metadata // fake "inlined" functions for proper init debug locations
	allDeferFuncs     []interface{}
	deferFuncs        map[*ssa.Function]int
	deferInvokeFuncs  map[string]int
	deferClosureFuncs map[*ssa.Function]int
	deferExprFuncs    map[ssa.Value]int
	selectRecvBuf     map[*ssa.Select]llvm.Value
	deferBuiltinFuncs map[ssa.Value]deferBuiltin
	runDefersBlock    []llvm.BasicBlock
	afterDefersBlock  []llvm.BasicBlock
}

func newBuilder(c *compilerContext, irbuilder llvm.Builder, f *ssa.Function) *builder {
	fnType, fn := c.getFunction(f)
	return &builder{
		compilerContext: c,
		Builder:         irbuilder,
		fn:              f,
		llvmFnType:      fnType,
		llvmFn:          fn,
		info:            c.getFunctionInfo(f),
		locals:          make(map[ssa.Value]llvm.Value),
		dilocals:        make(map[*types.Var]llvm.Metadata),
		blockEntries:    make(map[*ssa.BasicBlock]llvm.BasicBlock),
		blockExits:      make(map[*ssa.BasicBlock]llvm.BasicBlock),
	}
}

type deferBuiltin struct {
	callName string
	pos      token.Pos
	argTypes []types.Type
	callback int
}

type phiNode struct {
	ssa  *ssa.Phi
	llvm llvm.Value
}

// NewTargetMachine returns a new llvm.TargetMachine based on the passed-in
// configuration. It is used by the compiler and is needed for machine code
// emission.
func NewTargetMachine(config *Config) (llvm.TargetMachine, error) {
	target, err := llvm.GetTargetFromTriple(config.Triple)
	if err != nil {
		return llvm.TargetMachine{}, err
	}

	var codeModel llvm.CodeModel
	var relocationModel llvm.RelocMode

	switch config.CodeModel {
	case "default":
		codeModel = llvm.CodeModelDefault
	case "tiny":
		codeModel = llvm.CodeModelTiny
	case "small":
		codeModel = llvm.CodeModelSmall
	case "kernel":
		codeModel = llvm.CodeModelKernel
	case "medium":
		codeModel = llvm.CodeModelMedium
	case "large":
		codeModel = llvm.CodeModelLarge
	}

	switch config.RelocationModel {
	case "static":
		relocationModel = llvm.RelocStatic
	case "pic":
		relocationModel = llvm.RelocPIC
	case "dynamicnopic":
		relocationModel = llvm.RelocDynamicNoPic
	}

	machine := target.CreateTargetMachine(config.Triple, config.CPU, config.Features, llvm.CodeGenLevelDefault, relocationModel, codeModel)
	return machine, nil
}

// Sizes returns a types.Sizes appropriate for the given target machine. It
// includes the correct int size and alignment as is necessary for the Go
// typechecker.
func Sizes(machine llvm.TargetMachine) types.Sizes {
	targetData := machine.CreateTargetData()
	defer targetData.Dispose()

	var intWidth int
	if targetData.PointerSize() <= 4 {
		// 8, 16, 32 bits targets
		intWidth = 32
	} else if targetData.PointerSize() == 8 {
		// 64 bits target
		intWidth = 64
	} else {
		panic("unknown pointer size")
	}

	// Construct a complex128 type because that's likely the type with the
	// biggest alignment on most/all ABIs.
	ctx := llvm.NewContext()
	defer ctx.Dispose()
	complex128Type := ctx.StructType([]llvm.Type{ctx.DoubleType(), ctx.DoubleType()}, false)
	return &stdSizes{
		IntSize:  int64(intWidth / 8),
		PtrSize:  int64(targetData.PointerSize()),
		MaxAlign: int64(targetData.ABITypeAlignment(complex128Type)),
	}
}

// CompilePackage compiles a single package to a LLVM module.
func CompilePackage(moduleName string, pkg *loader.Package, ssaPkg *ssa.Package, machine llvm.TargetMachine, config *Config, dumpSSA bool) (llvm.Module, []error) {
	c := newCompilerContext(moduleName, machine, config, dumpSSA)
	defer c.dispose()
	c.packageDir = pkg.OriginalDir()
	c.embedGlobals = pkg.EmbedGlobals
	c.pkg = pkg.Pkg
	c.runtimePkg = ssaPkg.Prog.ImportedPackage("runtime").Pkg
	c.program = ssaPkg.Prog

	// Convert AST to SSA.
	ssaPkg.Build()

	// Initialize debug information.
	if c.Debug {
		c.cu = c.dibuilder.CreateCompileUnit(llvm.DICompileUnit{
			Language:  0xb, // DW_LANG_C99 (0xc, off-by-one?)
			File:      "<unknown>",
			Dir:       "",
			Producer:  "TinyGo",
			Optimized: true,
		})
	}

	// Load comments such as //go:extern on globals.
	c.loadASTComments(pkg)

	// Predeclare the runtime.alloc function, which is used by the wordpack
	// functionality.
	c.getFunction(c.program.ImportedPackage("runtime").Members["alloc"].(*ssa.Function))
	if c.NeedsStackObjects {
		// Predeclare trackPointer, which is used everywhere we use runtime.alloc.
		c.getFunction(c.program.ImportedPackage("runtime").Members["trackPointer"].(*ssa.Function))
	}

	// Compile all functions, methods, and global variables in this package.
	irbuilder := c.ctx.NewBuilder()
	defer irbuilder.Dispose()
	c.createPackage(irbuilder, ssaPkg)

	// see: https://reviews.llvm.org/D18355
	if c.Debug {
		c.mod.AddNamedMetadataOperand("llvm.module.flags",
			c.ctx.MDNode([]llvm.Metadata{
				llvm.ConstInt(c.ctx.Int32Type(), 2, false).ConstantAsMetadata(), // Warning on mismatch
				c.ctx.MDString("Debug Info Version"),
				llvm.ConstInt(c.ctx.Int32Type(), 3, false).ConstantAsMetadata(), // DWARF version
			}),
		)
		c.mod.AddNamedMetadataOperand("llvm.module.flags",
			c.ctx.MDNode([]llvm.Metadata{
				llvm.ConstInt(c.ctx.Int32Type(), 7, false).ConstantAsMetadata(), // Max on mismatch
				c.ctx.MDString("Dwarf Version"),
				llvm.ConstInt(c.ctx.Int32Type(), 4, false).ConstantAsMetadata(),
			}),
		)
		if c.TinyGoVersion != "" {
			// It is necessary to set llvm.ident, otherwise debugging on MacOS
			// won't work.
			c.mod.AddNamedMetadataOperand("llvm.ident",
				c.ctx.MDNode(([]llvm.Metadata{
					c.ctx.MDString("TinyGo version " + c.TinyGoVersion),
				})))
		}
		c.dibuilder.Finalize()
		c.dibuilder.Destroy()
	}

	// Add the "target-abi" flag, which is necessary on RISC-V otherwise it will
	// pick one that doesn't match the -mabi Clang flag.
	if c.ABI != "" {
		c.mod.AddNamedMetadataOperand("llvm.module.flags",
			c.ctx.MDNode([]llvm.Metadata{
				llvm.ConstInt(c.ctx.Int32Type(), 1, false).ConstantAsMetadata(), // Error on mismatch
				c.ctx.MDString("target-abi"),
				c.ctx.MDString(c.ABI),
			}),
		)
	}

	return c.mod, c.diagnostics
}

func (c *compilerContext) getRuntimeType(name string) types.Type {
	return c.runtimePkg.Scope().Lookup(name).(*types.TypeName).Type()
}

// getLLVMRuntimeType obtains a named type from the runtime package and returns
// it as a LLVM type, creating it if necessary. It is a shorthand for
// getLLVMType(getRuntimeType(name)).
func (c *compilerContext) getLLVMRuntimeType(name string) llvm.Type {
	return c.getLLVMType(c.getRuntimeType(name))
}

// getLLVMType returns a LLVM type for a Go type. It doesn't recreate already
// created types. This is somewhat important for performance, but especially
// important for named struct types (which should only be created once).
func (c *compilerContext) getLLVMType(goType types.Type) llvm.Type {
	// Try to load the LLVM type from the cache.
	// Note: *types.Named isn't unique when working with generics.
	// See https://github.com/golang/go/issues/53914
	// This is the reason for using typeutil.Map to lookup LLVM types for Go types.
	ival := c.llvmTypes.At(goType)
	if ival != nil {
		return ival.(llvm.Type)
	}
	// Not already created, so adding this type to the cache.
	llvmType := c.makeLLVMType(goType)
	c.llvmTypes.Set(goType, llvmType)
	return llvmType
}

// makeLLVMType creates a LLVM type for a Go type. Don't call this, use
// getLLVMType instead.
func (c *compilerContext) makeLLVMType(goType types.Type) llvm.Type {
	switch typ := types.Unalias(goType).(type) {
	case *types.Array:
		elemType := c.getLLVMType(typ.Elem())
		return llvm.ArrayType(elemType, int(typ.Len()))
	case *types.Basic:
		switch typ.Kind() {
		case types.Bool, types.UntypedBool:
			return c.ctx.Int1Type()
		case types.Int8, types.Uint8:
			return c.ctx.Int8Type()
		case types.Int16, types.Uint16:
			return c.ctx.Int16Type()
		case types.Int32, types.Uint32:
			return c.ctx.Int32Type()
		case types.Int, types.Uint:
			return c.intType
		case types.Int64, types.Uint64:
			return c.ctx.Int64Type()
		case types.Float32:
			return c.ctx.FloatType()
		case types.Float64:
			return c.ctx.DoubleType()
		case types.Complex64:
			return c.ctx.StructType([]llvm.Type{c.ctx.FloatType(), c.ctx.FloatType()}, false)
		case types.Complex128:
			return c.ctx.StructType([]llvm.Type{c.ctx.DoubleType(), c.ctx.DoubleType()}, false)
		case types.String, types.UntypedString:
			return c.getLLVMRuntimeType("_string")
		case types.Uintptr:
			return c.uintptrType
		case types.UnsafePointer:
			return c.dataPtrType
		default:
			panic("unknown basic type: " + typ.String())
		}
	case *types.Chan, *types.Map, *types.Pointer:
		return c.dataPtrType // all pointers are the same
	case *types.Interface:
		return c.getLLVMRuntimeType("_interface")
	case *types.Named:
		if st, ok := typ.Underlying().(*types.Struct); ok {
			// Structs are a special case. While other named types are ignored
			// in LLVM IR, named structs are implemented as named structs in
			// LLVM. This is because it is otherwise impossible to create
			// self-referencing types such as linked lists.
			llvmName := typ.String()
			llvmType := c.ctx.StructCreateNamed(llvmName)
			c.llvmTypes.Set(goType, llvmType) // avoid infinite recursion
			underlying := c.getLLVMType(st)
			llvmType.StructSetBody(underlying.StructElementTypes(), false)
			return llvmType
		}
		return c.getLLVMType(typ.Underlying())
	case *types.Signature: // function value
		return c.getFuncType(typ)
	case *types.Slice:
		members := []llvm.Type{
			c.dataPtrType,
			c.uintptrType, // len
			c.uintptrType, // cap
		}
		return c.ctx.StructType(members, false)
	case *types.Struct:
		members := make([]llvm.Type, typ.NumFields())
		for i := 0; i < typ.NumFields(); i++ {
			members[i] = c.getLLVMType(typ.Field(i).Type())
		}
		return c.ctx.StructType(members, false)
	case *types.TypeParam:
		return c.getLLVMType(typ.Underlying())
	case *types.Tuple:
		members := make([]llvm.Type, typ.Len())
		for i := 0; i < typ.Len(); i++ {
			members[i] = c.getLLVMType(typ.At(i).Type())
		}
		return c.ctx.StructType(members, false)
	default:
		panic("unknown type: " + goType.String())
	}
}

// Is this a pointer type of some sort? Can be unsafe.Pointer or any *T pointer.
func isPointer(typ types.Type) bool {
	if _, ok := typ.(*types.Pointer); ok {
		return true
	} else if typ, ok := typ.(*types.Basic); ok && typ.Kind() == types.UnsafePointer {
		return true
	} else {
		return false
	}
}

// Get the DWARF type for this Go type.
func (c *compilerContext) getDIType(typ types.Type) llvm.Metadata {
	if md, ok := c.ditypes[typ]; ok {
		return md
	}
	md := c.createDIType(typ)
	c.ditypes[typ] = md
	return md
}

// createDIType creates a new DWARF type. Don't call this function directly,
// call getDIType instead.
func (c *compilerContext) createDIType(typ types.Type) llvm.Metadata {
	llvmType := c.getLLVMType(typ)
	sizeInBytes := c.targetData.TypeAllocSize(llvmType)
	switch typ := typ.(type) {
	case *types.Alias:
		// Implement types.Alias just like types.Named: by treating them like a
		// C typedef.
		temporaryMDNode := c.dibuilder.CreateReplaceableCompositeType(llvm.Metadata{}, llvm.DIReplaceableCompositeType{
			Tag:         dwarf.TagTypedef,
			SizeInBits:  sizeInBytes * 8,
			AlignInBits: uint32(c.targetData.ABITypeAlignment(llvmType)) * 8,
		})
		c.ditypes[typ] = temporaryMDNode
		md := c.dibuilder.CreateTypedef(llvm.DITypedef{
			Type: c.getDIType(types.Unalias(typ)), // TODO: use typ.Rhs in Go 1.23
			Name: typ.String(),
		})
		temporaryMDNode.ReplaceAllUsesWith(md)
		return md
	case *types.Array:
		return c.dibuilder.CreateArrayType(llvm.DIArrayType{
			SizeInBits:  sizeInBytes * 8,
			AlignInBits: uint32(c.targetData.ABITypeAlignment(llvmType)) * 8,
			ElementType: c.getDIType(typ.Elem()),
			Subscripts: []llvm.DISubrange{
				{
					Lo:    0,
					Count: typ.Len(),
				},
			},
		})
	case *types.Basic:
		var encoding llvm.DwarfTypeEncoding
		if typ.Info()&types.IsBoolean != 0 {
			encoding = llvm.DW_ATE_boolean
		} else if typ.Info()&types.IsFloat != 0 {
			encoding = llvm.DW_ATE_float
		} else if typ.Info()&types.IsComplex != 0 {
			encoding = llvm.DW_ATE_complex_float
		} else if typ.Info()&types.IsUnsigned != 0 {
			encoding = llvm.DW_ATE_unsigned
		} else if typ.Info()&types.IsInteger != 0 {
			encoding = llvm.DW_ATE_signed
		} else if typ.Kind() == types.UnsafePointer {
			return c.dibuilder.CreatePointerType(llvm.DIPointerType{
				Name:         "unsafe.Pointer",
				SizeInBits:   c.targetData.TypeAllocSize(llvmType) * 8,
				AlignInBits:  uint32(c.targetData.ABITypeAlignment(llvmType)) * 8,
				AddressSpace: 0,
			})
		} else if typ.Info()&types.IsString != 0 {
			return c.dibuilder.CreateStructType(llvm.Metadata{}, llvm.DIStructType{
				Name:        "string",
				SizeInBits:  sizeInBytes * 8,
				AlignInBits: uint32(c.targetData.ABITypeAlignment(llvmType)) * 8,
				Elements: []llvm.Metadata{
					c.dibuilder.CreateMemberType(llvm.Metadata{}, llvm.DIMemberType{
						Name:         "ptr",
						SizeInBits:   c.targetData.TypeAllocSize(c.dataPtrType) * 8,
						AlignInBits:  uint32(c.targetData.ABITypeAlignment(c.dataPtrType)) * 8,
						OffsetInBits: 0,
						Type:         c.getDIType(types.NewPointer(types.Typ[types.Byte])),
					}),
					c.dibuilder.CreateMemberType(llvm.Metadata{}, llvm.DIMemberType{
						Name:         "len",
						SizeInBits:   c.targetData.TypeAllocSize(c.uintptrType) * 8,
						AlignInBits:  uint32(c.targetData.ABITypeAlignment(c.uintptrType)) * 8,
						OffsetInBits: c.targetData.ElementOffset(llvmType, 1) * 8,
						Type:         c.getDIType(types.Typ[types.Uintptr]),
					}),
				},
			})
		} else {
			panic("unknown basic type")
		}
		return c.dibuilder.CreateBasicType(llvm.DIBasicType{
			Name:       typ.String(),
			SizeInBits: sizeInBytes * 8,
			Encoding:   encoding,
		})
	case *types.Chan:
		return c.getDIType(types.NewPointer(c.program.ImportedPackage("runtime").Members["channel"].(*ssa.Type).Type()))
	case *types.Interface:
		return c.getDIType(c.program.ImportedPackage("runtime").Members["_interface"].(*ssa.Type).Type())
	case *types.Map:
		return c.getDIType(types.NewPointer(c.program.ImportedPackage("runtime").Members["hashmap"].(*ssa.Type).Type()))
	case *types.Named:
		// Placeholder metadata node, to be replaced afterwards.
		temporaryMDNode := c.dibuilder.CreateReplaceableCompositeType(llvm.Metadata{}, llvm.DIReplaceableCompositeType{
			Tag:         dwarf.TagTypedef,
			SizeInBits:  sizeInBytes * 8,
			AlignInBits: uint32(c.targetData.ABITypeAlignment(llvmType)) * 8,
		})
		c.ditypes[typ] = temporaryMDNode
		md := c.dibuilder.CreateTypedef(llvm.DITypedef{
			Type: c.getDIType(typ.Underlying()),
			Name: typ.String(),
		})
		temporaryMDNode.ReplaceAllUsesWith(md)
		return md
	case *types.Pointer:
		return c.dibuilder.CreatePointerType(llvm.DIPointerType{
			Pointee:      c.getDIType(typ.Elem()),
			SizeInBits:   c.targetData.TypeAllocSize(llvmType) * 8,
			AlignInBits:  uint32(c.targetData.ABITypeAlignment(llvmType)) * 8,
			AddressSpace: 0,
		})
	case *types.Signature:
		// actually a closure
		fields := llvmType.StructElementTypes()
		return c.dibuilder.CreateStructType(llvm.Metadata{}, llvm.DIStructType{
			SizeInBits:  sizeInBytes * 8,
			AlignInBits: uint32(c.targetData.ABITypeAlignment(llvmType)) * 8,
			Elements: []llvm.Metadata{
				c.dibuilder.CreateMemberType(llvm.Metadata{}, llvm.DIMemberType{
					Name:         "context",
					SizeInBits:   c.targetData.TypeAllocSize(fields[1]) * 8,
					AlignInBits:  uint32(c.targetData.ABITypeAlignment(fields[1])) * 8,
					OffsetInBits: 0,
					Type:         c.getDIType(types.Typ[types.UnsafePointer]),
				}),
				c.dibuilder.CreateMemberType(llvm.Metadata{}, llvm.DIMemberType{
					Name:         "fn",
					SizeInBits:   c.targetData.TypeAllocSize(fields[0]) * 8,
					AlignInBits:  uint32(c.targetData.ABITypeAlignment(fields[0])) * 8,
					OffsetInBits: c.targetData.ElementOffset(llvmType, 1) * 8,
					Type:         c.getDIType(types.Typ[types.UnsafePointer]),
				}),
			},
		})
	case *types.Slice:
		fields := llvmType.StructElementTypes()
		return c.dibuilder.CreateStructType(llvm.Metadata{}, llvm.DIStructType{
			Name:        typ.String(),
			SizeInBits:  sizeInBytes * 8,
			AlignInBits: uint32(c.targetData.ABITypeAlignment(llvmType)) * 8,
			Elements: []llvm.Metadata{
				c.dibuilder.CreateMemberType(llvm.Metadata{}, llvm.DIMemberType{
					Name:         "ptr",
					SizeInBits:   c.targetData.TypeAllocSize(fields[0]) * 8,
					AlignInBits:  uint32(c.targetData.ABITypeAlignment(fields[0])) * 8,
					OffsetInBits: 0,
					Type:         c.getDIType(types.NewPointer(typ.Elem())),
				}),
				c.dibuilder.CreateMemberType(llvm.Metadata{}, llvm.DIMemberType{
					Name:         "len",
					SizeInBits:   c.targetData.TypeAllocSize(c.uintptrType) * 8,
					AlignInBits:  uint32(c.targetData.ABITypeAlignment(c.uintptrType)) * 8,
					OffsetInBits: c.targetData.ElementOffset(llvmType, 1) * 8,
					Type:         c.getDIType(types.Typ[types.Uintptr]),
				}),
				c.dibuilder.CreateMemberType(llvm.Metadata{}, llvm.DIMemberType{
					Name:         "cap",
					SizeInBits:   c.targetData.TypeAllocSize(c.uintptrType) * 8,
					AlignInBits:  uint32(c.targetData.ABITypeAlignment(c.uintptrType)) * 8,
					OffsetInBits: c.targetData.ElementOffset(llvmType, 2) * 8,
					Type:         c.getDIType(types.Typ[types.Uintptr]),
				}),
			},
		})
	case *types.Struct:
		elements := make([]llvm.Metadata, typ.NumFields())
		for i := range elements {
			field := typ.Field(i)
			fieldType := field.Type()
			llvmField := c.getLLVMType(fieldType)
			elements[i] = c.dibuilder.CreateMemberType(llvm.Metadata{}, llvm.DIMemberType{
				Name:         field.Name(),
				SizeInBits:   c.targetData.TypeAllocSize(llvmField) * 8,
				AlignInBits:  uint32(c.targetData.ABITypeAlignment(llvmField)) * 8,
				OffsetInBits: c.targetData.ElementOffset(llvmType, i) * 8,
				Type:         c.getDIType(fieldType),
			})
		}
		md := c.dibuilder.CreateStructType(llvm.Metadata{}, llvm.DIStructType{
			SizeInBits:  sizeInBytes * 8,
			AlignInBits: uint32(c.targetData.ABITypeAlignment(llvmType)) * 8,
			Elements:    elements,
		})
		return md
	case *types.TypeParam:
		return c.getDIType(typ.Underlying())
	default:
		panic("unknown type while generating DWARF debug type: " + typ.String())
	}
}

// setDebugLocation sets the current debug location for the builder.
func (b *builder) setDebugLocation(pos token.Pos) {
	if pos == token.NoPos {
		// No debug information available for this instruction.
		b.SetCurrentDebugLocation(0, 0, b.difunc, llvm.Metadata{})
		return
	}

	position := b.program.Fset.Position(pos)
	if b.fn.Synthetic == "package initializer" {
		// Package initializers are treated specially, because while individual
		// Go SSA instructions have file/line/col information, the parent
		// function does not. LLVM doesn't store filename information per
		// instruction, only per function. We work around this difference by
		// creating a fake DIFunction for each Go file and say that the
		// instruction really came from that (fake) function but was inlined in
		// the package initializer function.
		position := b.program.Fset.Position(pos)
		name := filepath.Base(position.Filename)
		difunc, ok := b.initPseudoFuncs[name]
		if !ok {
			diFuncType := b.dibuilder.CreateSubroutineType(llvm.DISubroutineType{
				File: b.getDIFile(position.Filename),
			})
			difunc = b.dibuilder.CreateFunction(b.getDIFile(position.Filename), llvm.DIFunction{
				Name:         b.fn.RelString(nil) + "#" + name,
				File:         b.getDIFile(position.Filename),
				Line:         0,
				Type:         diFuncType,
				LocalToUnit:  true,
				IsDefinition: true,
				ScopeLine:    0,
				Flags:        llvm.FlagPrototyped,
				Optimized:    true,
			})
			b.initPseudoFuncs[name] = difunc
		}
		b.SetCurrentDebugLocation(uint(position.Line), uint(position.Column), difunc, b.initInlinedAt)
		return
	}

	// Regular debug information.
	b.SetCurrentDebugLocation(uint(position.Line), uint(position.Column), b.difunc, llvm.Metadata{})
}

// getLocalVariable returns a debug info entry for a local variable, which may
// either be a parameter or a regular variable. It will create a new metadata
// entry if there isn't one for the variable yet.
func (b *builder) getLocalVariable(variable *types.Var) llvm.Metadata {
	if dilocal, ok := b.dilocals[variable]; ok {
		// DILocalVariable was already created, return it directly.
		return dilocal
	}

	pos := b.program.Fset.Position(variable.Pos())

	// Check whether this is a function parameter.
	for i, param := range b.fn.Params {
		if param.Object().(*types.Var) == variable {
			// Yes it is, create it as a function parameter.
			dilocal := b.dibuilder.CreateParameterVariable(b.difunc, llvm.DIParameterVariable{
				Name:           param.Name(),
				File:           b.getDIFile(pos.Filename),
				Line:           pos.Line,
				Type:           b.getDIType(param.Type()),
				AlwaysPreserve: true,
				ArgNo:          i + 1,
			})
			b.dilocals[variable] = dilocal
			return dilocal
		}
	}

	// No, it's not a parameter. Create a regular (auto) variable.
	dilocal := b.dibuilder.CreateAutoVariable(b.difunc, llvm.DIAutoVariable{
		Name:           variable.Name(),
		File:           b.getDIFile(pos.Filename),
		Line:           pos.Line,
		Type:           b.getDIType(variable.Type()),
		AlwaysPreserve: true,
	})
	b.dilocals[variable] = dilocal
	return dilocal
}

// attachDebugInfo adds debug info to a function declaration. It returns the
// DISubprogram metadata node.
func (c *compilerContext) attachDebugInfo(f *ssa.Function) llvm.Metadata {
	pos := c.program.Fset.Position(f.Syntax().Pos())
	_, fn := c.getFunction(f)
	return c.attachDebugInfoRaw(f, fn, "", pos.Filename, pos.Line)
}

// attachDebugInfo adds debug info to a function declaration. It returns the
// DISubprogram metadata node. This method allows some more control over how
// debug info is added to the function.
func (c *compilerContext) attachDebugInfoRaw(f *ssa.Function, llvmFn llvm.Value, suffix, filename string, line int) llvm.Metadata {
	// Debug info for this function.
	params := getParams(f.Signature)
	diparams := make([]llvm.Metadata, 0, len(params))
	for _, param := range params {
		diparams = append(diparams, c.getDIType(param.Type()))
	}
	diFuncType := c.dibuilder.CreateSubroutineType(llvm.DISubroutineType{
		File:       c.getDIFile(filename),
		Parameters: diparams,
		Flags:      0, // ?
	})
	difunc := c.dibuilder.CreateFunction(c.getDIFile(filename), llvm.DIFunction{
		Name:         f.RelString(nil) + suffix,
		LinkageName:  c.getFunctionInfo(f).linkName + suffix,
		File:         c.getDIFile(filename),
		Line:         line,
		Type:         diFuncType,
		LocalToUnit:  true,
		IsDefinition: true,
		ScopeLine:    0,
		Flags:        llvm.FlagPrototyped,
		Optimized:    true,
	})
	llvmFn.SetSubprogram(difunc)
	return difunc
}

// getDIFile returns a DIFile metadata node for the given filename. It tries to
// use one that was already created, otherwise it falls back to creating a new
// one.
func (c *compilerContext) getDIFile(filename string) llvm.Metadata {
	if _, ok := c.difiles[filename]; !ok {
		dir, file := filepath.Split(filename)
		if dir != "" {
			dir = dir[:len(dir)-1]
		}
		c.difiles[filename] = c.dibuilder.CreateFile(file, dir)
	}
	return c.difiles[filename]
}

// createPackage builds the LLVM IR for all types, methods, and global variables
// in the given package.
func (c *compilerContext) createPackage(irbuilder llvm.Builder, pkg *ssa.Package) {
	// Sort by position, so that the order of the functions in the IR matches
	// the order of functions in the source file. This is useful for testing,
	// for example.
	var members []string
	for name := range pkg.Members {
		members = append(members, name)
	}
	sort.Slice(members, func(i, j int) bool {
		iPos := pkg.Members[members[i]].Pos()
		jPos := pkg.Members[members[j]].Pos()
		if i == j {
			// Cannot sort by pos, so do it by name.
			return members[i] < members[j]
		}
		return iPos < jPos
	})

	// Define all functions.
	for _, name := range members {
		member := pkg.Members[name]
		switch member := member.(type) {
		case *ssa.Function:
			if member.TypeParams() != nil {
				// Do not try to build generic (non-instantiated) functions.
				continue
			}
			// Create the function definition.
			b := newBuilder(c, irbuilder, member)
			if _, ok := mathToLLVMMapping[member.RelString(nil)]; ok {
				// The body of this function (if there is one) is ignored and
				// replaced with a LLVM intrinsic call.
				b.defineMathOp()
				continue
			}
			if ok := b.defineMathBitsIntrinsic(); ok {
				// Like a math intrinsic, the body of this function was replaced
				// with a LLVM intrinsic.
				continue
			}
			if member.Blocks == nil {
				// Try to define this as an intrinsic function.
				b.defineIntrinsicFunction()
				// It might not be an intrinsic function but simply an external
				// function (defined via //go:linkname). Leave it undefined in
				// that case.
				continue
			}
			b.createFunction()
		case *ssa.Type:
			if types.IsInterface(member.Type()) {
				// Interfaces don't have concrete methods.
				continue
			}
			if _, isalias := member.Type().(*types.Alias); isalias {
				// Aliases don't need to be redefined, since they just refer to
				// an already existing type whose methods will be defined.
				continue
			}

			// Named type. We should make sure all methods are created.
			// This includes both functions with pointer receivers and those
			// without.
			methods := getAllMethods(pkg.Prog, member.Type())
			methods = append(methods, getAllMethods(pkg.Prog, types.NewPointer(member.Type()))...)
			for _, method := range methods {
				// Parse this method.
				fn := pkg.Prog.MethodValue(method)
				if fn == nil {
					continue // probably a generic method
				}
				if member.Type().String() != member.String() {
					// This is a member on a type alias. Do not build such a
					// function.
					continue
				}
				if fn.Blocks == nil {
					continue // external function
				}
				if fn.Synthetic != "" && fn.Synthetic != "package initializer" {
					// This function is a kind of wrapper function (created by
					// the ssa package, not appearing in the source code) that
					// is created by the getFunction method as needed.
					// Therefore, don't build it here to avoid "function
					// redeclared" errors.
					continue
				}
				// Create the function definition.
				b := newBuilder(c, irbuilder, fn)
				b.createFunction()
			}
		case *ssa.Global:
			// Global variable.
			info := c.getGlobalInfo(member)
			global := c.getGlobal(member)
			if files, ok := c.embedGlobals[member.Name()]; ok {
				c.createEmbedGlobal(member, global, files)
			} else if !info.extern {
				global.SetInitializer(llvm.ConstNull(global.GlobalValueType()))
				global.SetVisibility(llvm.HiddenVisibility)
				if info.section != "" {
					global.SetSection(info.section)
				}
			}
		}
	}

	// Add forwarding functions for functions that would otherwise be
	// implemented in assembly.
	for _, name := range members {
		member := pkg.Members[name]
		switch member := member.(type) {
		case *ssa.Function:
			if member.Blocks != nil {
				continue // external function
			}
			info := c.getFunctionInfo(member)
			if aliasName, ok := stdlibAliases[info.linkName]; ok {
				alias := c.mod.NamedFunction(aliasName)
				if alias.IsNil() {
					// Shouldn't happen, but perhaps best to just ignore.
					// The error will be a link error, if there is an error.
					continue
				}
				b := newBuilder(c, irbuilder, member)
				b.createAlias(alias)
			}
		}
	}
}

// createEmbedGlobal creates an initializer for a //go:embed global variable.
func (c *compilerContext) createEmbedGlobal(member *ssa.Global, global llvm.Value, files []*loader.EmbedFile) {
	switch typ := member.Type().(*types.Pointer).Elem().Underlying().(type) {
	case *types.Basic:
		// String type.
		if typ.Kind() != types.String {
			// This is checked at the AST level, so should be unreachable.
			panic("expected a string type")
		}
		if len(files) != 1 {
			c.addError(member.Pos(), fmt.Sprintf("//go:embed for a string should be given exactly one file, got %d", len(files)))
			return
		}
		strObj := c.getEmbedFileString(files[0])
		global.SetInitializer(strObj)
		global.SetVisibility(llvm.HiddenVisibility)

	case *types.Slice:
		if typ.Elem().Underlying().(*types.Basic).Kind() != types.Byte {
			// This is checked at the AST level, so should be unreachable.
			panic("expected a byte slice")
		}
		if len(files) != 1 {
			c.addError(member.Pos(), fmt.Sprintf("//go:embed for a string should be given exactly one file, got %d", len(files)))
			return
		}
		file := files[0]
		bufferValue := c.ctx.ConstString(string(file.Data), false)
		bufferGlobal := llvm.AddGlobal(c.mod, bufferValue.Type(), c.pkg.Path()+"$embedslice")
		bufferGlobal.SetInitializer(bufferValue)
		bufferGlobal.SetLinkage(llvm.InternalLinkage)
		bufferGlobal.SetAlignment(1)
		slicePtr := llvm.ConstInBoundsGEP(bufferValue.Type(), bufferGlobal, []llvm.Value{
			llvm.ConstInt(c.uintptrType, 0, false),
			llvm.ConstInt(c.uintptrType, 0, false),
		})
		sliceLen := llvm.ConstInt(c.uintptrType, file.Size, false)
		sliceObj := c.ctx.ConstStruct([]llvm.Value{slicePtr, sliceLen, sliceLen}, false)
		global.SetInitializer(sliceObj)
		global.SetVisibility(llvm.HiddenVisibility)

		if c.Debug {
			// Add debug info to the slice backing array.
			position := c.program.Fset.Position(member.Pos())
			diglobal := c.dibuilder.CreateGlobalVariableExpression(llvm.Metadata{}, llvm.DIGlobalVariableExpression{
				File:        c.getDIFile(position.Filename),
				Line:        position.Line,
				Type:        c.getDIType(types.NewArray(types.Typ[types.Byte], int64(len(file.Data)))),
				LocalToUnit: true,
				Expr:        c.dibuilder.CreateExpression(nil),
			})
			bufferGlobal.AddMetadata(0, diglobal)
		}

	case *types.Struct:
		// Assume this is an embed.FS struct:
		// https://cs.opensource.google/go/go/+/refs/tags/go1.18.2:src/embed/embed.go;l=148
		// It looks like this:
		//   type FS struct {
		//       files *file
		//   }

		// Make a slice of the files, as they will appear in the binary. They
		// are sorted in a special way to allow for binary searches, see
		// src/embed/embed.go for details.
		dirset := map[string]struct{}{}
		var allFiles []*loader.EmbedFile
		for _, file := range files {
			allFiles = append(allFiles, file)
			dirname := file.Name
			for {
				dirname, _ = path.Split(path.Clean(dirname))
				if dirname == "" {
					break
				}
				if _, ok := dirset[dirname]; ok {
					break
				}
				dirset[dirname] = struct{}{}
				allFiles = append(allFiles, &loader.EmbedFile{
					Name: dirname,
				})
			}
		}
		sort.Slice(allFiles, func(i, j int) bool {
			dir1, name1 := path.Split(path.Clean(allFiles[i].Name))
			dir2, name2 := path.Split(path.Clean(allFiles[j].Name))
			if dir1 != dir2 {
				return dir1 < dir2
			}
			return name1 < name2
		})

		// Make the backing array for the []files slice. This is a LLVM global.
		embedFileStructType := typ.Field(0).Type().(*types.Pointer).Elem().(*types.Slice).Elem()
		llvmEmbedFileStructType := c.getLLVMType(embedFileStructType)
		var fileStructs []llvm.Value
		for _, file := range allFiles {
			fileStruct := llvm.ConstNull(llvmEmbedFileStructType)
			name := c.createConst(ssa.NewConst(constant.MakeString(file.Name), types.Typ[types.String]), getPos(member))
			fileStruct = c.builder.CreateInsertValue(fileStruct, name, 0, "") // "name" field
			if file.Hash != "" {
				data := c.getEmbedFileString(file)
				fileStruct = c.builder.CreateInsertValue(fileStruct, data, 1, "") // "data" field
			}
			fileStructs = append(fileStructs, fileStruct)
		}
		sliceDataInitializer := llvm.ConstArray(llvmEmbedFileStructType, fileStructs)
		sliceDataGlobal := llvm.AddGlobal(c.mod, sliceDataInitializer.Type(), c.pkg.Path()+"$embedfsfiles")
		sliceDataGlobal.SetInitializer(sliceDataInitializer)
		sliceDataGlobal.SetLinkage(llvm.InternalLinkage)
		sliceDataGlobal.SetGlobalConstant(true)
		sliceDataGlobal.SetUnnamedAddr(true)
		sliceDataGlobal.SetAlignment(c.targetData.ABITypeAlignment(sliceDataInitializer.Type()))
		if c.Debug {
			// Add debug information for code size attribution (among others).
			position := c.program.Fset.Position(member.Pos())
			diglobal := c.dibuilder.CreateGlobalVariableExpression(llvm.Metadata{}, llvm.DIGlobalVariableExpression{
				File:        c.getDIFile(position.Filename),
				Line:        position.Line,
				Type:        c.getDIType(types.NewArray(embedFileStructType, int64(len(allFiles)))),
				LocalToUnit: true,
				Expr:        c.dibuilder.CreateExpression(nil),
			})
			sliceDataGlobal.AddMetadata(0, diglobal)
		}

		// Create the slice object itself.
		// Because embed.FS refers to it as *[]embed.file instead of a plain
		// []embed.file, we have to store this as a global.
		slicePtr := llvm.ConstInBoundsGEP(sliceDataInitializer.Type(), sliceDataGlobal, []llvm.Value{
			llvm.ConstInt(c.uintptrType, 0, false),
			llvm.ConstInt(c.uintptrType, 0, false),
		})
		sliceLen := llvm.ConstInt(c.uintptrType, uint64(len(fileStructs)), false)
		sliceInitializer := c.ctx.ConstStruct([]llvm.Value{slicePtr, sliceLen, sliceLen}, false)
		sliceGlobal := llvm.AddGlobal(c.mod, sliceInitializer.Type(), c.pkg.Path()+"$embedfsslice")
		sliceGlobal.SetInitializer(sliceInitializer)
		sliceGlobal.SetLinkage(llvm.InternalLinkage)
		sliceGlobal.SetGlobalConstant(true)
		sliceGlobal.SetUnnamedAddr(true)
		sliceGlobal.SetAlignment(c.targetData.ABITypeAlignment(sliceInitializer.Type()))
		if c.Debug {
			position := c.program.Fset.Position(member.Pos())
			diglobal := c.dibuilder.CreateGlobalVariableExpression(llvm.Metadata{}, llvm.DIGlobalVariableExpression{
				File:        c.getDIFile(position.Filename),
				Line:        position.Line,
				Type:        c.getDIType(types.NewSlice(embedFileStructType)),
				LocalToUnit: true,
				Expr:        c.dibuilder.CreateExpression(nil),
			})
			sliceGlobal.AddMetadata(0, diglobal)
		}

		// Define the embed.FS struct. It has only one field: the files (as a
		// *[]embed.file).
		globalInitializer := llvm.ConstNull(c.getLLVMType(member.Type().(*types.Pointer).Elem()))
		globalInitializer = c.builder.CreateInsertValue(globalInitializer, sliceGlobal, 0, "")
		global.SetInitializer(globalInitializer)
		global.SetVisibility(llvm.HiddenVisibility)
		global.SetAlignment(c.targetData.ABITypeAlignment(globalInitializer.Type()))
	}
}

// getEmbedFileString returns the (constant) string object with the contents of
// the given file. This is a llvm.Value of a regular Go string.
func (c *compilerContext) getEmbedFileString(file *loader.EmbedFile) llvm.Value {
	dataGlobalName := "embed/file_" + file.Hash
	dataGlobal := c.mod.NamedGlobal(dataGlobalName)
	dataGlobalType := llvm.ArrayType(c.ctx.Int8Type(), int(file.Size))
	if dataGlobal.IsNil() {
		dataGlobal = llvm.AddGlobal(c.mod, dataGlobalType, dataGlobalName)
	}
	strPtr := llvm.ConstInBoundsGEP(dataGlobalType, dataGlobal, []llvm.Value{
		llvm.ConstInt(c.uintptrType, 0, false),
		llvm.ConstInt(c.uintptrType, 0, false),
	})
	strLen := llvm.ConstInt(c.uintptrType, file.Size, false)
	return llvm.ConstNamedStruct(c.getLLVMRuntimeType("_string"), []llvm.Value{strPtr, strLen})
}

// Start defining a function so that it can be filled with instructions: load
// parameters, create basic blocks, and set up debug information.
// This is separated out from createFunction() so that it is also usable to
// define compiler intrinsics like the atomic operations in sync/atomic.
func (b *builder) createFunctionStart(intrinsic bool) {
	if b.DumpSSA {
		fmt.Printf("\nfunc %s:\n", b.fn)
	}
	if !b.llvmFn.IsDeclaration() {
		errValue := b.llvmFn.Name() + " redeclared in this program"
		fnPos := getPosition(b.llvmFn)
		if fnPos.IsValid() {
			errValue += "\n\tprevious declaration at " + fnPos.String()
		}
		b.addError(b.fn.Pos(), errValue)
		return
	}

	b.addStandardDefinedAttributes(b.llvmFn)
	if !b.info.exported {
		// Do not set visibility for local linkage (internal or private).
		// Otherwise a "local linkage requires default visibility"
		// assertion error in llvm-project/llvm/include/llvm/IR/GlobalValue.h:236
		// is thrown.
		if b.llvmFn.Linkage() != llvm.InternalLinkage &&
			b.llvmFn.Linkage() != llvm.PrivateLinkage {
			b.llvmFn.SetVisibility(llvm.HiddenVisibility)
		}
		b.llvmFn.SetUnnamedAddr(true)
	}
	if b.info.section != "" {
		b.llvmFn.SetSection(b.info.section)
	}
	if b.info.exported && strings.HasPrefix(b.Triple, "wasm") {
		// Set the exported name. This is necessary for WebAssembly because
		// otherwise the function is not exported.
		functionAttr := b.ctx.CreateStringAttribute("wasm-export-name", b.info.linkName)
		b.llvmFn.AddFunctionAttr(functionAttr)
		// Unlike most targets, exported functions are actually visible in
		// WebAssembly (even if it's not called from within the WebAssembly
		// module). But LTO generally optimizes such functions away. Therefore,
		// exported functions must be explicitly marked as used.
		llvmutil.AppendToGlobal(b.mod, "llvm.used", b.llvmFn)
	}

	// Some functions have a pragma controlling the inlining level.
	switch b.info.inline {
	case inlineHint:
		// Add LLVM inline hint to functions with //go:inline pragma.
		inline := b.ctx.CreateEnumAttribute(llvm.AttributeKindID("inlinehint"), 0)
		b.llvmFn.AddFunctionAttr(inline)
	case inlineNone:
		// Add LLVM attribute to always avoid inlining this function.
		noinline := b.ctx.CreateEnumAttribute(llvm.AttributeKindID("noinline"), 0)
		b.llvmFn.AddFunctionAttr(noinline)
	}

	if b.info.interrupt {
		// Mark this function as an interrupt.
		// This is necessary on MCUs that don't push caller saved registers when
		// entering an interrupt, such as on AVR.
		if strings.HasPrefix(b.Triple, "avr") {
			b.llvmFn.AddFunctionAttr(b.ctx.CreateStringAttribute("signal", ""))
		} else {
			b.addError(b.fn.Pos(), "//go:interrupt not supported on this architecture")
		}
	}

	// Add debug info, if needed.
	if b.Debug {
		if b.fn.Synthetic == "package initializer" {
			// Package initializer functions have no debug info. Create some
			// fake debug info to at least have *something*.
			b.difunc = b.attachDebugInfoRaw(b.fn, b.llvmFn, "", b.packageDir, 0)
		} else if b.fn.Syntax() != nil {
			// Create debug info file if needed.
			b.difunc = b.attachDebugInfo(b.fn)
		}
		b.setDebugLocation(b.fn.Pos())
	}

	// Pre-create all basic blocks in the function.
	var entryBlock llvm.BasicBlock
	if intrinsic {
		// This function isn't defined in Go SSA. It is probably a compiler
		// intrinsic (like an atomic operation). Create the entry block
		// manually.
		entryBlock = b.ctx.AddBasicBlock(b.llvmFn, "entry")
	} else {
		for _, block := range b.fn.DomPreorder() {
			llvmBlock := b.ctx.AddBasicBlock(b.llvmFn, block.Comment)
			b.blockEntries[block] = llvmBlock
			b.blockExits[block] = llvmBlock
		}
		// Normal functions have an entry block.
		entryBlock = b.blockEntries[b.fn.Blocks[0]]
	}
	b.SetInsertPointAtEnd(entryBlock)

	if b.fn.Synthetic == "package initializer" {
		b.initPseudoFuncs = make(map[string]llvm.Metadata)

		// Create a fake 'inlined at' metadata node.
		// See setDebugLocation for details.
		alloca := b.CreateAlloca(b.uintptrType, "")
		b.initInlinedAt = alloca.InstructionDebugLoc()
		alloca.EraseFromParentAsInstruction()
	}

	// Load function parameters
	llvmParamIndex := 0
	for _, param := range b.fn.Params {
		llvmType := b.getLLVMType(param.Type())
		fields := make([]llvm.Value, 0, 1)
		for _, info := range b.expandFormalParamType(llvmType, param.Name(), param.Type()) {
			param := b.llvmFn.Param(llvmParamIndex)
			param.SetName(info.name)
			fields = append(fields, param)
			llvmParamIndex++
		}
		b.locals[param] = b.collapseFormalParam(llvmType, fields)

		// Add debug information to this parameter (if available)
		if b.Debug && b.fn.Syntax() != nil {
			dbgParam := b.getLocalVariable(param.Object().(*types.Var))
			loc := b.GetCurrentDebugLocation()
			if len(fields) == 1 {
				expr := b.dibuilder.CreateExpression(nil)
				b.dibuilder.InsertValueAtEnd(fields[0], dbgParam, expr, loc, entryBlock)
			} else {
				fieldOffsets := b.expandFormalParamOffsets(llvmType)
				for i, field := range fields {
					expr := b.dibuilder.CreateExpression([]uint64{
						0x1000,              // DW_OP_LLVM_fragment
						fieldOffsets[i] * 8, // offset in bits
						b.targetData.TypeAllocSize(field.Type()) * 8, // size in bits
					})
					b.dibuilder.InsertValueAtEnd(field, dbgParam, expr, loc, entryBlock)
				}
			}
		}
	}

	// Load free variables from the context. This is a closure (or bound
	// method).
	var context llvm.Value
	if !b.info.exported {
		context = b.llvmFn.LastParam()
		context.SetName("context")
	}
	if len(b.fn.FreeVars) != 0 {
		// Get a list of all variable types in the context.
		freeVarTypes := make([]llvm.Type, len(b.fn.FreeVars))
		for i, freeVar := range b.fn.FreeVars {
			freeVarTypes[i] = b.getLLVMType(freeVar.Type())
		}

		// Load each free variable from the context pointer.
		// A free variable is always a pointer when this is a closure, but it
		// can be another type when it is a wrapper for a bound method (these
		// wrappers are generated by the ssa package).
		for i, val := range b.emitPointerUnpack(context, freeVarTypes) {
			b.locals[b.fn.FreeVars[i]] = val
		}
	}

	if b.fn.Recover != nil {
		// This function has deferred function calls. Set some things up for
		// them.
		b.deferInitFunc()
	}

	if b.NeedsStackObjects {
		// Create a dummy alloca that will be used in runtime.trackPointer.
		// It is necessary to pass a dummy alloca to runtime.trackPointer
		// because runtime.trackPointer is replaced by an alloca store.
		b.stackChainAlloca = b.CreateAlloca(b.ctx.Int8Type(), "stackalloc")
	}
}

// createFunction builds the LLVM IR implementation for this function. The
// function must not yet be defined, otherwise this function will create a
// diagnostic.
func (b *builder) createFunction() {
	b.createFunctionStart(false)

	// Fill blocks with instructions.
	for _, block := range b.fn.DomPreorder() {
		if b.DumpSSA {
			fmt.Printf("%d: %s:\n", block.Index, block.Comment)
		}
		b.SetInsertPointAtEnd(b.blockEntries[block])
		b.currentBlock = block
		for _, instr := range block.Instrs {
			if instr, ok := instr.(*ssa.DebugRef); ok {
				if !b.Debug {
					continue
				}
				object := instr.Object()
				variable, ok := object.(*types.Var)
				if !ok {
					// Not a local variable.
					continue
				}
				if instr.IsAddr {
					// TODO, this may happen for *ssa.Alloc and *ssa.FieldAddr
					// for example.
					continue
				}
				dbgVar := b.getLocalVariable(variable)
				pos := b.program.Fset.Position(instr.Pos())
				b.dibuilder.InsertValueAtEnd(b.getValue(instr.X, getPos(instr)), dbgVar, b.dibuilder.CreateExpression(nil), llvm.DebugLoc{
					Line:  uint(pos.Line),
					Col:   uint(pos.Column),
					Scope: b.difunc,
				}, b.GetInsertBlock())
				continue
			}
			if b.DumpSSA {
				if val, ok := instr.(ssa.Value); ok && val.Name() != "" {
					fmt.Printf("\t%s = %s\n", val.Name(), val.String())
				} else {
					fmt.Printf("\t%s\n", instr.String())
				}
			}
			b.createInstruction(instr)
		}
		if b.fn.Name() == "init" && len(block.Instrs) == 0 {
			b.CreateRetVoid()
		}
	}

	// The rundefers instruction needs to be created after all defer
	// instructions have been created. Otherwise it won't handle all defer
	// cases.
	for i, bb := range b.runDefersBlock {
		b.SetInsertPointAtEnd(bb)
		b.createRunDefers()
		b.CreateBr(b.afterDefersBlock[i])
	}

	if b.hasDeferFrame() {
		// Create the landing pad block, where execution continues after a
		// panic.
		b.createLandingPad()
	}

	// Resolve phi nodes
	for _, phi := range b.phis {
		block := phi.ssa.Block()
		for i, edge := range phi.ssa.Edges {
			llvmVal := b.getValue(edge, getPos(phi.ssa))
			llvmBlock := b.blockExits[block.Preds[i]]
			phi.llvm.AddIncoming([]llvm.Value{llvmVal}, []llvm.BasicBlock{llvmBlock})
		}
	}

	if b.NeedsStackObjects {
		// Track phi nodes.
		for _, phi := range b.phis {
			insertPoint := llvm.NextInstruction(phi.llvm)
			for !insertPoint.IsAPHINode().IsNil() {
				insertPoint = llvm.NextInstruction(insertPoint)
			}
			b.SetInsertPointBefore(insertPoint)
			b.trackValue(phi.llvm)
		}
	}

	// Create anonymous functions (closures etc.).
	for _, sub := range b.fn.AnonFuncs {
		b := newBuilder(b.compilerContext, b.Builder, sub)
		b.llvmFn.SetLinkage(llvm.InternalLinkage)
		b.createFunction()
	}

	// Create wrapper function that can be called externally.
	if b.info.wasmExport != "" {
		b.createWasmExport()
	}
}

// posser is an interface that's implemented by both ssa.Value and
// ssa.Instruction. It is implemented by everything that has a Pos() method,
// which is all that getPos() needs.
type posser interface {
	Pos() token.Pos
}

// getPos returns position information for a ssa.Value or ssa.Instruction.
//
// Not all instructions have position information, especially when they're
// implicit (such as implicit casts or implicit returns at the end of a
// function). In these cases, it makes sense to try a bit harder to guess what
// the position really should be.
func getPos(val posser) token.Pos {
	pos := val.Pos()
	if pos != token.NoPos {
		// Easy: position is known.
		return pos
	}

	// No position information is known.
	switch val := val.(type) {
	case *ssa.MakeInterface:
		return getPos(val.X)
	case *ssa.MakeClosure:
		return val.Fn.(*ssa.Function).Pos()
	case *ssa.Return:
		syntax := val.Parent().Syntax()
		if syntax != nil {
			// non-synthetic
			return syntax.End()
		}
		return token.NoPos
	case *ssa.FieldAddr:
		return getPos(val.X)
	case *ssa.IndexAddr:
		return getPos(val.X)
	case *ssa.Slice:
		return getPos(val.X)
	case *ssa.Store:
		return getPos(val.Addr)
	case *ssa.Extract:
		return getPos(val.Tuple)
	default:
		// This is reachable, for example with *ssa.Const, *ssa.If, and
		// *ssa.Jump. They might be implemented in some way in the future.
		return token.NoPos
	}
}

// createInstruction builds the LLVM IR equivalent instructions for the
// particular Go SSA instruction.
func (b *builder) createInstruction(instr ssa.Instruction) {
	if b.Debug {
		b.setDebugLocation(getPos(instr))
	}

	switch instr := instr.(type) {
	case ssa.Value:
		if value, err := b.createExpr(instr); err != nil {
			// This expression could not be parsed. Add the error to the list
			// of diagnostics and continue with an undef value.
			// The resulting IR will be incorrect (but valid). However,
			// compilation can proceed which is useful because there may be
			// more compilation errors which can then all be shown together to
			// the user.
			b.diagnostics = append(b.diagnostics, err)
			b.locals[instr] = llvm.Undef(b.getLLVMType(instr.Type()))
		} else {
			b.locals[instr] = value
			if len(*instr.Referrers()) != 0 && b.NeedsStackObjects {
				b.trackExpr(instr, value)
			}
		}
	case *ssa.DebugRef:
		// ignore
	case *ssa.Defer:
		b.createDefer(instr)
	case *ssa.Go:
		// Start a new goroutine.
		b.createGo(instr)
	case *ssa.If:
		cond := b.getValue(instr.Cond, getPos(instr))
		block := instr.Block()
		blockThen := b.blockEntries[block.Succs[0]]
		blockElse := b.blockEntries[block.Succs[1]]
		b.CreateCondBr(cond, blockThen, blockElse)
	case *ssa.Jump:
		blockJump := b.blockEntries[instr.Block().Succs[0]]
		b.CreateBr(blockJump)
	case *ssa.MapUpdate:
		m := b.getValue(instr.Map, getPos(instr))
		key := b.getValue(instr.Key, getPos(instr))
		value := b.getValue(instr.Value, getPos(instr))
		mapType := instr.Map.Type().Underlying().(*types.Map)
		b.createMapUpdate(mapType.Key(), m, key, value, instr.Pos())
	case *ssa.Panic:
		value := b.getValue(instr.X, getPos(instr))
		b.createRuntimeInvoke("_panic", []llvm.Value{value}, "")
		b.CreateUnreachable()
	case *ssa.Return:
		if b.hasDeferFrame() {
			b.createRuntimeCall("destroyDeferFrame", []llvm.Value{b.deferFrame}, "")
		}
		if len(instr.Results) == 0 {
			b.CreateRetVoid()
		} else if len(instr.Results) == 1 {
			b.CreateRet(b.getValue(instr.Results[0], getPos(instr)))
		} else {
			// Multiple return values. Put them all in a struct.
			retVal := llvm.ConstNull(b.llvmFn.GlobalValueType().ReturnType())
			for i, result := range instr.Results {
				val := b.getValue(result, getPos(instr))
				retVal = b.CreateInsertValue(retVal, val, i, "")
			}
			b.CreateRet(retVal)
		}
	case *ssa.RunDefers:
		// Note where we're going to put the rundefers block
		run := b.insertBasicBlock("rundefers.block")
		b.CreateBr(run)
		b.runDefersBlock = append(b.runDefersBlock, run)

		after := b.insertBasicBlock("rundefers.after")
		b.SetInsertPointAtEnd(after)
		b.afterDefersBlock = append(b.afterDefersBlock, after)
	case *ssa.Send:
		b.createChanSend(instr)
	case *ssa.Store:
		llvmAddr := b.getValue(instr.Addr, getPos(instr))
		llvmVal := b.getValue(instr.Val, getPos(instr))
		b.createNilCheck(instr.Addr, llvmAddr, "store")
		if b.targetData.TypeAllocSize(llvmVal.Type()) == 0 {
			// nothing to store
			return
		}
		b.CreateStore(llvmVal, llvmAddr)
	default:
		b.addError(instr.Pos(), "unknown instruction: "+instr.String())
	}
}

// createBuiltin lowers a builtin Go function (append, close, delete, etc.) to
// LLVM IR. It uses runtime calls for some builtins.
func (b *builder) createBuiltin(argTypes []types.Type, argValues []llvm.Value, callName string, pos token.Pos) (llvm.Value, error) {
	switch callName {
	case "append":
		src := argValues[0]
		elems := argValues[1]
		srcBuf := b.CreateExtractValue(src, 0, "append.srcBuf")
		srcLen := b.CreateExtractValue(src, 1, "append.srcLen")
		srcCap := b.CreateExtractValue(src, 2, "append.srcCap")
		elemsBuf := b.CreateExtractValue(elems, 0, "append.elemsBuf")
		elemsLen := b.CreateExtractValue(elems, 1, "append.elemsLen")
		elemType := b.getLLVMType(argTypes[0].Underlying().(*types.Slice).Elem())
		elemSize := llvm.ConstInt(b.uintptrType, b.targetData.TypeAllocSize(elemType), false)
		result := b.createRuntimeCall("sliceAppend", []llvm.Value{srcBuf, elemsBuf, srcLen, srcCap, elemsLen, elemSize}, "append.new")
		newPtr := b.CreateExtractValue(result, 0, "append.newPtr")
		newLen := b.CreateExtractValue(result, 1, "append.newLen")
		newCap := b.CreateExtractValue(result, 2, "append.newCap")
		newSlice := llvm.Undef(src.Type())
		newSlice = b.CreateInsertValue(newSlice, newPtr, 0, "")
		newSlice = b.CreateInsertValue(newSlice, newLen, 1, "")
		newSlice = b.CreateInsertValue(newSlice, newCap, 2, "")
		return newSlice, nil
	case "cap":
		value := argValues[0]
		var llvmCap llvm.Value
		switch argTypes[0].Underlying().(type) {
		case *types.Chan:
			llvmCap = b.createRuntimeCall("chanCap", []llvm.Value{value}, "cap")
		case *types.Slice:
			llvmCap = b.CreateExtractValue(value, 2, "cap")
		default:
			return llvm.Value{}, b.makeError(pos, "todo: cap: unknown type")
		}
		if b.targetData.TypeAllocSize(llvmCap.Type()) < b.targetData.TypeAllocSize(b.intType) {
			llvmCap = b.CreateZExt(llvmCap, b.intType, "len.int")
		}
		return llvmCap, nil
	case "close":
		b.createChanClose(argValues[0])
		return llvm.Value{}, nil
	case "complex":
		r := argValues[0]
		i := argValues[1]
		t := argTypes[0].Underlying().(*types.Basic)
		var cplx llvm.Value
		switch t.Kind() {
		case types.Float32:
			cplx = llvm.Undef(b.ctx.StructType([]llvm.Type{b.ctx.FloatType(), b.ctx.FloatType()}, false))
		case types.Float64:
			cplx = llvm.Undef(b.ctx.StructType([]llvm.Type{b.ctx.DoubleType(), b.ctx.DoubleType()}, false))
		default:
			return llvm.Value{}, b.makeError(pos, "unsupported type in complex builtin: "+t.String())
		}
		cplx = b.CreateInsertValue(cplx, r, 0, "")
		cplx = b.CreateInsertValue(cplx, i, 1, "")
		return cplx, nil
	case "clear":
		value := argValues[0]
		switch typ := argTypes[0].Underlying().(type) {
		case *types.Slice:
			elementType := b.getLLVMType(typ.Elem())
			elementSize := b.targetData.TypeAllocSize(elementType)
			elementAlign := b.targetData.ABITypeAlignment(elementType)

			// The pointer to the data to be cleared.
			llvmBuf := b.CreateExtractValue(value, 0, "buf")

			// The length (in bytes) to be cleared.
			llvmLen := b.CreateExtractValue(value, 1, "len")
			llvmLen = b.CreateMul(llvmLen, llvm.ConstInt(llvmLen.Type(), elementSize, false), "")

			// Do the clear operation using the LLVM memset builtin.
			// This is also correct for nil slices: in those cases, len will be
			// 0 which means the memset call is a no-op (according to the LLVM
			// LangRef).
			memset := b.getMemsetFunc()
			call := b.createCall(memset.GlobalValueType(), memset, []llvm.Value{
				llvmBuf, // dest
				llvm.ConstInt(b.ctx.Int8Type(), 0, false), // val
				llvmLen, // len
				llvm.ConstInt(b.ctx.Int1Type(), 0, false), // isVolatile
			}, "")
			call.AddCallSiteAttribute(1, b.ctx.CreateEnumAttribute(llvm.AttributeKindID("align"), uint64(elementAlign)))

			return llvm.Value{}, nil
		case *types.Map:
			m := argValues[0]
			b.createMapClear(m)
			return llvm.Value{}, nil
		default:
			return llvm.Value{}, b.makeError(pos, "unsupported type in clear builtin: "+typ.String())
		}
	case "copy":
		dst := argValues[0]
		src := argValues[1]
		dstLen := b.CreateExtractValue(dst, 1, "copy.dstLen")
		srcLen := b.CreateExtractValue(src, 1, "copy.srcLen")
		dstBuf := b.CreateExtractValue(dst, 0, "copy.dstArray")
		srcBuf := b.CreateExtractValue(src, 0, "copy.srcArray")
		elemType := b.getLLVMType(argTypes[0].Underlying().(*types.Slice).Elem())
		elemSize := llvm.ConstInt(b.uintptrType, b.targetData.TypeAllocSize(elemType), false)
		return b.createRuntimeCall("sliceCopy", []llvm.Value{dstBuf, srcBuf, dstLen, srcLen, elemSize}, "copy.n"), nil
	case "delete":
		m := argValues[0]
		key := argValues[1]
		return llvm.Value{}, b.createMapDelete(argTypes[1], m, key, pos)
	case "imag":
		cplx := argValues[0]
		return b.CreateExtractValue(cplx, 1, "imag"), nil
	case "len":
		value := argValues[0]
		var llvmLen llvm.Value
		switch argTypes[0].Underlying().(type) {
		case *types.Basic, *types.Slice:
			// string or slice
			llvmLen = b.CreateExtractValue(value, 1, "len")
		case *types.Chan:
			llvmLen = b.createRuntimeCall("chanLen", []llvm.Value{value}, "len")
		case *types.Map:
			llvmLen = b.createRuntimeCall("hashmapLen", []llvm.Value{value}, "len")
		default:
			return llvm.Value{}, b.makeError(pos, "todo: len: unknown type")
		}
		if b.targetData.TypeAllocSize(llvmLen.Type()) < b.targetData.TypeAllocSize(b.intType) {
			llvmLen = b.CreateZExt(llvmLen, b.intType, "len.int")
		}
		return llvmLen, nil
	case "min", "max":
		// min and max builtins, added in Go 1.21.
		// We can simply reuse the existing binop comparison code, which has all
		// the edge cases figured out already.
		tok := token.LSS
		if callName == "max" {
			tok = token.GTR
		}
		result := argValues[0]
		typ := argTypes[0]
		for _, arg := range argValues[1:] {
			cmp, err := b.createBinOp(tok, typ, typ, result, arg, pos)
			if err != nil {
				return result, err
			}
			result = b.CreateSelect(cmp, result, arg, "")
		}
		return result, nil
	case "panic":
		// This is rare, but happens in "defer panic()".
		b.createRuntimeInvoke("_panic", argValues, "")
		return llvm.Value{}, nil
	case "print", "println":
		b.createRuntimeCall("printlock", nil, "")
		for i, value := range argValues {
			if i >= 1 && callName == "println" {
				b.createRuntimeCall("printspace", nil, "")
			}
			typ := argTypes[i].Underlying()
			switch typ := typ.(type) {
			case *types.Basic:
				switch typ.Kind() {
				case types.String, types.UntypedString:
					b.createRuntimeCall("printstring", []llvm.Value{value}, "")
				case types.Uintptr:
					b.createRuntimeCall("printptr", []llvm.Value{value}, "")
				case types.UnsafePointer:
					ptrValue := b.CreatePtrToInt(value, b.uintptrType, "")
					b.createRuntimeCall("printptr", []llvm.Value{ptrValue}, "")
				default:
					// runtime.print{int,uint}{8,16,32,64}
					if typ.Info()&types.IsInteger != 0 {
						name := "print"
						if typ.Info()&types.IsUnsigned != 0 {
							name += "uint"
						} else {
							name += "int"
						}
						name += strconv.FormatUint(b.targetData.TypeAllocSize(value.Type())*8, 10)
						b.createRuntimeCall(name, []llvm.Value{value}, "")
					} else if typ.Kind() == types.Bool {
						b.createRuntimeCall("printbool", []llvm.Value{value}, "")
					} else if typ.Kind() == types.Float32 {
						b.createRuntimeCall("printfloat32", []llvm.Value{value}, "")
					} else if typ.Kind() == types.Float64 {
						b.createRuntimeCall("printfloat64", []llvm.Value{value}, "")
					} else if typ.Kind() == types.Complex64 {
						b.createRuntimeCall("printcomplex64", []llvm.Value{value}, "")
					} else if typ.Kind() == types.Complex128 {
						b.createRuntimeCall("printcomplex128", []llvm.Value{value}, "")
					} else {
						return llvm.Value{}, b.makeError(pos, "unknown basic arg type: "+typ.String())
					}
				}
			case *types.Interface:
				b.createRuntimeCall("printitf", []llvm.Value{value}, "")
			case *types.Map:
				b.createRuntimeCall("printmap", []llvm.Value{value}, "")
			case *types.Pointer:
				ptrValue := b.CreatePtrToInt(value, b.uintptrType, "")
				b.createRuntimeCall("printptr", []llvm.Value{ptrValue}, "")
			case *types.Slice:
				bufptr := b.CreateExtractValue(value, 0, "")
				buflen := b.CreateExtractValue(value, 1, "")
				bufcap := b.CreateExtractValue(value, 2, "")
				ptrValue := b.CreatePtrToInt(bufptr, b.uintptrType, "")
				b.createRuntimeCall("printslice", []llvm.Value{ptrValue, buflen, bufcap}, "")
			default:
				return llvm.Value{}, b.makeError(pos, "unknown arg type: "+typ.String())
			}
		}
		if callName == "println" {
			b.createRuntimeCall("printnl", nil, "")
		}
		b.createRuntimeCall("printunlock", nil, "")
		return llvm.Value{}, nil // print() or println() returns void
	case "real":
		cplx := argValues[0]
		return b.CreateExtractValue(cplx, 0, "real"), nil
	case "recover":
		useParentFrame := uint64(0)
		if b.hasDeferFrame() {
			// recover() should return the panic value of the parent function,
			// not of the current function.
			useParentFrame = 1
		}
		return b.createRuntimeCall("_recover", []llvm.Value{llvm.ConstInt(b.ctx.Int1Type(), useParentFrame, false)}, ""), nil
	case "ssa:wrapnilchk":
		// TODO: do an actual nil check?
		return argValues[0], nil

	// Builtins from the unsafe package.
	case "Add": // unsafe.Add
		// This is basically just a GEP operation.
		// Note: the pointer is always of type *i8.
		ptr := argValues[0]
		len := argValues[1]
		return b.CreateGEP(b.ctx.Int8Type(), ptr, []llvm.Value{len}, ""), nil
	case "Alignof": // unsafe.Alignof
		align := b.targetData.ABITypeAlignment(argValues[0].Type())
		return llvm.ConstInt(b.uintptrType, uint64(align), false), nil
	case "Offsetof": // unsafe.Offsetof
		// This builtin is a bit harder to implement and may need a bit of
		// refactoring to work (it may be easier to implement if we have access
		// to the underlying Go SSA instruction). It is also rarely used: it
		// only applies in generic code and unsafe.Offsetof isn't very commonly
		// used anyway.
		// In other words, postpone it to some other day.
		return llvm.Value{}, b.makeError(pos, "todo: unsafe.Offsetof")
	case "Sizeof": // unsafe.Sizeof
		size := b.targetData.TypeAllocSize(argValues[0].Type())
		return llvm.ConstInt(b.uintptrType, size, false), nil
	case "Slice", "String": // unsafe.Slice, unsafe.String
		// This creates a slice or string from a pointer and a length.
		// Note that the exception mentioned in the documentation (if the
		// pointer and length are nil, the slice is also nil) is trivially
		// already the case.
		ptr := argValues[0]
		len := argValues[1]
		var elementType llvm.Type
		if callName == "Slice" {
			elementType = b.getLLVMType(argTypes[0].Underlying().(*types.Pointer).Elem())
		} else {
			elementType = b.ctx.Int8Type()
		}
		b.createUnsafeSliceStringCheck("unsafe."+callName, ptr, len, elementType, argTypes[1].Underlying().(*types.Basic))
		if len.Type().IntTypeWidth() < b.uintptrType.IntTypeWidth() {
			// Too small, zero-extend len.
			len = b.CreateZExt(len, b.uintptrType, "")
		} else if len.Type().IntTypeWidth() > b.uintptrType.IntTypeWidth() {
			// Too big, truncate len.
			len = b.CreateTrunc(len, b.uintptrType, "")
		}
		if callName == "Slice" {
			slice := llvm.Undef(b.ctx.StructType([]llvm.Type{
				ptr.Type(),
				b.uintptrType,
				b.uintptrType,
			}, false))
			slice = b.CreateInsertValue(slice, ptr, 0, "")
			slice = b.CreateInsertValue(slice, len, 1, "")
			slice = b.CreateInsertValue(slice, len, 2, "")
			return slice, nil
		} else {
			str := llvm.Undef(b.getLLVMRuntimeType("_string"))
			str = b.CreateInsertValue(str, argValues[0], 0, "")
			str = b.CreateInsertValue(str, len, 1, "")
			return str, nil
		}
	case "SliceData", "StringData": // unsafe.SliceData, unsafe.StringData
		return b.CreateExtractValue(argValues[0], 0, "slice.data"), nil
	default:
		return llvm.Value{}, b.makeError(pos, "todo: builtin: "+callName)
	}
}

// createFunctionCall lowers a Go SSA call instruction (to a simple function,
// closure, function pointer, builtin, method, etc.) to LLVM IR, usually a call
// instruction.
//
// This is also where compiler intrinsics are implemented.
func (b *builder) createFunctionCall(instr *ssa.CallCommon) (llvm.Value, error) {
	// See if this is an intrinsic function that is handled specially.
	if fn := instr.StaticCallee(); fn != nil {
		// Direct function call, either to a named or anonymous (directly
		// applied) function call. If it is anonymous, it may be a closure.
		name := fn.RelString(nil)
		switch {
		case name == "device.Asm" || name == "device/arm.Asm" || name == "device/arm64.Asm" || name == "device/avr.Asm" || name == "device/riscv.Asm":
			return b.createInlineAsm(instr.Args)
		case name == "device.AsmFull" || name == "device/arm.AsmFull" || name == "device/arm64.AsmFull" || name == "device/avr.AsmFull" || name == "device/riscv.AsmFull":
			return b.createInlineAsmFull(instr)
		case strings.HasPrefix(name, "device/arm.SVCall"):
			return b.emitSVCall(instr.Args, getPos(instr))
		case strings.HasPrefix(name, "device/arm64.SVCall"):
			return b.emitSV64Call(instr.Args, getPos(instr))
		case strings.HasPrefix(name, "(device/riscv.CSR)."):
			return b.emitCSROperation(instr)
		case strings.HasPrefix(name, "syscall.Syscall") || strings.HasPrefix(name, "syscall.RawSyscall") || strings.HasPrefix(name, "golang.org/x/sys/unix.Syscall") || strings.HasPrefix(name, "golang.org/x/sys/unix.RawSyscall"):
			if b.GOOS != "darwin" {
				return b.createSyscall(instr)
			}
		case strings.HasPrefix(name, "syscall.rawSyscallNoError") || strings.HasPrefix(name, "golang.org/x/sys/unix.RawSyscallNoError"):
			return b.createRawSyscallNoError(instr)
		case name == "runtime.supportsRecover":
			supportsRecover := uint64(0)
			if b.supportsRecover() {
				supportsRecover = 1
			}
			return llvm.ConstInt(b.ctx.Int1Type(), supportsRecover, false), nil
		case name == "runtime.panicStrategy":
			panicStrategy := map[string]uint64{
				"print": tinygo.PanicStrategyPrint,
				"trap":  tinygo.PanicStrategyTrap,
			}[b.Config.PanicStrategy]
			return llvm.ConstInt(b.ctx.Int8Type(), panicStrategy, false), nil
		case name == "runtime/interrupt.New":
			return b.createInterruptGlobal(instr)
		case name == "runtime.exportedFuncPtr":
			_, ptr := b.getFunction(instr.Args[0].(*ssa.Function))
			return b.CreatePtrToInt(ptr, b.uintptrType, ""), nil
		case name == "(*runtime/interrupt.Checkpoint).Save":
			return b.createInterruptCheckpoint(instr.Args[0]), nil
		case name == "internal/abi.FuncPCABI0":
			retval := b.createDarwinFuncPCABI0Call(instr)
			if !retval.IsNil() {
				return retval, nil
			}
		}
	}

	var params []llvm.Value
	for _, param := range instr.Args {
		params = append(params, b.getValue(param, getPos(instr)))
	}

	// Try to call the function directly for trivially static calls.
	var callee, context llvm.Value
	var calleeType llvm.Type
	exported := false
	if fn := instr.StaticCallee(); fn != nil {
		calleeType, callee = b.getFunction(fn)
		info := b.getFunctionInfo(fn)
		if callee.IsNil() {
			return llvm.Value{}, b.makeError(instr.Pos(), "undefined function: "+info.linkName)
		}
		switch value := instr.Value.(type) {
		case *ssa.Function:
			// Regular function call. No context is necessary.
			context = llvm.Undef(b.dataPtrType)
			if info.variadic && len(fn.Params) == 0 {
				// This matches Clang, see: https://godbolt.org/z/Gqv49xKMq
				// Eventually we might be able to eliminate this special case
				// entirely. For details, see:
				// https://discourse.llvm.org/t/rfc-enabling-wstrict-prototypes-by-default-in-c/60521
				calleeType = llvm.FunctionType(callee.GlobalValueType().ReturnType(), nil, false)
			}
		case *ssa.MakeClosure:
			// A call on a func value, but the callee is trivial to find. For
			// example: immediately applied functions.
			funcValue := b.getValue(value, getPos(value))
			context = b.extractFuncContext(funcValue)
		default:
			panic("StaticCallee returned an unexpected value")
		}
		exported = info.exported
	} else if call, ok := instr.Value.(*ssa.Builtin); ok {
		// Builtin function (append, close, delete, etc.).)
		var argTypes []types.Type
		for _, arg := range instr.Args {
			argTypes = append(argTypes, arg.Type())
		}
		return b.createBuiltin(argTypes, params, call.Name(), instr.Pos())
	} else if instr.IsInvoke() {
		// Interface method call (aka invoke call).
		itf := b.getValue(instr.Value, getPos(instr)) // interface value (runtime._interface)
		typecode := b.CreateExtractValue(itf, 0, "invoke.func.typecode")
		value := b.CreateExtractValue(itf, 1, "invoke.func.value") // receiver
		// Prefix the params with receiver value and suffix with typecode.
		params = append([]llvm.Value{value}, params...)
		params = append(params, typecode)
		callee = b.getInvokeFunction(instr)
		calleeType = callee.GlobalValueType()
		context = llvm.Undef(b.dataPtrType)
	} else {
		// Function pointer.
		value := b.getValue(instr.Value, getPos(instr))
		// This is a func value, which cannot be called directly. We have to
		// extract the function pointer and context first from the func value.
		callee, context = b.decodeFuncValue(value)
		calleeType = b.getLLVMFunctionType(instr.Value.Type().Underlying().(*types.Signature))
		b.createNilCheck(instr.Value, callee, "fpcall")
	}

	if !exported {
		// This function takes a context parameter.
		// Add it to the end of the parameter list.
		params = append(params, context)
	}

	return b.createInvoke(calleeType, callee, params, ""), nil
}

// getValue returns the LLVM value of a constant, function value, global, or
// already processed SSA expression.
func (b *builder) getValue(expr ssa.Value, pos token.Pos) llvm.Value {
	switch expr := expr.(type) {
	case *ssa.Const:
		if pos == token.NoPos {
			// If the position isn't known, at least try to find in which file
			// it is defined.
			file := b.program.Fset.File(b.fn.Pos())
			if file != nil {
				pos = file.Pos(0)
			}
		}
		return b.createConst(expr, pos)
	case *ssa.Function:
		if b.getFunctionInfo(expr).exported {
			b.addError(expr.Pos(), "cannot use an exported function as value: "+expr.String())
			return llvm.Undef(b.getLLVMType(expr.Type()))
		}
		_, fn := b.getFunction(expr)
		return b.createFuncValue(fn, llvm.Undef(b.dataPtrType), expr.Signature)
	case *ssa.Global:
		value := b.getGlobal(expr)
		if value.IsNil() {
			b.addError(expr.Pos(), "global not found: "+expr.RelString(nil))
			return llvm.Undef(b.getLLVMType(expr.Type()))
		}
		return value
	default:
		// other (local) SSA value
		if value, ok := b.locals[expr]; ok {
			return value
		} else {
			// indicates a compiler bug
			panic("SSA value not previously found in function: " + expr.String())
		}
	}
}

// maxSliceSize determines the maximum size a slice of the given element type
// can be.
func (c *compilerContext) maxSliceSize(elementType llvm.Type) uint64 {
	// Calculate ^uintptr(0), which is the max value that fits in uintptr.
	maxPointerValue := llvm.ConstNot(llvm.ConstInt(c.uintptrType, 0, false)).ZExtValue()
	// Calculate (^uint(0))/2, which is the max value that fits in an int.
	maxIntegerValue := llvm.ConstNot(llvm.ConstInt(c.intType, 0, false)).ZExtValue() / 2

	// Determine the maximum allowed size for a slice. The biggest possible
	// pointer (starting from 0) would be maxPointerValue*sizeof(elementType) so
	// divide by the element type to get the real maximum size.
	elementSize := c.targetData.TypeAllocSize(elementType)
	if elementSize == 0 {
		elementSize = 1
	}
	maxSize := maxPointerValue / elementSize

	// len(slice) is an int. Make sure the length remains small enough to fit in
	// an int.
	if maxSize > maxIntegerValue {
		maxSize = maxIntegerValue
	}

	return maxSize
}

// createExpr translates a Go SSA expression to LLVM IR. This can be zero, one,
// or multiple LLVM IR instructions and/or runtime calls.
func (b *builder) createExpr(expr ssa.Value) (llvm.Value, error) {
	if _, ok := b.locals[expr]; ok {
		// sanity check
		panic("instruction has already been created: " + expr.String())
	}

	switch expr := expr.(type) {
	case *ssa.Alloc:
		typ := b.getLLVMType(expr.Type().Underlying().(*types.Pointer).Elem())
		size := b.targetData.TypeAllocSize(typ)
		// Move all "large" allocations to the heap.
		if expr.Heap || size > b.MaxStackAlloc {
			// Calculate ^uintptr(0)
			maxSize := llvm.ConstNot(llvm.ConstInt(b.uintptrType, 0, false)).ZExtValue()
			if size > maxSize {
				// Size would be truncated if truncated to uintptr.
				return llvm.Value{}, b.makeError(expr.Pos(), fmt.Sprintf("value is too big (%v bytes)", size))
			}
			sizeValue := llvm.ConstInt(b.uintptrType, size, false)
			layoutValue := b.createObjectLayout(typ, expr.Pos())
			buf := b.createRuntimeCall("alloc", []llvm.Value{sizeValue, layoutValue}, expr.Comment)
			align := b.targetData.ABITypeAlignment(typ)
			buf.AddCallSiteAttribute(0, b.ctx.CreateEnumAttribute(llvm.AttributeKindID("align"), uint64(align)))
			return buf, nil
		} else {
			buf := llvmutil.CreateEntryBlockAlloca(b.Builder, typ, expr.Comment)
			if b.targetData.TypeAllocSize(typ) != 0 {
				b.CreateStore(llvm.ConstNull(typ), buf) // zero-initialize var
			}
			return buf, nil
		}
	case *ssa.BinOp:
		x := b.getValue(expr.X, getPos(expr))
		y := b.getValue(expr.Y, getPos(expr))
		return b.createBinOp(expr.Op, expr.X.Type(), expr.Y.Type(), x, y, expr.Pos())
	case *ssa.Call:
		return b.createFunctionCall(expr.Common())
	case *ssa.ChangeInterface:
		// Do not change between interface types: always use the underlying
		// (concrete) type in the type number of the interface. Every method
		// call on an interface will do a lookup which method to call.
		// This is different from how the official Go compiler works, because of
		// heap allocation and because it's easier to implement, see:
		// https://research.swtch.com/interfaces
		return b.getValue(expr.X, getPos(expr)), nil
	case *ssa.ChangeType:
		// This instruction changes the type, but the underlying value remains
		// the same. This is often a no-op, but sometimes we have to change the
		// LLVM type as well.
		x := b.getValue(expr.X, getPos(expr))
		llvmType := b.getLLVMType(expr.Type())
		if x.Type() == llvmType {
			// Different Go type but same LLVM type (for example, named int).
			// This is the common case.
			return x, nil
		}
		// Figure out what kind of type we need to cast.
		switch llvmType.TypeKind() {
		case llvm.StructTypeKind:
			// Unfortunately, we can't just bitcast structs. We have to
			// actually create a new struct of the correct type and insert the
			// values from the previous struct in there.
			value := llvm.Undef(llvmType)
			for i := 0; i < llvmType.StructElementTypesCount(); i++ {
				field := b.CreateExtractValue(x, i, "changetype.field")
				value = b.CreateInsertValue(value, field, i, "changetype.struct")
			}
			return value, nil
		default:
			return llvm.Value{}, errors.New("todo: unknown ChangeType type: " + expr.X.Type().String())
		}
	case *ssa.Const:
		panic("const is not an expression")
	case *ssa.Convert:
		x := b.getValue(expr.X, getPos(expr))
		return b.createConvert(expr.X.Type(), expr.Type(), x, expr.Pos())
	case *ssa.Extract:
		if _, ok := expr.Tuple.(*ssa.Select); ok {
			return b.getChanSelectResult(expr), nil
		}
		value := b.getValue(expr.Tuple, getPos(expr))
		return b.CreateExtractValue(value, expr.Index, ""), nil
	case *ssa.Field:
		value := b.getValue(expr.X, getPos(expr))
		result := b.CreateExtractValue(value, expr.Field, "")
		return result, nil
	case *ssa.FieldAddr:
		val := b.getValue(expr.X, getPos(expr))
		// Check for nil pointer before calculating the address, from the spec:
		// > For an operand x of type T, the address operation &x generates a
		// > pointer of type *T to x. [...] If the evaluation of x would cause a
		// > run-time panic, then the evaluation of &x does too.
		b.createNilCheck(expr.X, val, "gep")
		// Do a GEP on the pointer to get the field address.
		indices := []llvm.Value{
			llvm.ConstInt(b.ctx.Int32Type(), 0, false),
			llvm.ConstInt(b.ctx.Int32Type(), uint64(expr.Field), false),
		}
		elementType := b.getLLVMType(expr.X.Type().Underlying().(*types.Pointer).Elem())
		return b.CreateInBoundsGEP(elementType, val, indices, ""), nil
	case *ssa.Function:
		panic("function is not an expression")
	case *ssa.Global:
		panic("global is not an expression")
	case *ssa.Index:
		collection := b.getValue(expr.X, getPos(expr))
		index := b.getValue(expr.Index, getPos(expr))

		switch xType := expr.X.Type().Underlying().(type) {
		case *types.Basic: // extract byte from string
			// Value type must be a string, which is a basic type.
			if xType.Info()&types.IsString == 0 {
				panic("lookup on non-string?")
			}

			// Sometimes, the index can be e.g. an uint8 or int8, and we have to
			// correctly extend that type for two reasons:
			//  1. The lookup bounds check expects an index of at least uintptr
			//     size.
			//  2. getelementptr has signed operands, and therefore s[uint8(x)]
			//     can be lowered as s[int8(x)]. That would be a bug.
			index = b.extendInteger(index, expr.Index.Type(), b.uintptrType)

			// Bounds check.
			length := b.CreateExtractValue(collection, 1, "len")
			b.createLookupBoundsCheck(length, index)

			// Lookup byte
			buf := b.CreateExtractValue(collection, 0, "")
			bufElemType := b.ctx.Int8Type()
			bufPtr := b.CreateInBoundsGEP(bufElemType, buf, []llvm.Value{index}, "")
			return b.CreateLoad(bufElemType, bufPtr, ""), nil
		case *types.Array: // extract element from array
			// Extend index to at least uintptr size, because getelementptr
			// assumes index is a signed integer.
			index = b.extendInteger(index, expr.Index.Type(), b.uintptrType)

			// Check bounds.
			arrayLen := llvm.ConstInt(b.uintptrType, uint64(xType.Len()), false)
			b.createLookupBoundsCheck(arrayLen, index)

			// Can't load directly from array (as index is non-constant), so
			// have to do it using an alloca+gep+load.
			arrayType := collection.Type()
			alloca, allocaSize := b.createTemporaryAlloca(arrayType, "index.alloca")
			b.CreateStore(collection, alloca)
			zero := llvm.ConstInt(b.ctx.Int32Type(), 0, false)
			ptr := b.CreateInBoundsGEP(arrayType, alloca, []llvm.Value{zero, index}, "index.gep")
			result := b.CreateLoad(arrayType.ElementType(), ptr, "index.load")
			b.emitLifetimeEnd(alloca, allocaSize)
			return result, nil
		default:
			panic("unknown *ssa.Index type")
		}
	case *ssa.IndexAddr:
		val := b.getValue(expr.X, getPos(expr))
		index := b.getValue(expr.Index, getPos(expr))

		// Get buffer pointer and length
		var bufptr, buflen llvm.Value
		var bufType llvm.Type
		switch ptrTyp := expr.X.Type().Underlying().(type) {
		case *types.Pointer:
			typ := ptrTyp.Elem().Underlying()
			switch typ := typ.(type) {
			case *types.Array:
				bufptr = val
				buflen = llvm.ConstInt(b.uintptrType, uint64(typ.Len()), false)
				bufType = b.getLLVMType(typ)
				// Check for nil pointer before calculating the address, from
				// the spec:
				// > For an operand x of type T, the address operation &x
				// > generates a pointer of type *T to x. [...] If the
				// > evaluation of x would cause a run-time panic, then the
				// > evaluation of &x does too.
				b.createNilCheck(expr.X, bufptr, "gep")
			default:
				return llvm.Value{}, b.makeError(expr.Pos(), "todo: indexaddr: "+typ.String())
			}
		case *types.Slice:
			bufptr = b.CreateExtractValue(val, 0, "indexaddr.ptr")
			buflen = b.CreateExtractValue(val, 1, "indexaddr.len")
			bufType = b.getLLVMType(ptrTyp.Elem())
		default:
			return llvm.Value{}, b.makeError(expr.Pos(), "todo: indexaddr: "+ptrTyp.String())
		}

		// Make sure index is at least the size of uintptr because getelementptr
		// assumes index is a signed integer.
		index = b.extendInteger(index, expr.Index.Type(), b.uintptrType)

		// Bounds check.
		b.createLookupBoundsCheck(buflen, index)

		switch expr.X.Type().Underlying().(type) {
		case *types.Pointer:
			indices := []llvm.Value{
				llvm.ConstInt(b.ctx.Int32Type(), 0, false),
				index,
			}
			return b.CreateInBoundsGEP(bufType, bufptr, indices, ""), nil
		case *types.Slice:
			return b.CreateInBoundsGEP(bufType, bufptr, []llvm.Value{index}, ""), nil
		default:
			panic("unreachable")
		}
	case *ssa.Lookup: // map lookup
		value := b.getValue(expr.X, getPos(expr))
		index := b.getValue(expr.Index, getPos(expr))
		valueType := expr.Type()
		if expr.CommaOk {
			valueType = valueType.(*types.Tuple).At(0).Type()
		}
		return b.createMapLookup(expr.X.Type().Underlying().(*types.Map).Key(), valueType, value, index, expr.CommaOk, expr.Pos())
	case *ssa.MakeChan:
		return b.createMakeChan(expr), nil
	case *ssa.MakeClosure:
		return b.parseMakeClosure(expr)
	case *ssa.MakeInterface:
		val := b.getValue(expr.X, getPos(expr))
		return b.createMakeInterface(val, expr.X.Type(), expr.Pos()), nil
	case *ssa.MakeMap:
		return b.createMakeMap(expr)
	case *ssa.MakeSlice:
		sliceLen := b.getValue(expr.Len, getPos(expr))
		sliceCap := b.getValue(expr.Cap, getPos(expr))
		sliceType := expr.Type().Underlying().(*types.Slice)
		llvmElemType := b.getLLVMType(sliceType.Elem())
		elemSize := b.targetData.TypeAllocSize(llvmElemType)
		elemAlign := b.targetData.ABITypeAlignment(llvmElemType)
		elemSizeValue := llvm.ConstInt(b.uintptrType, elemSize, false)

		maxSize := b.maxSliceSize(llvmElemType)
		if elemSize > maxSize {
			// This seems to be checked by the typechecker already, but let's
			// check it again just to be sure.
			return llvm.Value{}, b.makeError(expr.Pos(), fmt.Sprintf("slice element type is too big (%v bytes)", elemSize))
		}

		// Bounds checking.
		lenType := expr.Len.Type().Underlying().(*types.Basic)
		capType := expr.Cap.Type().Underlying().(*types.Basic)
		maxSizeValue := llvm.ConstInt(b.uintptrType, maxSize, false)
		b.createSliceBoundsCheck(maxSizeValue, sliceLen, sliceCap, sliceCap, lenType, capType, capType)

		// Allocate the backing array.
		sliceCapCast, err := b.createConvert(expr.Cap.Type(), types.Typ[types.Uintptr], sliceCap, expr.Pos())
		if err != nil {
			return llvm.Value{}, err
		}
		sliceSize := b.CreateBinOp(llvm.Mul, elemSizeValue, sliceCapCast, "makeslice.cap")
		layoutValue := b.createObjectLayout(llvmElemType, expr.Pos())
		slicePtr := b.createRuntimeCall("alloc", []llvm.Value{sliceSize, layoutValue}, "makeslice.buf")
		slicePtr.AddCallSiteAttribute(0, b.ctx.CreateEnumAttribute(llvm.AttributeKindID("align"), uint64(elemAlign)))

		// Extend or truncate if necessary. This is safe as we've already done
		// the bounds check.
		sliceLen, err = b.createConvert(expr.Len.Type(), types.Typ[types.Uintptr], sliceLen, expr.Pos())
		if err != nil {
			return llvm.Value{}, err
		}
		sliceCap, err = b.createConvert(expr.Cap.Type(), types.Typ[types.Uintptr], sliceCap, expr.Pos())
		if err != nil {
			return llvm.Value{}, err
		}

		// Create the slice.
		slice := b.ctx.ConstStruct([]llvm.Value{
			llvm.Undef(slicePtr.Type()),
			llvm.Undef(b.uintptrType),
			llvm.Undef(b.uintptrType),
		}, false)
		slice = b.CreateInsertValue(slice, slicePtr, 0, "")
		slice = b.CreateInsertValue(slice, sliceLen, 1, "")
		slice = b.CreateInsertValue(slice, sliceCap, 2, "")
		return slice, nil
	case *ssa.Next:
		rangeVal := expr.Iter.(*ssa.Range).X
		llvmRangeVal := b.getValue(rangeVal, getPos(expr))
		it := b.getValue(expr.Iter, getPos(expr))
		if expr.IsString {
			return b.createRuntimeCall("stringNext", []llvm.Value{llvmRangeVal, it}, "range.next"), nil
		} else { // map
			return b.createMapIteratorNext(rangeVal, llvmRangeVal, it), nil
		}
	case *ssa.Phi:
		phi := b.CreatePHI(b.getLLVMType(expr.Type()), "")
		b.phis = append(b.phis, phiNode{expr, phi})
		return phi, nil
	case *ssa.Range:
		var iteratorType llvm.Type
		switch typ := expr.X.Type().Underlying().(type) {
		case *types.Basic: // string
			iteratorType = b.getLLVMRuntimeType("stringIterator")
		case *types.Map:
			iteratorType = b.getLLVMRuntimeType("hashmapIterator")
		default:
			panic("unknown type in range: " + typ.String())
		}
		it, _ := b.createTemporaryAlloca(iteratorType, "range.it")
		b.CreateStore(llvm.ConstNull(iteratorType), it)
		return it, nil
	case *ssa.Select:
		return b.createSelect(expr), nil
	case *ssa.Slice:
		value := b.getValue(expr.X, getPos(expr))

		var lowType, highType, maxType *types.Basic
		var low, high, max llvm.Value

		if expr.Low != nil {
			lowType = expr.Low.Type().Underlying().(*types.Basic)
			low = b.getValue(expr.Low, getPos(expr))
			low = b.extendInteger(low, lowType, b.uintptrType)
		} else {
			lowType = types.Typ[types.Uintptr]
			low = llvm.ConstInt(b.uintptrType, 0, false)
		}

		if expr.High != nil {
			highType = expr.High.Type().Underlying().(*types.Basic)
			high = b.getValue(expr.High, getPos(expr))
			high = b.extendInteger(high, highType, b.uintptrType)
		} else {
			highType = types.Typ[types.Uintptr]
		}

		if expr.Max != nil {
			maxType = expr.Max.Type().Underlying().(*types.Basic)
			max = b.getValue(expr.Max, getPos(expr))
			max = b.extendInteger(max, maxType, b.uintptrType)
		} else {
			maxType = types.Typ[types.Uintptr]
		}

		switch typ := expr.X.Type().Underlying().(type) {
		case *types.Pointer: // pointer to array
			// slice an array
			arrayType := typ.Elem().Underlying().(*types.Array)
			length := arrayType.Len()
			llvmLen := llvm.ConstInt(b.uintptrType, uint64(length), false)
			if high.IsNil() {
				high = llvmLen
			}
			if max.IsNil() {
				max = llvmLen
			}
			indices := []llvm.Value{
				llvm.ConstInt(b.ctx.Int32Type(), 0, false),
				low,
			}

			b.createNilCheck(expr.X, value, "slice")
			b.createSliceBoundsCheck(llvmLen, low, high, max, lowType, highType, maxType)

			// Truncate ints bigger than uintptr. This is after the bounds
			// check so it's safe.
			if b.targetData.TypeAllocSize(low.Type()) > b.targetData.TypeAllocSize(b.uintptrType) {
				low = b.CreateTrunc(low, b.uintptrType, "")
			}
			if b.targetData.TypeAllocSize(high.Type()) > b.targetData.TypeAllocSize(b.uintptrType) {
				high = b.CreateTrunc(high, b.uintptrType, "")
			}
			if b.targetData.TypeAllocSize(max.Type()) > b.targetData.TypeAllocSize(b.uintptrType) {
				max = b.CreateTrunc(max, b.uintptrType, "")
			}

			sliceLen := b.CreateSub(high, low, "slice.len")
			slicePtr := b.CreateInBoundsGEP(b.getLLVMType(arrayType), value, indices, "slice.ptr")
			sliceCap := b.CreateSub(max, low, "slice.cap")

			slice := b.ctx.ConstStruct([]llvm.Value{
				llvm.Undef(slicePtr.Type()),
				llvm.Undef(b.uintptrType),
				llvm.Undef(b.uintptrType),
			}, false)
			slice = b.CreateInsertValue(slice, slicePtr, 0, "")
			slice = b.CreateInsertValue(slice, sliceLen, 1, "")
			slice = b.CreateInsertValue(slice, sliceCap, 2, "")
			return slice, nil

		case *types.Slice:
			// slice a slice
			oldPtr := b.CreateExtractValue(value, 0, "")
			oldLen := b.CreateExtractValue(value, 1, "")
			oldCap := b.CreateExtractValue(value, 2, "")
			if high.IsNil() {
				high = oldLen
			}
			if max.IsNil() {
				max = oldCap
			}

			b.createSliceBoundsCheck(oldCap, low, high, max, lowType, highType, maxType)

			// Truncate ints bigger than uintptr. This is after the bounds
			// check so it's safe.
			if b.targetData.TypeAllocSize(low.Type()) > b.targetData.TypeAllocSize(b.uintptrType) {
				low = b.CreateTrunc(low, b.uintptrType, "")
			}
			if b.targetData.TypeAllocSize(high.Type()) > b.targetData.TypeAllocSize(b.uintptrType) {
				high = b.CreateTrunc(high, b.uintptrType, "")
			}
			if b.targetData.TypeAllocSize(max.Type()) > b.targetData.TypeAllocSize(b.uintptrType) {
				max = b.CreateTrunc(max, b.uintptrType, "")
			}

			ptrElemType := b.getLLVMType(typ.Elem())
			newPtr := b.CreateInBoundsGEP(ptrElemType, oldPtr, []llvm.Value{low}, "")
			newLen := b.CreateSub(high, low, "")
			newCap := b.CreateSub(max, low, "")
			slice := b.ctx.ConstStruct([]llvm.Value{
				llvm.Undef(newPtr.Type()),
				llvm.Undef(b.uintptrType),
				llvm.Undef(b.uintptrType),
			}, false)
			slice = b.CreateInsertValue(slice, newPtr, 0, "")
			slice = b.CreateInsertValue(slice, newLen, 1, "")
			slice = b.CreateInsertValue(slice, newCap, 2, "")
			return slice, nil

		case *types.Basic:
			if typ.Info()&types.IsString == 0 {
				return llvm.Value{}, b.makeError(expr.Pos(), "unknown slice type: "+typ.String())
			}
			// slice a string
			if expr.Max != nil {
				// This might as well be a panic, as the frontend should have
				// handled this already.
				return llvm.Value{}, b.makeError(expr.Pos(), "slicing a string with a max parameter is not allowed by the spec")
			}
			oldPtr := b.CreateExtractValue(value, 0, "")
			oldLen := b.CreateExtractValue(value, 1, "")
			if high.IsNil() {
				high = oldLen
			}

			b.createSliceBoundsCheck(oldLen, low, high, high, lowType, highType, maxType)

			// Truncate ints bigger than uintptr. This is after the bounds
			// check so it's safe.
			if b.targetData.TypeAllocSize(low.Type()) > b.targetData.TypeAllocSize(b.uintptrType) {
				low = b.CreateTrunc(low, b.uintptrType, "")
			}
			if b.targetData.TypeAllocSize(high.Type()) > b.targetData.TypeAllocSize(b.uintptrType) {
				high = b.CreateTrunc(high, b.uintptrType, "")
			}

			newPtr := b.CreateInBoundsGEP(b.ctx.Int8Type(), oldPtr, []llvm.Value{low}, "")
			newLen := b.CreateSub(high, low, "")
			str := llvm.Undef(b.getLLVMRuntimeType("_string"))
			str = b.CreateInsertValue(str, newPtr, 0, "")
			str = b.CreateInsertValue(str, newLen, 1, "")
			return str, nil

		default:
			return llvm.Value{}, b.makeError(expr.Pos(), "unknown slice type: "+typ.String())
		}
	case *ssa.SliceToArrayPointer:
		// Conversion from a slice to an array pointer, as the name clearly
		// says. This requires a runtime check to make sure the slice is at
		// least as big as the array.
		slice := b.getValue(expr.X, getPos(expr))
		sliceLen := b.CreateExtractValue(slice, 1, "")
		arrayLen := expr.Type().Underlying().(*types.Pointer).Elem().Underlying().(*types.Array).Len()
		b.createSliceToArrayPointerCheck(sliceLen, arrayLen)
		ptr := b.CreateExtractValue(slice, 0, "")
		return ptr, nil
	case *ssa.TypeAssert:
		return b.createTypeAssert(expr), nil
	case *ssa.UnOp:
		return b.createUnOp(expr)
	default:
		return llvm.Value{}, b.makeError(expr.Pos(), "todo: unknown expression: "+expr.String())
	}
}

// createBinOp creates a LLVM binary operation (add, sub, mul, etc) for a Go
// binary operation. This is almost a direct mapping, but there are some subtle
// differences such as the requirement in LLVM IR that both sides must have the
// same type, even for bitshifts. Also, signedness in Go is encoded in the type
// and is encoded in the operation in LLVM IR: this is important for some
// operations such as divide.
func (b *builder) createBinOp(op token.Token, typ, ytyp types.Type, x, y llvm.Value, pos token.Pos) (llvm.Value, error) {
	switch typ := typ.Underlying().(type) {
	case *types.Basic:
		if typ.Info()&types.IsInteger != 0 {
			// Operations on integers
			signed := typ.Info()&types.IsUnsigned == 0
			switch op {
			case token.ADD: // +
				return b.CreateAdd(x, y, ""), nil
			case token.SUB: // -
				return b.CreateSub(x, y, ""), nil
			case token.MUL: // *
				return b.CreateMul(x, y, ""), nil
			case token.QUO, token.REM: // /, %
				// Check for a divide by zero. If y is zero, the Go
				// specification says that a runtime error must be triggered.
				b.createDivideByZeroCheck(y)

				if signed {
					// Deal with signed division overflow.
					// The LLVM LangRef says:
					//
					//   Overflow also leads to undefined behavior; this is a
					//   rare case, but can occur, for example, by doing a
					//   32-bit division of -2147483648 by -1.
					//
					// The Go specification however says this about division:
					//
					//   The one exception to this rule is that if the dividend
					//   x is the most negative value for the int type of x, the
					//   quotient q = x / -1 is equal to x (and r = 0) due to
					//   two's-complement integer overflow.
					//
					// In other words, in the special case that the lowest
					// possible signed integer is divided by -1, the result of
					// the division is the same as x (the dividend).
					// This is implemented by checking for this condition and
					// changing y to 1 if it occurs, for example for 32-bit
					// ints:
					//
					//   if x == -2147483648 && y == -1 {
					//       y = 1
					//   }
					//
					// Dividing x by 1 obviously returns x, therefore satisfying
					// the Go specification without a branch.
					llvmType := x.Type()
					minusOne := llvm.ConstSub(llvm.ConstInt(llvmType, 0, false), llvm.ConstInt(llvmType, 1, false))
					lowestInteger := llvm.ConstInt(x.Type(), 1<<(llvmType.IntTypeWidth()-1), false)
					yIsMinusOne := b.CreateICmp(llvm.IntEQ, y, minusOne, "")
					xIsLowestInteger := b.CreateICmp(llvm.IntEQ, x, lowestInteger, "")
					hasOverflow := b.CreateAnd(yIsMinusOne, xIsLowestInteger, "")
					y = b.CreateSelect(hasOverflow, llvm.ConstInt(llvmType, 1, true), y, "")

					if op == token.QUO {
						return b.CreateSDiv(x, y, ""), nil
					} else {
						return b.CreateSRem(x, y, ""), nil
					}
				} else {
					if op == token.QUO {
						return b.CreateUDiv(x, y, ""), nil
					} else {
						return b.CreateURem(x, y, ""), nil
					}
				}
			case token.AND: // &
				return b.CreateAnd(x, y, ""), nil
			case token.OR: // |
				return b.CreateOr(x, y, ""), nil
			case token.XOR: // ^
				return b.CreateXor(x, y, ""), nil
			case token.SHL, token.SHR:
				if ytyp.Underlying().(*types.Basic).Info()&types.IsUnsigned == 0 {
					// Ensure that y is not negative.
					b.createNegativeShiftCheck(y)
				}

				sizeX := b.targetData.TypeAllocSize(x.Type())
				sizeY := b.targetData.TypeAllocSize(y.Type())

				// Check if the shift is bigger than the bit-width of the shifted value.
				// This is UB in LLVM, so it needs to be handled separately.
				// The Go spec indirectly defines the result as 0.
				// Negative shifts are handled earlier, so we can treat y as unsigned.
				overshifted := b.CreateICmp(llvm.IntUGE, y, llvm.ConstInt(y.Type(), 8*sizeX, false), "shift.overflow")

				// Adjust the size of y to match x.
				switch {
				case sizeX > sizeY:
					y = b.CreateZExt(y, x.Type(), "")
				case sizeX < sizeY:
					// If it gets truncated, overshifted will be true and it will not matter.
					y = b.CreateTrunc(y, x.Type(), "")
				}

				// Create a shift operation.
				var val llvm.Value
				switch op {
				case token.SHL: // <<
					val = b.CreateShl(x, y, "")
				case token.SHR: // >>
					if signed {
						// Arithmetic right shifts work differently, since shifting a negative number right yields -1.
						// Cap the shift input rather than selecting the output.
						y = b.CreateSelect(overshifted, llvm.ConstInt(y.Type(), 8*sizeX-1, false), y, "shift.offset")
						return b.CreateAShr(x, y, ""), nil
					} else {
						val = b.CreateLShr(x, y, "")
					}
				default:
					panic("unreachable")
				}

				// Select between the shift result and zero depending on whether there was an overshift.
				return b.CreateSelect(overshifted, llvm.ConstInt(val.Type(), 0, false), val, "shift.result"), nil
			case token.EQL: // ==
				return b.CreateICmp(llvm.IntEQ, x, y, ""), nil
			case token.NEQ: // !=
				return b.CreateICmp(llvm.IntNE, x, y, ""), nil
			case token.AND_NOT: // &^
				// Go specific. Calculate "and not" with x & (~y)
				inv := b.CreateNot(y, "") // ~y
				return b.CreateAnd(x, inv, ""), nil
			case token.LSS: // <
				if signed {
					return b.CreateICmp(llvm.IntSLT, x, y, ""), nil
				} else {
					return b.CreateICmp(llvm.IntULT, x, y, ""), nil
				}
			case token.LEQ: // <=
				if signed {
					return b.CreateICmp(llvm.IntSLE, x, y, ""), nil
				} else {
					return b.CreateICmp(llvm.IntULE, x, y, ""), nil
				}
			case token.GTR: // >
				if signed {
					return b.CreateICmp(llvm.IntSGT, x, y, ""), nil
				} else {
					return b.CreateICmp(llvm.IntUGT, x, y, ""), nil
				}
			case token.GEQ: // >=
				if signed {
					return b.CreateICmp(llvm.IntSGE, x, y, ""), nil
				} else {
					return b.CreateICmp(llvm.IntUGE, x, y, ""), nil
				}
			default:
				panic("binop on integer: " + op.String())
			}
		} else if typ.Info()&types.IsFloat != 0 {
			// Operations on floats
			switch op {
			case token.ADD: // +
				return b.CreateFAdd(x, y, ""), nil
			case token.SUB: // -
				return b.CreateFSub(x, y, ""), nil
			case token.MUL: // *
				return b.CreateFMul(x, y, ""), nil
			case token.QUO: // /
				return b.CreateFDiv(x, y, ""), nil
			case token.EQL: // ==
				return b.CreateFCmp(llvm.FloatOEQ, x, y, ""), nil
			case token.NEQ: // !=
				return b.CreateFCmp(llvm.FloatUNE, x, y, ""), nil
			case token.LSS: // <
				return b.CreateFCmp(llvm.FloatOLT, x, y, ""), nil
			case token.LEQ: // <=
				return b.CreateFCmp(llvm.FloatOLE, x, y, ""), nil
			case token.GTR: // >
				return b.CreateFCmp(llvm.FloatOGT, x, y, ""), nil
			case token.GEQ: // >=
				return b.CreateFCmp(llvm.FloatOGE, x, y, ""), nil
			default:
				panic("binop on float: " + op.String())
			}
		} else if typ.Info()&types.IsComplex != 0 {
			r1 := b.CreateExtractValue(x, 0, "r1")
			r2 := b.CreateExtractValue(y, 0, "r2")
			i1 := b.CreateExtractValue(x, 1, "i1")
			i2 := b.CreateExtractValue(y, 1, "i2")
			switch op {
			case token.EQL: // ==
				req := b.CreateFCmp(llvm.FloatOEQ, r1, r2, "")
				ieq := b.CreateFCmp(llvm.FloatOEQ, i1, i2, "")
				return b.CreateAnd(req, ieq, ""), nil
			case token.NEQ: // !=
				req := b.CreateFCmp(llvm.FloatOEQ, r1, r2, "")
				ieq := b.CreateFCmp(llvm.FloatOEQ, i1, i2, "")
				neq := b.CreateAnd(req, ieq, "")
				return b.CreateNot(neq, ""), nil
			case token.ADD, token.SUB:
				var r, i llvm.Value
				switch op {
				case token.ADD:
					r = b.CreateFAdd(r1, r2, "")
					i = b.CreateFAdd(i1, i2, "")
				case token.SUB:
					r = b.CreateFSub(r1, r2, "")
					i = b.CreateFSub(i1, i2, "")
				default:
					panic("unreachable")
				}
				cplx := llvm.Undef(b.ctx.StructType([]llvm.Type{r.Type(), i.Type()}, false))
				cplx = b.CreateInsertValue(cplx, r, 0, "")
				cplx = b.CreateInsertValue(cplx, i, 1, "")
				return cplx, nil
			case token.MUL:
				// Complex multiplication follows the current implementation in
				// the Go compiler, with the difference that complex64
				// components are not first scaled up to float64 for increased
				// precision.
				// https://github.com/golang/go/blob/170b8b4b12be50eeccbcdadb8523fb4fc670ca72/src/cmd/compile/internal/gc/ssa.go#L2089-L2127
				// The implementation is as follows:
				//   r := real(a) * real(b) - imag(a) * imag(b)
				//   i := real(a) * imag(b) + imag(a) * real(b)
				// Note: this does NOT follow the C11 specification (annex G):
				// http://www.open-std.org/jtc1/sc22/wg14/www/docs/n1548.pdf#page=549
				// See https://github.com/golang/go/issues/29846 for a related
				// discussion.
				r := b.CreateFSub(b.CreateFMul(r1, r2, ""), b.CreateFMul(i1, i2, ""), "")
				i := b.CreateFAdd(b.CreateFMul(r1, i2, ""), b.CreateFMul(i1, r2, ""), "")
				cplx := llvm.Undef(b.ctx.StructType([]llvm.Type{r.Type(), i.Type()}, false))
				cplx = b.CreateInsertValue(cplx, r, 0, "")
				cplx = b.CreateInsertValue(cplx, i, 1, "")
				return cplx, nil
			case token.QUO:
				// Complex division.
				// Do this in a library call because it's too difficult to do
				// inline.
				switch r1.Type().TypeKind() {
				case llvm.FloatTypeKind:
					return b.createRuntimeCall("complex64div", []llvm.Value{x, y}, ""), nil
				case llvm.DoubleTypeKind:
					return b.createRuntimeCall("complex128div", []llvm.Value{x, y}, ""), nil
				default:
					panic("unexpected complex type")
				}
			default:
				panic("binop on complex: " + op.String())
			}
		} else if typ.Info()&types.IsBoolean != 0 {
			// Operations on booleans
			switch op {
			case token.EQL: // ==
				return b.CreateICmp(llvm.IntEQ, x, y, ""), nil
			case token.NEQ: // !=
				return b.CreateICmp(llvm.IntNE, x, y, ""), nil
			default:
				panic("binop on bool: " + op.String())
			}
		} else if typ.Kind() == types.UnsafePointer {
			// Operations on pointers
			switch op {
			case token.EQL: // ==
				return b.CreateICmp(llvm.IntEQ, x, y, ""), nil
			case token.NEQ: // !=
				return b.CreateICmp(llvm.IntNE, x, y, ""), nil
			default:
				panic("binop on pointer: " + op.String())
			}
		} else if typ.Info()&types.IsString != 0 {
			// Operations on strings
			switch op {
			case token.ADD: // +
				return b.createRuntimeCall("stringConcat", []llvm.Value{x, y}, ""), nil
			case token.EQL: // ==
				return b.createRuntimeCall("stringEqual", []llvm.Value{x, y}, ""), nil
			case token.NEQ: // !=
				result := b.createRuntimeCall("stringEqual", []llvm.Value{x, y}, "")
				return b.CreateNot(result, ""), nil
			case token.LSS: // x < y
				return b.createRuntimeCall("stringLess", []llvm.Value{x, y}, ""), nil
			case token.LEQ: // x <= y becomes NOT (y < x)
				result := b.createRuntimeCall("stringLess", []llvm.Value{y, x}, "")
				return b.CreateNot(result, ""), nil
			case token.GTR: // x > y becomes y < x
				return b.createRuntimeCall("stringLess", []llvm.Value{y, x}, ""), nil
			case token.GEQ: // x >= y becomes NOT (x < y)
				result := b.createRuntimeCall("stringLess", []llvm.Value{x, y}, "")
				return b.CreateNot(result, ""), nil
			default:
				panic("binop on string: " + op.String())
			}
		} else {
			return llvm.Value{}, b.makeError(pos, "todo: unknown basic type in binop: "+typ.String())
		}
	case *types.Signature:
		// Get raw scalars from the function value and compare those.
		// Function values may be implemented in multiple ways, but they all
		// have some way of getting a scalar value identifying the function.
		// This is safe: function pointers are generally not comparable
		// against each other, only against nil. So one of these has to be nil.
		x = b.extractFuncScalar(x)
		y = b.extractFuncScalar(y)
		switch op {
		case token.EQL: // ==
			return b.CreateICmp(llvm.IntEQ, x, y, ""), nil
		case token.NEQ: // !=
			return b.CreateICmp(llvm.IntNE, x, y, ""), nil
		default:
			return llvm.Value{}, b.makeError(pos, "binop on signature: "+op.String())
		}
	case *types.Interface:
		switch op {
		case token.EQL, token.NEQ: // ==, !=
			nilInterface := llvm.ConstNull(x.Type())
			var result llvm.Value
			if x == nilInterface || y == nilInterface {
				// An interface value is compared against nil.
				// This is a very common case and is easy to optimize: simply
				// compare the typecodes (of which one is nil).
				typecodeX := b.CreateExtractValue(x, 0, "")
				typecodeY := b.CreateExtractValue(y, 0, "")
				result = b.CreateICmp(llvm.IntEQ, typecodeX, typecodeY, "")
			} else {
				// Fall back to a full interface comparison.
				result = b.createRuntimeCall("interfaceEqual", []llvm.Value{x, y}, "")
			}
			if op == token.NEQ {
				result = b.CreateNot(result, "")
			}
			return result, nil
		default:
			return llvm.Value{}, b.makeError(pos, "binop on interface: "+op.String())
		}
	case *types.Chan, *types.Map, *types.Pointer:
		// Maps are in general not comparable, but can be compared against nil
		// (which is a nil pointer). This means they can be trivially compared
		// by treating them as a pointer.
		// Channels behave as pointers in that they are equal as long as they
		// are created with the same call to make or if both are nil.
		switch op {
		case token.EQL: // ==
			return b.CreateICmp(llvm.IntEQ, x, y, ""), nil
		case token.NEQ: // !=
			return b.CreateICmp(llvm.IntNE, x, y, ""), nil
		default:
			return llvm.Value{}, b.makeError(pos, "todo: binop on pointer: "+op.String())
		}
	case *types.Slice:
		// Slices are in general not comparable, but can be compared against
		// nil. Assume at least one of them is nil to make the code easier.
		xPtr := b.CreateExtractValue(x, 0, "")
		yPtr := b.CreateExtractValue(y, 0, "")
		switch op {
		case token.EQL: // ==
			return b.CreateICmp(llvm.IntEQ, xPtr, yPtr, ""), nil
		case token.NEQ: // !=
			return b.CreateICmp(llvm.IntNE, xPtr, yPtr, ""), nil
		default:
			return llvm.Value{}, b.makeError(pos, "todo: binop on slice: "+op.String())
		}
	case *types.Array:
		// Compare each array element and combine the result. From the spec:
		//     Array values are comparable if values of the array element type
		//     are comparable. Two array values are equal if their corresponding
		//     elements are equal.
		result := llvm.ConstInt(b.ctx.Int1Type(), 1, true)
		for i := 0; i < int(typ.Len()); i++ {
			xField := b.CreateExtractValue(x, i, "")
			yField := b.CreateExtractValue(y, i, "")
			fieldEqual, err := b.createBinOp(token.EQL, typ.Elem(), typ.Elem(), xField, yField, pos)
			if err != nil {
				return llvm.Value{}, err
			}
			result = b.CreateAnd(result, fieldEqual, "")
		}
		switch op {
		case token.EQL: // ==
			return result, nil
		case token.NEQ: // !=
			return b.CreateNot(result, ""), nil
		default:
			return llvm.Value{}, b.makeError(pos, "unknown: binop on struct: "+op.String())
		}
	case *types.Struct:
		// Compare each struct field and combine the result. From the spec:
		//     Struct values are comparable if all their fields are comparable.
		//     Two struct values are equal if their corresponding non-blank
		//     fields are equal.
		result := llvm.ConstInt(b.ctx.Int1Type(), 1, true)
		for i := 0; i < typ.NumFields(); i++ {
			if typ.Field(i).Name() == "_" {
				// skip blank fields
				continue
			}
			fieldType := typ.Field(i).Type()
			xField := b.CreateExtractValue(x, i, "")
			yField := b.CreateExtractValue(y, i, "")
			fieldEqual, err := b.createBinOp(token.EQL, fieldType, fieldType, xField, yField, pos)
			if err != nil {
				return llvm.Value{}, err
			}
			result = b.CreateAnd(result, fieldEqual, "")
		}
		switch op {
		case token.EQL: // ==
			return result, nil
		case token.NEQ: // !=
			return b.CreateNot(result, ""), nil
		default:
			return llvm.Value{}, b.makeError(pos, "unknown: binop on struct: "+op.String())
		}
	default:
		return llvm.Value{}, b.makeError(pos, "todo: binop type: "+typ.String())
	}
}

// createConst creates a LLVM constant value from a Go constant.
func (c *compilerContext) createConst(expr *ssa.Const, pos token.Pos) llvm.Value {
	switch typ := expr.Type().Underlying().(type) {
	case *types.Basic:
		llvmType := c.getLLVMType(typ)
		if typ.Info()&types.IsBoolean != 0 {
			n := uint64(0)
			if constant.BoolVal(expr.Value) {
				n = 1
			}
			return llvm.ConstInt(llvmType, n, false)
		} else if typ.Info()&types.IsString != 0 {
			str := constant.StringVal(expr.Value)
			strLen := llvm.ConstInt(c.uintptrType, uint64(len(str)), false)
			var strPtr llvm.Value
			if str != "" {
				objname := c.pkg.Path() + "$string"
				globalType := llvm.ArrayType(c.ctx.Int8Type(), len(str))
				global := llvm.AddGlobal(c.mod, globalType, objname)
				global.SetInitializer(c.ctx.ConstString(str, false))
				global.SetLinkage(llvm.InternalLinkage)
				global.SetGlobalConstant(true)
				global.SetUnnamedAddr(true)
				global.SetAlignment(1)
				if c.Debug {
					// Unfortunately, expr.Pos() is always token.NoPos.
					position := c.program.Fset.Position(pos)
					diglobal := c.dibuilder.CreateGlobalVariableExpression(llvm.Metadata{}, llvm.DIGlobalVariableExpression{
						File:        c.getDIFile(position.Filename),
						Line:        position.Line,
						Type:        c.getDIType(types.NewArray(types.Typ[types.Byte], int64(len(str)))),
						LocalToUnit: true,
						Expr:        c.dibuilder.CreateExpression(nil),
					})
					global.AddMetadata(0, diglobal)
				}
				zero := llvm.ConstInt(c.ctx.Int32Type(), 0, false)
				strPtr = llvm.ConstInBoundsGEP(globalType, global, []llvm.Value{zero, zero})
			} else {
				strPtr = llvm.ConstNull(c.dataPtrType)
			}
			strObj := llvm.ConstNamedStruct(c.getLLVMRuntimeType("_string"), []llvm.Value{strPtr, strLen})
			return strObj
		} else if typ.Kind() == types.UnsafePointer {
			if !expr.IsNil() {
				value, _ := constant.Uint64Val(constant.ToInt(expr.Value))
				return llvm.ConstIntToPtr(llvm.ConstInt(c.uintptrType, value, false), c.dataPtrType)
			}
			return llvm.ConstNull(c.dataPtrType)
		} else if typ.Info()&types.IsUnsigned != 0 {
			n, _ := constant.Uint64Val(constant.ToInt(expr.Value))
			return llvm.ConstInt(llvmType, n, false)
		} else if typ.Info()&types.IsInteger != 0 { // signed
			n, _ := constant.Int64Val(constant.ToInt(expr.Value))
			return llvm.ConstInt(llvmType, uint64(n), true)
		} else if typ.Info()&types.IsFloat != 0 {
			n, _ := constant.Float64Val(expr.Value)
			return llvm.ConstFloat(llvmType, n)
		} else if typ.Kind() == types.Complex64 {
			r := c.createConst(ssa.NewConst(constant.Real(expr.Value), types.Typ[types.Float32]), pos)
			i := c.createConst(ssa.NewConst(constant.Imag(expr.Value), types.Typ[types.Float32]), pos)
			cplx := llvm.Undef(c.ctx.StructType([]llvm.Type{c.ctx.FloatType(), c.ctx.FloatType()}, false))
			cplx = c.builder.CreateInsertValue(cplx, r, 0, "")
			cplx = c.builder.CreateInsertValue(cplx, i, 1, "")
			return cplx
		} else if typ.Kind() == types.Complex128 {
			r := c.createConst(ssa.NewConst(constant.Real(expr.Value), types.Typ[types.Float64]), pos)
			i := c.createConst(ssa.NewConst(constant.Imag(expr.Value), types.Typ[types.Float64]), pos)
			cplx := llvm.Undef(c.ctx.StructType([]llvm.Type{c.ctx.DoubleType(), c.ctx.DoubleType()}, false))
			cplx = c.builder.CreateInsertValue(cplx, r, 0, "")
			cplx = c.builder.CreateInsertValue(cplx, i, 1, "")
			return cplx
		} else {
			panic("unknown constant of basic type: " + expr.String())
		}
	case *types.Chan:
		if expr.Value != nil {
			panic("expected nil chan constant")
		}
		return llvm.ConstNull(c.getLLVMType(expr.Type()))
	case *types.Signature:
		if expr.Value != nil {
			panic("expected nil signature constant")
		}
		return llvm.ConstNull(c.getLLVMType(expr.Type()))
	case *types.Interface:
		if expr.Value != nil {
			panic("expected nil interface constant")
		}
		// Create a generic nil interface with no dynamic type (typecode=0).
		fields := []llvm.Value{
			llvm.ConstInt(c.uintptrType, 0, false),
			llvm.ConstPointerNull(c.dataPtrType),
		}
		return llvm.ConstNamedStruct(c.getLLVMRuntimeType("_interface"), fields)
	case *types.Pointer:
		if expr.Value != nil {
			panic("expected nil pointer constant")
		}
		return llvm.ConstPointerNull(c.getLLVMType(typ))
	case *types.Array:
		if expr.Value != nil {
			panic("expected nil array constant")
		}
		return llvm.ConstNull(c.getLLVMType(expr.Type()))
	case *types.Slice:
		if expr.Value != nil {
			panic("expected nil slice constant")
		}
		llvmPtr := llvm.ConstPointerNull(c.dataPtrType)
		llvmLen := llvm.ConstInt(c.uintptrType, 0, false)
		slice := c.ctx.ConstStruct([]llvm.Value{
			llvmPtr, // backing array
			llvmLen, // len
			llvmLen, // cap
		}, false)
		return slice
	case *types.Struct:
		if expr.Value != nil {
			panic("expected nil struct constant")
		}
		return llvm.ConstNull(c.getLLVMType(expr.Type()))
	case *types.Map:
		if !expr.IsNil() {
			// I believe this is not allowed by the Go spec.
			panic("non-nil map constant")
		}
		llvmType := c.getLLVMType(typ)
		return llvm.ConstNull(llvmType)
	default:
		panic("unknown constant: " + expr.String())
	}
}

// createConvert creates a Go type conversion instruction.
func (b *builder) createConvert(typeFrom, typeTo types.Type, value llvm.Value, pos token.Pos) (llvm.Value, error) {
	llvmTypeFrom := value.Type()
	llvmTypeTo := b.getLLVMType(typeTo)

	// Conversion between unsafe.Pointer and uintptr.
	isPtrFrom := isPointer(typeFrom.Underlying())
	isPtrTo := isPointer(typeTo.Underlying())
	if isPtrFrom && !isPtrTo {
		return b.CreatePtrToInt(value, llvmTypeTo, ""), nil
	} else if !isPtrFrom && isPtrTo {
		return b.CreateIntToPtr(value, llvmTypeTo, ""), nil
	}

	// Conversion between pointers and unsafe.Pointer.
	if isPtrFrom && isPtrTo {
		return value, nil
	}

	switch typeTo := typeTo.Underlying().(type) {
	case *types.Basic:
		sizeFrom := b.targetData.TypeAllocSize(llvmTypeFrom)

		if typeTo.Info()&types.IsString != 0 {
			switch typeFrom := typeFrom.Underlying().(type) {
			case *types.Basic:
				// Assume a Unicode code point, as that is the only possible
				// value here.
				// Cast to an i32 value as expected by
				// runtime.stringFromUnicode.
				if sizeFrom > 4 {
					value = b.CreateTrunc(value, b.ctx.Int32Type(), "")
				} else if sizeFrom < 4 && typeTo.Info()&types.IsUnsigned != 0 {
					value = b.CreateZExt(value, b.ctx.Int32Type(), "")
				} else if sizeFrom < 4 {
					value = b.CreateSExt(value, b.ctx.Int32Type(), "")
				}
				return b.createRuntimeCall("stringFromUnicode", []llvm.Value{value}, ""), nil
			case *types.Slice:
				switch typeFrom.Elem().(*types.Basic).Kind() {
				case types.Byte:
					return b.createRuntimeCall("stringFromBytes", []llvm.Value{value}, ""), nil
				case types.Rune:
					return b.createRuntimeCall("stringFromRunes", []llvm.Value{value}, ""), nil
				default:
					return llvm.Value{}, b.makeError(pos, "todo: convert to string: "+typeFrom.String())
				}
			default:
				return llvm.Value{}, b.makeError(pos, "todo: convert to string: "+typeFrom.String())
			}
		}

		typeFrom := typeFrom.Underlying().(*types.Basic)
		sizeTo := b.targetData.TypeAllocSize(llvmTypeTo)

		if typeFrom.Info()&types.IsInteger != 0 && typeTo.Info()&types.IsInteger != 0 {
			// Conversion between two integers.
			if sizeFrom > sizeTo {
				return b.CreateTrunc(value, llvmTypeTo, ""), nil
			} else if typeFrom.Info()&types.IsUnsigned != 0 { // if unsigned
				return b.CreateZExt(value, llvmTypeTo, ""), nil
			} else { // if signed
				return b.CreateSExt(value, llvmTypeTo, ""), nil
			}
		}

		if typeFrom.Info()&types.IsFloat != 0 && typeTo.Info()&types.IsFloat != 0 {
			// Conversion between two floats.
			if sizeFrom > sizeTo {
				return b.CreateFPTrunc(value, llvmTypeTo, ""), nil
			} else if sizeFrom < sizeTo {
				return b.CreateFPExt(value, llvmTypeTo, ""), nil
			} else {
				return value, nil
			}
		}

		if typeFrom.Info()&types.IsFloat != 0 && typeTo.Info()&types.IsInteger != 0 {
			// Conversion from float to int.
			// Passing an out-of-bounds float to LLVM would cause UB, so that UB is trapped by select instructions.
			// The Go specification says that this should be implementation-defined behavior.
			// This implements saturating behavior, except that NaN is mapped to the minimum value.
			var significandBits int
			switch typeFrom.Kind() {
			case types.Float32:
				significandBits = 23
			case types.Float64:
				significandBits = 52
			}
			if typeTo.Info()&types.IsUnsigned != 0 { // if unsigned
				// Select the maximum value for this unsigned integer type.
				max := ^(^uint64(0) << uint(llvmTypeTo.IntTypeWidth()))
				maxFloat := float64(max)
				if bits.Len64(max) > significandBits {
					// Round the max down to fit within the significand.
					maxFloat = float64(max & (^uint64(0) << uint(bits.Len64(max)-significandBits)))
				}

				// Check if the value is in-bounds (0 <= value <= max).
				positive := b.CreateFCmp(llvm.FloatOLE, llvm.ConstNull(llvmTypeFrom), value, "positive")
				withinMax := b.CreateFCmp(llvm.FloatOLE, value, llvm.ConstFloat(llvmTypeFrom, maxFloat), "withinmax")
				inBounds := b.CreateAnd(positive, withinMax, "inbounds")

				// Assuming that the value is out-of-bounds, select a saturated value.
				saturated := b.CreateSelect(positive,
					llvm.ConstInt(llvmTypeTo, max, false), // value > max
					llvm.ConstNull(llvmTypeTo),            // value < 0 (or NaN)
					"saturated",
				)

				// Do a normal conversion.
				normal := b.CreateFPToUI(value, llvmTypeTo, "normal")

				return b.CreateSelect(inBounds, normal, saturated, ""), nil
			} else { // if signed
				// Select the minimum value for this signed integer type.
				min := uint64(1) << uint(llvmTypeTo.IntTypeWidth()-1)
				minFloat := -float64(min)

				// Select the maximum value for this signed integer type.
				max := ^(^uint64(0) << uint(llvmTypeTo.IntTypeWidth()-1))
				maxFloat := float64(max)
				if bits.Len64(max) > significandBits {
					// Round the max down to fit within the significand.
					maxFloat = float64(max & (^uint64(0) << uint(bits.Len64(max)-significandBits)))
				}

				// Check if the value is in-bounds (min <= value <= max).
				aboveMin := b.CreateFCmp(llvm.FloatOLE, llvm.ConstFloat(llvmTypeFrom, minFloat), value, "abovemin")
				belowMax := b.CreateFCmp(llvm.FloatOLE, value, llvm.ConstFloat(llvmTypeFrom, maxFloat), "belowmax")
				inBounds := b.CreateAnd(aboveMin, belowMax, "inbounds")

				// Assuming that the value is out-of-bounds, select a saturated value.
				saturated := b.CreateSelect(aboveMin,
					llvm.ConstInt(llvmTypeTo, max, false), // value > max
					llvm.ConstInt(llvmTypeTo, min, false), // value < min
					"saturated",
				)

				// Map NaN to 0.
				saturated = b.CreateSelect(b.CreateFCmp(llvm.FloatUNO, value, value, "isnan"),
					llvm.ConstNull(llvmTypeTo),
					saturated,
					"remapped",
				)

				// Do a normal conversion.
				normal := b.CreateFPToSI(value, llvmTypeTo, "normal")

				return b.CreateSelect(inBounds, normal, saturated, ""), nil
			}
		}

		if typeFrom.Info()&types.IsInteger != 0 && typeTo.Info()&types.IsFloat != 0 {
			// Conversion from int to float.
			if typeFrom.Info()&types.IsUnsigned != 0 { // if unsigned
				return b.CreateUIToFP(value, llvmTypeTo, ""), nil
			} else { // if signed
				return b.CreateSIToFP(value, llvmTypeTo, ""), nil
			}
		}

		if typeFrom.Kind() == types.Complex128 && typeTo.Kind() == types.Complex64 {
			// Conversion from complex128 to complex64.
			r := b.CreateExtractValue(value, 0, "real.f64")
			i := b.CreateExtractValue(value, 1, "imag.f64")
			r = b.CreateFPTrunc(r, b.ctx.FloatType(), "real.f32")
			i = b.CreateFPTrunc(i, b.ctx.FloatType(), "imag.f32")
			cplx := llvm.Undef(b.ctx.StructType([]llvm.Type{b.ctx.FloatType(), b.ctx.FloatType()}, false))
			cplx = b.CreateInsertValue(cplx, r, 0, "")
			cplx = b.CreateInsertValue(cplx, i, 1, "")
			return cplx, nil
		}

		if typeFrom.Kind() == types.Complex64 && typeTo.Kind() == types.Complex128 {
			// Conversion from complex64 to complex128.
			r := b.CreateExtractValue(value, 0, "real.f32")
			i := b.CreateExtractValue(value, 1, "imag.f32")
			r = b.CreateFPExt(r, b.ctx.DoubleType(), "real.f64")
			i = b.CreateFPExt(i, b.ctx.DoubleType(), "imag.f64")
			cplx := llvm.Undef(b.ctx.StructType([]llvm.Type{b.ctx.DoubleType(), b.ctx.DoubleType()}, false))
			cplx = b.CreateInsertValue(cplx, r, 0, "")
			cplx = b.CreateInsertValue(cplx, i, 1, "")
			return cplx, nil
		}

		return llvm.Value{}, b.makeError(pos, "todo: convert: basic non-integer type: "+typeFrom.String()+" -> "+typeTo.String())

	case *types.Slice:
		if basic, ok := typeFrom.Underlying().(*types.Basic); !ok || basic.Info()&types.IsString == 0 {
			panic("can only convert from a string to a slice")
		}

		elemType := typeTo.Elem().Underlying().(*types.Basic) // must be byte or rune
		switch elemType.Kind() {
		case types.Byte:
			return b.createRuntimeCall("stringToBytes", []llvm.Value{value}, ""), nil
		case types.Rune:
			return b.createRuntimeCall("stringToRunes", []llvm.Value{value}, ""), nil
		default:
			panic("unexpected type in string to slice conversion")
		}

	default:
		return llvm.Value{}, b.makeError(pos, "todo: convert "+typeTo.String()+" <- "+typeFrom.String())
	}
}

// createUnOp creates LLVM IR for a given Go unary operation.
// Most unary operators are pretty simple, such as the not and minus operator
// which can all be directly lowered to IR. However, there is also the channel
// receive operator which is handled in the runtime directly.
func (b *builder) createUnOp(unop *ssa.UnOp) (llvm.Value, error) {
	x := b.getValue(unop.X, getPos(unop))
	switch unop.Op {
	case token.NOT: // !x
		return b.CreateNot(x, ""), nil
	case token.SUB: // -x
		if typ, ok := unop.X.Type().Underlying().(*types.Basic); ok {
			if typ.Info()&types.IsInteger != 0 {
				return b.CreateSub(llvm.ConstInt(x.Type(), 0, false), x, ""), nil
			} else if typ.Info()&types.IsFloat != 0 {
				return b.CreateFNeg(x, ""), nil
			} else if typ.Info()&types.IsComplex != 0 {
				// Negate both components of the complex number.
				r := b.CreateExtractValue(x, 0, "r")
				i := b.CreateExtractValue(x, 1, "i")
				r = b.CreateFNeg(r, "")
				i = b.CreateFNeg(i, "")
				cplx := llvm.Undef(x.Type())
				cplx = b.CreateInsertValue(cplx, r, 0, "")
				cplx = b.CreateInsertValue(cplx, i, 1, "")
				return cplx, nil
			} else {
				return llvm.Value{}, b.makeError(unop.Pos(), "todo: unknown basic type for negate: "+typ.String())
			}
		} else {
			return llvm.Value{}, b.makeError(unop.Pos(), "todo: unknown type for negate: "+unop.X.Type().Underlying().String())
		}
	case token.MUL: // *x, dereference pointer
		valueType := b.getLLVMType(unop.X.Type().Underlying().(*types.Pointer).Elem())
		if b.targetData.TypeAllocSize(valueType) == 0 {
			// zero-length data
			return llvm.ConstNull(valueType), nil
		} else if strings.HasSuffix(unop.X.String(), "$funcaddr") {
			// CGo function pointer. The cgo part has rewritten CGo function
			// pointers as stub global variables of the form:
			//     var C.add unsafe.Pointer
			// Instead of a load from the global, create a bitcast of the
			// function pointer itself.
			name := strings.TrimSuffix(unop.X.(*ssa.Global).Name(), "$funcaddr")
			pkg := b.fn.Pkg
			if pkg == nil {
				pkg = b.fn.Origin().Pkg
			}
			_, fn := b.getFunction(pkg.Members[name].(*ssa.Function))
			if fn.IsNil() {
				return llvm.Value{}, b.makeError(unop.Pos(), "cgo function not found: "+name)
			}
			return fn, nil
		} else {
			b.createNilCheck(unop.X, x, "deref")
			load := b.CreateLoad(valueType, x, "")
			return load, nil
		}
	case token.XOR: // ^x, toggle all bits in integer
		return b.CreateXor(x, llvm.ConstInt(x.Type(), ^uint64(0), false), ""), nil
	case token.ARROW: // <-x, receive from channel
		return b.createChanRecv(unop), nil
	default:
		return llvm.Value{}, b.makeError(unop.Pos(), "todo: unknown unop")
	}
}
