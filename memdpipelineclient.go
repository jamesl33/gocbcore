package gocbcore

import (
	"io"
	"sync"
)

type memdPipelineClient struct {
	parent    *memdPipeline
	address   string
	client    *memdClient
	consumer  *memdOpConsumer
	lock      sync.Mutex
	closedSig chan struct{}
	breaker   circuitBreaker
}

func newMemdPipelineClient(parent *memdPipeline) *memdPipelineClient {
	client := &memdPipelineClient{
		parent:    parent,
		address:   parent.address,
		closedSig: make(chan struct{}),
	}

	if parent.breakerCfg.Enabled {
		client.breaker = newLazyCircuitBreaker(parent.breakerCfg, client.sendCanary)
	} else {
		client.breaker = newNoopCircuitBreaker()
	}

	return client
}

func (pipecli *memdPipelineClient) ReassignTo(parent *memdPipeline) {
	pipecli.lock.Lock()
	pipecli.parent = parent
	oldConsumer := pipecli.consumer
	pipecli.consumer = nil
	pipecli.lock.Unlock()

	if oldConsumer != nil {
		oldConsumer.Close()
	}
}

func (pipecli *memdPipelineClient) ioLoop(client *memdClient) {
	pipecli.lock.Lock()
	if pipecli.parent == nil {
		logDebugf("Pipeline client ioLoop started with no parent pipeline")
		pipecli.lock.Unlock()

		err := client.Close()
		if err != nil {
			logErrorf("Failed to close client for shut down ioLoop (%s)", err)
		}

		return
	}

	pipecli.client = client
	pipecli.lock.Unlock()

	killSig := make(chan struct{})

	// This goroutine is responsible for monitoring the client and handling
	// the cleanup whenever it shuts down.  All cases of the client being
	// shut down flow through this goroutine, even cases where we may already
	// be aware that the client is shutdown, outside this scope.
	go func() {
		logDebugf("Pipeline client `%s/%p` client watcher starting...", pipecli.address, pipecli)

		<-client.CloseNotify()

		logDebugf("Pipeline client `%s/%p` client died", pipecli.address, pipecli)

		pipecli.lock.Lock()
		pipecli.client = nil
		activeConsumer := pipecli.consumer
		pipecli.consumer = nil
		pipecli.lock.Unlock()

		logDebugf("Pipeline client `%s/%p` closing consumer %p", pipecli.address, pipecli, activeConsumer)

		// If we have a consumer, we need to close it to signal the loop below that
		// something has happened.  If there is no consumer, we don't need to signal
		// as the loop below will already be in the process of fetching a new one,
		// where it will inevitably detect the problem.
		if activeConsumer != nil {
			activeConsumer.Close()
		}

		killSig <- struct{}{}
	}()

	logDebugf("Pipeline client `%s/%p` IO loop starting...", pipecli.address, pipecli)

	var localConsumer *memdOpConsumer
	for {
		if localConsumer == nil {
			logDebugf("Pipeline client `%s/%p` fetching new consumer", pipecli.address, pipecli)

			pipecli.lock.Lock()

			if pipecli.consumer != nil {
				// If we still have an active consumer, lets close it to make room for the new one
				pipecli.consumer.Close()
				pipecli.consumer = nil
			}

			if pipecli.client == nil {
				// The client has disconnected from the server, this only occurs AFTER the watcher
				// goroutine running above has detected the client is closed and has cleaned it up.
				pipecli.lock.Unlock()
				break
			}

			if pipecli.parent == nil {
				// This pipelineClient has been shut down
				logDebugf("Pipeline client `%s/%p` found no parent pipeline", pipecli.address, pipecli)
				pipecli.lock.Unlock()

				// Close our client to force the watcher goroutine above to clean it up
				err := client.Close()
				if err != nil {
					logErrorf("Pipeline client `%s/%p` failed to shut down client socket (%s)", pipecli.address, pipecli, err)
				}

				break
			}

			// Fetch a new consumer to use for this iteration
			localConsumer = pipecli.parent.queue.Consumer()
			pipecli.consumer = localConsumer

			pipecli.lock.Unlock()
		}

		req := localConsumer.Pop()
		if req == nil {
			// Set the local consumer to null, this will force our normal logic to run
			// which will clean up the original consumer and then attempt to acquire a
			// new one if we are not being cleaned up.  This is a minor code-optimization
			// to avoid having to do a lock/unlock just to lock above anyways.  It does
			// have the downside of not being able to detect where we've looped around
			// in error though.
			localConsumer = nil
			continue
		}

		if !pipecli.breaker.AllowsRequest() {
			client.parent.stopCmdTrace(req)
			canRetry := client.parent.waitAndRetryOperation(req, CircuitBreakerOpenRetryReason)
			if canRetry {
				// If the retry orchestrator is going to attempt to retry this then we don't want to return an
				// error to the user.
				continue
			}

			req.tryCallback(nil, ErrCircuitBreakerOpen)

			// Keep looping, there may be more requests and those might be able to send
			continue
		}

		req.onCompletion = func(err error) {
			if pipecli.breaker.CompletionCallback(err) {
				pipecli.breaker.MarkSuccessful()
			} else {
				pipecli.breaker.MarkFailure()
			}
		}

		err := client.SendRequest(req)
		if err != nil {
			logDebugf("Pipeline client `%s/%p` encountered a socket write error: %v", pipecli.address, pipecli, err)

			if err != io.EOF {
				// If we errored the write, and the client was not already closed,
				// lets go ahead and close it.  This will trigger the shutdown
				// logic via the client watcher above.  If the socket error was EOF
				// we already did shut down, and the watcher should already be
				// cleaning up.
				err := client.Close()
				if err != nil {
					logErrorf("Pipeline client `%s/%p` failed to shut down errored client socket (%s)", pipecli.address, pipecli, err)
				}
			}

			// These are special case that can arise
			// ErrCollectionsUnsupported should never be seen here but theoretically can be.
			// ErrCancelled can occur if the request was cancelled during dispatch.
			// In either case we should respond with the relevant error and neither should be retried.
			if err == ErrCollectionsUnsupported || err == ErrCancelled {
				req.tryCallback(nil, err)
				break
			}

			// We can attempt to retry ops of this type if the socket fails on write.
			canRetry := client.parent.waitAndRetryOperation(req, SocketNotAvailableRetryReason)
			if canRetry {
				// If we've successfully retried this then don't return an error to the caller, just refresh
				// the client and pick the request up again later.
				break
			}

			// We need to alert the caller that there was a network error
			req.tryCallback(nil, ErrNetwork)

			// Stop looping
			break
		}
	}

	logDebugf("Pipeline client `%s/%p` waiting for client shutdown", pipecli.address, pipecli)

	// We must wait for the close wait goroutine to die as well before we can continue.
	<-killSig

	logDebugf("Pipeline client `%s/%p` received client shutdown notification", pipecli.address, pipecli)
}

