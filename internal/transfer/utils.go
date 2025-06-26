package transfer

/*
#include <stdlib.h>
*/
import "C"
import "unsafe"

func getEnv(key string) string {
	ck := C.CString(key)
	defer C.free(unsafe.Pointer(ck))
	cp := C.getenv(ck)
	if cp == nil {
		return ""
	}
	return C.GoString(cp)
}
