//go:build windows

package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// startGameProcess starts the game WITHOUT handing it the launcher's standard
// handles.
//
// os/exec cannot do that: Go's StartProcess always sets STARTF_USESTDHANDLES
// and passes the parent's stdin/stdout/stderr (syscall/exec_windows.go). Since
// v1.0.7 the launcher is a GUI program (-H windowsgui) and has no standard
// handles, so the game was started with NULL ones -- and the game's own log
// window ("Show the game's log window", -LOG) stayed black: the engine opens a
// console with AllocConsole and then writes to GetStdHandle(STD_OUTPUT_HANDLE),
// which still held the inherited NULL instead of the new console (GUESS from
// the symptom; the engine side was not traced). A process started without
// STARTF_USESTDHANDLES gets the console's handles from AllocConsole, as when
// the game is started from Explorer.
//
// Nothing of the game's output is lost by this: it writes no stdout unless
// started with -stdout, which the launcher never passes. The launcher keeps
// waiting on the process as before (os.FindProcess; the CreateProcess handle
// stays open until then, so the pid cannot be reused in between).
func startGameProcess(path string, args []string, dir string) (*os.Process, error) {
	app, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	cmdLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{path}, args...)))
	if err != nil {
		return nil, err
	}
	cwd, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	si := &windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(app, cmdLine, nil, nil, false, 0, nil, cwd, si, &pi); err != nil {
		return nil, err
	}
	defer func() {
		_ = windows.CloseHandle(pi.Thread)
		_ = windows.CloseHandle(pi.Process)
	}()
	return os.FindProcess(int(pi.ProcessId))
}