func (pipecli *memdPipelineClient) Run() {
	for {
		logDebugf("Pipeline Client `%s/%p` preparing for new client loop", pipecli.address, pipecli)

		pipecli.lock.Lock()
		pipeline := pipecli.parent
		pipecli.lock.Unlock()

		if pipeline == nil {
			// If our pipeline is nil, it indicates that we need to shut down.
			logDebugf("Pipeline Client `%s/%p` is shutting down", pipecli.address, pipecli)
			break
		}

		pipecli.breaker.Reset()

		logDebugf("Pipeline Client `%s/%p` retrieving new client connection for parent %p", pipecli.address, pipecli, pipeline)
		client, err := pipeline.getClientFn()
		if err != nil {
			continue
		}

		// Runs until the connection has died (for whatever reason)
		logDebugf("Pipeline Client `%s/%p` starting new client loop for %p", pipecli.address, pipecli, client)
		pipecli.ioLoop(client)
	}

	// Lets notify anyone who is watching that we are now shut down
	close(pipecli.closedSig)

	logDebugf("Pipeline Client `%s/%p` is now exiting", pipecli.address, pipecli)
}

// Close will close this pipeline client.  Note that this method will not wait for
// everything to be cleaned up before returning.
func (pipecli *memdPipelineClient) Close() error {
	logDebugf("Pipeline Client `%s/%p` received close request", pipecli.address, pipecli)

	// To shut down the client, we remove our reference to the parent. This
	// causes our ioLoop see that we are being shut down and perform cleanup
	// before exiting.
	pipecli.lock.Lock()
	pipecli.parent = nil
	activeConsumer := pipecli.consumer
	pipecli.consumer = nil
	pipecli.lock.Unlock()

	// If we have an active consumer, we need to close it to cause the running
	// ioLoop to unpause and pick up that our parent has been removed.  Note
	// that in some cases, we might not have an active consumer. This means
	// that the ioLoop is about to try and fetch one, finding the missing
	// parent in doing so.
	if activeConsumer != nil {
		activeConsumer.Close()
	}

	// Lets wait till the ioLoop has shut everything down before returning.
	<-pipecli.closedSig

	return nil
}

func (pipecli *memdPipelineClient) sendCanary() {
	errChan := make(chan error)
	handler := func(resp *memdQResponse, req *memdQRequest, err error) {
		errChan <- err
	}

	req := &memdQRequest{
		memdPacket: memdPacket{
			Magic:    reqMagic,
			Opcode:   cmdNoop,
			Datatype: 0,
			Cas:      0,
			Key:      nil,
			Value:    nil,
		},
		Callback:      handler,
		RetryStrategy: NewFailFastRetryStrategy(),
	}

	logDebugf("Sending NOOP request for %p/%s", pipecli, pipecli.address)
	err := pipecli.client.SendRequest(req)
	if err != nil {
		pipecli.breaker.MarkFailure()
	}

	timer := AcquireTimer(pipecli.breaker.CanaryTimeout())
	select {
	case <-timer.C:
		if !req.Cancel() {
			err := <-errChan
			if err == nil {
				logDebugf("NOOP request successful for %p/%s", pipecli, pipecli.address)
				pipecli.breaker.MarkSuccessful()
			} else {
				logDebugf("NOOP request failed for %p/%s", pipecli, pipecli.address)
				pipecli.breaker.MarkFailure()
			}
		}
		pipecli.breaker.MarkFailure()
	case err := <-errChan:
		if err == nil {
			pipecli.breaker.MarkSuccessful()
		} else {
			pipecli.breaker.MarkFailure()
		}
	}
}
