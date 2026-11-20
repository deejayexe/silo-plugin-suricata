"use strict";

const el = id => document.getElementById(id);
const base = new URL(location.href);
base.pathname = base.pathname.replace(/\/support\/?$/, "/");
base.search = "";
base.hash = "";

const translations = {
  "Volver": "Back",
  "Chat privado de soporte para usuarios de Silo.": "Private support chat for Silo users.",
  "Usuario": "User",
  "Iniciar conversación": "Start a conversation",
  "Cargando usuarios…": "Loading users…",
  "Selecciona un usuario": "Choose a user",
  "No hay usuarios activos.": "No active users.",
  "Reintentar": "Retry",
  "Idioma": "Language",
  "Conversaciones": "Conversations",
  "Administrador de Silo": "Silo administrator",
  "Abriendo la conversación…": "Opening the conversation…",
  "Ver mensajes anteriores": "View older messages",
  "Historial": "Message history",
  "Mensaje": "Message",
  "Escribe un mensaje…": "Write a message…",
  "Enviar": "Send",
  "Marcar como leído": "Mark as read",
  "Todavía no hay mensajes.": "No messages yet.",
  "Selecciona una conversación o un usuario.": "Choose a conversation or a user.",
  "Vuelve a abrir Surichat desde tu sesión de Silo.": "Reopen Surichat from your Silo session.",
  "No se pudo conectar con Surichat.": "Could not connect to Surichat.",
  "No se pudo completar la solicitud.": "Could not complete the request.",
  "Acceso reservado a administración.": "Administrator access required.",
  "Ruta desconocida.": "Unknown route.",
  "No se pudo abrir el historial. Comprueba la ruta y los permisos de SQLite.": "Could not open the history. Check the SQLite path and permissions.",
  "Solo puedes ver tu conversación.": "You can only view your own conversation.",
  "Solo puedes escribir en tu conversación.": "You can only post to your own conversation.",
  "Se esperaba una solicitud JSON.": "Expected a JSON request.",
  "Solicitud inválida.": "Invalid request.",
  "La sesión del chat cambió. Vuelve a cargar Surichat.": "Your chat session changed. Reload Surichat.",
  "Escribe entre 1 y 2000 caracteres.": "Enter between 1 and 2000 characters.",
  "Selecciona un usuario.": "Select a user.",
  "No se pudo verificar la cuenta de Silo.": "Could not verify the Silo account.",
  "No se pudo acceder al historial.": "Could not access the history.",
  "Configura la URL interna y la clave de Silo para iniciar chats.": "Configure the internal Silo URL and API key to start chats.",
  "No se pudo consultar las cuentas del servidor.": "Could not look up accounts on the server.",
};

let viewer = "";
let language = "es";
let date;
let accounts = [];
let renderedAccounts = "";
let unread = 0;
let chatUnread = 0;
let status = "Abriendo la conversación…";
let directoryStatus = "";
let directoryUsers = [];
let directoryToken = "";
let marking = false;
const tr = value => language === "en" && Object.hasOwn(translations, value) ? translations[value] : value;
const languageKey = () => "silo-support.language." + base.pathname + viewer;

function setStatus(value) {
  status = value;
  el("status").textContent = tr(status);
}

function renderUnread() {
  el("unread").hidden = unread === 0;
  const text = unreadLabel(unread);
  if (el("unread").textContent !== text) el("unread").textContent = text;
  el("read").hidden = !chatUnread || messages.length === 0;
}

function renderLanguage() {
  document.documentElement.lang = language;
  document.title = "Surichat · Silo";
  el("language").value = language;
  for (const [selector, label] of Object.entries({
    "#back": "Volver", "#intro": "Chat privado de soporte para usuarios de Silo.", "label[for=language]": "Idioma", "#panel h2": "Conversaciones",
    "label[for=directory]": "Iniciar conversación", "#retry-directory": "Reintentar",
    "#older": "Ver mensajes anteriores",
    "label[for=message]": "Mensaje", "#send": "Enviar", "#read": "Marcar como leído",
  })) document.querySelector(selector).textContent = tr(label);
  el("message").placeholder = tr("Escribe un mensaje…");
  el("messages").setAttribute("aria-label", tr("Historial"));
  renderTitle();
  renderDirectory();
  date = new Intl.DateTimeFormat(language, { dateStyle: "short", timeStyle: "short" });
  setStatus(status);
  renderAccounts(accounts);
  render("preserve");
}

async function request(route, options = {}) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 12000);
  try {
    return await readResponse(await fetch(new URL(route, base), {
      ...options,
      cache: "no-store",
      redirect: "error",
      signal: controller.signal,
    }), route);
  } catch (error) {
    if (controller.signal.aborted) throw new Error("No se pudo conectar con Surichat.");
    throw error;
  } finally {
    clearTimeout(timeout);
  }
}

