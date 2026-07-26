const sections = {
  insights: ["Insights", "Usage, model selection, and estimated savings over time."],
  general: ["General", "Server, routing, and classifier behavior."],
  providers: ["Providers", "Credentials and endpoints for each backend."],
  models: ["Models", "Catalog capabilities, prices, and routing scores."],
  yaml: ["Advanced YAML", "Edit the complete configuration source."],
};

let state = null;
let activeSection = "general";
let dirty = false;
let insightsReport = null;

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

function setValue(selector, value) {
  const element = $(selector);
  if (element) element.value = value ?? "";
}

function setChecked(selector, value) {
  const element = $(selector);
  if (element) element.checked = Boolean(value);
}

function numberValue(selector) {
  return Number($(selector).value);
}

function escapeHTML(value) {
  return String(value ?? "").replace(/[&<>"]/g, character => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"})[character]);
}

async function loadState() {
  try {
    const response = await fetch("/api/state", {cache: "no-store"});
    if (!response.ok) throw new Error(`Settings returned ${response.status}`);
    state = await response.json();
    renderState();
  } catch (error) {
    showNotice(`Could not load settings: ${error.message}`, false);
    updateStatus({running: false, config_valid: false, last_error: error.message});
  }
}

function renderState() {
  const doc = state.document;
  setValue("#server-address", doc.server_addr);
  setValue("#weight-quality", doc.weights.quality);
  setValue("#weight-cost", doc.weights.cost);
  setValue("#weight-speed", doc.weights.speed);
  setChecked("#session-sticky", doc.routing.session_sticky);
  setValue("#session-ttl", doc.routing.session_ttl);
  setValue("#max-attempts", doc.routing.max_attempts);
  setChecked("#cache-aware", doc.routing.cache_aware);
  setChecked("#classifier-enabled", doc.classifier_enabled);
  setValue("#classifier-model", doc.classifier.model);
  setValue("#classifier-tokens", doc.classifier.max_tokens || 256);
  setValue("#classifier-timeout", doc.classifier.timeout || "10s");
  toggleClassifier();

  renderProviders(doc.providers || []);
  renderModels(doc.catalog || []);
  setValue("#yaml-editor", state.yaml);
  $("#config-path").textContent = state.router.config_path || "~/.octopus/config.yaml";
  $("#file-state").textContent = state.exists ? "Saved configuration" : "New configuration";
  $("#app-version").textContent = state.version === "dev" ? "Development build" : `Version ${state.version}`;
  updateStatus(state.router);
  if (state.load_error) showNotice(state.load_error, false);
  dirty = false;
}

function updateStatus(router) {
  const container = $("#runtime-status");
  container.classList.remove("is-running", "is-error");
  let text = "Router is not running";
  if (router?.running) {
    container.classList.add("is-running");
    text = `Router running at ${router.address}`;
  } else if (router?.last_error) {
    container.classList.add("is-error");
    text = router.last_error;
  }
  $(".status-copy", container).textContent = text;
}

async function loadInsights() {
  const days = Number($("#insights-range").value);
  $("#file-state").textContent = `Last ${days} days`;
  try {
    const response = await fetch(`/api/insights?days=${days}`, {cache: "no-store"});
    const report = await response.json();
    if (!response.ok) throw new Error(report.error || `Insights returned ${response.status}`);
    insightsReport = report;
    renderInsights(report);
  } catch (error) {
    showNotice(`Could not load insights: ${error.message}`, false);
  }
}

