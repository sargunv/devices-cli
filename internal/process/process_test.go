package process

import (
	"net"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestBindable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if port := l.Addr().(*net.TCPAddr).Port; Bindable(port) {
		t.Errorf("port %d is bound but bindable", port)
	}
}

func TestRunningAndStopGroup(t *testing.T) {
	// The shell's child sleep shares its process group.
	cmd := exec.Command("sh", "-c", "sleep 600 & wait", "marker", "-port", "4242")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid

	if !Running(pid, "sleep 600", "-port 4242") {
		t.Error("Running does not match the process's command line")
	}
	if Running(pid, "-port 4243") {
		t.Error("Running matches another port")
	}
	if err := StopGroup(pid, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if Running(pid) || !WaitGone(pid, 0) {
		t.Errorf("process group %s survived StopGroup", strconv.Itoa(pid))
	}
}
