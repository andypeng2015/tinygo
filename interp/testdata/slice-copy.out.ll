target datalayout = "e-m:e-i64:64-f80:128-n8:16:32:64-S128"
target triple = "x86_64--linux"

@main.sliceSrcTaint.buf = internal global [2 x i8] c"cd"
@main.sliceDstTaint.buf = internal global [2 x i8] zeroinitializer
@main.sliceSrcExternal1.buf = external global [2 x i8]
@main.sliceDstExternal1.buf = internal global [2 x i8] zeroinitializer
@main.sliceSrcExternal2.buf = internal global [2 x i8] zeroinitializer
@main.sliceDstExternal2.buf = external global [2 x i8]

declare i64 @runtime.sliceCopy(ptr, ptr, i64, i64, i64) unnamed_addr

declare void @runtime.printuint8(i8) local_unnamed_addr

declare void @runtime.printint16(i16) local_unnamed_addr

declare void @use(ptr) local_unnamed_addr

define void @runtime.initAll() unnamed_addr {
entry:
  call void @use(ptr @main.sliceSrcTaint.buf)
  %copy.n4 = call i64 @runtime.sliceCopy(ptr @main.sliceDstTaint.buf, ptr @main.sliceSrcTaint.buf, i64 2, i64 2, i64 1)
  %copy.n5 = call i64 @runtime.sliceCopy(ptr @main.sliceDstExternal1.buf, ptr @main.sliceSrcExternal1.buf, i64 2, i64 2, i64 1)
  %copy.n6 = call i64 @runtime.sliceCopy(ptr @main.sliceDstExternal2.buf, ptr @main.sliceSrcExternal2.buf, i64 2, i64 2, i64 1)
  ret void
}

define void @main() unnamed_addr {
entry:
  call void @runtime.printuint8(i8 3)
  call void @runtime.printuint8(i8 3)
  call void @runtime.printint16(i16 5)
  call void @runtime.printint16(i16 5)
  call void @runtime.printuint8(i8 97)
  %sliceDstTaint.val = load i8, ptr @main.sliceDstTaint.buf, align 1
  call void @runtime.printuint8(i8 %sliceDstTaint.val)
  %sliceDstExternal1.val = load i8, ptr @main.sliceDstExternal1.buf, align 1
  call void @runtime.printuint8(i8 %sliceDstExternal1.val)
  %sliceDstExternal2.val = load i8, ptr @main.sliceDstExternal2.buf, align 1
  call void @runtime.printuint8(i8 %sliceDstExternal2.val)
  ret void
}
