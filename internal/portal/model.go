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

import "time"

// InventoryResponse is the portal's point-in-time Sandbox inventory.
type InventoryResponse struct {
	GeneratedAt   time.Time       `json:"generatedAt"`
	Context       string          `json:"context"`
	Namespace     string          `json:"namespace,omitempty"`
	AllNamespaces bool            `json:"allNamespaces"`
	Warnings      []string        `json:"warnings,omitempty"`
	Sandboxes     []SandboxRecord `json:"sandboxes"`
}

// SandboxRecord is the portal projection of one Sandbox.
type SandboxRecord struct {
	Namespace        string             `json:"namespace"`
	Name             string             `json:"name"`
	ClaimName        string             `json:"claimName,omitempty"`
	CreatedAt        time.Time          `json:"createdAt"`
	OperatingMode    string             `json:"operatingMode"`
	Ready            ConditionSummary   `json:"ready"`
	User             string             `json:"user,omitempty"`
	Agent            string             `json:"agent,omitempty"`
	Containers       []ContainerRecord  `json:"containers"`
	RuntimeClass     string             `json:"runtimeClass,omitempty"`
	Lifecycle        LifecycleSummary   `json:"lifecycle"`
	PodIPs           []string           `json:"podIPs"`
	ServiceFQDN      string             `json:"serviceFQDN,omitempty"`
	Connections      []ConnectionRecord `json:"connections"`
	TerminalEligible bool               `json:"terminalEligible"`
}

// ConditionSummary contains the user-facing fields of a Kubernetes condition.
type ConditionSummary struct {
	Status         string     `json:"status"`
	Reason         string     `json:"reason,omitempty"`
	Message        string     `json:"message,omitempty"`
	TransitionTime *time.Time `json:"transitionTime,omitempty"`
}

// PortRecord describes a declared container port.
type PortRecord struct {
	Name     string `json:"name,omitempty"`
	Protocol string `json:"protocol"`
	Port     int32  `json:"port"`
}

// ContainerRecord describes a Sandbox workload container.
type ContainerRecord struct {
	Name  string       `json:"name"`
	Image string       `json:"image"`
	Ports []PortRecord `json:"ports"`
}

// LifecycleSummary describes the authoritative lifecycle policy for a Sandbox.
type LifecycleSummary struct {
	Source                  string     `json:"source,omitempty"`
	ExpiresAt               *time.Time `json:"expiresAt,omitempty"`
	TTLSecondsAfterFinished *int32     `json:"ttlSecondsAfterFinished,omitempty"`
	RetentionDeadline       *time.Time `json:"retentionDeadline,omitempty"`
	Expired                 bool       `json:"expired"`
}

// ConnectionRecord describes a copyable or navigable Sandbox connection target.
type ConnectionRecord struct {
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Value     string `json:"value"`
	URL       string `json:"url,omitempty"`
	Container string `json:"container,omitempty"`
	Port      int32  `json:"port,omitempty"`
}

// ClientControl is a bounded browser-to-terminal protocol message.
type ClientControl struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// ServerControl reports terminal lifecycle events to the browser.
type ServerControl struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}
