package protocol_test

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/mpm/dworm/internal/protocol"
	"github.com/mpm/dworm/internal/protocol/testutil"
)

func TestBiProxyPropagatesHalfCloseThroughYamux(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	hostStream, endpointStream := openStreamPair(t, h)
	client, hostConn := tcpConnPair(t)
	endpointConn, server := tcpConnPair(t)
	defer client.Close()
	defer server.Close()

	proxyDone := make(chan error, 2)
	go func() { proxyDone <- protocol.BiProxy(hostConn, hostStream) }()
	go func() { proxyDone <- protocol.BiProxy(endpointStream, endpointConn) }()

	request := []byte("request body")
	if _, err := client.Write(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("half-close client: %v", err)
	}

	received, err := io.ReadAll(server)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	if !bytes.Equal(received, request) {
		t.Fatalf("request = %q, want %q", received, request)
	}

	response := []byte("close-delimited response")
	if _, err := server.Write(response); err != nil {
		t.Fatalf("write response: %v", err)
	}
	if err := server.CloseWrite(); err != nil {
		t.Fatalf("half-close server: %v", err)
	}

	received, err = io.ReadAll(client)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !bytes.Equal(received, response) {
		t.Fatalf("response = %q, want %q", received, response)
	}

	for range 2 {
		select {
		case err := <-proxyDone:
			if err != nil {
				t.Fatalf("proxy failed: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("proxy did not stop after both peers half-closed")
		}
	}
}

func TestBiProxyErrorUnblocksOppositeDirection(t *testing.T) {
	leftProxy, leftPeer := net.Pipe()
	rightProxy, rightPeer := net.Pipe()
	defer leftPeer.Close()
	defer rightPeer.Close()

	done := make(chan error, 1)
	go func() { done <- protocol.BiProxy(leftProxy, rightProxy) }()

	if err := leftProxy.SetWriteDeadline(time.Now()); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	_, _ = rightPeer.Write([]byte("trigger write failure"))

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected proxy error")
		}
	case <-time.After(time.Second):
		t.Fatal("opposite copy remained blocked after proxy error")
	}
}

func openStreamPair(t *testing.T, h *testutil.TestHarness) (net.Conn, net.Conn) {
	t.Helper()

	type result struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan result, 1)
	go func() {
		conn, err := h.EndpointMux.AcceptStream()
		accepted <- result{conn: conn, err: err}
	}()

	hostStream, err := h.HostMux.OpenStream()
	if err != nil {
		t.Fatalf("open host stream: %v", err)
	}
	endpointResult := <-accepted
	if endpointResult.err != nil {
		hostStream.Close()
		t.Fatalf("accept endpoint stream: %v", endpointResult.err)
	}
	return hostStream, endpointResult.conn
}

func tcpConnPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	type result struct {
		conn *net.TCPConn
		err  error
	}
	accepted := make(chan result, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		accepted <- result{conn: conn, err: err}
	}()

	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	serverResult := <-accepted
	if serverResult.err != nil {
		client.Close()
		t.Fatalf("accept: %v", serverResult.err)
	}
	return client, serverResult.conn
}
