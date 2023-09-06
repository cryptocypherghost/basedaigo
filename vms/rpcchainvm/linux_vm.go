// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

//go:build linux
// +build linux

// ^ SIGUSR1 signal is not available on Windows

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
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)

	server := newVMServer(vm, opts...)
	go func(ctx context.Context) {
		defer func() {
			server.GracefulStop()
			fmt.Println("vm server: graceful termination success")
		}()

		for {
			select {
			case s := <-signals:
				switch s {
				case syscall.SIGINT, syscall.SIGTERM:
					// these signals are ignored because ctrl-c will send SIGINT
					// to all processes and systemd will send SIGTERM to all
					// processes by default during stop.
					fmt.Printf("runtime engine: ignoring signal: %s\n", s)
				case syscall.SIGUSR1:
					fmt.Printf("runtime engine: received shutdown signal: %s\n", s)
					return
				}
			case <-ctx.Done():
				fmt.Println("runtime engine: context has been cancelled")
				return
			}
		}
	}(ctx)

	return startVMServer(ctx, server)
}