async function readResponse(response, route) {
  const json = response.headers.get("content-type")?.includes("json");
  if (response.status === 401 || (response.status === 403 && (route.startsWith("state?") || !json))) {
    const error = new Error("Vuelve a abrir Surichat desde tu sesión de Silo.");
    error.sessionExpired = true;
    throw error;
  }
  if (!json) {
    throw new Error("No se pudo conectar con Surichat.");
  }
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || "No se pudo completar la solicitud.");
  return data;
}

let selected = null;
let admin = null;
let token = "";
let messages = [];
let more = false;
let revision = 0;
let busy = false;
let sending = false;


function render(scroll) {
  const history = el("history");
  const offset = history.scrollHeight - history.scrollTop;
  const items = messages.map(message => {
    const item = document.createElement("li");
    item.classList.toggle("mine", message.admin === admin);
    const info = document.createElement("span");
    info.className = "byline";
    info.textContent = message.sender + " · " + date.format(new Date(message.at));
    const body = document.createElement("p");
    body.textContent = message.body;
    item.append(info, body);
    return item;
  });
  el("messages").replaceChildren(...items);
  el("older").hidden = !more;
  el("empty").hidden = items.length > 0;
  el("empty").textContent = tr(admin && !selected ? "Selecciona una conversación o un usuario." : "Todavía no hay mensajes.");
  renderUnread();
  if (scroll === "bottom") history.scrollTop = history.scrollHeight;
  if (scroll === "preserve") history.scrollTop = history.scrollHeight - offset;
}

function unreadLabel(count) {
  if (language === "en") return count === 1 ? "1 unread message" : count + " unread messages";
  return count === 1 ? "1 mensaje sin leer" : count + " mensajes sin leer";
}

function renderTitle() {
  el("title").textContent = selected ? tr("Usuario") + ": " + selected.username : tr("Administrador de Silo");
}

function renderAccounts(accounts) {
  if (!admin) return;
  const list = el("accounts");
  const key = JSON.stringify([language, selected?.id, accounts]);
  if (key === renderedAccounts) return;
  renderedAccounts = key;
  list.setAttribute("aria-label", tr("Conversaciones"));
  const active = list.contains(document.activeElement) ? document.activeElement.dataset.account : null;
  const ordered = [...accounts].sort((a, b) => Number(Boolean(b.unread)) - Number(Boolean(a.unread)));
  let focused;
  list.replaceChildren(...ordered.map(account => {
    const row = document.createElement("li");
    const button = document.createElement("button");
    button.type = "button";
    button.className = "account";
    button.dataset.account = account.id;
    if (account.id === selected?.id) button.setAttribute("aria-current", "true");
    const name = document.createElement("span");
    name.textContent = account.username;
    button.append(name);
    if (account.unread) {
      const badge = document.createElement("span");
      badge.className = "account-unread";
      badge.textContent = account.unread;
      badge.setAttribute("aria-hidden", "true");
      button.setAttribute("aria-label", account.username + ", " + unreadLabel(account.unread));
      button.append(badge);
    }
    if (account.id === active) focused = button;
    button.addEventListener("click", () => openChat(account));
    row.append(button);
    return row;
  }));
  focused?.focus({ preventScroll: true });
}

async function refresh(mode = "latest", autoRead = true) {
  const current = revision;
  const params = new URLSearchParams();
  if (selected) params.set("user", selected.id);
  if (mode === "older" && messages.length) params.set("before", messages[0].id);
  const data = await request("state?" + params);
  if (current !== revision) return;
  if (token && token !== data.token || admin !== null && admin !== data.admin) {
    location.reload();
    return;
  }
  admin = data.admin;
  const siloPath = base.pathname.split("/api/v2/plugin-content/")[0];
  const back = el("back");
  back.href = siloPath + "/";
  back.hidden = false;
  token = data.token;
  el("panel").hidden = !admin;
  if (admin && directoryToken !== token) loadDirectory();
  el("compose").hidden = admin && !selected;
  accounts = data.accounts;
  unread = data.unread;
  chatUnread = data.chat_unread;
  if (viewer !== data.viewer) {
    viewer = data.viewer;
    try { language = localStorage.getItem(languageKey()) === "en" ? "en" : "es"; } catch {}
    renderLanguage();
  } else renderAccounts(accounts);
  const fresh = data.messages.reverse();
  if (mode === "older") {
    messages = [...fresh, ...messages];
    more = data.more;
    render("preserve");
  } else {
    const newest = messages.at(-1)?.id || 0;
    if (!messages.length || fresh.at(-1)?.id > newest) {
      const history = el("history");
      const atBottom = history.scrollHeight - history.scrollTop - history.clientHeight < 28;
      const extra = fresh.filter(message => message.id > newest);
      if (!messages.length || extra.length === 50) {
        messages = fresh;
        more = data.more;
      } else messages.push(...extra);
      render(mode === "open" || atBottom ? "bottom" : "");
    }
  }
  renderUnread();
  if (mode !== "latest" || status === "Vuelve a abrir Surichat desde tu sesión de Silo.") setStatus("");
  const last = el("messages").lastElementChild?.getBoundingClientRect();
  const history = el("history").getBoundingClientRect();
  if (autoRead && mode !== "older" && chatUnread && !document.hidden && last &&
      last.bottom <= Math.min(history.bottom, document.documentElement.clientHeight) + 1 &&
      last.bottom > Math.max(history.top, 0)) {
    if (await markRead() && revision === current) await refresh("latest", false);
  }
}

