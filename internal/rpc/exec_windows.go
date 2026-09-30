package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"strings"
	"sync"

	"github.com/cirruslabs/tart-guest-agent/pkg/v1"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Exit codes of processes ended by Signal or a canceled exec, the same as for Unix signals.
const (
	sigkillExitCode = signalExitCodeOffset + 9
	sigtermExitCode = signalExitCodeOffset + 15
)

// ttyEOF ends the input of a console program reading a line: Ctrl+Z, then Enter.
var ttyEOF = []byte{0x1A, '\r'}

// execProcess is what Signal needs to signal a running exec.
type execProcess = *process

//nolint:gocognit,gocyclo,maintidx // Exec coordinates process startup, bidirectional I/O, and cleanup.
func (rpc *RPC) Exec(stream grpc.BidiStreamingServer[v1.ExecRequest, v1.ExecResponse]) error {
	// Read the first exec request, it should describe a command to execute
	firstExecRequest, err := stream.Recv()
	if err != nil {
		return err
	}
	firstExecRequestCommand, ok := firstExecRequest.GetType().(*v1.ExecRequest_Command_)
	if !ok {
		return errors.New("first exec request should describe a command to execute")
	}
	command := firstExecRequestCommand.Command

	zap.S().Infof("executing %s", formatCommandAndArgs(command.GetName(), command.GetArgs()))

	if command.GetDetach() && (command.GetInteractive() || command.GetTty()) {
		return errors.New("detach cannot be used with interactive or tty")
	}

	if user := command.GetUser(); user != "" && !isAgentUser(user) {
		return status.Errorf(codes.Unimplemented, "running commands as another user (%q) is not supported "+
			"on Windows, where commands run as the user running the guest agent", user)
	}

	cmd := rpc.execCommand(context.Background(), command.GetName(), command.GetArgs())
	applyExecOverrides(cmd, command)

	if command.GetDetach() {
		if err := startDetachedProcess(cmd); err != nil {
			zap.S().Warnf("failed to start %s: %v", formatCommandAndArgs(command.GetName(), command.GetArgs()), err)

			return sendStartFailure(stream)
		}

		// Explicitly notify the client that the process was started,
		// but don't provide an exec ID since it's a detached process
		err = sendStartSuccess(stream, "")
		if err != nil {
			return err
		}

		if err := stream.Send(&v1.ExecResponse{
			Type: &v1.ExecResponse_Exit_{
				Exit: &v1.ExecResponse_Exit{
					Code: 0,
				},
			},
		}); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}

		return nil
	}

	process, err := startProcess(cmd, processOptions{
		interactive: command.GetInteractive(),
		tty:         command.GetTty(),
		rows:        command.GetTerminalSize().GetRows(),
		cols:        command.GetTerminalSize().GetCols(),
	})
	if err != nil {
		zap.S().Warnf("failed to start %s: %v", formatCommandAndArgs(command.GetName(), command.GetArgs()), err)

		return sendStartFailure(stream)
	}
	// Release the process without terminating it, so that background children survive
	defer process.close()

	// Kill the whole process tree when the exec stream is canceled before the command finishes
	finished := make(chan struct{})
	finish := sync.OnceFunc(func() { close(finished) })
	defer finish()

	go func() {
		select {
		case <-stream.Context().Done():
			_ = process.terminate(sigkillExitCode)
		case <-finished:
		}
	}()

	execID := uuid.NewString()
	rpc.execs.Store(execID, process)
	defer rpc.execs.Delete(execID)

	// Explicitly notify the client that the process was started
	err = sendStartSuccess(stream, execID)
	if err != nil {
		// Output readers have not started yet, so terminate and reap directly
		_ = process.terminate(sigkillExitCode)
		_, _ = process.wait()

		return err
	}

	var stdin io.WriteCloser
	if command.GetInteractive() {
		stdin = process.stdin
	}

	// Handle standard input and terminal resize events from the client
	fromClientErrCh := make(chan error, 1)
	reportClientError := func(err error) {
		fromClientErrCh <- err
		_ = process.terminate(sigkillExitCode)
	}

	go func() {
		var stdinClosed bool

		for {
			request, err := stream.Recv()
			if err != nil {
				// Allow the client to close its sending side while continuing to receive responses
				if errors.Is(err, io.EOF) {
					if err := closeStdin(stdin, command.GetTty(), &stdinClosed); err != nil {
						reportClientError(err)
					}

					return
				}

				if !errors.Is(err, context.Canceled) && status.Code(err) != codes.Canceled {
					reportClientError(err)
				}

				return
			}

			switch typedAction := request.GetType().(type) {
			case *v1.ExecRequest_StandardInput:
				if !command.GetInteractive() {
					// Ignore standard input from the client
					// as non-interactive command is running
					continue
				}

				// Check if the remote client has received EOF on their standard input
				if len(typedAction.StandardInput.GetData()) == 0 {
					if err := closeStdin(stdin, command.GetTty(), &stdinClosed); err != nil {
						reportClientError(err)

						return
					}

					continue
				}

				if _, err := stdin.Write(typedAction.StandardInput.GetData()); err != nil {
					reportClientError(err)

					return
				}
			case *v1.ExecRequest_TerminalResize:
				// Ignore terminal resize requests
				// when pseudo terminal is disabled
				if !command.GetTty() {
					continue
				}

				if err := process.resize(typedAction.TerminalResize.GetRows(),
					typedAction.TerminalResize.GetCols()); err != nil {
					reportClientError(err)

					return
				}
			}
		}
	}()

	// Serialize responses from the stdout and stderr goroutines
	var sendMutex sync.Mutex

	sendResponse := func(response *v1.ExecResponse) error {
		sendMutex.Lock()
		defer sendMutex.Unlock()

		return stream.Send(response)
	}

	group, _ := errgroup.WithContext(stream.Context())

	// Handle standard output from the command, which carries
	// the pseudo console output when TTY is requested
	group.Go(func() error {
		return forwardOutput(process.stdout, func(data []byte) error {
			return sendResponse(&v1.ExecResponse{
				Type: &v1.ExecResponse_StandardOutput{
					StandardOutput: &v1.IOChunk{Data: data},
				},
			})
		})
	})

	if process.stderr != nil {
		group.Go(func() error {
			return forwardOutput(process.stderr, func(data []byte) error {
				return sendResponse(&v1.ExecResponse{
					Type: &v1.ExecResponse_StandardError{
						StandardError: &v1.IOChunk{Data: data},
					},
				})
			})
		})
	}

	// Wait for the command to finish, then end the pseudo console,
	// which only then stops its output
	type waitResult struct {
		exitCode uint32
		err      error
	}
	waitResultCh := make(chan waitResult, 1)

	go func() {
		exitCode, err := process.wait()
		process.closePseudoConsole()
		waitResultCh <- waitResult{exitCode: exitCode, err: err}
	}()

	if err := group.Wait(); err != nil {
		zap.S().Warnf("%v", err)
	}

	// Output stops being read here even if forwarding failed early, so let writes fail instead
	// of blocking the command, and closing the pseudo console on older Windows versions
	process.closeOutput()

	result := <-waitResultCh

	// Stop killing the process tree on cancellation now that the command finished
	finish()

	// Minimize the window in which a finished exec can still be signaled
	rpc.execs.Delete(execID)

	// Prefer a client error over the command exit result
	select {
	case err := <-fromClientErrCh:
		return err
	default:
	}

	if result.err != nil {
		return result.err
	}

	// Windows exit codes are unsigned, so NTSTATUS failure codes turn negative
	return stream.Send(&v1.ExecResponse{
		Type: &v1.ExecResponse_Exit_{
			Exit: &v1.ExecResponse_Exit{
				Code: int32(result.exitCode),
			},
		},
	})
}

