package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file reads the process table.
//
// It exists because the runtime has to answer one question - is the agent still
// running - and the only honest way to answer it is to look. The alternatives
// are both worse. Reading the terminal's text and looking for a prompt is a
// guess that changes with every release of the program being watched, and it is
// exactly the guess Phase 3 forbids. Asking the shell is impossible: a process
// cannot report the exit status of a child that another process reaped.
//
// So: the kernel's own answer, from /proc. A process AgentMux started inside a
// pane is a descendant of that pane's leader, and its executable is the file
// AgentMux resolved before typing the command. Both of those are facts, not
// inferences.
//
// Nothing here reads a process's environment, its file descriptors, or its
// memory. Only the process's identity, its parent, its start time, its
// executable, and its working directory - all of which AgentMux owns, because
// these are processes AgentMux started.

// clockTicks is the kernel's USER_HZ, the unit of the start-time field in
// /proc/<pid>/stat.
//
// It is 100 on every Linux architecture AgentMux runs on, and it is not
// configurable at build time in a way that reaches user space: the value the
// kernel exposes through sysconf(_SC_CLK_TCK) is fixed at 100 regardless of
// CONFIG_HZ. Reading it would mean cgo, and being wrong about it would move a
// reported start time by a few milliseconds.
const clockTicks = 100

// processRef is what AgentMux knows about one running process.
type processRef struct {
	PID int

	// Executable is the file the process is running, as the kernel reports it.
	// Symlinks are already resolved by the kernel, so this is the same string
	// that resolving the configured binary produced.
	Executable string

	// Dir is the process's working directory.
	Dir string

	// Started is when the process started, derived from the kernel's own
	// uptime and start-time fields rather than from when AgentMux noticed it.
	// That matters after a server restart: a runtime adopted from a previous
	// process reports the agent's real start time, not the time it was
	// rediscovered.
	Started time.Time
}

// processTreeScan reads the process table once.
//
// The scan is a full pass over /proc rather than a walk of each process's
// children file. It costs one stat per process on a machine with a few hundred
// of them, and in exchange it does not depend on CONFIG_PROC_CHILDREN being
// enabled or on the children file being readable - which is the kind of
// dependency that turns a working feature into an empty answer on somebody
// else's kernel.
type processTreeScan struct {
	ppid  map[int]int
	start map[int]time.Time
}

// scanProcessTree reads the parent and start time of every process.
//
// A process that exits while the scan is running is skipped: it is gone, and
// gone is not an error.
func scanProcessTree() (*processTreeScan, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	boot, err := bootTime()
	if err != nil {
		return nil, err
	}

	scan := &processTreeScan{
		ppid:  make(map[int]int, len(entries)),
		start: make(map[int]time.Time, len(entries)),
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		ppid, startTicks, err := readStatFields(pid)
		if err != nil {
			continue
		}
		scan.ppid[pid] = ppid
		scan.start[pid] = boot.Add(time.Duration(startTicks) * time.Second / clockTicks)
	}
	return scan, nil
}

// bootTime reports when the machine started.
//
// /proc/uptime is the kernel's own count of how long it has been up, and the
// difference between it and a process's start ticks is that process's age. A
// wall clock read at the same moment has already applied the answer.
//
// The value is computed once and kept. It is a fact about the machine that
// cannot change, and recomputing it per scan would make a reported start time
// wander: /proc/uptime has two decimal places, so it moves in steps of ten
// milliseconds while the clock does not, and two readings taken a millisecond
// apart can derive boot times ten milliseconds apart. A client that read an
// agent's start time twice and got two answers would be right to distrust both.
func bootTime() (time.Time, error) {
	bootOnce.Do(func() {
		bootValue, bootErr = readBootTime()
	})
	return bootValue, bootErr
}

// bootOnce guards bootValue, which is derived once per process.
var (
	bootOnce  sync.Once
	bootValue time.Time
	bootErr   error
)

// readBootTime derives the machine's boot time from the kernel's uptime.
func readBootTime() (time.Time, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return time.Time{}, os.ErrInvalid
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Now().Add(-time.Duration(seconds * float64(time.Second))), nil
}