function run(mode) {
  if (busy && mode !== "open") return Promise.resolve();
  const current = revision;
  busy = true;
  return refresh(mode).catch(error => {
    if (current !== revision) return;
    if (error.sessionExpired) {
      token = "";
      viewer = "";
      accounts = [];
      unread = chatUnread = 0;
      directoryStatus = "";
      directoryUsers = [];
      directoryToken = "";
      renderDirectory();
      selected = null;
      messages = [];
      more = false;
      renderedAccounts = "";
      el("accounts").replaceChildren();
      el("results").replaceChildren();
      el("message").value = "";
      el("title").textContent = tr("Administrador de Silo");
      el("panel").hidden = true;
      el("back").hidden = true;
      el("compose").hidden = true;
      render("bottom");
    }
    setStatus(error.message);
  }).finally(() => {
    if (current === revision) busy = false;
  });
}

function openChat(account) {
  revision++;
  selected = account;
  messages = [];
  more = false;
  chatUnread = 0;
  renderTitle();
  renderAccounts(accounts);
  el("message").value = "";
  el("compose").hidden = admin && !selected;
  render("bottom");
  el("title").focus({ preventScroll: true });
  if (matchMedia("(max-width: 730px)").matches) {
    el("title").closest(".chat").scrollIntoView({ block: "start" });
  }
  run("open");
}

function renderDirectory() {
  const select = el("directory");
  const placeholder = directoryUsers.length ? "Selecciona un usuario" : directoryStatus || "Cargando usuarios…";
  const ordered = [...directoryUsers].sort((a, b) => a.username.localeCompare(b.username, language));
  select.replaceChildren(new Option(tr(placeholder), ""), ...ordered.map(user => new Option(user.username, user.id)));
  select.disabled = !directoryUsers.length;
  el("results").textContent = tr(directoryStatus);
  el("retry-directory").hidden = !directoryStatus || directoryStatus === "No hay usuarios activos.";
}

async function loadDirectory() {
  const current = token;
  directoryToken = current;
  directoryStatus = "";
  renderDirectory();
  try {
    const users = new Map();
    const cursors = new Set();
    let cursor = "";
    do {
      const data = await request("directory?" + new URLSearchParams({ cursor }));
      if (token !== current) return;
      for (const user of data.items) users.set(user.id, user);
      cursor = data.next_cursor || "";
      if (cursor && (cursors.has(cursor) || cursors.size >= 1000)) throw new Error("No se pudo consultar las cuentas del servidor.");
      cursors.add(cursor);
    } while (cursor);
    directoryUsers = [...users.values()];
    if (!directoryUsers.length) directoryStatus = "No hay usuarios activos.";
  } catch (error) {
    if (token !== current) return;
    directoryStatus = error.message;
  }
  renderDirectory();
}

el("directory").addEventListener("change", event => {
  const account = directoryUsers.find(user => user.id === event.currentTarget.value);
  event.currentTarget.value = "";
  if (account) openChat(account);
});
el("retry-directory").addEventListener("click", loadDirectory);

el("message").addEventListener("keydown", event => {
  if (event.key !== "Enter" || event.shiftKey || event.isComposing || event.keyCode === 229) return;
  event.preventDefault();
  if (!event.repeat && event.currentTarget.value.trim()) el("compose").requestSubmit();
});

el("compose").addEventListener("submit", async event => {
  event.preventDefault();
  if (sending || !token || admin && !selected) return;
  sending = el("send").disabled = true;
  const input = el("message");
  const body = input.value;
  const current = revision;
  try {
    await request("send", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ body, user: selected?.id || "", token }),
    });
    if (current === revision) {
      if (input.value === body) input.value = "";
      await run("open");
      el("history").scrollTop = el("history").scrollHeight;
    }
  } catch (error) {
    if (current === revision) setStatus(error.message);
  } finally {
    sending = el("send").disabled = false;
  }
});
el("older").addEventListener("click", () => run("older"));
el("language").addEventListener("change", event => {
  language = event.currentTarget.value;
  try { if (viewer) localStorage.setItem(languageKey(), language); } catch {}
  renderLanguage();
});
async function markRead() {
  const through = messages.at(-1)?.id;
  if (marking || !through || !token) return;
  const current = revision;
  marking = el("read").disabled = true;
  try {
    await request("read", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user: selected?.id || "", through, token }),
    });
    return true;
  } catch (error) {
    if (revision === current) setStatus(error.message);
    return false;
  } finally {
    marking = el("read").disabled = false;
  }
}
el("read").addEventListener("click", async () => {
  const current = revision;
  if (await markRead() && revision === current) await run("latest");
});
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && !marking) run("latest");
});
renderLanguage();
run("open");
setInterval(() => { if (!document.hidden && !sending && !marking) run("latest"); }, 5000);
