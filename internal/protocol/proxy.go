package protocol

import (
	"io"
	"net"
)

// BiProxy copies data bidirectionally between two connections.
// It waits for BOTH directions to complete before returning.
// When one direction finishes, it closes the write side of the other
// connection to signal EOF and allow graceful shutdown.
func BiProxy(conn1, conn2 net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(conn1, conn2)
		// Signal EOF to conn1 reader by closing write side
		if c, ok := conn1.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
		done <- struct{}{}
	}()

	go func() {
		io.Copy(conn2, conn1)
		// Signal EOF to conn2 reader by closing write side
		if c, ok := conn2.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
		done <- struct{}{}
	}()

	// Wait for BOTH directions to complete
	<-done
	<-done
}

// BiProxyReadWriter copies data bidirectionally between two ReadWriters.
// Unlike BiProxy, this version works with any io.ReadWriter and doesn't
// attempt half-close. It waits for BOTH directions to complete.
func BiProxyReadWriter(rw1, rw2 io.ReadWriter) {
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(rw1, rw2)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(rw2, rw1)
		done <- struct{}{}
	}()

	// Wait for BOTH directions to complete
	<-done
	<-done
}
