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
(() => {
  "use strict";

  const elements = {
    rows: document.querySelector("#sandbox-rows"),
    search: document.querySelector("#filter-search"),
    statusFilter: document.querySelector("#filter-status"),
    refresh: document.querySelector("#refresh-inventory"),
    inventorySummary: document.querySelector("#inventory-summary"),
    inventoryStatus: document.querySelector("#inventory-status"),
    scopeSummary: document.querySelector("#scope-summary"),
    lastRefresh: document.querySelector("#last-refresh"),
    warning: document.querySelector("#inventory-warning"),
    error: document.querySelector("#inventory-error"),
    empty: document.querySelector("#empty-state"),
    summaryTotal: document.querySelector("#summary-total"),
    summaryReady: document.querySelector("#summary-ready"),
    summaryUnready: document.querySelector("#summary-unready"),
    summaryExpiring: document.querySelector("#summary-expiring"),
    drawer: document.querySelector("#terminal-drawer"),
    terminal: document.querySelector("#terminal"),
    terminalIdentity: document.querySelector("#terminal-identity"),
    terminalStatus: document.querySelector("#terminal-status"),
    terminalContainer: document.querySelector("#terminal-container"),
    reconnect: document.querySelector("#reconnect-terminal"),
    closeTerminal: document.querySelector("#close-terminal"),
  };

  let latestInventory = null;
  let inventoryRequestActive = false;
  let activeRecord = null;
  let activeSocket = null;
  let terminal = null;
  let fitAddon = null;
  let terminalDataHandler = null;
  let terminalResizeHandler = null;
  let terminalReturnFocus = null;

  function createElement(tagName, className, text) {
    const node = document.createElement(tagName);
    if (className) {
      node.className = className;
    }
    if (text !== undefined) {
      node.textContent = text;
    }
    return node;
  }

  function readyState(record) {
    if (record.ready && record.ready.status === "True") {
      return "ready";
    }
    if (record.ready && record.ready.status === "False") {
      return "unready";
    }
    return "unknown";
  }

  function formatRemaining(expiresAt, now) {
    const deadline = Date.parse(expiresAt);
    if (!Number.isFinite(deadline)) {
      return "Unknown";
    }

    const remainingSeconds = Math.ceil((deadline - now) / 1000);
    if (remainingSeconds <= 0) {
      return "Expired";
    }

    const days = Math.floor(remainingSeconds / 86400);
    const hours = Math.floor((remainingSeconds % 86400) / 3600);
    const minutes = Math.floor((remainingSeconds % 3600) / 60);
    const seconds = remainingSeconds % 60;
    if (days > 0) {
      return `${days}d ${hours}h remaining`;
    }
    if (hours > 0) {
      return `${hours}h ${minutes}m remaining`;
    }
    if (minutes > 0) {
      return `${minutes}m ${seconds}s remaining`;
    }
    return `${seconds}s remaining`;
  }

  function updateCountdowns() {
    const now = Date.now();
    document.querySelectorAll("[data-expires-at]").forEach((countdown) => {
      countdown.textContent = formatRemaining(countdown.dataset.expiresAt, now);
    });
  }

  function matchesFilters(record) {
    const query = elements.search.value.trim().toLowerCase();
    const containers = Array.isArray(record.containers) ? record.containers : [];
    const searchText = [
      record.namespace,
      record.name,
      record.claimName,
      record.user,
      record.agent,
      containers.map((container) => `${container.name} ${container.image}`).join(" "),
      record.runtimeClass,
      record.ready && record.ready.reason,
      Array.isArray(record.podIPs) ? record.podIPs.join(" ") : "",
      record.serviceFQDN,
    ].filter(Boolean).join(" ").toLowerCase();
    const selectedStatus = elements.statusFilter.value;
    return (!query || searchText.includes(query))
      && (selectedStatus === "all" || readyState(record) === selectedStatus);
  }

  function appendIdentity(cell, record) {
    cell.append(createElement("strong", "identity", `${record.namespace}/${record.name}`));
    if (record.claimName) {
      cell.append(createElement("span", "cell-detail", `Claim: ${record.claimName}`));
    }
    const created = new Date(record.createdAt);
    const createdLabel = Number.isNaN(created.getTime()) ? "Unknown creation time" : `Created ${created.toLocaleString()}`;
    cell.append(createElement("span", "cell-detail", `${record.operatingMode || "Running"} · ${createdLabel}`));
  }

  function appendReadiness(cell, record) {
    const state = readyState(record);
    const labels = { ready: "Ready", unready: "Not Ready", unknown: "Unknown" };
    const badge = createElement("span", `badge ${state}`, labels[state]);
    const message = record.ready && record.ready.message ? record.ready.message : "";
    if (message) {
      badge.title = message;
    }
    cell.append(badge);

    const reason = record.ready && record.ready.reason ? record.ready.reason : "No reason reported";
    if (message) {
      const details = createElement("details", "condition-details");
      details.append(createElement("summary", "", reason));
      details.append(createElement("p", "condition-message", message));
      cell.append(details);
    } else {
      cell.append(createElement("span", "cell-detail", reason));
    }
    if (record.ready && record.ready.transitionTime) {
      const changedAt = new Date(record.ready.transitionTime);
      if (!Number.isNaN(changedAt.getTime())) {
        cell.append(createElement("span", "cell-detail", `Changed ${changedAt.toLocaleString()}`));
      }
    }
  }

  function appendAssignment(cell, record) {
    const user = record.user || "Unassigned";
    const agent = record.agent || "Unassigned";
    cell.append(createElement("span", "assignment", `User: ${user}`));
    cell.append(createElement("span", "assignment", `Agent: ${agent}`));
  }

  function containerLabel(container) {
    return `${container.name || "Unnamed"}: ${container.image || "Image unspecified"}`;
  }

  function appendImageAndRuntime(cell, record) {
    const containers = Array.isArray(record.containers) ? record.containers : [];
    if (containers.length === 0) {
      cell.append(createElement("span", "muted", "No containers"));
    } else {
      cell.append(createElement("code", "image", containers[0].image || "Image unspecified"));
      cell.append(createElement("span", "cell-detail", `Container: ${containers[0].name}`));
      if (containers.length > 1) {
        const details = createElement("details", "container-details");
        details.append(createElement("summary", "", `${containers.length - 1} more container${containers.length === 2 ? "" : "s"}`));
        const list = createElement("ul", "compact-list");
        containers.slice(1).forEach((container) => {
          const item = createElement("li", "", containerLabel(container));
          const ports = Array.isArray(container.ports) ? container.ports : [];
          if (ports.length > 0) {
            item.title = ports.map((port) => `${port.name || "TCP"} ${port.port}/${port.protocol || "TCP"}`).join(", ");
          }
          list.append(item);
        });
        details.append(list);
        cell.append(details);
      }
    }
    cell.append(createElement("span", "runtime", `Runtime: ${record.runtimeClass || "Cluster default"}`));
  }

  function appendExpiry(cell, record) {
    const lifecycle = record.lifecycle || {};
    if (lifecycle.expiresAt) {
      const deadline = new Date(lifecycle.expiresAt);
      const absolute = createElement("time", "expiry-absolute", Number.isNaN(deadline.getTime()) ? "Unknown expiry" : deadline.toLocaleString());
      absolute.dateTime = lifecycle.expiresAt;
      cell.append(absolute);
      const countdown = createElement("span", lifecycle.expired ? "countdown expired" : "countdown");
      countdown.dataset.expiresAt = lifecycle.expiresAt;
      countdown.textContent = formatRemaining(lifecycle.expiresAt, Date.now());
      cell.append(countdown);
    } else if (lifecycle.ttlSecondsAfterFinished !== undefined && lifecycle.ttlSecondsAfterFinished !== null) {
      cell.append(createElement("span", "expiry-policy", `${lifecycle.ttlSecondsAfterFinished} seconds after finish`));
    } else {
      cell.append(createElement("span", "muted", "No expiry"));
    }

    if (lifecycle.source) {
      cell.title = `Lifecycle source: ${lifecycle.source}`;
    }
    if (lifecycle.retentionDeadline) {
      cell.dataset.retentionDeadline = lifecycle.retentionDeadline;
    }
  }

  async function copyValue(value, button) {
    try {
      await navigator.clipboard.writeText(value);
      const previous = button.textContent;
      button.textContent = "Copied";
      window.setTimeout(() => {
        button.textContent = previous;
      }, 1500);
    } catch (_error) {
      elements.inventoryStatus.textContent = "Copy failed. Select the value and copy it manually.";
    }
  }

  function appendConnections(cell, record) {
    const connections = Array.isArray(record.connections) ? record.connections : [];
    if (connections.length === 0) {
      cell.append(createElement("span", "muted", "None"));
      return;
    }

    const list = createElement("ul", "connection-list");
    connections.forEach((connection) => {
      const item = createElement("li", "");
      if (connection.kind === "router" && connection.url) {
        try {
          const target = new URL(connection.url);
          if (target.protocol !== "http:" && target.protocol !== "https:") {
            return;
          }
          const link = createElement("a", "connection-link", connection.label || `${connection.container}:${connection.port}`);
          link.href = target.href;
          link.target = "_blank";
          link.rel = "noopener noreferrer";
          item.append(link);
        } catch (_error) {
          return;
        }
      } else if ((connection.kind === "podIP" || connection.kind === "serviceFQDN") && connection.value) {
        const button = createElement("button", "copy-button", `${connection.label}: ${connection.value}`);
        button.type = "button";
        button.title = `Copy ${connection.label}`;
        button.setAttribute("aria-label", `Copy ${connection.label} ${connection.value}`);
        button.addEventListener("click", () => copyValue(connection.value, button));
        item.append(button);
      } else {
        return;
      }
      list.append(item);
    });
    if (list.childElementCount === 0) {
      cell.append(createElement("span", "muted", "None"));
    } else {
      cell.append(list);
    }
  }

  function terminalUnavailableReason(record) {
    if (!record.terminalEligible) {
      return "Terminal unavailable because the Sandbox is not Ready, is expiring, or its lifecycle cannot be verified";
    }
    if (!Array.isArray(record.containers) || record.containers.length === 0) {
      return "Terminal unavailable because the Sandbox has no regular containers";
    }
    return "";
  }

  function appendActions(cell, record) {
    const button = createElement("button", "button primary", "Terminal");
    button.type = "button";
    const unavailable = terminalUnavailableReason(record);
    if (unavailable) {
      button.disabled = true;
      button.title = unavailable;
      button.setAttribute("aria-label", unavailable);
    } else {
      button.setAttribute("aria-label", `Open terminal for ${record.namespace}/${record.name}`);
      button.addEventListener("click", () => openTerminal(record));
    }
    cell.append(button);
  }

  function renderRecord(record) {
    const row = document.createElement("tr");
    const appendCell = (renderer) => {
      const cell = document.createElement("td");
      renderer(cell, record);
      row.append(cell);
    };
    appendCell(appendIdentity);
    appendCell(appendReadiness);
    appendCell(appendAssignment);
    appendCell(appendImageAndRuntime);
    appendCell(appendExpiry);
    appendCell(appendConnections);
    appendCell(appendActions);
    return row;
  }

  function renderSummary(records) {
    const now = Date.now();
    const ready = records.filter((record) => readyState(record) === "ready").length;
    const expiring = records.filter((record) => {
      const expiresAt = record.lifecycle && record.lifecycle.expiresAt ? Date.parse(record.lifecycle.expiresAt) : Number.NaN;
      return Number.isFinite(expiresAt) && expiresAt > now && expiresAt - now <= 15 * 60 * 1000;
    }).length;
    elements.summaryTotal.textContent = String(records.length);
    elements.summaryReady.textContent = String(ready);
    elements.summaryUnready.textContent = String(records.length - ready);
    elements.summaryExpiring.textContent = String(expiring);
  }

  function renderDashboard() {
    if (!latestInventory) {
      return;
    }

    const records = Array.isArray(latestInventory.sandboxes) ? latestInventory.sandboxes : [];
    const visibleRecords = records.filter(matchesFilters);
    const fragment = document.createDocumentFragment();
    visibleRecords.forEach((record) => fragment.append(renderRecord(record)));
    elements.rows.replaceChildren(fragment);
    elements.empty.hidden = visibleRecords.length !== 0;
    elements.inventorySummary.textContent = `${records.length} Sandbox${records.length === 1 ? "" : "es"}`;
    elements.inventoryStatus.textContent = visibleRecords.length === records.length
      ? `${records.length} shown`
      : `${visibleRecords.length} of ${records.length} shown`;
    renderSummary(records);
    updateCountdowns();
  }

  function showInventoryScope(inventory) {
    const namespace = inventory.allNamespaces ? "All namespaces" : (inventory.namespace || "default");
    elements.scopeSummary.textContent = `Context: ${inventory.context || "current"} · Namespace: ${namespace}`;
    const generated = new Date(inventory.generatedAt);
    elements.lastRefresh.textContent = Number.isNaN(generated.getTime())
      ? "Refreshed just now"
      : `Refreshed ${generated.toLocaleString()}`;
  }

  async function fetchInventory() {
    if (document.hidden || inventoryRequestActive) {
      return;
    }
    inventoryRequestActive = true;
    elements.refresh.disabled = true;
    elements.error.hidden = true;
    if (!latestInventory) {
      elements.inventoryStatus.textContent = "Loading…";
    }

    try {
      const response = await fetch("/api/v1/sandboxes", {
        cache: "no-store",
        headers: { Accept: "application/json" },
      });
      if (!response.ok) {
        throw new Error("inventory request failed");
      }
      latestInventory = await response.json();
      showInventoryScope(latestInventory);
      const warnings = Array.isArray(latestInventory.warnings) ? latestInventory.warnings : [];
      elements.warning.hidden = warnings.length === 0;
      elements.warning.textContent = warnings.length === 0 ? "" : `Partial data: ${warnings.join(" ")}`;
      renderDashboard();
    } catch (_error) {
      elements.error.textContent = "Sandbox inventory is temporarily unavailable. Refresh to try again.";
      elements.error.hidden = false;
      elements.inventoryStatus.textContent = latestInventory ? "Showing the last successful inventory." : "Inventory unavailable";
    } finally {
      inventoryRequestActive = false;
      elements.refresh.disabled = false;
    }
  }

  function setTerminalStatus(message, state) {
    elements.terminalStatus.textContent = message;
    elements.terminalStatus.className = state ? `terminal-status ${state}` : "terminal-status";
  }

  function disconnectTerminalSocket() {
    const socket = activeSocket;
    activeSocket = null;
    if (!socket) {
      return;
    }
    socket.onopen = null;
    socket.onmessage = null;
    socket.onerror = null;
    socket.onclose = null;
    if (socket.readyState === WebSocket.CONNECTING || socket.readyState === WebSocket.OPEN) {
      socket.close(1000, "Terminal reconnecting or closing");
    }
  }

  function sendTerminalControl(control) {
    if (activeSocket && activeSocket.readyState === WebSocket.OPEN) {
      activeSocket.send(JSON.stringify(control));
    }
  }

  function sendTerminalSize() {
    if (terminal) {
      sendTerminalControl({ type: "resize", cols: terminal.cols, rows: terminal.rows });
    }
  }

  function fitTerminal() {
    if (!elements.drawer.hidden && fitAddon) {
      fitAddon.fit();
      sendTerminalSize();
    }
  }

  function handleTerminalControl(data) {
    let control;
    try {
      control = JSON.parse(data);
    } catch (_error) {
      setTerminalStatus("The terminal server sent an invalid control message.", "error");
      return;
    }
    if (control.type === "exit") {
      setTerminalStatus("Shell exited. Reconnect to start a new session.", "exited");
    } else if (control.type === "error") {
      setTerminalStatus(control.message || "The terminal session ended with an error.", "error");
    }
  }

  function connectTerminal(record, containerName) {
    disconnectTerminalSocket();
    if (!terminal || !record || !containerName) {
      setTerminalStatus("A container is required to open a terminal.", "error");
      return;
    }

    terminal.clear();
    setTerminalStatus(`Connecting to ${containerName}…`, "connecting");
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const terminalPath = `/api/v1/namespaces/${encodeURIComponent(record.namespace)}/sandboxes/${encodeURIComponent(record.name)}/terminal`;
    const socket = new WebSocket(`${protocol}//${window.location.host}${terminalPath}?container=${encodeURIComponent(containerName)}`);
    activeSocket = socket;
    socket.binaryType = "arraybuffer";

    socket.onopen = () => {
      if (activeSocket !== socket) {
        return;
      }
      setTerminalStatus(`Connected to ${record.namespace}/${record.name} · ${containerName}`, "connected");
      fitTerminal();
      terminal.focus();
    };
    socket.onmessage = (event) => {
      if (activeSocket !== socket) {
        return;
      }
      if (event.data instanceof ArrayBuffer) {
        terminal.write(new Uint8Array(event.data));
        return;
      }
      if (typeof event.data === "string") {
        handleTerminalControl(event.data);
      }
    };
    socket.onerror = () => {
      if (activeSocket === socket) {
        setTerminalStatus("Terminal connection error. Reconnect to try again.", "error");
      }
    };
    socket.onclose = () => {
      if (activeSocket === socket) {
        activeSocket = null;
        if (!elements.terminalStatus.classList.contains("error") && !elements.terminalStatus.classList.contains("exited")) {
          setTerminalStatus("Terminal disconnected. Reconnect to start a new session.", "disconnected");
        }
      }
    };
  }

  function closeTerminal() {
    disconnectTerminalSocket();
    if (terminalDataHandler) {
      terminalDataHandler.dispose();
      terminalDataHandler = null;
    }
    if (terminalResizeHandler) {
      terminalResizeHandler.dispose();
      terminalResizeHandler = null;
    }
    window.removeEventListener("resize", fitTerminal);
    if (terminal) {
      terminal.clear();
      terminal.dispose();
      terminal = null;
    }
    fitAddon = null;
    activeRecord = null;
    elements.terminal.replaceChildren();
    elements.terminalContainer.replaceChildren();
    elements.terminalIdentity.textContent = "";
    elements.drawer.hidden = true;
    setTerminalStatus("Disconnected", "disconnected");
    if (terminalReturnFocus && typeof terminalReturnFocus.focus === "function") {
      terminalReturnFocus.focus();
    }
    terminalReturnFocus = null;
  }

  function openTerminal(record) {
    const containers = Array.isArray(record.containers) ? record.containers : [];
    if (containers.length === 0) {
      return;
    }
    const returnFocus = document.activeElement;
    closeTerminal();
    terminalReturnFocus = returnFocus;
    activeRecord = record;
    elements.terminalIdentity.textContent = `${record.namespace}/${record.name}`;
    containers.forEach((container) => {
      const option = createElement("option", "", container.name);
      option.value = container.name;
      elements.terminalContainer.append(option);
    });
    elements.terminalContainer.value = containers[0].name;
    elements.drawer.hidden = false;

    terminal = new window.Terminal({
      convertEol: true,
      cursorBlink: true,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
      fontSize: 14,
      scrollback: 5000,
      theme: { background: "#0b1016", foreground: "#e6edf3" },
    });
    fitAddon = new window.FitAddon.FitAddon();
    terminal.loadAddon(fitAddon);
    terminal.open(elements.terminal);
    terminalDataHandler = terminal.onData((data) => {
      sendTerminalControl({ type: "input", data });
    });
    terminalResizeHandler = terminal.onResize(({ cols, rows }) => {
      sendTerminalControl({ type: "resize", cols, rows });
    });
    window.addEventListener("resize", fitTerminal);
    window.requestAnimationFrame(() => {
      fitTerminal();
      terminal.focus();
    });
    connectTerminal(record, containers[0].name);
  }

  elements.search.addEventListener("input", renderDashboard);
  elements.statusFilter.addEventListener("change", renderDashboard);
  elements.refresh.addEventListener("click", fetchInventory);
  elements.closeTerminal.addEventListener("click", closeTerminal);
  elements.reconnect.addEventListener("click", () => {
    if (activeRecord) {
      connectTerminal(activeRecord, elements.terminalContainer.value);
    }
  });
  elements.terminalContainer.addEventListener("change", () => {
    if (activeRecord) {
      connectTerminal(activeRecord, elements.terminalContainer.value);
    }
  });
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) {
      fetchInventory();
    }
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && !elements.drawer.hidden) {
      closeTerminal();
    }
  });
  window.addEventListener("beforeunload", closeTerminal);

  fetchInventory();
  window.setInterval(fetchInventory, 5000);
  window.setInterval(updateCountdowns, 1000);
})();
