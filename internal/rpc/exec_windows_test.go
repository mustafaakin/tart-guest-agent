//nolint:testpackage
package rpc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/cirruslabs/tart-guest-agent/pkg/v1"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var execTestShell = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")

// execTestOutput collects the output of an exec until it exits.
func execTestOutput(t *testing.T, stream *execTestStream) (stdout, stderr string, exitCode int32) {
	t.Helper()

	var stdoutBuilder, stderrBuilder strings.Builder

	for {
		response := receiveExecResponse(t, stream)
		switch response := response.GetType().(type) {
		case *v1.ExecResponse_StandardOutput:
			stdoutBuilder.Write(response.StandardOutput.GetData())
		case *v1.ExecResponse_StandardError:
			stderrBuilder.Write(response.StandardError.GetData())
		case *v1.ExecResponse_Exit_:
			return stdoutBuilder.String(), stderrBuilder.String(), response.Exit.GetCode()
		default:
			t.Fatalf("unexpected exec response %T", response)
		}
	}
}

func TestExecWindowsForwardsOutputAndExitCode(t *testing.T) {
	_, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name: "cmd",
		Args: []string{"/c", "echo out& echo err 1>&2& exit 3"},
	})
	require.NotEmpty(t, receiveExecResponse(t, stream).GetStarted().GetExecId())

	stdout, stderr, exitCode := execTestOutput(t, stream)
	require.Equal(t, "out\r\n", stdout)
	require.Equal(t, "err \r\n", stderr)
	require.EqualValues(t, 3, exitCode)
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecWindowsAppliesWorkdirAndEnv(t *testing.T) {
	dir := t.TempDir()

	_, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name:    execTestShell,
		Args:    []string{"/c", "cd& echo %TART_TEST%& echo %path%"},
		Workdir: dir,
		Env:     map[string]string{"TART_TEST": "value", "path": `C:\tart-test`},
	})
	require.NotNil(t, receiveExecResponse(t, stream).GetStarted())

	stdout, _, exitCode := execTestOutput(t, stream)
	require.Equal(t, dir+"\r\nvalue\r\n"+`C:\tart-test`+"\r\n", stdout)
	require.Zero(t, exitCode)
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecWindowsClosesStandardInputOnEmptyChunk(t *testing.T) {
	_, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name:        "sort",
		Interactive: true,
	})
	require.NotNil(t, receiveExecResponse(t, stream).GetStarted())

	stream.requests <- &v1.ExecRequest{Type: &v1.ExecRequest_StandardInput{
		StandardInput: &v1.IOChunk{Data: []byte("b\r\na\r\n")},
	}}
	stream.requests <- &v1.ExecRequest{Type: &v1.ExecRequest_StandardInput{
		StandardInput: &v1.IOChunk{},
	}}

	stdout, _, exitCode := execTestOutput(t, stream)
	require.Equal(t, "a\r\nb\r\n", stdout)
	require.Zero(t, exitCode)
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecWindowsTTY(t *testing.T) {
	_, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name:         execTestShell,
		Args:         []string{"/c", "echo hello from conpty& exit 4"},
		Tty:          true,
		TerminalSize: &v1.TerminalSize{Rows: 30, Cols: 100},
	})
	require.NotNil(t, receiveExecResponse(t, stream).GetStarted())

	stdout, stderr, exitCode := execTestOutput(t, stream)
	require.Contains(t, stdout, "hello from conpty")
	require.Empty(t, stderr)
	require.EqualValues(t, 4, exitCode)
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecWindowsInteractiveTTY(t *testing.T) {
	_, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name:        execTestShell,
		Args:        []string{"/q", "/k"},
		Interactive: true,
		Tty:         true,
	})
	require.NotNil(t, receiveExecResponse(t, stream).GetStarted())

	stream.requests <- &v1.ExecRequest{Type: &v1.ExecRequest_TerminalResize{
		TerminalResize: &v1.TerminalSize{Rows: 40, Cols: 120},
	}}
	stream.requests <- &v1.ExecRequest{Type: &v1.ExecRequest_StandardInput{
		StandardInput: &v1.IOChunk{Data: []byte("exit 5\r")},
	}}

	_, _, exitCode := execTestOutput(t, stream)
	require.EqualValues(t, 5, exitCode)
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecWindowsReportsStartFailureBeforeStarted(t *testing.T) {
	for name, command := range map[string]*v1.ExecRequest_Command{
		"missing executable": {Name: `C:\definitely\missing\tart-guest-agent-test-command.exe`},
		"missing workdir":    {Name: execTestShell, Workdir: `C:\definitely\missing\tart-guest-agent-test-workdir`},
		"missing TTY workdir": {
			Name: execTestShell, Workdir: `C:\definitely\missing\tart-guest-agent-test-workdir`, Tty: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, stream, result := startExecTest(t, command)
			response := receiveExecResponse(t, stream)
			require.Nil(t, response.GetStarted())
			require.EqualValues(t, execRuntimeFailureExitCode, response.GetExit().GetCode())
			require.NoError(t, receiveExecResult(t, result))
		})
	}
}

func TestExecWindowsRejectsOtherUsers(t *testing.T) {
	_, _, result := startExecTest(t, &v1.ExecRequest_Command{
		Name: execTestShell,
		User: "tart-guest-agent-test-missing-user",
	})
	require.Equal(t, codes.Unimplemented, status.Code(receiveExecResult(t, result)))
}

