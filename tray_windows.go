//go:build windows

package main

// The launcher's notification-area (tray) icon and the hidden window behind
// it.
//
// The hidden window does three jobs:
//   - it receives the tray icon's clicks;
//   - it is how another thread reaches the UI thread reliably. go-webview2's
//     Dispatch posts a THREAD message, and Windows drops those while a modal
//     loop runs (dragging the window, an open menu, a dialog); a message
//     posted to a window is kept. "The game has closed" must not be lost;
//   - it is how a second launcher finds the first: it looks the window up by
//     its class name and asks it to show itself (showRunningLauncher).
//
// It is created on the UI thread before the window's message loop starts
// (webview.Run dispatches every message of that thread), so its window
// procedure, and with it every callback here, runs on the UI thread.
//
// A top-level window, not a message-only one: FindWindow does not find
// message-only windows and they do not get the "TaskbarCreated" broadcast.

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const trayClassName = "DreadnoughtRevivalLauncherTray"

// Messages to the hidden window.
const (
	wmTrayIcon     = 0x8000 + 1 // WM_APP+1: the icon's mouse events (lParam)
	wmTrayShow     = 0x8000 + 2 // show the launcher window
	wmTrayHide     = 0x8000 + 3 // hide it
	wmTrayGameExit = 0x8000 + 4 // the game process has ended
)

// Tray menu commands.
const (
	trayCmdOpen = 1 + iota
	trayCmdPlay
	trayCmdLogs
	trayCmdQuit
)

const (
	wmNull        = 0x0000
	wmLButtonUp   = 0x0202
	wmLButtonDbl  = 0x0203
	wmRButtonUp   = 0x0205
	wmContextMenu = 0x007B

	nimAdd    = 0
	nimDelete = 2
	nifMsg    = 0x1
	nifIcon   = 0x2
	nifTip    = 0x4

	imageIcon      = 1
	lrDefaultColor = 0
	lrShared       = 0x8000
	smCxSmIcon     = 49
	smCySmIcon     = 50
	idiApplication = 32512

	mfString      = 0x0
	mfGrayed      = 0x1
	mfSeparator   = 0x800
	tpmRightBtn   = 0x2
	tpmNoNotify   = 0x80
	tpmReturnCmd  = 0x100
	tpmBottomAlgn = 0x20
)

var (
	shell32                    = windows.NewLazySystemDLL("shell32.dll")
	procShellNotifyIconW       = shell32.NewProc("Shell_NotifyIconW")
	procRegisterClassExW       = user32.NewProc("RegisterClassExW")
	procCreateWindowExW        = user32.NewProc("CreateWindowExW")
	procDestroyWindow          = user32.NewProc("DestroyWindow")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procFindWindowW            = user32.NewProc("FindWindowW")
	procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	procLoadImageW             = user32.NewProc("LoadImageW")
	procLoadIconW              = user32.NewProc("LoadIconW")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procAppendMenuW            = user32.NewProc("AppendMenuW")
	procTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procIsIconic               = user32.NewProc("IsIconic")
	procGetModuleHandleW       = kernel32.NewProc("GetModuleHandleW")

	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

// notifyIconData is NOTIFYICONDATAW (shellapi.h).
type notifyIconData struct {
	Size            uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        windows.GUID
	BalloonIcon     uintptr
}

type winPoint struct{ X, Y int32 }

// trayHandlers are what the hidden window calls, all on the UI thread.
type trayHandlers struct {
	show     func()        // show the launcher window
	hide     func()        // hide it
	gameExit func()        // the game process has ended
	command  func(cmd int) // a tray menu item (trayCmd*)
	// playEnabled says whether the menu's Play item can be used now.
	playEnabled func() bool
}

// tray is the hidden window and its icon.
type tray struct {
	hwnd           uintptr
	icon           uintptr
	added          bool
	taskbarCreated uint32
	h              trayHandlers
}

// One tray per process: the window procedure finds it here.
var (
	trayMu      sync.Mutex
	theTray     *tray
	trayWndProc = windows.NewCallback(trayProc)

	trayClassOnce sync.Once
	trayClassErr  error
)

// newTray creates the hidden window and adds the icon. Call it on the UI
// thread. The icon is optional (a desktop without a notification area, a
// shell that is not running yet): the window alone still does its other
// jobs, and "TaskbarCreated" adds the icon when a shell appears.
func newTray(tip string, h trayHandlers) (*tray, error) {
	instance, _, _ := procGetModuleHandleW.Call(0)
	class, _ := windows.UTF16PtrFromString(trayClassName)
	// trayMu is never held across a Win32 call: CreateWindowEx and
	// DestroyWindow call the window procedure, which takes it.
	trayClassOnce.Do(func() {
		wc := wndClassEx{WndProc: trayWndProc, Instance: instance, ClassName: class}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
			trayClassErr = err
		}
	})
	if trayClassErr != nil {
		return nil, trayClassErr
	}
	// Never shown: no WS_VISIBLE, no size.
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(class)),
		0, 0, 0, 0, 0, 0, 0, instance, 0)
	if hwnd == 0 {
		return nil, err
	}
	t := &tray{hwnd: hwnd, h: h}
	name, _ := windows.UTF16PtrFromString("TaskbarCreated")
	if msg, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(name))); msg != 0 {
		t.taskbarCreated = uint32(msg)
	}
	// The exe's own icon (group icon 1, winres.json) at the small-icon size.
	cx, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	cy, _, _ := procGetSystemMetrics.Call(smCySmIcon)
	t.icon, _, _ = procLoadImageW.Call(instance, 1, imageIcon, cx, cy, lrDefaultColor|lrShared)
	if t.icon == 0 {
		t.icon, _, _ = procLoadIconW.Call(0, idiApplication)
	}
	trayMu.Lock()
	theTray = t
	trayMu.Unlock()
	t.addIcon(tip)
	return t, nil
}

