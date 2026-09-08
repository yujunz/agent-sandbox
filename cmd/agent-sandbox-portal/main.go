/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Import all Kubernetes client auth plugins so every kubeconfig authentication
	// method available to kubectl is also available to the portal.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/client-go/kubernetes"
	sandboxclientset "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
	claimclientset "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned"
	"sigs.k8s.io/agent-sandbox/internal/portal"
	"sigs.k8s.io/agent-sandbox/internal/version"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	portalProgramName     = "agent-sandbox-portal"
	serverShutdownTimeout = 10 * time.Second
)

func main() {
	loggingOptions := zap.Options{Development: false}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&loggingOptions)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		ctrl.Log.WithName("portal").Error(err, "run Sandbox portal")
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	opts := defaultOptions()
	fs := newFlagSet(&opts, stderr)
	if err := fs.Parse(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fs.Usage()
		}
		return fmt.Errorf("parse flags: %w", err)
	}
	if opts.printVersion {
		_, err := fmt.Fprintln(stdout, version.Print(portalProgramName))
		return err
	}

	cfg, err := resolveConfig(&opts, fs)
	if err != nil {
		return fmt.Errorf("resolve configuration: %w", err)
	}
	cfg.restConfig.UserAgent = fmt.Sprintf("%s/%s", portalProgramName, version.Get().GitVersion)

	sandboxClientset, err := sandboxclientset.NewForConfig(cfg.restConfig)
	if err != nil {
		return fmt.Errorf("create Sandbox client: %w", err)
	}
	claimClientset, err := claimclientset.NewForConfig(cfg.restConfig)
	if err != nil {
		return fmt.Errorf("create SandboxClaim client: %w", err)
	}
	coreClientset, err := kubernetes.NewForConfig(cfg.restConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	log := ctrl.Log.WithName("portal")
	inventory := portal.NewInventory(
		sandboxClientset.AgentsV1beta1(),
		claimClientset.ExtensionsV1beta1(),
		portal.InventoryOptions{
			Context:          cfg.contextName,
			Namespace:        cfg.namespace,
			AllNamespaces:    cfg.allNamespaces,
			UserLabel:        opts.userLabel,
			AgentLabel:       opts.agentLabel,
			RouterURL:        cfg.routerURL,
			RouterPathPrefix: opts.routerPathPrefix,
		},
	)
	resolver := portal.NewTerminalResolver(
		sandboxClientset.AgentsV1beta1(),
		claimClientset.ExtensionsV1beta1(),
		coreClientset.CoreV1(),
		portal.TerminalScope{Namespace: cfg.namespace, AllNamespaces: cfg.allNamespaces},
		time.Now,
	)
	factory := portal.NewSPDYExecutorFactory(cfg.restConfig, coreClientset.CoreV1())
	terminal := portal.NewTerminalHandler(ctx, resolver, factory, log.WithName("terminal"))
	handler, err := portal.NewServer(portal.ServerOptions{Inventory: inventory, Terminal: terminal, Log: log})
	if err != nil {
		return fmt.Errorf("build portal server: %w", err)
	}

	listener, err := net.Listen("tcp", opts.listenAddress)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", opts.listenAddress, err)
	}
	defer listener.Close()

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	shutdownResult := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		defer cancel()
		shutdownResult <- server.Shutdown(shutdownContext)
	}()

	namespaceScope := cfg.namespace
	if cfg.allNamespaces {
		namespaceScope = "all"
	}
	log.Info("Sandbox portal listening",
		"url", "http://"+listener.Addr().String(),
		"context", cfg.contextName,
		"namespaceScope", namespaceScope,
	)
	serveErr := server.Serve(listener)
	if !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve portal: %w", serveErr)
	}
	if shutdownErr := <-shutdownResult; shutdownErr != nil {
		return fmt.Errorf("shut down portal server: %w", shutdownErr)
	}
	return nil
}