// readStatFields returns a process's parent and its start time in clock ticks.
//
// The second field of /proc/<pid>/stat is the executable name in parentheses,
// and it may contain spaces and parentheses of its own - a program can rename
// itself to anything. Splitting on whitespace would therefore misread every
// field after it, so the name is skipped by finding the last ')' and counting
// from there.
func readStatFields(pid int) (ppid int, startTicks int64, err error) {
	path := "/proc/" + strconv.Itoa(pid) + "/stat"
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	line := string(data)

	open := strings.IndexByte(line, '(')
	close := strings.LastIndexByte(line, ')')
	if open < 0 || close < open || close+2 > len(line) {
		return 0, 0, os.ErrInvalid
	}

	// Fields after the name, starting at field 3 (state).
	fields := strings.Fields(line[close+1:])
	// ppid is field 4, so index 1 here; starttime is field 22, so index 19.
	const (
		ppidIndex       = 4 - 3
		startTimeIndex  = 22 - 3
		fieldsPastState = startTimeIndex + 1
	)
	if len(fields) < fieldsPastState {
		return 0, 0, os.ErrInvalid
	}
	ppid, err = strconv.Atoi(fields[ppidIndex])
	if err != nil {
		return 0, 0, err
	}
	startTicks, err = strconv.ParseInt(fields[startTimeIndex], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return ppid, startTicks, nil
}

// descendants returns every process descended from root, root included.
//
// Including root is deliberate. A runtime AgentMux created runs a shell and the
// agent is its child, but a session adopted from a previous server, or created
// by hand, may have been started with the agent as its leader. Excluding the
// leader would report that agent as absent while it was running.
func (s *processTreeScan) descendants(root int) []int {
	if root <= 0 {
		return nil
	}
	children := make(map[int][]int, len(s.ppid))
	for pid, ppid := range s.ppid {
		children[ppid] = append(children[ppid], pid)
	}

	// The visited set is not paranoia. It bounds the walk if the table is read
	// while a process is being reparented, and it makes the loop's termination
	// a property of the code rather than of the kernel.
	visited := map[int]bool{root: true}
	out := []int{root}
	for queue := []int{root}; len(queue) > 0; {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if visited[child] {
				continue
			}
			visited[child] = true
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}

// describe fills in the executable and working directory of one process.
func (s *processTreeScan) describe(pid int) (processRef, bool) {
	started, ok := s.start[pid]
	if !ok {
		return processRef{}, false
	}
	ref := processRef{PID: pid, Started: started}

	// A process that exited between the scan and this read fails here. It is
	// reported as absent rather than as an error, which is what it is.
	exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return processRef{}, false
	}
	ref.Executable = cleanExecutablePath(exe)

	if dir, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd"); err == nil {
		ref.Dir = cleanExecutablePath(dir)
	}
	return ref, true
}

// cleanExecutablePath normalises a path the kernel reported for a process.
//
// The kernel appends " (deleted)" to the target of /proc/<pid>/exe when the
// file it was started from no longer exists. Claude Code updates itself in
// place, replacing the versioned directory it is running from, so a session
// that has been up across an update would otherwise stop being recognisable -
// which would report a running agent as stopped.
func cleanExecutablePath(path string) string {
	return filepath.Clean(strings.TrimSuffix(path, " (deleted)"))
}

// findProcessRunning returns the process in a pane's tree that is running the
// given executable, and whether there is one.
//
// It reads the table itself. Callers that have several questions to ask of one
// reading use findInScan instead.
func findProcessRunning(root int, executable string) (processRef, bool, error) {
	scan, err := scanProcessTree()
	if err != nil {
		return processRef{}, false, err
	}
	return findInScan(scan, root, executable)
}

// findInScan is findProcessRunning against an already-read table.
func findInScan(scan *processTreeScan, root int, executable string) (processRef, bool, error) {
	if strings.TrimSpace(executable) == "" {
		return processRef{}, false, os.ErrInvalid
	}
	target := cleanExecutablePath(executable)

	// The first match in the walk wins, and the walk is breadth-first, so the
	// shallowest match wins: the agent itself rather than a helper it spawned
	// later from a copy of its own executable.
	for _, pid := range scan.descendants(root) {
		ref, ok := scan.describe(pid)
		if !ok {
			continue
		}
		if ref.Executable == target {
			return ref, true, nil
		}
	}
	return processRef{}, false, nil
}
