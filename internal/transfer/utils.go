package transfer

/*
#include <stdlib.h>
*/
import "C"
import "unsafe"

// Bypasses Go's environment variables cache and reads directly from OS.
// Enables changing env variables at runtime (delta-rs is a use case)
func getEnv(key string) string {
	ck := C.CString(key)
	defer C.free(unsafe.Pointer(ck))
	cp := C.getenv(ck)
	if cp == nil {
		return ""
	}
	return C.GoString(cp)
}
