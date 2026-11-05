//go:build !linux && !darwin && !windows

package sysstat

import (
	"syscall"

	"algod.top/internal/algod"
)

// Methods names how each stat is measured. Other Unix systems get disk
// free only.
func Methods() map[string]string {
	return map[string]string{StatDiskFree: "statfs", StatRestartBy: "pid"}
}

type platformState struct{}

func (p *platformState) init()                         {}
func (p *platformState) close()                        {}
func (p *platformState) procs(pids []int) map[int]Proc { return map[int]Proc{} }
func (p *platformState) netBytes([]int) map[int]NetIO  { return map[int]NetIO{} }

func interfaces() (float64, float64, error)      { return 0, 0, syscall.ENOTSUP }
func loadAvg() (float64, float64, float64, bool) { return 0, 0, 0, false }
func listAlgod() []algod.Process                 { return nil }
func alive(pid int) bool                         { return syscall.Kill(pid, 0) == nil }
