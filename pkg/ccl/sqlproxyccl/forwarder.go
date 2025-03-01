// Copyright 2022 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package sqlproxyccl

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/sqlproxyccl/interceptor"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

// forwarder is used to forward pgwire messages from the client to the server,
// and vice-versa. The forwarder instance should always be constructed through
// the forward function, which also starts the forwarder.
type forwarder struct {
	// ctx is a single context used to control all goroutines spawned by the
	// forwarder. An exception to this is that if the goroutines are blocked
	// due to IO on clientConn or serverConn, cancelling the context does not
	// unblock them. Due to this, it is important to invoke Close() on the
	// forwarder whenever ctx has been cancelled to prevent leaks.
	ctx       context.Context
	ctxCancel context.CancelFunc

	// connector is an instance of the connector, which will be used to open a
	// new connection to a SQL pod. This connector instance must be associated
	// to the same tenant as the forwarder.
	connector *connector

	// metrics contains various counters reflecting proxy operations. This is
	// the same as the metrics field in the proxyHandler instance.
	metrics *metrics

	// errCh is a buffered channel that contains the first forwarder error.
	// This channel may receive nil errors. When an error is written to this
	// channel, it is guaranteed that the forwarder and all connections will
	// be closed.
	errCh chan error

	// While not all of these fields may need to be guarded by a mutex, we do
	// so for consistency. Fields like clientConn and serverConn need them
	// because Close can be invoked anytime from a different goroutine while
	// the connection migration is in progress. On the other hand, the processor
	// fields will only be updated during connection migration, and we can
	// guarantee that processors will be suspended, so we don't need mutexes
	// for them.
	mu struct {
		syncutil.Mutex

		// isTransferring indicates that a connection migration is in progress.
		isTransferring bool

		// clientConn and serverConn provide a convenient way to read and forward
		// Postgres messages, while minimizing IO reads and memory allocations.
		//
		// clientConn is set once during initialization, and stays the same
		// throughout the lifetime of the forwarder.
		//
		// serverConn is set during initialization, which happens after the
		// authentication phase, and will be replaced if a connection migration
		// occurs. During a connection migration, serverConn is only replaced once
		// the session has successfully been deserialized, and the old connection
		// will be closed.
		//
		// All reads from these connections must go through the PG interceptors.
		// It is not safe to call Read directly as the interceptors may have
		// buffered data.
		clientConn *interceptor.PGConn // client <-> proxy
		serverConn *interceptor.PGConn // proxy <-> server

		// request and response both represent the processors used to handle
		// client-to-server and server-to-client messages.
		//
		// WARNING: When acquiring locks on both of the processors, they should
		// be acquired in the following order: request->response to avoid any
		// potential deadlocks.
		request  *processor // client -> server
		response *processor // server -> client
	}
}

// forward returns a new instance of forwarder, and starts forwarding messages
// from clientConn to serverConn (and vice-versa). When this is called, it is
// expected that the caller passes ownership of both clientConn and serverConn
// to the forwarder, which implies that the forwarder will clean them up.
// clientConn and serverConn must not be nil in all cases except for testing.
//
// Note that callers MUST call Close in all cases, even if ctx was cancelled,
// and callers will need to detect that (for now).
func forward(
	ctx context.Context,
	connector *connector,
	metrics *metrics,
	clientConn net.Conn,
	serverConn net.Conn,
) (*forwarder, error) {
	ctx, cancelFn := context.WithCancel(ctx)
	f := &forwarder{
		ctx:       ctx,
		ctxCancel: cancelFn,
		errCh:     make(chan error, 1),
		connector: connector,
		metrics:   metrics,
	}
	f.mu.clientConn = interceptor.NewPGConn(clientConn)
	f.mu.serverConn = interceptor.NewPGConn(serverConn)

	clockFn := makeLogicalClockFn()
	f.mu.request = newProcessor(clockFn, f.mu.clientConn, f.mu.serverConn)  // client -> server
	f.mu.response = newProcessor(clockFn, f.mu.serverConn, f.mu.clientConn) // server -> client
	if err := f.resumeProcessors(); err != nil {
		return nil, err
	}
	return f, nil
}

