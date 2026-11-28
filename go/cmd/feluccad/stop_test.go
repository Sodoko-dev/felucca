package main

// The stop (PS-16): SIGTERM or SIGINT closes the listeners, ends the
// background loops and waits for the final exec flush.

import (
	"errors"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestStopOnSignalClosesTheListenersAndWaitsForTheFinalFlush(t *testing.T) {
	cfg, db := bootFixture(t)
	srv, err := boot(cfg, db)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(srv.StopBackground)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{
		Handler:        srv.Handler(),
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   60 * time.Second,
		MaxHeaderBytes: maxHeaderBytes,
	}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()

	sigs := make(chan os.Signal, 1)
	stop := make(chan struct{})
	flushed := make(chan struct{})
	// Stands in for UsageFlushLoop: it flushes once stop closes, and takes a
	// moment to do it.
	go func() {
		<-stop
		time.Sleep(100 * time.Millisecond)
		close(flushed)
	}()
	done := make(chan struct{})
	go func() {
		stopOnSignal(sigs, []*http.Server{hs}, srv, stop, flushed)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("stopOnSignal returned without a signal")
	case <-time.After(50 * time.Millisecond):
	}

	sigs <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stopOnSignal did not return after SIGTERM")
	}
	select {
	case <-flushed:
	default:
		t.Error("stopOnSignal returned before the final flush")
	}
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("listener after the stop: %v, want http.ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		hs.Close()
		t.Error("the listener was not closed by the stop")
	}
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Error("the listener still accepts connections after the stop")
	}
}
