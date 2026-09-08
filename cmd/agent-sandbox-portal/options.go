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
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultListenAddress = "127.0.0.1:8080"

type options struct {
	listenAddress    string
	kubeconfig       string
	context          string
	namespace        string
	allNamespaces    bool
	userLabel        string
	agentLabel       string
	routerURL        string
	routerPathPrefix string
	printVersion     bool
}

type resolvedConfig struct {
	restConfig    *rest.Config
	contextName   string
	namespace     string
	allNamespaces bool
	routerURL     *url.URL
}

func defaultOptions() options {
	return options{
		listenAddress:    defaultListenAddress,
		userLabel:        "sandbox.users.io/user",
		agentLabel:       "sandbox.users.io/agent",
		routerPathPrefix: "/sandboxes",
	}
}

func newFlagSet(opts *options, writer io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("agent-sandbox-portal", flag.ContinueOnError)
	fs.SetOutput(writer)
	fs.StringVar(&opts.listenAddress, "listen-address", opts.listenAddress, "HTTP listen address (loopback only)")
	fs.StringVar(&opts.kubeconfig, "kubeconfig", opts.kubeconfig, "Path to the kubeconfig file")
	fs.StringVar(&opts.context, "context", opts.context, "Kubernetes context")
	fs.StringVar(&opts.namespace, "namespace", opts.namespace, "Kubernetes namespace")
	fs.BoolVar(&opts.allNamespaces, "all-namespaces", opts.allNamespaces, "List Sandboxes in all namespaces")
	fs.StringVar(&opts.userLabel, "user-label", opts.userLabel, "Label key used for assigned users")
	fs.StringVar(&opts.agentLabel, "agent-label", opts.agentLabel, "Label key used for assigned agents")
	fs.StringVar(&opts.routerURL, "router-url", opts.routerURL, "Optional sandbox-router base URL")
	fs.StringVar(&opts.routerPathPrefix, "router-path-prefix", opts.routerPathPrefix, "sandbox-router path prefix")
	fs.BoolVar(&opts.printVersion, "version", opts.printVersion, "Print version and exit")
	return fs
}

func validateListenAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid listen port %q", portText)
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("listen address %q is not loopback", address)
		}
	}
	return nil
}

func validateOptions(opts *options, namespaceExplicit bool) error {
	if err := validateListenAddress(opts.listenAddress); err != nil {
		return err
	}
	if opts.allNamespaces && namespaceExplicit {
		return errors.New("--namespace and --all-namespaces are mutually exclusive")
	}
	for name, value := range map[string]string{"--user-label": opts.userLabel, "--agent-label": opts.agentLabel} {
		if len(validation.IsQualifiedName(value)) != 0 {
			return fmt.Errorf("%s is not a valid label key", name)
		}
	}
	if opts.routerPathPrefix != "" && (!strings.HasPrefix(opts.routerPathPrefix, "/") || strings.HasPrefix(opts.routerPathPrefix, "//")) {
		return fmt.Errorf("--router-path-prefix %q must begin with exactly one slash", opts.routerPathPrefix)
	}
	_, err := parseRouterURL(opts.routerURL)
	return err
}

func parseRouterURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid --router-url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("--router-url must be an absolute http or https URL")
	}
	if u.User != nil {
		return nil, errors.New("--router-url must not contain credentials")
	}
	if u.RawQuery != "" {
		return nil, errors.New("--router-url must not contain a query")
	}
	if u.Fragment != "" {
		return nil, errors.New("--router-url must not contain a fragment")
	}
	return u, nil
}

func resolveConfig(opts *options, fs *flag.FlagSet) (*resolvedConfig, error) {
	namespaceExplicit := false
	fs.Visit(func(f *flag.Flag) { namespaceExplicit = namespaceExplicit || f.Name == "namespace" })
	if err := validateOptions(opts, namespaceExplicit); err != nil {
		return nil, err
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.kubeconfig != "" {
		rules.ExplicitPath = opts.kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.context}
	if namespaceExplicit {
		overrides.Context.Namespace = opts.namespace
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	restConfig, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	namespace, _, err := loader.Namespace()
	if err != nil {
		return nil, fmt.Errorf("resolve namespace: %w", err)
	}
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	raw, err := loader.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig contexts: %w", err)
	}
	contextName := opts.context
	if contextName == "" {
		contextName = raw.CurrentContext
	}
	routerURL, err := parseRouterURL(opts.routerURL)
	if err != nil {
		return nil, err
	}
	return &resolvedConfig{restConfig: restConfig, contextName: contextName, namespace: namespace, allNamespaces: opts.allNamespaces, routerURL: routerURL}, nil
}
