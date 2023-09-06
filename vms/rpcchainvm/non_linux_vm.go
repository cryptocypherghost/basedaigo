// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

//go:build !linux
// +build !linux

package rpcchainvm

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/vms/rpcchainvm/grpcutils"
)

func serve(ctx context.Context, vm block.ChainVM, opts ...grpcutils.ServerOption) error {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGKILL)

	server := newVMServer(vm, opts...)
	go func(ctx context.Context) {
		defer func() {
			server.Stop()
		}()

		select {
		case s := <-signals:
			fmt.Printf("runtime engine: received shutdown signal: %s\n", s)
		case <-ctx.Done():
			fmt.Println("runtime engine: context has been cancelled")
		}
	}(ctx)

	// start RPC Chain VM server
	return startVMServer(ctx, server)
}
