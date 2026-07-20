//go:build darwin

// Package menubar provides the native macOS status item.
package menubar

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>

void octopus_run(const char *settings_url);
*/
import "C"

import (
	"sync"
	"unsafe"
)

var (
	quitMu       sync.Mutex
	quitCallback func()
)

// Run installs a status item with exactly two menu entries and blocks on the
// native application event loop until Quit Octopus is selected.
func Run(settingsURL string, onQuit func()) error {
	quitMu.Lock()
	quitCallback = onQuit
	quitMu.Unlock()
	cURL := C.CString(settingsURL)
	defer C.free(unsafe.Pointer(cURL))
	C.octopus_run(cURL)
	return nil
}

//export octopusWillQuit
func octopusWillQuit() {
	quitMu.Lock()
	callback := quitCallback
	quitCallback = nil
	quitMu.Unlock()
	if callback != nil {
		callback()
	}
}
