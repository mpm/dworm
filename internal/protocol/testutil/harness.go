// Package testutil provides test utilities for the protocol package.
package testutil

import (
	"io"
	"log"
	"sync"

	"github.com/mpm/dworm/internal/protocol"
)

// pipeRWC combines a PipeReader and PipeWriter into a ReadWriteCloser.
type pipeRWC struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func (p *pipeRWC) Read(data []byte) (int, error) {
	return p.reader.Read(data)
}

func (p *pipeRWC) Write(data []byte) (int, error) {
	return p.writer.Write(data)
}

func (p *pipeRWC) Close() error {
	p.writer.Close()
	return p.reader.Close()
}

// TestHarness connects a host and endpoint mux over io.Pipe for testing.
type TestHarness struct {
	HostMux     *protocol.Mux
	EndpointMux *protocol.Mux

	// For cleanup
	hostRWC     *pipeRWC
	endpointRWC *pipeRWC
}

// NewTestHarness creates a connected host/endpoint mux pair using io.Pipe.
// The host mux operates in client mode, the endpoint in server mode.
func NewTestHarness() (*TestHarness, error) {
	// Create two pipes for bidirectional communication
	// Pipe 1: host writes -> endpoint reads
	endpointReader, hostWriter := io.Pipe()
	// Pipe 2: endpoint writes -> host reads
	hostReader, endpointWriter := io.Pipe()

	// Create ReadWriteCloser for each side
	hostRWC := &pipeRWC{reader: hostReader, writer: hostWriter}
	endpointRWC := &pipeRWC{reader: endpointReader, writer: endpointWriter}

	var hostMux, endpointMux *protocol.Mux
	var hostErr, endpointErr error

	var wg sync.WaitGroup
	wg.Add(2)

	// Create host mux (client mode) in goroutine
	go func() {
		defer wg.Done()
		hostMux, hostErr = protocol.NewClientMux(hostRWC, log.New(io.Discard, "", 0))
	}()

	// Create endpoint mux (server mode) in goroutine
	go func() {
		defer wg.Done()
		endpointMux, endpointErr = protocol.NewServerMux(endpointRWC, log.New(io.Discard, "", 0))
	}()

	wg.Wait()

	if hostErr != nil {
		return nil, hostErr
	}
	if endpointErr != nil {
		return nil, endpointErr
	}

	return &TestHarness{
		HostMux:     hostMux,
		EndpointMux: endpointMux,
		hostRWC:     hostRWC,
		endpointRWC: endpointRWC,
	}, nil
}

// Close cleans up the harness by closing both muxes.
func (h *TestHarness) Close() {
	if h.HostMux != nil {
		h.HostMux.Close()
	}
	if h.EndpointMux != nil {
		h.EndpointMux.Close()
	}
}