// Close closes the forwarder and all connections. This is idempotent.
func (f *forwarder) Close() {
	f.ctxCancel()

	// Whenever Close is called while both of the processors are suspended, the
	// main goroutine will be stuck waiting for a reponse from the forwarder.
	// Send an error to unblock that. If an error has been sent, this error will
	// be ignored.
	//
	// We don't use tryReportError here since that will call Close, leading to
	// a recursive call.
	select {
	case f.errCh <- errors.New("forwarder closed"): /* error reported */
	default: /* the channel already contains an error */
	}

	// Since Close is idempotent, we'll ignore the error from Close calls in
	// case they have already been closed.
	clientConn, serverConn := f.getConns()
	clientConn.Close()
	serverConn.Close()
}

// resumeProcessors starts both the request and response processors
// asynchronously. The forwarder will be closed if any of the processors
// return an error while resuming. This is idempotent as resume() will return
// nil if the processor has already been started.
func (f *forwarder) resumeProcessors() error {
	requestProc, responseProc := f.getProcessors()
	go func() {
		if err := requestProc.resume(f.ctx); err != nil {
			f.tryReportError(wrapClientToServerError(err))
		}
	}()
	go func() {
		if err := responseProc.resume(f.ctx); err != nil {
			f.tryReportError(wrapServerToClientError(err))
		}
	}()
	if err := requestProc.waitResumed(f.ctx); err != nil {
		return err
	}
	if err := responseProc.waitResumed(f.ctx); err != nil {
		return err
	}
	return nil
}

// tryReportError tries to send err to errCh, and closes the forwarder if
// it succeeds. If an error has already been reported, err will be dropped.
func (f *forwarder) tryReportError(err error) {
	select {
	case f.errCh <- err: /* error reported */
		// Whenever an error has been reported, all processors must terminate to
		// stop processing on either sides, and the easiest way to do so is to
		// close the forwarder, which closes all connections. Doing this also
		// ensures that resuming a processor again will return an error.
		f.Close()
	default: /* the channel already contains an error */
	}
}

// getProcessors returns the processors associated with the forwarder.
func (f *forwarder) getProcessors() (request, response *processor) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mu.request, f.mu.response
}

// getConns returns the connections associated with the forwarder.
func (f *forwarder) getConns() (client, server *interceptor.PGConn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mu.clientConn, f.mu.serverConn
}

// replaceServerConn replaces serverConn with newServerConn. When that happens,
// serverConn will be closed, and new processors will be recreated.
//
// NOTE: It is important for the processors to be suspended before calling
// this function.
func (f *forwarder) replaceServerConn(newServerConn *interceptor.PGConn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clockFn := makeLogicalClockFn()
	f.mu.serverConn.Close()
	f.mu.serverConn = newServerConn
	f.mu.request = newProcessor(clockFn, f.mu.clientConn, f.mu.serverConn)
	f.mu.response = newProcessor(clockFn, f.mu.serverConn, f.mu.clientConn)
}

// wrapClientToServerError overrides client to server errors for external
// consumption.
//
// TODO(jaylim-crl): We don't send any of these to the client today,
// unfortunately. At the moment, this is only used for metrics. See TODO in
// proxy_handler about sending safely to avoid corrupted packets. Handle these
// errors in a friendly manner.
func wrapClientToServerError(err error) error {
	if err == nil ||
		errors.IsAny(err, context.Canceled, context.DeadlineExceeded) {
		return nil
	}
	return newErrorf(codeClientDisconnected, "copying from client to target server: %v", err)
}

// wrapServerToClientError overrides server to client errors for external
// consumption.
//
// TODO(jaylim-crl): We don't send any of these to the client today,
// unfortunately. At the moment, this is only used for metrics. See TODO in
// proxy_handler about sending safely to avoid corrupted packets. Handle these
// errors in a friendly manner.
func wrapServerToClientError(err error) error {
	if err == nil ||
		errors.IsAny(err, context.Canceled, context.DeadlineExceeded) {
		return nil
	}
	return newErrorf(codeBackendDisconnected, "copying from target server to client: %s", err)
}

// makeLogicalClockFn returns a function that implements a simple logical clock.
// This implementation could overflow in theory, but it doesn't matter for the
// forwarder since the worst that could happen is that we are unable to transfer
// for an extremely short period of time until all the processors have wrapped
// around. That said, this situation is rare since uint64 is a huge number, and
// we restart the clock on each transfer.
func makeLogicalClockFn() func() uint64 {
	var counter uint64
	return func() uint64 {
		return atomic.AddUint64(&counter, 1)
	}
}

// aLongTimeAgo is a non-zero time, far in the past, used for immediate
// cancellation of dials.
var aLongTimeAgo = timeutil.Unix(1, 0)

var errProcessorResumed = errors.New("processor has already been resumed")

