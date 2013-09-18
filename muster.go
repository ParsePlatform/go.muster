// Package muster provides a framework for writing libraries that internally
// batch operations.
//
// It will be useful to you if you're building an API that benefits from
// performing work in batches for whatever reason. Batching is triggered based
// on a maximum number of items in a batch, and/or based on a timeout for how
// long a batch waits before it is dispatched. For example if you're willing to
// wait for a maximum of a 1m duration, you can just set BatchTimeout and keep
// adding things. Or if you want batches of 50 just set MaxBatchSize and it
// will only fire when the batch is filled. For best results set both.
//
// It would be in your best interest to use this library in a hidden fashion in
// order to avoid unnecessary coupling. You will typically achieve this by
// ensuring your implementation of muster.Batch and the use of muster.Client
// are private.
package muster

import (
	"errors"
	"sync"
	"time"
)

var errZeroBoth = errors.New(
	"muster: MaxBatchSize and BatchTimeout can't both be zero",
)

// The notifier is used to indicate to the Client when a batch has finished
// processing.
type Notifier interface {
	// Calling Done will indicate the batch has finished processing.
	Done()
}

// Represents a single batch where items will be added and Fire will be called
// exactly once. The Batch does not need to be safe for concurrent access;
// synchronization will be handled by the Client.
type Batch interface {
	// This should add the given single item to the Batch. This is the "other
	// end" of the Client.Work channel where your application will send items.
	Add(item interface{})

	// Fire off the Batch. It should call Notifier.Done() when it has finished
	// processing the Batch.
	Fire(notifier Notifier)
}

// The Client manages the background process that makes, populates & fires
// Batches.
type Client struct {
	// Maximum number of items in a batch. If this is zero batches will only be
	// dispatched upon hitting the BatchTimeout. It is an error for both this and
	// the BatchTimeout to be zero.
	MaxBatchSize int

	// Duration after which to send a pending batch. If this is zero batches will
	// only be dispatched upon hitting the MaxBatchSize. It is an error for both
	// this and the MaxBatchSize to be zero.
	BatchTimeout time.Duration

	// This function should create a new empty Batch on each invocation.
	BatchMaker func() Batch

	// Capacity of work channel. If this is zero, the Work channel will be
	// blocking.
	PendingWorkCapacity int

	// Once this Client has been started, send work items here to add to batch.
	Work chan interface{}

	workGroup sync.WaitGroup
}

// Start the background worker goroutines and get ready for accepting requests.
func (c *Client) Start() error {
	if int64(c.BatchTimeout) == 0 && c.MaxBatchSize == 0 {
		return errZeroBoth
	}

	c.Work = make(chan interface{}, c.PendingWorkCapacity)
	c.workGroup.Add(1) // this is the worker itself
	go c.worker()
	return nil
}

// Gracefully stop and return once all processing has finished.
func (c *Client) Stop() error {
	close(c.Work)
	c.workGroup.Wait()
	return nil
}

// Background process.
func (c *Client) worker() {
	defer c.workGroup.Done()
	var batch = c.BatchMaker()
	var count int
	var batchTimeout <-chan time.Time
	send := func() {
		c.workGroup.Add(1)
		go batch.Fire(&c.workGroup)
		batch = c.BatchMaker()
		count = 0
		batchTimeout = nil
	}
	recv := func(item interface{}, open bool) bool {
		if !open {
			if count != 0 {
				send()
			}
			return true
		}
		batch.Add(item)
		count++
		if c.MaxBatchSize != 0 && count >= c.MaxBatchSize {
			send()
		} else if int64(c.BatchTimeout) != 0 && batchTimeout == nil {
			batchTimeout = time.After(c.BatchTimeout)
		}
		return false
	}
	for {
		// We use two selects in order to first prefer draining the work queue.
		select {
		case item, open := <-c.Work:
			if recv(item, open) {
				return
			}
		default:
			select {
			case item, open := <-c.Work:
				if recv(item, open) {
					return
				}
			case <-batchTimeout:
				send()
			}
		}
	}
}
