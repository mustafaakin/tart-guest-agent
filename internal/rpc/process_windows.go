package rpc

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// Used when a TTY is requested without a terminal size, since ConPTY rejects a zero size.
	defaultTerminalRows = 24
	defaultTerminalCols = 80

	maxTerminalDimension = math.MaxInt16
)

var (
	modkernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	procUpdateProcThreadAttribute = modkernel32.NewProc("UpdateProcThreadAttribute")
)

type processOptions struct {
	interactive bool
	tty         bool
	rows, cols  uint32
}

// process is a command started by Exec: os/exec can neither attach
// a pseudo console nor start a process inside a job object.
type process struct {
	// Standard streams on the agent's side. With a TTY, stdin and stdout are the pseudo console's
	// input and output, and stderr is nil. Without one, stdin is nil unless interactive.
	stdin  *os.File
	stdout *os.File
	stderr *os.File

	mu            sync.Mutex
	handle        windows.Handle
	job           windows.Handle
	pseudoConsole windows.Handle
}

// startProcess starts the command inside a new job object so that
// terminating the job ends the process tree the way signaling a process group does on Unix.
func startProcess(cmd *exec.Cmd, options processOptions) (_ *process, err error) {
	process := &process{}
	defer func() {
		if err != nil {
			process.close()
		}
	}()

	var childFiles []*os.File
	defer func() {
		for _, file := range childFiles {
			_ = file.Close()
		}
	}()

	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attributes.Delete()

	startupInfo := &windows.StartupInfoEx{ProcThreadAttributeList: attributes.List()}
	startupInfo.Cb = uint32(unsafe.Sizeof(*startupInfo))
	startupInfo.Flags = windows.STARTF_USESTDHANDLES

	creationFlags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	inheritHandles := false

	if options.tty {
		if err := process.createPseudoConsole(options.rows, options.cols); err != nil {
			return nil, err
		}

		if err := attachPseudoConsole(attributes, process.pseudoConsole); err != nil {
			return nil, err
		}

		// STARTF_USESTDHANDLES without handles makes the child use the pseudo console
		// instead of inheriting the agent's own standard handles
	} else {
		childFiles, err = process.createPipes(options.interactive)
		if err != nil {
			return nil, err
		}

		startupInfo.StdInput = windows.Handle(childFiles[0].Fd())
		startupInfo.StdOutput = windows.Handle(childFiles[1].Fd())
		startupInfo.StdErr = windows.Handle(childFiles[2].Fd())

		// Inherit only these handles, not ones meant for commands started concurrently
		inheritedHandles := []windows.Handle{startupInfo.StdInput, startupInfo.StdOutput, startupInfo.StdErr}
		if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&inheritedHandles[0]),
			uintptr(len(inheritedHandles))*unsafe.Sizeof(inheritedHandles[0])); err != nil {
			return nil, err
		}
		inheritHandles = true

		// Give console programs a console without a window, even if the agent has none
		creationFlags |= windows.CREATE_NO_WINDOW
	}

	process.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}

	// Start suspended so that the process can't start children outside the job
	processInformation, err := createProcess(cmd, inheritHandles, creationFlags|windows.CREATE_SUSPENDED,
		&startupInfo.StartupInfo)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(processInformation.Thread)
	process.handle = processInformation.Process

	if err := windows.AssignProcessToJobObject(process.job, process.handle); err != nil {
		_ = windows.TerminateProcess(process.handle, execRuntimeFailureExitCode)

		return nil, fmt.Errorf("failed to assign the process to a job object: %w", err)
	}

	if _, err := windows.ResumeThread(processInformation.Thread); err != nil {
		_ = windows.TerminateJobObject(process.job, execRuntimeFailureExitCode)

		return nil, err
	}

	return process, nil
}

// startDetachedProcess starts the command with no standard streams, its own console and, where
// allowed, outside the agent's job, the way the Unix implementation starts a new session.
func startDetachedProcess(cmd *exec.Cmd) error {
	nul, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer nul.Close()

	if err := inheritable(nul); err != nil {
		return err
	}

	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return err
	}
	defer attributes.Delete()

	inheritedHandle := windows.Handle(nul.Fd())
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&inheritedHandle),
		unsafe.Sizeof(inheritedHandle)); err != nil {
		return err
	}

	startupInfo := &windows.StartupInfoEx{ProcThreadAttributeList: attributes.List()}
	startupInfo.Cb = uint32(unsafe.Sizeof(*startupInfo))
	startupInfo.Flags = windows.STARTF_USESTDHANDLES
	startupInfo.StdInput = inheritedHandle
	startupInfo.StdOutput = inheritedHandle
	startupInfo.StdErr = inheritedHandle

	creationFlags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT |
		windows.CREATE_NO_WINDOW)

	// Leave the agent's job, if any, so that the process survives the agent,
	// but a job that doesn't allow that rejects the flag
	processInformation, err := createProcess(cmd, true, creationFlags|windows.CREATE_BREAKAWAY_FROM_JOB,
		&startupInfo.StartupInfo)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		processInformation, err = createProcess(cmd, true, creationFlags, &startupInfo.StartupInfo)
	}
	if err != nil {
		return err
	}

	_ = windows.CloseHandle(processInformation.Thread)
	_ = windows.CloseHandle(processInformation.Process)

	return nil
}

