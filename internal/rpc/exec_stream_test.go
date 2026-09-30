// In-process stream scaffolding intentionally favors direct test construction.
//
//nolint:containedctx,testpackage,wsl_v5
package rpc

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/cirruslabs/tart-guest-agent/pkg/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const execTestTimeout = 5 * time.Second

type execTestStream struct {
	grpc.ServerStream

	ctx       context.Context
	requests  chan *v1.ExecRequest
	responses chan *v1.ExecResponse
	sendHook  func(*v1.ExecResponse) error
}

var _ grpc.BidiStreamingServer[v1.ExecRequest, v1.ExecResponse] = (*execTestStream)(nil)

func newExecTestStream(ctx context.Context) *execTestStream {
	return &execTestStream{
		ctx:       ctx,
		requests:  make(chan *v1.ExecRequest, 8),
		responses: make(chan *v1.ExecResponse, 8),
	}
}

func (stream *execTestStream) Send(response *v1.ExecResponse) error {
	if stream.sendHook != nil {
		if err := stream.sendHook(response); err != nil {
			return err
		}
	}

	select {
	case stream.responses <- response:
		return nil
	case <-stream.ctx.Done():
		return stream.ctx.Err()
	}
}

func (stream *execTestStream) Recv() (*v1.ExecRequest, error) {
	select {
	case request, ok := <-stream.requests:
		if !ok {
			return nil, io.EOF
		}
		return request, nil
	case <-stream.ctx.Done():
		return nil, stream.ctx.Err()
	}
}

func (stream *execTestStream) Context() context.Context { return stream.ctx }

func startExecTest(
	t *testing.T,
	command *v1.ExecRequest_Command,
	configure ...func(*execTestStream),
) (*RPC, *execTestStream, <-chan error) {
	t.Helper()
	rpc, err := New(nil)
	require.NoError(t, err)
	return startExecTestWithRPC(t, rpc, command, configure...)
}

func startExecTestWithRPC(
	t *testing.T,
	rpc *RPC,
	command *v1.ExecRequest_Command,
	configure ...func(*execTestStream),
) (*RPC, *execTestStream, <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream := newExecTestStream(ctx)
	for _, configureStream := range configure {
		configureStream(stream)
	}
	result := make(chan error, 1)
	go func() {
		result <- rpc.Exec(stream)
	}()
	stream.requests <- &v1.ExecRequest{
		Type: &v1.ExecRequest_Command_{Command: command},
	}
	return rpc, stream, result
}

func receiveExecResponse(t *testing.T, stream *execTestStream) *v1.ExecResponse {
	t.Helper()

	select {
	case response := <-stream.responses:
		return response
	case <-time.After(execTestTimeout):
		t.Fatal("timed out waiting for exec response")
		return nil
	}
}

func receiveExecResult(t *testing.T, result <-chan error) error {
	t.Helper()

	select {
	case err := <-result:
		return err
	case <-time.After(execTestTimeout):
		t.Fatal("timed out waiting for Exec to return")
		return nil
	}
}