function renderInsights(report) {
  const summary = report.summary || {};
  const hasUsage = Number(summary.requests) > 0;
  $("#insights-empty").classList.toggle("is-hidden", hasUsage);
  $("#insights-content").classList.toggle("is-hidden", !hasUsage);
  if (!hasUsage) return;

  const unpricedRequests = Number(summary.requests || 0) - Number(summary.priced_requests || 0);
  const pricingNote = $("#insights-pricing-note");
  pricingNote.classList.toggle("is-hidden", unpricedRequests <= 0);
  pricingNote.textContent = unpricedRequests > 0 ? `${formatInteger(unpricedRequests)} requests used models without non-zero catalog pricing; their savings may be understated.` : "";

  $("#insight-net-savings").textContent = formatMoney(summary.net_savings_usd);
  $("#insight-net-savings").classList.toggle("is-negative", Number(summary.net_savings_usd) < 0);
  $("#insight-savings-percent").textContent = `${formatPercent(summary.savings_percent)} of baseline`;
  $("#insight-actual-cost").textContent = formatMoney(summary.actual_cost_usd);
  $("#insight-baseline-cost").textContent = formatMoney(summary.baseline_cost_usd);
  $("#insight-requests").textContent = formatInteger(summary.requests);
  const tokens = Number(summary.input_tokens || 0) + Number(summary.output_tokens || 0) +
    Number(summary.cache_creation_tokens || 0) + Number(summary.cache_read_tokens || 0);
  $("#insight-token-count").textContent = `${formatCompact(tokens)} tokens`;
  $("#insight-routing-savings").textContent = formatMoney(summary.routing_savings_usd);
  $("#insight-cache-savings").textContent = formatMoney(summary.cache_savings_usd);
  $("#insight-classifier-cost").textContent = formatMoney(-Number(summary.classifier_overhead_usd || 0));
  $("#insight-cache-hit").textContent = formatPercent(summary.cache_hit_percent);
  $("#insights-methodology").textContent = report.methodology || "";
  renderInsightsChart(report.days || []);
  renderInsightModels(report.models || []);
  if (report.last_error) showNotice(report.last_error, false);
}

function formatMoney(value) {
  const number = Number(value || 0);
  const digits = Math.abs(number) > 0 && Math.abs(number) < 0.01 ? 4 : 2;
  return new Intl.NumberFormat(undefined, {style: "currency", currency: "USD", minimumFractionDigits: digits, maximumFractionDigits: digits}).format(number);
}

function formatPercent(value) {
  return `${new Intl.NumberFormat(undefined, {maximumFractionDigits: 1}).format(Number(value || 0))}%`;
}

function formatInteger(value) {
  return new Intl.NumberFormat().format(Number(value || 0));
}

function formatCompact(value) {
  return new Intl.NumberFormat(undefined, {notation: "compact", maximumFractionDigits: 1}).format(Number(value || 0));
}

function renderInsightModels(models) {
  const body = $("#insights-models");
  body.replaceChildren();
  models.forEach(model => {
    const row = document.createElement("tr");
    const cells = [
      model.model,
      formatInteger(model.requests),
      formatCompact(Number(model.input_tokens || 0) + Number(model.output_tokens || 0)),
      formatMoney(model.actual_cost_usd),
    ];
    cells.forEach((value, index) => {
      const cell = document.createElement(index === 0 ? "th" : "td");
      if (index === 0) cell.scope = "row";
      cell.textContent = value;
      row.append(cell);
    });
    body.append(row);
  });
}

function renderInsightsChart(days) {
  const container = $("#insights-chart");
  container.replaceChildren();
  if (!days.length) return;
  const savings = [];
  const actual = [];
  let cumulativeSavings = 0;
  let cumulativeActual = 0;
  days.forEach(day => {
    cumulativeSavings += Number(day.net_savings_usd || 0);
    cumulativeActual += Number(day.actual_cost_usd || 0);
    savings.push(cumulativeSavings);
    actual.push(cumulativeActual);
  });
  const width = 760;
  const height = 220;
  const padding = 18;
  const values = [0, ...savings, ...actual];
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min || 1;
  const x = index => padding + index / Math.max(days.length - 1, 1) * (width - padding * 2);
  const y = value => padding + (max - value) / span * (height - padding * 2);
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", `Cumulative net savings ${formatMoney(cumulativeSavings)} and actual cost ${formatMoney(cumulativeActual)}`);

  for (let step = 0; step <= 4; step++) {
    const line = document.createElementNS(svg.namespaceURI, "line");
    const lineY = padding + step / 4 * (height - padding * 2);
    line.setAttribute("x1", padding);
    line.setAttribute("x2", width - padding);
    line.setAttribute("y1", lineY);
    line.setAttribute("y2", lineY);
    line.setAttribute("class", "chart-grid-line");
    svg.append(line);
  }
  if (min < 0) {
    const zero = document.createElementNS(svg.namespaceURI, "line");
    zero.setAttribute("x1", padding);
    zero.setAttribute("x2", width - padding);
    zero.setAttribute("y1", y(0));
    zero.setAttribute("y2", y(0));
    zero.setAttribute("class", "chart-zero-line");
    svg.append(zero);
  }
  [[actual, "chart-cost-line"], [savings, "chart-savings-line"]].forEach(([series, className]) => {
    const path = document.createElementNS(svg.namespaceURI, "path");
    const points = series.map((value, index) => `${index === 0 ? "M" : "L"}${x(index).toFixed(2)},${y(value).toFixed(2)}`).join(" ");
    path.setAttribute("d", points);
    path.setAttribute("class", className);
    svg.append(path);
  });
  container.append(svg);
  $("#chart-start-date").textContent = formatDate(days[0].date);
  $("#chart-end-date").textContent = formatDate(days[days.length - 1].date);
}

