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
