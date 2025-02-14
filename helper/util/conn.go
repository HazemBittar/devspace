package util

import (
	"context"
	"google.golang.org/grpc"
	"io"
	"net"
)

// NewClientConnection creates a new client connection for the given reader and writer
func NewClientConnection(reader io.Reader, writer io.Writer) (*grpc.ClientConn, error) {
	pipe := NewStdStreamJoint(reader, writer, false)

	// Set up a connection to the server.
	return grpc.Dial("", grpc.WithInsecure(), grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
		return pipe, nil
	}))
}
