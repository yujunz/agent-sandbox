// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package portal

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func connectionRecords(sandbox *sandboxv1beta1.Sandbox, routerURL *url.URL, routerPathPrefix string) []ConnectionRecord {
	records := make([]ConnectionRecord, 0, len(sandbox.Status.PodIPs)+1)
	for _, podIP := range sandbox.Status.PodIPs {
		if podIP == "" {
			continue
		}
		records = append(records, ConnectionRecord{Kind: "podIP", Label: "Pod IP", Value: podIP})
	}
	if sandbox.Status.ServiceFQDN != "" {
		records = append(records, ConnectionRecord{
			Kind:  "serviceFQDN",
			Label: "Service FQDN",
			Value: sandbox.Status.ServiceFQDN,
		})
	}
	if routerURL == nil {
		return records
	}

	for _, container := range sandbox.Spec.PodTemplate.Spec.Containers {
		for _, port := range container.Ports {
			if port.ContainerPort <= 0 || (port.Protocol != "" && port.Protocol != corev1.ProtocolTCP) {
				continue
			}
			link, err := routerLink(routerURL, routerPathPrefix, sandbox.Namespace, sandbox.Name, port.ContainerPort)
			if err != nil {
				continue
			}
			records = append(records, ConnectionRecord{
				Kind:      "router",
				Label:     port.Name,
				URL:       link,
				Container: container.Name,
				Port:      port.ContainerPort,
			})
		}
	}
	return records
}

func routerLink(base *url.URL, prefix, namespace, sandbox string, port int32) (string, error) {
	joined, err := url.JoinPath(base.String(), strings.Trim(prefix, "/"), namespace, sandbox, strconv.FormatInt(int64(port), 10))
	if err != nil {
		return "", fmt.Errorf("build router link: %w", err)
	}
	return strings.TrimSuffix(joined, "/") + "/", nil
}
