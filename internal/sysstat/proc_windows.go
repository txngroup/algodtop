//go:build windows

package sysstat

import (
	"strings"
	"syscall"
	"unsafe"

	"algod.top/internal/algod"
)

// Every call is a plain Win32 API in kernel32, psapi or iphlpapi through
// the stdlib syscall package. Per-connection byte counts need
// Administrator rights to switch on, so process network bytes use
// interface totals, labelled as such.

// Methods names how each stat is measured.
func Methods() map[string]string {
	return map[string]string{
		StatCPU:       "GetProcessTimes",
		StatRSS:       "GetProcessMemoryInfo (working set)",
		StatThreads:   "Toolhelp32 snapshot",
		StatHandles:   "GetProcessHandleCount",
		StatDiskIO:    "GetProcessIoCounters",
		StatProcNet:   "interface totals",
		StatIfBytes:   "GetIfTable2",
		StatDiskFree:  "GetDiskFreeSpaceExW",
		StatLoad:      "", // Windows has no load average
		StatRestartBy: "pid and process creation time",
	}
}

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	psapi                     = syscall.NewLazyDLL("psapi.dll")
	iphlpapi                  = syscall.NewLazyDLL("iphlpapi.dll")
	procGetProcessMemoryInfo  = psapi.NewProc("GetProcessMemoryInfo")
	procGetProcessHandleCount = kernel32.NewProc("GetProcessHandleCount")
	procGetProcessIoCounters  = kernel32.NewProc("GetProcessIoCounters")
	procGetIfTable2           = iphlpapi.NewProc("GetIfTable2")
	procFreeMibTable          = iphlpapi.NewProc("FreeMibTable")
)

const processQueryLimitedInformation = 0x1000

type platformState struct{}

func (p *platformState) init()  {}
func (p *platformState) close() {}

type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

func filetimeNS(ft syscall.Filetime) int64 { return ft.Nanoseconds() }

func (p *platformState) procs(pids []int) map[int]Proc {
	out := map[int]Proc{}
	threads := threadCounts()
	for _, pid := range pids {
		h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
		if err != nil {
			continue
		}
		pr := Proc{PID: pid, Threads: NA, Handles: NA, ReadBytes: NA, WriteBytes: NA, RSSBytes: NA}
		var c, e, k, u syscall.Filetime
		if syscall.GetProcessTimes(h, &c, &e, &k, &u) == nil {
			// Kernel and user times are 100 ns intervals, not epoch times.
			kt := uint64(k.HighDateTime)<<32 | uint64(k.LowDateTime)
			ut := uint64(u.HighDateTime)<<32 | uint64(u.LowDateTime)
			pr.CPUSeconds = float64(kt+ut) / 1e7
			pr.StartTime = filetimeNS(c)
		}
		var mc processMemoryCounters
		mc.Cb = uint32(unsafe.Sizeof(mc))
		if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.Cb)); r != 0 {
			pr.RSSBytes = float64(mc.WorkingSetSize)
		}
		var hc uint32
		if r, _, _ := procGetProcessHandleCount.Call(uintptr(h), uintptr(unsafe.Pointer(&hc))); r != 0 {
			pr.Handles = float64(hc)
		}
		var io ioCounters
		if r, _, _ := procGetProcessIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&io))); r != 0 {
			pr.ReadBytes = float64(io.ReadTransferCount)
			pr.WriteBytes = float64(io.WriteTransferCount)
		}
		if n, ok := threads[uint32(pid)]; ok {
			pr.Threads = float64(n)
		}
		syscall.CloseHandle(h)
		out[pid] = pr
	}
	return out
}

func snapshotProcs(fn func(e *syscall.ProcessEntry32)) {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer syscall.CloseHandle(snap)
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if syscall.Process32First(snap, &e) != nil {
		return
	}
	for {
		fn(&e)
		if syscall.Process32Next(snap, &e) != nil {
			return
		}
	}
}

func threadCounts() map[uint32]uint32 {
	out := map[uint32]uint32{}
	snapshotProcs(func(e *syscall.ProcessEntry32) { out[e.ProcessID] = e.Threads })
	return out
}

func (p *platformState) netBytes(pids []int) map[int]NetIO {
	out := map[int]NetIO{}
	in, o, err := interfaces()
	if err != nil {
		return out
	}
	for _, pid := range pids {
		out[pid] = NetIO{In: in, Out: o, Method: "interface totals"}
	}
	return out
}

// MIB_IF_ROW2 is 1352 bytes. InOctets is at offset 1208 and OutOctets at
// 1280. Type 24 is the software loopback.
const (
	ifRow2Size   = 1352
	ifRowType    = 1128
	ifRowInOct   = 1208
	ifRowOutOct  = 1280
	ifTypeLoop   = 24
	ifTableStart = 8
)

func interfaces() (float64, float64, error) {
	if err := procGetIfTable2.Find(); err != nil {
		return 0, 0, err
	}
	var table unsafe.Pointer
	if r, _, _ := procGetIfTable2.Call(uintptr(unsafe.Pointer(&table))); r != 0 {
		return 0, 0, syscall.Errno(r)
	}
	defer procFreeMibTable.Call(uintptr(table))
	n := *(*uint32)(table)
	var in, out float64
	for i := uint32(0); i < n; i++ {
		row := unsafe.Add(table, ifTableStart+int(i)*ifRow2Size)
		if *(*uint32)(unsafe.Add(row, ifRowType)) == ifTypeLoop {
			continue
		}
		in += float64(*(*uint64)(unsafe.Add(row, ifRowInOct)))
		out += float64(*(*uint64)(unsafe.Add(row, ifRowOutOct)))
	}
	return in, out, nil
}

func loadAvg() (float64, float64, float64, bool) { return 0, 0, 0, false }

func listAlgod() []algod.Process {
	var out []algod.Process
	snapshotProcs(func(e *syscall.ProcessEntry32) {
		name := syscall.UTF16ToString(e.ExeFile[:])
		if strings.EqualFold(name, "algod.exe") {
			// The command line of another process needs NtQuery calls;
			// discovery matches PIDs against algod.pid instead.
			out = append(out, algod.Process{PID: int(e.ProcessID), Args: []string{name}})
		}
	})
	return out
}

func alive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if syscall.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}