function formatDate(value) {
  const date = new Date(`${value}T00:00:00`);
  return new Intl.DateTimeFormat(undefined, {month: "short", day: "numeric"}).format(date);
}

function renderProviders(providers) {
  const list = $("#providers-list");
  list.replaceChildren();
  providers.forEach(provider => addProvider(provider, false));
  $("#providers-empty").classList.toggle("is-hidden", providers.length !== 0);
}

function addProvider(provider = {}, markDirty = true) {
  const fragment = $("#provider-template").content.cloneNode(true);
  const item = $(".provider-item", fragment);
  const values = {
    name: provider.name || "",
    kind: provider.kind || provider.name || "anthropic",
    api_key_env: provider.api_key_env || "",
    api_key: provider.api_key || "",
    base_url: provider.base_url || "",
  };
  Object.entries(values).forEach(([field, value]) => {
    const input = $(`[data-field="${field}"]`, item);
    if (input) input.value = value;
  });
  $(".item-title", item).textContent = values.name || "New provider";
  $("#providers-list").append(fragment);
  $("#providers-empty").classList.add("is-hidden");
  if (markDirty) dirty = true;
}

function renderModels(models) {
  const list = $("#models-list");
  list.replaceChildren();
  models.forEach(model => addModel(model, false));
  $("#models-empty").classList.toggle("is-hidden", models.length !== 0);
}

function addModel(model = {}, markDirty = true) {
  const fragment = $("#model-template").content.cloneNode(true);
  const item = $(".model-item", fragment);
  const scalarValues = {
    id: model.id || "",
    quality: model.quality ?? 0.7,
    speed: model.speed ?? 0.7,
    cost_per_mtok_in: model.cost_per_mtok_in ?? 0,
    cost_per_mtok_out: model.cost_per_mtok_out ?? 0,
    max_context: model.caps?.max_context ?? 200000,
    // Zero means unconstrained, so show it as an empty field with the "No limit"
    // placeholder rather than a bare 0. collectDocument maps it back to 0.
    max_output_tokens: model.caps?.max_output_tokens || "",
  };
  Object.entries(scalarValues).forEach(([field, value]) => {
    $(`[data-field="${field}"]`, item).value = value;
  });
  ["tools", "vision", "reasoning"].forEach(field => {
    $(`[data-field="${field}"]`, item).checked = Boolean(model.caps?.[field]);
  });
  $(".item-title", item).textContent = scalarValues.id || "New model";
  $("#models-list").append(fragment);
  $("#models-empty").classList.add("is-hidden");
  if (markDirty) dirty = true;
}

function collectDocument() {
  const providers = $$(".provider-item").map(item => ({
    name: $('[data-field="name"]', item).value.trim(),
    kind: $('[data-field="kind"]', item).value,
    api_key_env: $('[data-field="api_key_env"]', item).value.trim(),
    api_key: $('[data-field="api_key"]', item).value,
    base_url: $('[data-field="base_url"]', item).value.trim(),
  }));
  const catalog = $$(".model-item").map(item => ({
    id: $('[data-field="id"]', item).value.trim(),
    quality: Number($('[data-field="quality"]', item).value),
    speed: Number($('[data-field="speed"]', item).value),
    cost_per_mtok_in: Number($('[data-field="cost_per_mtok_in"]', item).value),
    cost_per_mtok_out: Number($('[data-field="cost_per_mtok_out"]', item).value),
    caps: {
      tools: $('[data-field="tools"]', item).checked,
      vision: $('[data-field="vision"]', item).checked,
      reasoning: $('[data-field="reasoning"]', item).checked,
      max_context: Number($('[data-field="max_context"]', item).value),
      max_output_tokens: Number($('[data-field="max_output_tokens"]', item).value || 0),
    },
  }));
  return {
    server_addr: $("#server-address").value.trim(),
    classifier_enabled: $("#classifier-enabled").checked,
    classifier: {
      model: $("#classifier-model").value.trim(),
      max_tokens: numberValue("#classifier-tokens"),
      timeout: $("#classifier-timeout").value.trim(),
    },
    weights: {quality: numberValue("#weight-quality"), cost: numberValue("#weight-cost"), speed: numberValue("#weight-speed")},
    routing: {session_sticky: $("#session-sticky").checked, session_ttl: $("#session-ttl").value.trim(), cache_aware: $("#cache-aware").checked, max_attempts: Number($("#max-attempts").value.trim() || 3)},
    providers,
    catalog,
  };
}