func TestExecWindowsDetach(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "detached")

	_, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name:   execTestShell,
		Args:   []string{"/c", "echo detached> " + marker},
		Detach: true,
	})
	started := receiveExecResponse(t, stream).GetStarted()
	require.NotNil(t, started)
	require.Empty(t, started.GetExecId())
	require.Zero(t, receiveExecResponse(t, stream).GetExit().GetCode())
	require.NoError(t, receiveExecResult(t, result))

	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)

		return err == nil
	}, execTestTimeout, 10*time.Millisecond)
}

func TestExecWindowsSignalsProcessTree(t *testing.T) {
	for name, test := range map[string]struct {
		signal v1.SignalRequest_Signal
		code   int32
	}{
		"SIGTERM": {signal: v1.SignalRequest_SIGNAL_SIGTERM, code: sigtermExitCode},
		"SIGKILL": {signal: v1.SignalRequest_SIGNAL_SIGKILL, code: sigkillExitCode},
	} {
		t.Run(name, func(t *testing.T) {
			// The output pipe only closes once the background ping, which inherits it, is gone too
			rpc, stream, result := startExecTest(t, &v1.ExecRequest_Command{
				Name: execTestShell,
				Args: []string{"/c", "start /b ping -n 60 127.0.0.1& echo ready& ping -n 60 127.0.0.1 >NUL"},
			})
			started := receiveExecResponse(t, stream).GetStarted()
			require.NotNil(t, started)

			var output strings.Builder
			for !strings.Contains(output.String(), "ready") {
				response := receiveExecResponse(t, stream)
				require.Nil(t, response.GetExit())
				output.Write(response.GetStandardOutput().GetData())
			}

			_, err := rpc.Signal(context.Background(), &v1.SignalRequest{
				ExecId: started.GetExecId(),
				Signal: test.signal,
			})
			require.NoError(t, err)

			_, _, exitCode := execTestOutput(t, stream)
			require.Equal(t, test.code, exitCode)
			require.NoError(t, receiveExecResult(t, result))
		})
	}
}

func TestExecWindowsRejectsUnsupportedSignal(t *testing.T) {
	rpc, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name: "ping",
		Args: []string{"-n", "60", "127.0.0.1"},
	})
	started := receiveExecResponse(t, stream).GetStarted()
	require.NotNil(t, started)

	_, err := rpc.Signal(context.Background(), &v1.SignalRequest{ExecId: started.GetExecId()})
	require.EqualError(t, err, `unsupported exec signal "SIGNAL_UNSPECIFIED"`)

	_, err = rpc.Signal(context.Background(), &v1.SignalRequest{
		ExecId: started.GetExecId(),
		Signal: v1.SignalRequest_SIGNAL_SIGKILL,
	})
	require.NoError(t, err)
	_, _, exitCode := execTestOutput(t, stream)
	require.EqualValues(t, sigkillExitCode, exitCode)
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecWindowsWrapper(t *testing.T) {
	// The wrapper runs instead of the command, which it receives as arguments
	rpc, err := New(nil, execTestShell, "/c", "echo", "wrapped")
	require.NoError(t, err)

	_, stream, result := startExecTestWithRPC(t, rpc, &v1.ExecRequest_Command{
		Name: "tart-guest-agent-test-missing-command",
		Args: []string{"arg"},
	})
	require.NotNil(t, receiveExecResponse(t, stream).GetStarted())

	stdout, _, exitCode := execTestOutput(t, stream)
	require.Equal(t, "wrapped tart-guest-agent-test-missing-command arg\r\n", stdout)
	require.Zero(t, exitCode)
	require.NoError(t, receiveExecResult(t, result))
}

func TestValidateExecWrapperWindows(t *testing.T) {
	require.NoError(t, ValidateExecWrapper([]string{execTestShell, "/c"}))

	batch := filepath.Join(t.TempDir(), "wrapper.cmd")
	require.NoError(t, os.WriteFile(batch, []byte("@%*\r\n"), 0o600))
	require.ErrorContains(t, ValidateExecWrapper([]string{batch}), ".exe")

	require.Error(t, ValidateExecWrapper([]string{"cmd.exe"}))
}

func TestMergeEnvIsCaseInsensitive(t *testing.T) {
	t.Setenv("TART_TEST_CASE", "old")

	merged := mergeEnv(map[string]string{"tart_test_case": "new"})

	var matches []string
	for _, entry := range merged {
		if strings.HasPrefix(strings.ToUpper(entry), "TART_TEST_CASE=") {
			matches = append(matches, entry)
		}
	}
	require.Equal(t, []string{"tart_test_case=new"}, matches)
}

func TestEnvironmentBlock(t *testing.T) {
	block, err := environmentBlock([]string{"A=1", "B=2"})
	require.NoError(t, err)
	expected := utf16.Encode([]rune("A=1\x00B=2\x00\x00"))
	require.Equal(t, expected, unsafe.Slice(block, len(expected)))

	block, err = environmentBlock(nil)
	require.NoError(t, err)
	require.Equal(t, []uint16{0, 0}, unsafe.Slice(block, 2))

	_, err = environmentBlock([]string{"A=\x00"})
	require.Error(t, err)
}

func TestTerminalSize(t *testing.T) {
	require.Equal(t, windows.Coord{X: 120, Y: 40}, terminalSize(40, 120))
	require.Equal(t, windows.Coord{X: defaultTerminalCols, Y: defaultTerminalRows}, terminalSize(0, 0))
	require.Equal(t, windows.Coord{X: 0x7FFF, Y: 0x7FFF}, terminalSize(1<<20, 1<<20))
}
