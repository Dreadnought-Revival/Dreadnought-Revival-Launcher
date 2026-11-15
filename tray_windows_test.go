//go:build windows

package main

import (
	"runtime"
	"testing"
	"unsafe"
)

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      winPoint
}

// pump dispatches this thread's pending window messages.
func pump() {
	peek := user32.NewProc("PeekMessageW")
	dispatch := user32.NewProc("DispatchMessageW")
	var m winMsg
	for {
		if got, _, _ := peek.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1 /* PM_REMOVE */); got == 0 {
			return
		}
		_, _, _ = dispatch.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// The hidden window carries "show", "hide" and "the game closed" to the UI
// thread, and a second launcher finds the first through it.
func TestTrayWindowMessages(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if showRunningLauncher() {
		t.Skip("a launcher is running on this desktop")
	}
	var shown, hidden, exited int
	tr, err := newTray("test", trayHandlers{
		show:     func() { shown++ },
		hide:     func() { hidden++ },
		gameExit: func() { exited++ },
	})
	if err != nil {
		t.Skipf("no window can be created here: %v", err)
	}
	tr.post(wmTrayHide)
	tr.post(wmTrayGameExit)
	if !showRunningLauncher() {
		t.Error("a second launcher did not find the running one")
	}
	// The icon's left click shows the window too.
	_, _, _ = procPostMessageW.Call(tr.hwnd, wmTrayIcon, 1, wmLButtonUp)
	pump()
	if shown != 2 || hidden != 1 || exited != 1 {
		t.Errorf("shown %d (want 2), hidden %d (want 1), game exits %d (want 1)", shown, hidden, exited)
	}
	tr.close()
	pump()
	if showRunningLauncher() {
		t.Error("the closed launcher is still found")
	}
	var nilTray *tray
	nilTray.post(wmTrayShow) // no tray: nothing to do, no crash
	nilTray.close()
	if nilTray.shown() {
		t.Error("a missing tray reports its icon as shown")
	}
}

// NOTIFYICONDATAW must have the size Windows expects (shellapi.h, 64-bit):
// Shell_NotifyIcon rejects any other.
func TestNotifyIconDataSize(t *testing.T) {
	if got := unsafe.Sizeof(notifyIconData{}); got != 976 {
		t.Errorf("notifyIconData is %d bytes, want 976", got)
	}
}