func forwardOutput(reader io.Reader, send func(data []byte) error) error {
	buf := make([]byte, standardStreamsBufferSize)

	for {
		count, err := reader.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}

		if err := send(slices.Clone(buf[:count])); err != nil {
			return err
		}
	}
}

// Signal ends the process tree of an exec, since Windows has no signals: SIGTERM and SIGKILL both
// terminate it, and the exit code is the one a Unix process killed by that signal reports.
func (rpc *RPC) Signal(_ context.Context, request *v1.SignalRequest) (*emptypb.Empty, error) {
	process, ok := rpc.execs.Load(request.GetExecId())
	if !ok {
		return nil, fmt.Errorf("exec %q is not running", request.GetExecId())
	}

	var exitCode uint32

	switch request.GetSignal() {
	case v1.SignalRequest_SIGNAL_SIGTERM:
		exitCode = sigtermExitCode
	case v1.SignalRequest_SIGNAL_SIGKILL:
		exitCode = sigkillExitCode
	default:
		return nil, fmt.Errorf("unsupported exec signal %q", request.GetSignal().String())
	}

	if err := process.terminate(exitCode); err != nil {
		// The process may exit after lookup, so treat the missing process as a no-op
		if errors.Is(err, os.ErrProcessDone) {
			return &emptypb.Empty{}, nil
		}

		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func applyExecOverrides(cmd *exec.Cmd, command *v1.ExecRequest_Command) {
	if command.GetWorkdir() != "" {
		cmd.Dir = command.GetWorkdir()
	}

	if len(command.GetEnv()) > 0 {
		cmd.Env = mergeEnv(command.GetEnv())
	}
}

// isAgentUser reports whether name, with or without a domain, is the user running the agent.
func isAgentUser(name string) bool {
	current, err := user.Current()
	if err != nil {
		return false
	}

	_, currentName, _ := strings.Cut(current.Username, `\`)

	return strings.EqualFold(name, current.Username) || strings.EqualFold(name, currentName)
}

func envKey(name string) string {
	return strings.ToUpper(name)
}
