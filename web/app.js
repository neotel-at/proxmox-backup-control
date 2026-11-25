"use strict";

const el = id => document.getElementById(id);
const state = { dashboard: null, guests: [], selected: [], edit: "", schedule: "", log: null, username: "", editUser: "" };
const routes = { inventory: "Inventar", plans: "Backuppläne", history: "Verlauf", users: "Benutzer" };
const labels = {
  pending: ["Wartet", ""], queued: ["Eingereiht", "blue"], running: ["Läuft", "blue"],
  submitting: ["Startet", "blue"], success: ["Erfolgreich", "good"], failed: ["Fehler", "bad"],
  uncertain: ["Prüfung erforderlich", "amber"], skipped: ["Übersprungen", ""], cancelled: ["Abgebrochen", ""]
};
const dates = new Intl.DateTimeFormat("de-AT", { dateStyle: "medium", timeStyle: "short" });

function node(tag, text, className) {
  const element = document.createElement(tag);
  if (text != null) element.textContent = text;
  if (className) element.className = className;
  return element;
}
function button(text, handler, className = "quiet") {
  const element = node("button", text, "button " + className);
  element.type = "button";
  element.addEventListener("click", handler);
  return element;
}
function badge(status) {
  const [text, color] = labels[status] || [status, ""];
  return node("span", text, "badge " + color);
}
function message(text = "", error = false) {
  const output = el("message");
  output.textContent = text;
  output.classList.toggle("error", error);
  output.hidden = !text;
}
async function api(path, payload) {
  const options = payload === undefined ? {} : {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-PVE-Request": "1" },
    body: JSON.stringify(payload)
  };
  const response = await fetch("/api" + path, options);
  if (!response.ok) {
    if (response.status === 401 && state.username && path !== "/login") location.reload();
    const body = await response.json().catch(() => null);
    const error = new Error(body?.error || "HTTP " + response.status);
    error.status = response.status;
    error.httpsRequired = response.headers.get("X-PVE-Login-HTTPS") === "required";
    throw error;
  }
  return response.json();
}
async function action(fn) {
  try { await fn(); } catch (error) { message(error.message, true); }
}
function currentPage() {
  const key = location.hash.slice(1);
  return Object.hasOwn(routes, key) ? key : "inventory";
}
function renderRoute() {
  const key = currentPage();
  for (const id of Object.keys(routes)) {
    el(id).hidden = id !== key;
    const link = document.querySelector('nav a[href="#' + id + '"]');
    if (id === key) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
  el("breadcrumb").textContent = routes[key];
  renderDashboard();
  if (key === "users" && state.username) void action(refreshUsers);
}
let dashboardVersion = 0;
async function dashboard() {
  const version = ++dashboardVersion;
  const data = await api("/dashboard");
  if (version !== dashboardVersion) return;
  const plansChanged = JSON.stringify(state.dashboard?.plans) !== JSON.stringify(data.plans);
  state.dashboard = data;
  if (plansChanged && currentPage() === "inventory" && el("unplanned-only").checked) renderInventory();
  el("demo").hidden = !data.demo;
  el("release-version").textContent = "v" + data.version.replace(/(\.\d+)\.(\d+)$/, "$1$2");
  el("webhook-state").textContent = data.webhook ? "Fehler-Webhook aktiv" : "Fehler-Webhook nicht konfiguriert";
  if (!el("cluster").options.length) {
    for (const cluster of data.clusters) {
      el("cluster").add(new Option(cluster.name, cluster.id));
    }
  }
  renderDashboard();
}
function renderDashboard() {
  if (!state.dashboard) return;
  el("queued-count").textContent = state.dashboard.runs.filter(run => run.state === "queued").length;
  if (currentPage() === "plans") renderPlans();
  if (currentPage() === "history") renderRuns();
}

let inventoryVersion = 0;
async function inventory() {
  const version = ++inventoryVersion;
  const id = el("cluster").value;
  state.guests = [];
  el("guest-count").textContent = "–";
  renderInventory();
  void action(targets);
  const guests = (await api("/inventory/" + encodeURIComponent(id))) || [];
  if (version !== inventoryVersion) return;
  state.guests = guests;
  el("guest-count").textContent = guests.length;
  renderInventory();
  await targets();
}
function filteredGuests() {
  const term = el("search").value.trim().toLocaleLowerCase("de");
  const runningOnly = el("running-only").checked;
  const planned = new Set();
  if (el("unplanned-only").checked) {
    const cluster = el("cluster").value;
    for (const plan of state.dashboard?.plans || []) {
      if (plan.cluster !== cluster) continue;
      for (const guest of plan.guests) planned.add(guest.type + ":" + guest.vmid);
    }
  }
  return state.guests.filter(g => (!runningOnly || g.status === "running") && !planned.has(g.type + ":" + g.vmid) &&
    [g.vmid, g.name, g.node, g.type].join(" ").toLocaleLowerCase("de").includes(term));
}
function renderInventory() {
  const guests = filteredGuests();
  const checked = new Set(state.selected);
  const list = el("guests");
  list.replaceChildren();
  if (!guests.length) {
    const row = node("tr");
    const cell = node("td", "Keine passenden Gäste gefunden.", "empty");
    cell.colSpan = 6;
    row.append(cell);
    list.append(row);
  }
  for (const guest of guests) {
    const row = node("tr", null, checked.has(guest.vmid) ? "chosen" : "");
    const control = node("input");
    control.type = "checkbox";
    control.checked = checked.has(guest.vmid);
    control.dataset.vmid = guest.vmid;
    control.setAttribute("aria-label", guest.vmid + " " + guest.name + " auswählen");
    control.addEventListener("change", () => {
      if (control.checked) state.selected.push(guest.vmid);
      else state.selected = state.selected.filter(id => id !== guest.vmid);
      renderInventory();
      list.querySelector('[data-vmid="' + guest.vmid + '"]')?.focus();
      void action(targets);
    });
    const select = node("td");
    select.append(control);
    const status = node("td");
    status.append(node("span", guest.status === "running" ? "Aktiv" : guest.status === "stopped" ? "Gestoppt" : guest.status || "Unbekannt",
      "badge " + (guest.status === "running" ? "good" : "")));
    row.append(select, node("td", guest.vmid), node("td", guest.name || "–", "vm-name"),
      node("td", guest.type === "qemu" ? "VM" : "LXC"), node("td", guest.node), status);
    list.append(row);
  }
  const all = el("select-all");
  const count = guests.filter(g => checked.has(g.vmid)).length;
  all.checked = count > 0 && count === guests.length;
  all.indeterminate = count > 0 && count < guests.length;
  all.disabled = !guests.length;
  renderOrder();
}
function renderOrder() {
  el("selected-count").textContent = state.selected.length;
  el("empty-order").hidden = state.selected.length > 0;
  const list = el("order");
  list.replaceChildren();
  state.selected.forEach((id, index) => {
    const guest = state.guests.find(g => g.vmid === id);
    const item = node("li");
    item.append(node("span", id + (guest?.name ? " · " + guest.name : "")));
    for (const [delta, arrow, label] of [[-1, "↑", "nach oben"], [1, "↓", "nach unten"]]) {
      const move = button(arrow, () => {
        [state.selected[index], state.selected[index + delta]] = [state.selected[index + delta], state.selected[index]];
        renderOrder();
        const controls = list.children[index + delta].querySelectorAll("button");
        const next = controls[delta < 0 ? 0 : 1];
        (next.disabled ? controls[delta < 0 ? 1 : 0] : next).focus();
      }, "quiet mini");
      move.disabled = index + delta < 0 || index + delta >= state.selected.length;
      move.setAttribute("aria-label", id + " " + label);
      item.append(move);
    }
    const remove = button("×", () => {
      state.selected = state.selected.filter(value => value !== id);
      renderInventory();
      void action(targets);
    }, "quiet mini");
    remove.setAttribute("aria-label", id + " entfernen");
    item.append(remove);
    list.append(item);
  });
}
let targetVersion = 0;
let chosenTarget = "";
async function targets() {
  const version = ++targetVersion;
  const select = el("storage");
  const nodes = [...new Set(state.guests.filter(g => state.selected.includes(g.vmid)).map(g => g.node))];
  select.replaceChildren(new Option(nodes.length ? "Ziele werden geladen …" : "Zuerst Gäste auswählen", ""));
  select.disabled = el("save").disabled = true;
  if (!nodes.length) return;
  const query = new URLSearchParams();
  for (const name of nodes) query.append("node", name);
  let choices;
  try {
    choices = (await api("/storage/" + encodeURIComponent(el("cluster").value) + "?" + query)) || [];
  } catch (error) {
    if (version !== targetVersion) return;
    select.replaceChildren(new Option("Ziele konnten nicht geladen werden", ""));
    throw error;
  }
  if (version !== targetVersion) return;
  select.replaceChildren(new Option(choices.length ? "Ziel auswählen" : "Kein gemeinsames Backup-Ziel", ""));
  for (const choice of choices) select.add(new Option(choice.storage + " · " + choice.type + (choice.shared ? "" : " · lokal"), choice.storage));
  select.value = choices.some(choice => choice.storage === chosenTarget) ? chosenTarget : "";
  select.disabled = !choices.length;
  el("save").disabled = !select.value;
}
function resetEditor() {
  targetVersion++;
  state.edit = "";
  state.selected = [];
  chosenTarget = "";
  el("plan-name").value = "";
  el("continue").checked = false;
  el("editor-title").textContent = "Backupplan erstellen";
  el("reset-editor").hidden = true;
  renderInventory();
  void action(targets);
}
function clusterName(id) {
  return state.dashboard.clusters.find(cluster => cluster.id === id)?.name || id;
}
function empty(text) { return node("div", text, "card empty"); }
const weekdayLabels = ["So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"];
let lastPlanView = "";
function renderPlans() {
  const list = el("plan-list");
  const plans = state.dashboard.plans || [];
  const current = JSON.stringify([plans, state.dashboard.schedules, state.dashboard.clusters]);
  if (lastPlanView === current) return;
  lastPlanView = current;
  list.replaceChildren();
  if (!plans.length) list.append(empty("Es sind noch keine Backuppläne gespeichert."));
  for (const plan of plans) {
    const card = node("article", null, "card plan-card");
    const info = node("div");
    info.append(node("h2", plan.name), node("p", clusterName(plan.cluster) + " · " + plan.storage + (plan.target_node ? " auf " + plan.target_node : "") + " · VMIDs " + plan.guests.map(g => g.vmid).join(" → ")));
    const savedSchedule = state.dashboard.schedules?.[plan.id];
    if (savedSchedule) {
      const days = savedSchedule.days.length === 7 ? "Täglich" :
        [1, 2, 3, 4, 5, 6, 0].filter(day => savedSchedule.days.includes(day)).map(day => weekdayLabels[day]).join(", ");
      const next = new Intl.DateTimeFormat("de-AT", {
        dateStyle: "medium", timeStyle: "short", timeZone: savedSchedule.zone
      }).format(new Date(savedSchedule.next));
      info.append(node("p", days + " um " + savedSchedule.clock + " · " + savedSchedule.zone + " · Nächster Termin " + next));
    }
    const actions = node("div", null, "actions");
    actions.append(
      button("Bearbeiten", () => action(async () => {
        if (!state.dashboard.clusters.some(cluster => cluster.id === plan.cluster)) throw new Error("Cluster fehlt in der Konfiguration.");
        resetEditor();
        state.edit = plan.id;
        state.selected = plan.guests.map(g => g.vmid);
        chosenTarget = plan.storage;
        el("cluster").value = plan.cluster;
        el("plan-name").value = plan.name;
        el("continue").checked = plan.continue_on_error;
        el("editor-title").textContent = "Backupplan bearbeiten";
        el("reset-editor").hidden = false;
        location.hash = "inventory";
        await inventory();
      })),
      button("Löschen", () => action(async () => {
        if (!confirm("Backupplan „" + plan.name + "“ löschen? Bereits eingereihte Läufe und der Verlauf bleiben erhalten.")) return;
        await api("/plans/" + plan.id + "/delete", {});
        await dashboard();
        message("Backupplan gelöscht.");
      })),
      button(savedSchedule ? "Terminplan" : "Terminieren", () => openSchedule(plan, savedSchedule)),
      button("Jetzt starten", () => action(async () => {
        if (!confirm("Backupplan „" + plan.name + "“ jetzt einreihen?")) return;
        await enqueue(plan.id, null);
      }), "primary")
    );
    card.append(info, actions);
    list.append(card);
  }
}
async function enqueue(plan, due) {
  await api("/plans/" + plan + "/runs", { due });
  el("schedule-dialog").close();
  location.hash = "history";
  await dashboard();
  message("Backup-Lauf eingereiht.");
}
let historyData = "";
function renderRuns() {
  const runs = state.dashboard.runs || [];
  const list = el("run-list");
  const current = JSON.stringify(runs);
  if (historyData === current) return;
  historyData = current;
  const open = new Set([...list.querySelectorAll("details[open]")].map(item => item.dataset.id));
  el("hold").hidden = !runs.some(run => run.state === "uncertain" && !run.reviewed);
  list.replaceChildren();
  if (!runs.length) list.append(empty("Es sind noch keine Backup-Läufe vorhanden."));
  for (const run of runs) {
    const details = node("details", null, "history-item");
    details.dataset.id = run.id;
    details.open = open.has(run.id);
    const summary = node("summary");
    const date = dates.format(new Date(run.started || run.due));
    summary.append(node("strong", run.plan.name), badge(run.state), node("small", date + " · " + run.plan.storage + (run.recurring ? " · Serie" : "")));
    const body = node("div", null, "run-body");
    let text = clusterName(run.plan.cluster);
    if (run.finished) text += " · Ende " + dates.format(new Date(run.finished));
    if (run.notice) text += " · Fehler-Webhook " + (labels[run.notice.status]?.[0] || run.notice.status);
    if (run.email_notice) text += " · Fehler-E-Mail " + (labels[run.email_notice.status]?.[0] || run.email_notice.status);
    if (run.reviewed) text += " · Warteschlange freigegeben";
    body.append(node("p", text));
    const scroll = node("div", null, "scroll");
    const table = node("table", null, "step-table");
    const head = node("thead");
    const heading = node("tr");
    for (const title of ["VMID", "Gast", "Knoten", "Ergebnis", "Task-Log"]) heading.append(node("th", title));
    head.append(heading);
    const rows = node("tbody");
    run.steps.forEach((step, index) => {
      const row = node("tr");
      const result = node("td", null, "step-error");
      result.append(badge(step.state));
      if (step.error) result.append(node("div", step.error));
      const log = node("td");
      if (step.upid) log.append(button("Log öffnen", () => openLog(run, step, index), "quiet mini"));
      row.append(node("td", step.guest.vmid), node("td", step.guest.name || "–"), node("td", step.node || step.guest.node), result, log);
      rows.append(row);
    });
    table.append(head, rows);
    scroll.append(table);
    body.append(scroll);
    const controls = node("div", null, "actions");
    if (run.state === "queued") controls.append(button("Lauf abbrechen", () => runAction(run, "cancel", "Eingereihten Lauf abbrechen?")));
    if (run.state === "uncertain" && !run.reviewed) controls.append(button("Warteschlange freigeben", () =>
      runAction(run, "review", "Bestätigen Sie, dass Sie den Task in Proxmox geprüft haben und kein Backup dieses Laufs mehr aktiv ist.")));
    if (run.finished && (["success", "failed", "cancelled"].includes(run.state) || run.state === "uncertain" && run.reviewed)) {
      controls.append(button("Aus Verlauf löschen", () =>
        runAction(run, "delete", "Diesen Backup-Lauf samt lokalen Logs endgültig aus dem Verlauf löschen?")));
    }
    if (controls.childElementCount) body.append(controls);
    details.append(summary, body);
    list.append(details);
  }
}
function runAction(run, path, prompt) {
  return action(async () => {
    if (!confirm(prompt)) return;
    await api("/runs/" + run.id + "/" + path, {});
    await dashboard();
    if (path === "delete") message("Verlaufseintrag gelöscht.");
  });
}
function openLog(run, step, index) {
  state.log = { path: "/runs/" + run.id + "/logs/" + index, upid: step.upid, offset: 0, busy: false };
  el("log-title").textContent = "Task-Log · VMID " + step.guest.vmid;
  el("log-info").textContent = step.upid;
  el("log-text").textContent = "";
  el("log-refresh").disabled = false;
  el("log-dialog").showModal();
  void loadLog();
}
async function loadLog() {
  const log = state.log;
  if (!log || log.busy) return;
  log.busy = true;
  el("log-refresh").disabled = true;
  try {
    const page = await api(log.path + "?start=" + log.offset);
    if (state.log !== log) return;
    const lines = page.lines || [];
    if (lines.length) {
      el("log-text").append(document.createTextNode(lines.map(line => line.t).join("\n") + "\n"));
      log.offset += lines.length;
    }
    el("log-info").textContent = [log.upid, log.offset + " Zeilen", page.warning].filter(Boolean).join(" · ");
  } catch (error) {
    if (state.log === log) el("log-info").textContent = log.upid + " · " + error.message;
  } finally {
    log.busy = false;
    if (state.log === log) el("log-refresh").disabled = false;
  }
}

el("search").addEventListener("input", renderInventory);
el("running-only").addEventListener("change", renderInventory);
el("unplanned-only").addEventListener("change", renderInventory);
el("cluster").addEventListener("change", () => action(async () => { resetEditor(); message(); await inventory(); }));
el("select-all").addEventListener("change", event => {
  const visible = new Set(filteredGuests().map(g => g.vmid));
  if (event.target.checked) state.selected = [...new Set([...state.selected, ...visible])];
  else state.selected = state.selected.filter(id => !visible.has(id));
  renderInventory();
  void action(targets);
});
el("storage").addEventListener("change", () => {
  chosenTarget = el("storage").value;
  el("save").disabled = !chosenTarget;
});
el("reset-editor").addEventListener("click", resetEditor);
el("plan-form").addEventListener("submit", event => {
  event.preventDefault();
  const payload = {
    id: state.edit, name: el("plan-name").value, cluster: el("cluster").value,
    storage: el("storage").value, vmids: state.selected, continue_on_error: el("continue").checked
  };
  el("save").disabled = true;
  void action(async () => {
    try {
      await api("/plans", payload);
      resetEditor();
      await dashboard();
      location.hash = "plans";
      message("Backupplan gespeichert.");
    } finally {
      el("save").disabled = !el("storage").value;
    }
  });
});
function scheduleMessage(text = "") {
  el("schedule-message").textContent = text;
  el("schedule-message").hidden = !text;
}
function updateScheduleMode() {
  const mode = el("schedule-mode").value;
  el("schedule-once").hidden = mode !== "once";
  el("schedule-time").required = mode === "once";
  el("schedule-repeat").hidden = mode === "once";
  el("schedule-clock").required = mode !== "once";
  el("schedule-days").hidden = mode !== "weekly";
  el("schedule-submit").textContent = mode === "once" ? "Einreihen" : "Serie speichern";
  scheduleMessage();
}
function openSchedule(plan, schedule) {
  state.schedule = plan.id;
  el("schedule-name").textContent = plan.name;
  el("schedule-time").value = "";
  el("schedule-mode").value = schedule ? (schedule.days.length === 7 ? "daily" : "weekly") : "once";
  el("schedule-clock").value = schedule?.clock || "";
  el("schedule-zone").textContent = "Uhrzeit (" + (schedule?.zone || state.dashboard.schedule_timezone) + ")";
  for (const day of el("schedule-days").querySelectorAll("input")) {
    day.checked = schedule?.days.includes(Number(day.value)) || false;
  }
  el("schedule-delete").hidden = !schedule;
  updateScheduleMode();
  el("schedule-dialog").showModal();
}
async function scheduleAction(update) {
  const submit = el("schedule-submit");
  const remove = el("schedule-delete");
  scheduleMessage();
  submit.disabled = remove.disabled = true;
  try { await update(); }
  catch (error) {
    if (el("schedule-dialog").open) scheduleMessage(error.message);
    else message(error.message, true);
  } finally { submit.disabled = remove.disabled = false; }
}
el("schedule-mode").addEventListener("change", updateScheduleMode);
el("schedule-form").addEventListener("submit", event => {
  event.preventDefault();
  const mode = el("schedule-mode").value;
  const plan = state.schedule;
  if (mode === "once") {
    const date = new Date(el("schedule-time").value);
    if (Number.isNaN(date.getTime()) || date <= new Date()) {
      scheduleMessage("Bitte eine zukünftige Startzeit wählen.");
      return;
    }
    void scheduleAction(() => enqueue(plan, date.toISOString()));
    return;
  }
  const days = mode === "daily" ? [1, 2, 3, 4, 5, 6, 0] :
    [...el("schedule-days").querySelectorAll("input:checked")].map(day => Number(day.value));
  if (!days.length) {
    scheduleMessage("Bitte mindestens einen Wochentag auswählen.");
    return;
  }
  const clock = el("schedule-clock").value;
  void scheduleAction(async () => {
    await api("/plans/" + plan + "/schedule", { clock, days });
    el("schedule-dialog").close();
    await dashboard();
    message("Terminserie gespeichert.");
  });
});
el("schedule-delete").addEventListener("click", () => {
  if (!confirm("Terminserie löschen? Bereits eingereihte Läufe können Sie im Verlauf abbrechen.")) return;
  const plan = state.schedule;
  void scheduleAction(async () => {
    await api("/plans/" + plan + "/schedule", null);
    el("schedule-dialog").close();
    await dashboard();
    message("Terminserie gelöscht.");
  });
});
el("schedule-cancel").addEventListener("click", () => el("schedule-dialog").close());
el("log-close").addEventListener("click", () => el("log-dialog").close());
el("log-dialog").addEventListener("close", () => { state.log = null; });
el("log-refresh").addEventListener("click", loadLog);
el("refresh").addEventListener("click", () => action(async () => {
  message();
  await dashboard();
  if (currentPage() === "inventory") await inventory();
  if (currentPage() === "users") await refreshUsers();
}));
function formMessage(id, text = "") {
  const output = el(id);
  output.textContent = text;
  output.hidden = !text;
}
async function submitForm(event, errorID, callback) {
  event.preventDefault();
  const submit = event.submitter;
  if (submit) submit.disabled = true;
  formMessage(errorID);
  try { await callback(); }
  catch (error) { formMessage(errorID, error.message); }
  finally { if (submit) submit.disabled = false; }
}

el("purge-open").addEventListener("click", () => {
  formMessage("purge-message");
  el("purge-dialog").showModal();
});
el("purge-cancel").addEventListener("click", () => el("purge-dialog").close());
el("purge-form").addEventListener("submit", event => submitForm(event, "purge-message", async () => {
  const result = await api("/history/purge", { days: Number(el("purge-days").value) });
  el("purge-dialog").close();
  await action(async () => {
    await dashboard();
    message(result.deleted === 1 ? "1 Verlaufseintrag gelöscht." : result.deleted + " Verlaufseinträge gelöscht.");
  });
}));

async function refreshUsers() {
  const users = await api("/users");
  const list = el("user-list");
  list.replaceChildren();
  for (const name of users) {
    const current = name === state.username;
    const row = node("article", null, "card plan-card");
    const controls = node("div", null, "actions");
    controls.append(button("Passwort ändern", () => openUser(name)));
    if (!current) {
      controls.append(button("Konto löschen", () => action(async () => {
        if (!confirm("Konto „" + name + "“ löschen?")) return;
        await api("/users/" + encodeURIComponent(name) + "/delete", {});
        await refreshUsers();
        message("Benutzerkonto gelöscht.");
      })));
    }
    row.append(node("strong", name + (current ? " · eigenes Konto" : "")), controls);
    list.append(row);
  }
}

function openUser(name = "") {
  state.editUser = name;
  el("user-title").textContent = name ? "Passwort ändern" : "Benutzer anlegen";
  el("user-name").value = name;
  el("user-name").readOnly = !!name;
  el("user-password").value = "";
  formMessage("user-message");
  el("user-dialog").showModal();
}
el("user-add").addEventListener("click", () => openUser());
el("user-cancel").addEventListener("click", () => el("user-dialog").close());
el("user-dialog").addEventListener("close", () => { el("user-password").value = ""; });
el("user-form").addEventListener("submit", event => submitForm(event, "user-message", async () => {
  const name = state.editUser;
  const path = name ? "/users/" + encodeURIComponent(name) + "/password" : "/users";
  await api(path, { username: el("user-name").value, password: el("user-password").value });
  if (name === state.username) {
    location.reload();
    return;
  }
  el("user-dialog").close();
  await action(async () => {
    await refreshUsers();
    message(name ? "Passwort geändert." : "Benutzerkonto angelegt.");
  });
}));

el("login-form").addEventListener("submit", event => submitForm(event, "login-message", async () => {
  await api("/login", { username: el("login-name").value, password: el("login-password").value });
  el("login-password").value = "";
  let session;
  try {
    session = await api("/session");
  } catch (error) {
    if (error.status === 401) {
      throw new Error("Das Passwort wurde akzeptiert, aber die Sitzung fehlt. Prüfen Sie, ob der Browser den Sitzungscookie speichert und der Reverse Proxy ihn weiterleitet.");
    }
    throw error;
  }
  showApp(session.username);
}));
el("logout").addEventListener("click", () => action(async () => {
  await api("/logout", {});
  location.reload();
}));

function showApp(username) {
  state.username = username;
  el("login").hidden = true;
  el("app-nav").hidden = el("app-main").hidden = false;
  window.addEventListener("hashchange", renderRoute);
  renderRoute();
  void action(async () => { await dashboard(); await inventory(); });
  setInterval(() => {
    if (!document.hidden) void dashboard().catch(() => {});
    if (el("log-dialog").open && state.log) void loadLog();
  }, 4000);
}

async function start() {
  try {
    const { username } = await api("/session");
    showApp(username);
  } catch (error) {
    el("login").hidden = false;
    if (error.httpsRequired && location.protocol !== "https:") {
      formMessage("login-message", "Die Anmeldung benötigt HTTPS. Öffnen Sie die HTTPS-Adresse des Reverse Proxy.");
      for (const control of el("login-form").querySelectorAll("input, button")) control.disabled = true;
      return;
    }
    if (error.status !== 401) formMessage("login-message", error.message);
  }
}
void start();
