package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// Minimal terminal handling for install's root-password prompt
// (PLAN.md §3.15, M8 D5): hidden input with no new dependencies
// (single-static-binary discipline, M6 D1) and no live-env `stty`.
// Linux-only syscalls, like updatecore's untagged flock use —
// nodectl builds GOOS=linux only (see Makefile build-nodectl).

// Linux termios ioctl numbers (asm-generic, same on amd64/arm64).
const (
	termiosGet = 0x5401
	termiosSet = 0x5402
)

// termiosLFlagOff is the byte offset of c_lflag in the kernel
// struct termios (iflag/oflag/cflag first); both build arches are
// little-endian, ECHO is bit 0x8 of that word.
const (
	termiosLFlagOff = 12
	termiosEchoBit  = uint32(0x8)
)

func ioctlTermios(fd int, req uint, buf *[64]byte) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return errno
	}
	return nil
}

// isTerminalStdin reports whether stdin is a terminal. A TCGETS
// probe, not a mode check — /dev/null is also a char device but
// fails the probe.
func isTerminalStdin() bool {
	var buf [64]byte
	return ioctlTermios(int(os.Stdin.Fd()), termiosGet, &buf) == nil
}

// readPasswordLine prints prompt to stderr and reads one line from
// /dev/tty with echo disabled, restoring the terminal mode
// afterwards (best-effort restore on every path).
func readPasswordLine(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer tty.Close()
	fd := int(tty.Fd())
	var old [64]byte
	if err := ioctlTermios(fd, termiosGet, &old); err != nil {
		return "", err
	}
	mod := old
	lflag := binary.LittleEndian.Uint32(mod[termiosLFlagOff:])
	lflag &^= termiosEchoBit
	binary.LittleEndian.PutUint32(mod[termiosLFlagOff:], lflag)
	if err := ioctlTermios(fd, termiosSet, &mod); err != nil {
		return "", err
	}
	defer ioctlTermios(fd, termiosSet, &old) //nolint:errcheck
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(tty).ReadString('\n')
	fmt.Fprint(os.Stderr, "\n")
	if err != nil && len(line) == 0 {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func trimRightNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
