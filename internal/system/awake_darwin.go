//go:build darwin && cgo

package system

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <stdlib.h>
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/pwr_mgt/IOPMLib.h>

static IOReturn vibeRemoteHoldAssertion(const char *kind, const char *name, IOPMAssertionID *out) {
	CFStringRef assertionKind = CFStringCreateWithCString(kCFAllocatorDefault, kind, kCFStringEncodingUTF8);
	CFStringRef assertionName = CFStringCreateWithCString(kCFAllocatorDefault, name, kCFStringEncodingUTF8);
	IOReturn result = IOPMAssertionCreateWithName(assertionKind, kIOPMAssertionLevelOn, assertionName, out);
	CFRelease(assertionKind);
	CFRelease(assertionName);
	return result;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// assertionKind is the narrowest assertion that keeps Remote Control
// connections alive. Unlike PreventSystemSleep — what `caffeinate -s` took —
// it blocks only idle sleep, so closing the lid or sleeping the Mac deliberately
// still works and still parks the battery in its lowest-power state.
const assertionKind = "NetworkClientActive"

func holdSleepAssertion(name string) (func() error, error) {
	kind := C.CString(assertionKind)
	defer C.free(unsafe.Pointer(kind))
	label := C.CString(name)
	defer C.free(unsafe.Pointer(label))
	var id C.IOPMAssertionID
	if result := C.vibeRemoteHoldAssertion(kind, label, &id); result != 0 {
		return nil, fmt.Errorf("create power assertion: IOKit error %d", int(result))
	}
	released := false
	return func() error {
		if released {
			return nil
		}
		released = true
		if result := C.IOPMAssertionRelease(id); result != 0 {
			return fmt.Errorf("release power assertion: IOKit error %d", int(result))
		}
		return nil
	}, nil
}