func (t *tray) iconData() notifyIconData {
	d := notifyIconData{Wnd: t.hwnd, ID: 1}
	d.Size = uint32(unsafe.Sizeof(d))
	return d
}

func (t *tray) addIcon(tip string) {
	d := t.iconData()
	d.Flags = nifMsg | nifIcon | nifTip
	d.CallbackMessage = wmTrayIcon
	d.Icon = t.icon
	copy(d.Tip[:len(d.Tip)-1], windows.StringToUTF16(tip))
	ok, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&d)))
	t.added = ok != 0
}

// shown reports whether the icon is in the notification area: only then may
// the window hide there (it could not be brought back otherwise).
func (t *tray) shown() bool { return t != nil && t.added }

// close removes the icon and the window. UI thread.
func (t *tray) close() {
	if t == nil {
		return
	}
	if t.added {
		d := t.iconData()
		_, _, _ = procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&d)))
		t.added = false
	}
	trayMu.Lock()
	if theTray == t {
		theTray = nil
	}
	trayMu.Unlock()
	_, _, _ = procDestroyWindow.Call(t.hwnd)
}

// post sends one of the wmTray* messages to the hidden window; safe from any
// thread.
func (t *tray) post(msg uint32) {
	if t != nil {
		_, _, _ = procPostMessageW.Call(t.hwnd, uintptr(msg), 0, 0)
	}
}

func trayProc(hwnd, msg, wp, lp uintptr) uintptr {
	trayMu.Lock()
	t := theTray
	trayMu.Unlock()
	if t == nil || t.hwnd != hwnd {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}
	switch uint32(msg) {
	case wmTrayShow:
		call(t.h.show)
		return 0
	case wmTrayHide:
		call(t.h.hide)
		return 0
	case wmTrayGameExit:
		call(t.h.gameExit)
		return 0
	case wmTrayIcon:
		switch uint32(lp) {
		case wmLButtonUp, wmLButtonDbl:
			call(t.h.show)
		case wmRButtonUp, wmContextMenu:
			t.menu()
		}
		return 0
	}
	if t.taskbarCreated != 0 && uint32(msg) == t.taskbarCreated {
		// Explorer (re)started: its notification area is empty again.
		t.addIcon("Dreadnought Revival")
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func call(f func()) {
	if f != nil {
		f()
	}
}

// menu shows the icon's right-click menu at the pointer and runs the choice.
func (t *tray) menu() {
	m, _, _ := procCreatePopupMenu.Call()
	if m == 0 {
		return
	}
	defer func() { _, _, _ = procDestroyMenu.Call(m) }()
	add := func(id int, text string, grayed bool) {
		p, _ := windows.UTF16PtrFromString(text)
		flags := uintptr(mfString)
		if grayed {
			flags |= mfGrayed
		}
		_, _, _ = procAppendMenuW.Call(m, flags, uintptr(id), uintptr(unsafe.Pointer(p)))
	}
	add(trayCmdOpen, "Open launcher", false)
	add(trayCmdPlay, "Play", t.h.playEnabled != nil && !t.h.playEnabled())
	add(trayCmdLogs, "Open logs", false)
	_, _, _ = procAppendMenuW.Call(m, mfSeparator, 0, 0)
	add(trayCmdQuit, "Quit", false)

	var pt winPoint
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Documented for tray menus: without the foreground the menu does not
	// close on a click elsewhere, and the WM_NULL lets it close at once.
	_, _, _ = procSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(m, tpmRightBtn|tpmNoNotify|tpmReturnCmd|tpmBottomAlgn,
		uintptr(pt.X), uintptr(pt.Y), 0, t.hwnd, 0)
	_, _, _ = procPostMessageW.Call(t.hwnd, wmNull, 0, 0)
	if cmd != 0 && t.h.command != nil {
		t.h.command(int(cmd))
	}
}

// showRunningLauncher asks a launcher that is already running (its window may
// be hidden while the game runs) to show itself. false = none is running.
func showRunningLauncher() bool {
	class, _ := windows.UTF16PtrFromString(trayClassName)
	hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(class)), 0)
	if hwnd == 0 {
		return false
	}
	// Windows only lets the foreground process hand the foreground on.
	var pid uint32
	_, _, _ = procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid != 0 {
		_, _, _ = procAllowSetForegroundWindow.Call(uintptr(pid))
	}
	_, _, _ = procPostMessageW.Call(hwnd, wmTrayShow, 0, 0)
	return true
}
