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
const summary = document.querySelector("#inventory-summary");
const status = document.querySelector("#inventory-status");

fetch("/api/v1/sandboxes", { headers: { Accept: "application/json" } })
  .then((response) => {
    if (!response.ok) {
      throw new Error("Inventory request failed");
    }
    return response.json();
  })
  .then((inventory) => {
    const count = inventory.sandboxes.length;
    summary.textContent = `${count} Sandbox${count === 1 ? "" : "es"}`;
    status.textContent = count === 0
      ? "No Sandboxes are visible in the current scope."
      : "Sandbox details are ready to display.";
  })
  .catch(() => {
    summary.textContent = "Inventory unavailable";
    status.textContent = "Sandbox inventory is temporarily unavailable.";
  });
