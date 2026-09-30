package rpc

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cirruslabs/tart-guest-agent/pkg/v1"
	"github.com/samber/lo"
	"google.golang.org/grpc"
)

const (
	standardStreamsBufferSize = 4096

	// execRuntimeFailureExitCode matches Docker's exit code for runtime failures before a process starts.
	execRuntimeFailureExitCode = 125
	// signalExitCodeOffset is the base for shell-style exit codes of processes terminated by signals.
	signalExitCodeOffset = 128
)

func closeStdin(stdin io.WriteCloser, tty bool, closed *bool) error {
	if stdin == nil || *closed {
		return nil
	}

	if tty {
		// When using pseudo-terminal, we can't simply close the
		// standard input, as the file descriptor is shared for
		// standard output and standard error too, so we send
		// an EOF character sequence instead
		if _, err := stdin.Write(ttyEOF); err != nil {
			return err
		}
	} else if err := stdin.Close(); err != nil {
		return err
	}

	*closed = true

	return nil
}

func sendStartSuccess(stream grpc.BidiStreamingServer[v1.ExecRequest, v1.ExecResponse], execID string) error {
	return stream.Send(&v1.ExecResponse{
		Type: &v1.ExecResponse_Started_{
			Started: &v1.ExecResponse_Started{
				ExecId: execID,
			},
		},
	})
}

func sendStartFailure(stream grpc.BidiStreamingServer[v1.ExecRequest, v1.ExecResponse]) error {
	return stream.Send(&v1.ExecResponse{
		Type: &v1.ExecResponse_Exit_{
			Exit: &v1.ExecResponse_Exit{
				Code: execRuntimeFailureExitCode,
			},
		},
	})
}

func mergeEnv(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return os.Environ()
	}

	// Key variables by envKey, since names are case-insensitive on Windows
	envMap := make(map[string]string, len(overrides))
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		envMap[envKey(name)] = entry
	}

	for key, value := range overrides {
		envMap[envKey(key)] = key + "=" + value
	}

	merged := make([]string, 0, len(envMap))
	for _, entry := range envMap {
		merged = append(merged, entry)
	}

	return merged
}

func formatCommandAndArgs(name string, args []string) string {
	var all []string

	all = append(all, name)
	all = append(all, args...)

	all = lo.Map(all, func(item string, _ int) string {
		return fmt.Sprintf("%q", item)
	})

	return fmt.Sprintf("[%s]", strings.Join(all, ", "))
}
