// Package process holds the helpers that kinds share for the device processes
// they launch: each runs in its own process group and is recognized by its
// command line.
package process

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Running reports whether pid runs a command line that contains every one of
// the substrings, so a reused process ID does not count. A process that is
// replacing its program, as the Android emulator does, briefly shows no
// command line, so a mismatch is checked once more.
func Running(pid int, substrings ...string) bool {
	if pid <= 0 {
		return false
	}
	for attempt := 0; ; attempt++ {
		out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			// ps fails when no process has the ID.
			return false
		}
		if containsAll(" "+strings.TrimSpace(string(out))+" ", substrings) {
			return true
		}
		if attempt == 1 {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// GroupRunning reports whether the process group pgid has a process whose
// command line matches the extended regular expression pattern. As in Running,
// a miss is checked once more unless the group is gone.
func GroupRunning(pgid int, pattern string) bool {
	if pgid <= 0 {
		return false
	}
	for attempt := 0; ; attempt++ {
		if exec.Command("pgrep", "-g", strconv.Itoa(pgid), "-f", pattern).Run() == nil {
			return true
		}
		if attempt == 1 || WaitGone(pgid, 0) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func containsAll(s string, substrings []string) bool {
	for _, sub := range substrings {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// StopGroup sends SIGTERM to the process group pgid, then SIGKILL if it
// outlives timeout, and waits until the group is gone.
func StopGroup(pgid int, timeout time.Duration) error {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		if WaitGone(pgid, timeout) {
			return nil
		}
	}
	return fmt.Errorf("process group %d survived SIGKILL", pgid)
}

// WaitGone waits up to timeout for the process group pgid to end.
func WaitGone(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Bindable reports whether a TCP port on 127.0.0.1 is free.
func Bindable(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// LogTail returns the last lines of a log, introduced for an error message,
// or nothing when the log cannot be read.
func LogTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lines = lines[max(0, len(lines)-20):]
	return fmt.Sprintf("; the end of %s:\n%s", path, strings.Join(lines, "\n"))
}
