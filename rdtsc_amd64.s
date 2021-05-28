#include "textflag.h"

// func RDTSC() int64
TEXT ·RDTSC(SB), NOSPLIT, $0

    RDTSC
    SALQ $32, DX
    ORQ  DX, AX
    MOVQ AX, ret+0(FP)
    RET
