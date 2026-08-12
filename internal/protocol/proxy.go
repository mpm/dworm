package protocol

import (
	"errors"
	"io"
	"net"
)

// BiProxy copies data bidirectionally between two connections.
// EOF is propagated with a half-close where available. yamux streams implement
// Close as a write-side FIN while continuing to permit reads, so Close is the
// fallback for connections without CloseWrite.
func BiProxy(conn1, conn2 net.Conn) error {
	errs := make(chan error, 2)

	go func() {
		_, err := io.Copy(conn1, conn2)
		if err == nil {
			err = closeWrite(conn1)
		}
		errs <- err
	}()

	go func() {
		_, err := io.Copy(conn2, conn1)
		if err == nil {
			err = closeWrite(conn2)
		}
		errs <- err
	}()

	firstErr := <-errs
	if firstErr != nil {
		// A copy failure is not a graceful half-close. Tear down both sides so
		// the opposite copy cannot remain blocked indefinitely.
		conn1.Close()
		conn2.Close()
	}
	secondErr := <-errs

	return errors.Join(firstErr, secondErr)
}

func closeWrite(conn net.Conn) error {
	if conn, ok := conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return conn.Close()
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