async function save() {
  const form = $("#settings-form");
  if (activeSection !== "yaml" && !form.reportValidity()) return;
  const isYAML = activeSection === "yaml";
  const body = isYAML ? {yaml: $("#yaml-editor").value} : collectDocument();
  const button = $("#save-button");
  button.disabled = true;
  button.textContent = "Saving…";
  hideNotice();
  try {
    const response = await fetch(isYAML ? "/api/yaml" : "/api/structured", {
      method: "POST",
      headers: {"Content-Type": "application/json", "X-Octopus-Settings": "1"},
      body: JSON.stringify(body),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || `Save returned ${response.status}`);
    state = result;
    renderState();
    showNotice(result.load_error || "Configuration saved and router reloaded.", !result.load_error);
  } catch (error) {
    showNotice(error.message, false);
  } finally {
    button.disabled = false;
    button.textContent = "Save & Reload";
  }
}

function showSection(name) {
  activeSection = name;
  $$(".nav-item").forEach(button => {
    const active = button.dataset.section === name;
    button.classList.toggle("is-active", active);
    if (active) button.setAttribute("aria-current", "page"); else button.removeAttribute("aria-current");
  });
  $$(".settings-page").forEach(page => page.classList.toggle("is-active", page.dataset.page === name));
  $("#page-title").textContent = sections[name][0];
  $("#page-description").textContent = sections[name][1];
  const insightsActive = name === "insights";
  $(".save-bar").classList.toggle("is-hidden", insightsActive);
  if (!insightsActive) {
    $("#file-state").textContent = state?.exists ? "Saved configuration" : "New configuration";
    $("#save-hint").textContent = name === "yaml" ? "Advanced YAML reflects the last saved configuration." : "Changes reload the router immediately.";
  }
  hideNotice();
  if (insightsActive) loadInsights();
}

function toggleClassifier() {
  const enabled = $("#classifier-enabled").checked;
  $$("input", $("#classifier-fields")).forEach(input => {
    input.disabled = !enabled;
    input.required = enabled;
  });
}

function showNotice(message, success) {
  const notice = $("#notice");
  notice.textContent = message;
  notice.classList.remove("is-hidden", "is-success");
  if (success) notice.classList.add("is-success");
}

function hideNotice() {
  $("#notice").classList.add("is-hidden");
}

document.addEventListener("click", event => {
  const nav = event.target.closest(".nav-item");
  if (nav) showSection(nav.dataset.section);
  if (event.target.closest("#add-provider")) addProvider();
  if (event.target.closest("#add-model")) addModel();
  const remove = event.target.closest(".remove-item");
  if (remove) {
    const item = remove.closest(".repeat-item");
    const list = item.parentElement;
    item.remove();
    const empty = list.id === "providers-list" ? $("#providers-empty") : $("#models-empty");
    empty.classList.toggle("is-hidden", list.children.length !== 0);
    dirty = true;
  }
  if (event.target.closest("#save-button")) save();
});

document.addEventListener("input", event => {
  if (event.target.closest("#settings-form") && !event.target.closest('[data-page="insights"]')) dirty = true;
  const item = event.target.closest(".repeat-item");
  if (item && event.target.dataset.field === "name") $(".item-title", item).textContent = event.target.value || "New provider";
  if (item && event.target.dataset.field === "id") $(".item-title", item).textContent = event.target.value || "New model";
});

$("#insights-range").addEventListener("change", loadInsights);

$("#classifier-enabled").addEventListener("change", toggleClassifier);
window.addEventListener("beforeunload", event => {
  if (!dirty) return;
  event.preventDefault();
  event.returnValue = "";
});

loadState();