// processor must always be constructed through newProcessor.
type processor struct {
	// src and dst are immutable fields. A new processor should be created if
	// any of those fields need to be updated. When that happens, all existing
	// processors must be terminated first to prevent concurrent reads on src.
	src *interceptor.PGConn
	dst *interceptor.PGConn

	mu struct {
		syncutil.Mutex
		cond       *sync.Cond
		resumed    bool
		inPeek     bool
		suspendReq bool // Indicates that a suspend has been requested.

		lastMessageTransferredAt uint64 // Updated through logicalClockFn
		lastMessageType          byte
	}
	logicalClockFn func() uint64

	testingKnobs struct {
		beforeForwardMsg func()
	}
}

func newProcessor(logicalClockFn func() uint64, src, dst *interceptor.PGConn) *processor {
	p := &processor{logicalClockFn: logicalClockFn, src: src, dst: dst}
	p.mu.cond = sync.NewCond(&p.mu)
	return p
}

// resume starts the processor and blocks during the processing. When the
// processing has been terminated, this returns nil if the processor can be
// resumed again in the future. If an error (except errProcessorResumed) was
// returned, the processor should not be resumed again, and the forwarder should
// be closed.
func (p *processor) resume(ctx context.Context) error {
	enterResume := func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.mu.resumed {
			return errProcessorResumed
		}
		p.mu.resumed = true
		p.mu.cond.Broadcast()
		return nil
	}
	exitResume := func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.mu.resumed = false
		p.mu.cond.Broadcast()
	}
	prepareNextMessage := func() (terminate bool, err error) {
		// If suspend was requested, or a transfer has been started, we
		// terminate to avoid blocking on PeekMsg as an optimization.
		if terminate := func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			// Suspend has been requested. Suspend now before blocking.
			if p.mu.suspendReq {
				return true
			}
			p.mu.inPeek = true
			return false
		}(); terminate {
			return true, nil
		}

		// Always peek the message to ensure that we're blocked on reading the
		// header, rather than when forwarding during idle periods.
		typ, _, peekErr := p.src.PeekMsg()

		// Update peek state, and check for suspension.
		p.mu.Lock()
		defer p.mu.Unlock()
		p.mu.inPeek = false

		// If suspend was requested, there are two cases where we terminate:
		//   1. peekErr == nil, where we read a header. In that case, suspension
		//      gets priority.
		//   2. peekErr != nil, where the error was due to a timeout. Connection
		//      was likely idle here.
		//
		// When suspending, we return nil so that the processor can be resumed
		// in the future.
		var netErr net.Error
		switch {
		case p.mu.suspendReq && peekErr == nil:
			return true, nil
		case p.mu.suspendReq && errors.As(peekErr, &netErr) && netErr.Timeout():
			return true, nil
		case peekErr != nil:
			return false, errors.Wrap(peekErr, "peeking message")
		}

		// Update last message. Once we prepare the next message, we must
		// forward that message.
		p.mu.lastMessageType = typ
		p.mu.lastMessageTransferredAt = p.logicalClockFn()
		return false, nil
	}

	if err := enterResume(); err != nil {
		return err
	}
	defer exitResume()

	for ctx.Err() == nil {
		if terminate, err := prepareNextMessage(); err != nil || terminate {
			return err
		}
		if p.testingKnobs.beforeForwardMsg != nil {
			p.testingKnobs.beforeForwardMsg()
		}
		if _, err := p.src.ForwardMsg(p.dst); err != nil {
			return errors.Wrap(err, "forwarding message")
		}
	}
	return ctx.Err()
}

// waitResumed waits until the processor has been resumed. This can be used to
// ensure that suspend actually suspends the running processor, and there won't
// be a race where the goroutines have not started running, and suspend returns.
func (p *processor) waitResumed(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for !p.mu.resumed {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.mu.cond.Wait()
	}
	return nil
}

// suspend requests for the processor to be suspended if it is in a safe state,
// and blocks until the processor has been terminated. If the suspend request
// failed, suspend returns an error, and the caller is safe to retry again.
func (p *processor) suspend(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	defer func() {
		if p.mu.suspendReq {
			p.mu.suspendReq = false
			_ = p.src.SetReadDeadline(time.Time{})
		}
	}()

	for p.mu.resumed {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.mu.suspendReq = true
		if p.mu.inPeek {
			if err := p.src.SetReadDeadline(aLongTimeAgo); err != nil {
				return err
			}
		}
		p.mu.cond.Wait()
	}
	return nil
}
