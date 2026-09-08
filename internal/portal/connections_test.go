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
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestConnectionRecordsExposePodIPsAndServiceFQDN(t *testing.T) {
	sb := readySandbox("box-a", "team-a")
	sb.Status.PodIPs = []string{"", "10.0.0.8", "2001:db8::8"}
	sb.Status.ServiceFQDN = "box-a.team-a.svc.cluster.local"

	got := connectionRecords(sb, nil, "/sandboxes")

	assert.Equal(t, []ConnectionRecord{
		{Kind: "podIP", Label: "Pod IP", Value: "10.0.0.8"},
		{Kind: "podIP", Label: "Pod IP", Value: "2001:db8::8"},
		{Kind: "serviceFQDN", Label: "Service FQDN", Value: "box-a.team-a.svc.cluster.local"},
	}, got)
}

func TestConnectionRecordsEscapeRouterSegments(t *testing.T) {
	base, err := url.Parse("https://router.example/base")
	require.NoError(t, err)
	sb := readySandbox("box-a", "team-a")
	sb.Spec.PodTemplate.Spec.Containers[0].Ports = []corev1.ContainerPort{
		{Name: "web", ContainerPort: 8080},
		{Name: "dns", ContainerPort: 53, Protocol: corev1.ProtocolUDP},
	}

	got := connectionRecords(sb, base, "/sandboxes")

	assert.Contains(t, got, ConnectionRecord{Kind: "router", Label: "web", URL: "https://router.example/base/sandboxes/team-a/box-a/8080/", Container: "workspace", Port: 8080})
	assert.NotContains(t, connectionURLs(got), "https://router.example/base/sandboxes/team-a/box-a/53/")
}

func TestConnectionRecordsOnlyUseDeclaredPositiveTCPPorts(t *testing.T) {
	base, err := url.Parse("https://router.example")
	require.NoError(t, err)
	sb := readySandbox("box-a", "team-a")
	sb.Spec.PodTemplate.Spec.Containers = append(sb.Spec.PodTemplate.Spec.Containers,
		corev1.Container{Name: "empty"},
		corev1.Container{Name: "ports", Ports: []corev1.ContainerPort{
			{Name: "zero", ContainerPort: 0, Protocol: corev1.ProtocolTCP},
			{Name: "dns", ContainerPort: 53, Protocol: corev1.ProtocolUDP},
			{Name: "default-tcp", ContainerPort: 8080},
			{Name: "https", ContainerPort: 8443, Protocol: corev1.ProtocolTCP},
		}},
	)

	got := connectionRecords(sb, base, "/sandboxes")

	assert.Equal(t, []ConnectionRecord{
		{Kind: "router", Label: "default-tcp", URL: "https://router.example/sandboxes/team-a/box-a/8080/", Container: "ports", Port: 8080},
		{Kind: "router", Label: "https", URL: "https://router.example/sandboxes/team-a/box-a/8443/", Container: "ports", Port: 8443},
	}, got)
}

func TestConnectionRecordsCanonicalizeBaseAndPrefixSlashes(t *testing.T) {
	base, err := url.Parse("https://router.example/base/")
	require.NoError(t, err)
	sb := readySandbox("box-a", "team-a")
	sb.Spec.PodTemplate.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "web", ContainerPort: 8080}}

	got := connectionRecords(sb, base, "///sandboxes///")

	require.Len(t, got, 1)
	assert.Equal(t, "https://router.example/base/sandboxes/team-a/box-a/8080/", got[0].URL)
}

func TestConnectionRecordsPreserveEscapedRouterBasePath(t *testing.T) {
	base, err := url.Parse("https://router.example/tenant%2Fblue")
	require.NoError(t, err)
	sb := readySandbox("box-a", "team-a")
	sb.Spec.PodTemplate.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "web", ContainerPort: 8080}}

	got := connectionRecords(sb, base, "/sandboxes")

	require.Len(t, got, 1)
	assert.Equal(t, "https://router.example/tenant%2Fblue/sandboxes/team-a/box-a/8080/", got[0].URL)
}

func connectionURLs(records []ConnectionRecord) []string {
	urls := make([]string, 0, len(records))
	for _, record := range records {
		urls = append(urls, record.URL)
	}
	return urls
}
