package runner

import (
	"log"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
}

// killTree stops a job and everything it started. A shell command Claude launched would otherwise
// keep running after the job was cancelled.
func killTree(pid int) {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	hideWindow(cmd)
	_ = cmd.Run()
}

// procGroup ties Claude's processes to this program with a job object: when the runner stops for any
// reason, including mcpctl stop claude, Windows ends every job it started.
type procGroup struct{ job windows.Handle }

func newProcGroup() *procGroup {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log.Printf("no job object, so jobs won't stop with the runner: %v", err)
		return &procGroup{}
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		log.Printf("no job object limit: %v", err)
		_ = windows.CloseHandle(job)
		return &procGroup{}
	}
	return &procGroup{job: job}
}

func (p *procGroup) add(proc *os.Process) {
	if p.job == 0 {
		return
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(proc.Pid))
	if err != nil {
		log.Printf("can't tie job to the runner: %v", err)
		return
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(p.job, handle); err != nil {
		log.Printf("can't tie job to the runner: %v", err)
	}
}
