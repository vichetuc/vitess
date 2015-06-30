// Copyright 2015, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package worker

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/vt/logutil"
	"github.com/youtube/vitess/go/vt/tabletmanager/tmclient"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/vt/wrangler"
	"golang.org/x/net/context"
)

// Instance encapsulate the execution state of vtworker.
type Instance struct {
	// Default wrangler for all operations.
	// Users can specify their own in RunCommand() e.g. the gRPC server does this.
	wr *wrangler.Wrangler

	// mutex is protecting all the following variables
	// 3 states here:
	// - no job ever ran (or reset was run): currentWorker is nil,
	// currentContext/currentCancelFunc is nil, lastRunError is nil
	// - one worker running: currentWorker is set,
	//   currentContext/currentCancelFunc is set, lastRunError is nil
	// - (at least) one worker already ran, none is running atm:
	//   currentWorker is set, currentContext is nil, lastRunError
	//   has the error returned by the worker.
	currentWorkerMutex  sync.Mutex
	currentWorker       Worker
	currentMemoryLogger *logutil.MemoryLogger
	currentContext      context.Context
	currentCancelFunc   context.CancelFunc
	lastRunError        error

	topoServer             topo.Server
	cell                   string
	lockTimeout            time.Duration
	commandDisplayInterval time.Duration
}

// NewInstance creates a new Instance.
func NewInstance(ts topo.Server, cell string, lockTimeout, commandDisplayInterval time.Duration) *Instance {
	wi := &Instance{topoServer: ts, cell: cell, commandDisplayInterval: commandDisplayInterval}
	// Note: setAndStartWorker() also adds a MemoryLogger for the webserver.
	wi.wr = wi.CreateWrangler(logutil.NewConsoleLogger())
	return wi
}

// CreateWrangler creates a new wrangler using the instance specific configuration.
func (wi *Instance) CreateWrangler(logger logutil.Logger) *wrangler.Wrangler {
	return wrangler.New(logger, wi.topoServer, tmclient.NewTabletManagerClient(), wi.lockTimeout)
}

// setAndStartWorker will set the current worker.
// We always log to both memory logger (for display on the web) and
// console logger (for records / display of command line worker).
func (wi *Instance) setAndStartWorker(wrk Worker, wr *wrangler.Wrangler) (chan struct{}, error) {
	wi.currentWorkerMutex.Lock()
	defer wi.currentWorkerMutex.Unlock()
	if wi.currentWorker != nil {
		return nil, fmt.Errorf("A worker is already in progress: %v", wi.currentWorker)
	}

	wi.currentWorker = wrk
	wi.currentMemoryLogger = logutil.NewMemoryLogger()
	wi.currentContext, wi.currentCancelFunc = context.WithCancel(context.Background())
	wi.lastRunError = nil
	done := make(chan struct{})
	wranglerLogger := wr.Logger()
	if wr == wi.wr {
		// If it's the default wrangler, do not reuse its logger because it may have been set before.
		// Resuing it would result into an endless recursion.
		wranglerLogger = logutil.NewConsoleLogger()
	}
	wr.SetLogger(logutil.NewTeeLogger(wi.currentMemoryLogger, wranglerLogger))

	// one go function runs the worker, changes state when done
	go func() {
		// run will take a long time
		log.Infof("Starting worker...")
		err := wrk.Run(wi.currentContext)

		// it's done, let's save our state
		wi.currentWorkerMutex.Lock()
		wi.currentContext = nil
		wi.currentCancelFunc = nil
		wi.lastRunError = err
		wi.currentWorkerMutex.Unlock()
		close(done)
	}()

	return done, nil
}

// InstallSignalHandlers installs signal handler which exit vtworker gracefully.
func (wi *Instance) InstallSignalHandlers() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigChan
		// we got a signal, notify our modules
		wi.currentWorkerMutex.Lock()
		defer wi.currentWorkerMutex.Unlock()
		if wi.currentCancelFunc != nil {
			wi.currentCancelFunc()
		} else {
			fmt.Printf("Shutting down idle worker after receiving signal: %v", s)
			os.Exit(0)
		}
	}()
}
