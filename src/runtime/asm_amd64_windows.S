.section .text.tinygo_scanCurrentStack,"ax"
.global tinygo_scanCurrentStack
tinygo_scanCurrentStack:
    // Save callee-saved registers.
    pushq %rbx
    pushq %rbp
    pushq %rdi
    pushq %rsi
    pushq %r12
    pushq %r13
    pushq %r14
    pushq %r15

    // Scan the stack.
    subq $8, %rsp   // adjust the stack before the call to maintain 16-byte alignment
    movq %rsp, %rcx // pass the stack pointer as the first parameter
    callq tinygo_scanstack

    // Restore the stack pointer. Registers do not need to be restored as they
    // were only pushed to be discoverable by the GC.
    addq $72, %rsp
    retq

.section .text.tinygo_longjmp,"ax"
.global tinygo_longjmp
tinygo_longjmp:
    // Note: the code we jump to assumes rax is set to a non-zero value if we
    // jump from here, so we use rax as the temporary value for jumpPC.
    movq 0(%rcx), %rsp // jumpSP
    movq 8(%rcx), %rax // jumpPC
    jmpq *%rax