func createProcess(
	cmd *exec.Cmd,
	inheritHandles bool,
	creationFlags uint32,
	startupInfo *windows.StartupInfo,
) (*windows.ProcessInformation, error) {
	// Resolving the executable failed, which cmd.Start would report
	if cmd.Err != nil {
		return nil, cmd.Err
	}

	path, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		return nil, err
	}

	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(cmd.Args))
	if err != nil {
		return nil, err
	}

	var dir *uint16
	if cmd.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(cmd.Dir); err != nil {
			return nil, err
		}
	}

	environment, err := environmentBlock(cmd.Environ())
	if err != nil {
		return nil, err
	}

	var processInformation windows.ProcessInformation

	if err := windows.CreateProcess(path, commandLine, nil, nil, inheritHandles, creationFlags,
		environment, dir, startupInfo, &processInformation); err != nil {
		return nil, &os.PathError{Op: "CreateProcess", Path: cmd.Path, Err: err}
	}

	return &processInformation, nil
}

func (process *process) createPseudoConsole(rows, cols uint32) error {
	// The console duplicates its ends of the pipes, so close them once it exists.
	// Keep its input open even without an interactive client, since the console
	// exits when its input closes.
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer inputRead.Close()
	process.stdin = inputWrite

	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer outputWrite.Close()
	process.stdout = outputRead

	return windows.CreatePseudoConsole(terminalSize(rows, cols), windows.Handle(inputRead.Fd()),
		windows.Handle(outputWrite.Fd()), 0, &process.pseudoConsole)
}

// createPipes returns the child's ends of its standard input, output and error,
// which the caller closes even on failure.
func (process *process) createPipes(interactive bool) ([]*os.File, error) {
	var stdin, stdout, stderr *os.File
	var err error

	if interactive {
		stdin, process.stdin, err = os.Pipe()
	} else {
		stdin, err = os.Open(os.DevNull)
	}
	if err != nil {
		return nil, err
	}

	process.stdout, stdout, err = os.Pipe()
	if err != nil {
		return []*os.File{stdin}, err
	}

	process.stderr, stderr, err = os.Pipe()
	if err != nil {
		return []*os.File{stdin, stdout}, err
	}

	childFiles := []*os.File{stdin, stdout, stderr}

	for _, file := range childFiles {
		if err := inheritable(file); err != nil {
			return childFiles, err
		}
	}

	return childFiles, nil
}

// wait waits for the process, but not its children, to exit and returns its exit code.
func (process *process) wait() (uint32, error) {
	if _, err := windows.WaitForSingleObject(process.handle, windows.INFINITE); err != nil {
		return 0, err
	}

	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.handle, &exitCode); err != nil {
		return 0, err
	}

	return exitCode, nil
}

// terminate ends the process and its children, which then exit with the given code.
func (process *process) terminate(exitCode uint32) error {
	process.mu.Lock()
	defer process.mu.Unlock()

	if process.job == 0 {
		return os.ErrProcessDone
	}

	return windows.TerminateJobObject(process.job, exitCode)
}

func (process *process) resize(rows, cols uint32) error {
	process.mu.Lock()
	defer process.mu.Unlock()

	if process.pseudoConsole == 0 {
		return nil
	}

	return windows.ResizePseudoConsole(process.pseudoConsole, terminalSize(rows, cols))
}

// closePseudoConsole ends the pseudo console once its process exits, which ends its output.
// Its remaining output must be drained concurrently, as older Windows versions wait for that.
func (process *process) closePseudoConsole() {
	process.mu.Lock()
	pseudoConsole := process.pseudoConsole
	process.pseudoConsole = 0
	process.mu.Unlock()

	if pseudoConsole != 0 {
		windows.ClosePseudoConsole(pseudoConsole)
	}
}

func (process *process) closeOutput() {
	for _, file := range []*os.File{process.stdout, process.stderr} {
		if file != nil {
			_ = file.Close()
		}
	}
}

// close releases the process without terminating it, so its background children keep running.
func (process *process) close() {
	// Close the pseudo console's output first so that closing it doesn't wait for a reader
	for _, file := range []*os.File{process.stdin, process.stdout, process.stderr} {
		if file != nil {
			_ = file.Close()
		}
	}

	process.closePseudoConsole()

	process.mu.Lock()
	defer process.mu.Unlock()

	for _, handle := range []*windows.Handle{&process.handle, &process.job} {
		if *handle != 0 {
			_ = windows.CloseHandle(*handle)
			*handle = 0
		}
	}
}

// attachPseudoConsole passes the pseudo console by value, which ProcThreadAttributeListContainer.Update can't do.
func attachPseudoConsole(attributes *windows.ProcThreadAttributeListContainer, pseudoConsole windows.Handle) error {
	if ret, _, err := procUpdateProcThreadAttribute.Call(uintptr(unsafe.Pointer(attributes.List())), 0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(pseudoConsole), unsafe.Sizeof(pseudoConsole),
		0, 0); ret == 0 {
		return fmt.Errorf("failed to attach the pseudo console: %w", err)
	}

	return nil
}

func inheritable(file *os.File) error {
	return windows.SetHandleInformation(windows.Handle(file.Fd()), windows.HANDLE_FLAG_INHERIT,
		windows.HANDLE_FLAG_INHERIT)
}

func terminalSize(rows, cols uint32) windows.Coord {
	if rows == 0 || cols == 0 {
		rows, cols = defaultTerminalRows, defaultTerminalCols
	}

	return windows.Coord{X: int16(min(cols, maxTerminalDimension)), Y: int16(min(rows, maxTerminalDimension))}
}

// environmentBlock encodes the environment for CreateProcess.
func environmentBlock(env []string) (*uint16, error) {
	var block []uint16

	for _, variable := range env {
		encoded, err := windows.UTF16FromString(variable)
		if err != nil {
			return nil, fmt.Errorf("invalid environment variable %q: %w", variable, err)
		}

		block = append(block, encoded...)
	}

	// An empty block still needs its own terminator
	if len(block) == 0 {
		block = append(block, 0)
	}

	block = append(block, 0)

	return &block[0], nil
}
