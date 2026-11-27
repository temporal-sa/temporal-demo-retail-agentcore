// The crew order page. It polls GET /api/view once a second while an order runs, one
// request at a time, and draws everything from that one answer. The API reads Temporal
// with Describe, the Event History and Worker heartbeats, never a Query (guide 15).
"use strict";

const DEFAULT_REQUEST = "Same crew order as last Friday, for Titan Ring. Mo's vegan now. Use whatever reward saves the most.";
const FAST_POLL_MS = 1000;
const SLOW_POLL_MS = 4000;

const $ = (id) => document.getElementById(id);
const params = new URLSearchParams(location.search);
const state = { orderId: params.get("order"), supportId: params.get("support"), view: null, pending: null, pendingSupport: null, paying: false, sending: false, marking: false, choosing: false, reviewing: false, timer: null };

// ---------- small DOM helpers ----------
function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") node.className = v;
    else if (k === "text") node.textContent = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) if (c !== null && c !== undefined && c !== false) node.append(c);
  return node;
}
const money = (v) => Number(v).toFixed(2);
const clock = (iso) => (iso ? new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false }) : "");
const ago = (iso, now) => {
  const s = Math.max(0, Math.round((new Date(now) - new Date(iso)) / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
};
const newId = () => Array.from(crypto.getRandomValues(new Uint8Array(4)), (b) => b.toString(16).padStart(2, "0")).join("");

// ---------- the API ----------
async function api(method, path, body) {
  const resp = await fetch(path, { method, headers: body ? { "Content-Type": "application/json" } : {}, body: body ? JSON.stringify(body) : undefined, credentials: "same-origin" });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    const err = new Error(data.detail || (data.errors || []).join("; ") || data.title || resp.statusText);
    err.status = resp.status;
    throw err;
  }
  return data;
}

async function poll() {
  clearTimeout(state.timer);
  let next = SLOW_POLL_MS;
  try {
    const query = state.orderId ? `?order=${state.orderId}${state.supportId ? `&support=${state.supportId}` : ""}` : "";
    const view = await api("GET", "/api/view" + query);
    state.view = view;
    if (view.order) state.pending = null;
    if (view.support) state.pendingSupport = null;
    $("error-banner").hidden = true;
    render();
    const o = view.order;
    if ((o && o.status === "Running") || view.support?.status === "Running" || view.worker.running) next = FAST_POLL_MS;
  } catch (err) {
    if (err.status === 404 && state.orderId) {
      // Another browser's order, or one the dev server forgot after a restart.
      setOrder(null);
      next = 0;
    } else {
      $("error-text").textContent = `${err.message}. Retrying.`;
      $("error-banner").hidden = false;
      next = 3000;
    }
  }
  state.timer = setTimeout(poll, next);
}

function setOrder(id) {
  state.orderId = id;
  state.paying = false;
  state.marking = false;
  setSupport(null);
}

function setSupport(id) {
  state.supportId = id;
  state.pendingSupport = null;
  state.choosing = false;
  state.reviewing = false;
  const url = new URL(location.href);
  for (const [k, v] of [["order", state.orderId], ["support", id]]) {
    if (v) url.searchParams.set(k, v);
    else url.searchParams.delete(k);
  }
  history.replaceState(null, "", url);
}

async function startOrder(text) {
  const id = newId();
  setOrder(id);
  state.pending = { phase: "planning", status: "Running", messages: [{ from: "member", text, at: new Date().toISOString() }], work: [], proof: { rows: [], workers: [] } };
  render();
  try {
    const view = await api("POST", "/api/orders", { id, text });
    state.view = { ...state.view, order: view.order };
  } catch (err) {
    if (err.status !== 409) {
      setOrder(null);
      state.pending = null;
      $("error-text").textContent = `Couldn't start the order: ${err.message}`;
      $("error-banner").hidden = false;
    }
  }
  poll();
}

async function pay(quoteId) {
  state.paying = true;
  render();
  try {
    await api("POST", `/api/orders/${state.orderId}/pay`, { quoteId });
  } catch (err) {
    state.paying = false;
    $("error-text").textContent = `Payment not sent: ${err.message}`;
    $("error-banner").hidden = false;
  }
  poll();
}

async function markReady() {
  state.marking = true;
  render();
  try {
    await api("POST", `/api/orders/${state.orderId}/ready`, {});
  } catch (err) {
    state.marking = false;
    $("error-text").textContent = `Ready not sent: ${err.message}`;
    $("error-banner").hidden = false;
  }
  poll();
}

// ---------- the support chat ----------
async function startSupport() {
  const id = newId();
  setSupport(id);
  state.pendingSupport = { id, status: "Running", phase: "listening", messages: [], work: [], proof: { rows: [], workers: [] } };
  if (state.view) state.view = { ...state.view, support: null };
  render();
  try {
    const res = await api("POST", `/api/orders/${state.orderId}/support`, { id });
    state.pendingSupport = { ...state.pendingSupport, ...res.support };
  } catch (err) {
    if (err.status !== 409) {
      setSupport(null);
      $("error-text").textContent = `Couldn't open a support chat: ${err.message}`;
      $("error-banner").hidden = false;
    }
  }
  poll();
}

const supportPath = (what) => `/api/orders/${state.orderId}/support/${state.supportId}/${what}`;

async function choose(optionId) {
  state.choosing = true;
  render();
  try {
    await api("POST", supportPath("choose"), { optionId });
  } catch (err) {
    state.choosing = false;
    $("error-text").textContent = `Choice not sent: ${err.message}`;
    $("error-banner").hidden = false;
  }
  poll();
}

async function review(approve) {
  state.reviewing = true;
  render();
  try {
    await api("POST", supportPath("review"), { approve });
  } catch (err) {
    state.reviewing = false;
    $("error-text").textContent = `Review not sent: ${err.message}`;
    $("error-banner").hidden = false;
  }
  poll();
}

async function sendMessage(text) {
  state.sending = true;
  render();
  try {
    await api("POST", state.supportId ? supportPath("messages") : `/api/orders/${state.orderId}/messages`, { text });
  } catch (err) {
    $("error-text").textContent = `Message not sent: ${err.message}`;
    $("error-banner").hidden = false;
  }
  state.sending = false;
  poll();
}

// ---------- what is happening now ----------
// One name for the moment, used by every panel so they always agree.
function moment(view, o) {
  if (!o) return "none";
  const running = view?.worker?.running;
  if (o.phase === "ready" || o.phase === "placed") return running ? "done-draining" : "done";
  if (o.phase === "expired" || o.status !== "Running") return "closed";
  if (o.phase === "confirm" || o.phase === "chat") return running ? "draining" : "waiting";
  if (o.phase === "preparing" && !kitchenSignaled(o)) return running ? "kitchen-draining" : "kitchen";
  if (!running) return "invoking";
  return "working";
}

// The support chat's moment. "invoking" when a Signal or timer has made a
// task that no Worker has picked up yet.
function supportMoment(view, s) {
  const running = view?.worker?.running;
  if (s.status !== "Running") return running ? "done-draining" : "s-closed";
  if (s.waiting && !running) return "invoking";
  if (["listening", "choosing"].includes(s.phase)) return running ? "s-draining" : "s-member";
  if (s.phase === "review") return running ? "s-draining" : "s-review";
  return running ? "working" : "invoking";
}

// The backstage panels count both Workflows: the order and its support chat.
function combined(o, s) {
  if (!s?.proof) return o;
  const rows = [...(o?.proof?.rows || []), ...s.proof.rows].sort((a, b) => new Date(a.at) - new Date(b.at));
  const workers = [...new Set([...(o?.proof?.workers || []), ...s.proof.workers])];
  const sum = (k) => (o?.proof?.[k] || 0) + (s.proof[k] || 0);
  return {
    ...o,
    proof: { rows, workers, modelCalls: sum("modelCalls"), repeats: sum("repeats") },
    ledger: { entries: [...(o?.ledger?.entries || []), ...(s.ledger?.entries || [])], replays: (o?.ledger?.replays || 0) + (s.ledger?.replays || 0) },
  };
}

const countdown = (until, now) => {
  const left = Math.max(0, Math.round((new Date(until) - new Date(now)) / 1000));
  return `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`;
};

// The kitchen marked the order ready: the Signal is in the history, and
// Ziggy's message about it follows once a Worker picks it up.
const kitchenSignaled = (o) => (o?.proof?.rows || []).some((r) => r.kind === "signal" && r.name === "kitchen_ready");

function sandboxes(view, o) {
  const workers = o?.proof?.workers || [];
  const live = view?.worker?.running ? view.worker.identity : null;
  const list = workers.map((identity, i) => ({ identity, n: i + 1, live: identity === live }));
  if (live && !workers.includes(live)) list.push({ identity: live, n: list.length + 1, live: true, fresh: true });
  return list;
}

const sandboxName = (o, identity) => {
  const i = (o?.proof?.workers || []).indexOf(identity);
  return i < 0 ? "a new sandbox" : `Sandbox ${i + 1}`;
};

// ---------- render ----------
function render() {
  const view = state.view || {};
  const o = view.order || state.pending;
  const s = state.supportId ? view.support || state.pendingSupport : null;
  const m = s ? supportMoment(view, s) : moment(view, o);
  const c = combined(o, s);
  renderHeader(view, o, s);
  if (s) {
    renderSupportPhone(view, o, s, m);
    renderSupportAgent(view, c, s, m);
  } else {
    renderPhone(view, o, m);
    renderAgent(view, o, m);
  }
  renderKitchen(view, o, m, s);
  renderBackstage(view, c, m);
}

function renderHeader(view, o, s) {
  const p = view.profile;
  $("brand-line").textContent = p ? `${p.brand.name} · Ziggy crew orders` : "Ziggy crew orders";
  $("app-brand").textContent = p ? p.brand.name : "Café app";
  $("app-cafe").textContent = p ? `Pickup · ${p.cafe} café` : "Pickup";
  const extra = (o?.placed?.points || 0) + (s?.resolution?.points || 0);
  $("app-points").textContent = p ? `${(p.stardust + extra).toLocaleString()} ${p.brand.rewards}` : "";
  $("phone-clock").textContent = new Date(view.serverTime || Date.now()).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  const shown = s?.workflowId ? s : o;
  $("session-line").textContent = shown?.workflowId ? shown.workflowId.replace(/-[0-9a-f]{32}-/, "-…-") : "";
  for (const id of ["temporal-link", "history-link"]) {
    $(id).hidden = !shown?.temporalUrl;
    if (shown?.temporalUrl) $(id).href = shown.temporalUrl;
  }
  $("new-order").hidden = !o || o.status === "Running";
}

function renderPhone(view, o, m) {
  const chat = $("chat");
  const items = [];
  if (!o) {
    items.push(el("p", { class: "hint", text: "Ask Ziggy for an order. It reads your history, today's menu and stock, and your rewards, then builds the cart." }));
  } else {
    const timeline = [
      ...o.messages.map((msg) => ({ at: msg.at, kind: "msg", msg })),
      ...o.work.map((w) => ({ at: w.at, kind: "work", w })),
    ].sort((a, b) => new Date(a.at) - new Date(b.at));
    // The cart follows Ziggy's reply to the last priced cart.
    const lastQuote = timeline.map((t) => t.kind === "work" && t.w.tool === "quote_cart" && t.w.status === "completed").lastIndexOf(true);
    const cartAfter = lastQuote < 0 ? -1 : timeline.findIndex((t, i) => i > lastQuote && t.kind === "msg" && t.msg.from === "ziggy");
    let work = null;
    timeline.forEach((t, i) => {
      if (t.kind === "work") {
        if (!work) items.push((work = el("ul", { class: "work", "aria-label": "What Ziggy checked" })));
        work.append(el("li", { "data-status": t.w.status, text: `${t.w.status === "completed" ? "✓" : "!"} ${t.w.text}` }));
        return;
      }
      work = null;
      items.push(t.msg.from === "member" ? el("div", { class: "bubble me", text: t.msg.text }) : zigLine(t.msg.text));
      if (i === cartAfter && o.quote) items.push(cart(o));
    });
    if (o.status === "Running" && (["planning", "placing"].includes(o.phase) || (o.phase === "preparing" && kitchenSignaled(o)))) items.push(zigLine(null));
    // The Workflow has finished: Ziggy has nothing more to say about this order.
    if (o.status !== "Running") items.push(el("p", { class: "chat-closed", role: "status", text: "Chat closed" }));
    if (o.status !== "Running" && o.placed) items.push(helpButton());
  }
  drawChat(chat, items);

  const text = $("composer-text");
  const canStart = !o;
  const canMessage = o && ["confirm", "chat"].includes(o.phase) && o.status === "Running" && !state.sending;
  text.disabled = !(canStart || canMessage);
  $("composer-send").disabled = text.disabled;
  if (canStart && !text.value && !text.dataset.touched) text.value = DEFAULT_REQUEST;
  text.placeholder = canMessage ? "Ask for a change" : o && o.status !== "Running" ? "Chat closed" : { preparing: "Ziggy will message you when it's ready", ready: "Ready for pickup", placed: "Order placed" }[o?.phase] || "Ziggy is working";
  if (o && text.value === DEFAULT_REQUEST) text.value = "";

  $("phone-caption").textContent = {
    none: "The member's phone. Send the order to start.",
    invoking: "Temporal is starting a sandbox for Ziggy.",
    working: "Ziggy is working out the order.",
    draining: "Wait about a minute before tapping Pay: the sandbox drains, then AgentCore ends it.",
    waiting: "No sandbox is running. Tapping Pay starts a new one.",
    "kitchen-draining": "Paid once, placed once. Wait about a minute before marking it ready: the sandbox drains, then AgentCore ends it.",
    kitchen: "No sandbox is running. Marking the order ready in the kitchen starts a new one.",
    "done-draining": o?.phase === "ready" ? "Paid once, placed once, ready for pickup." : "Paid once, placed once.",
    done: o?.phase === "ready" ? "Paid once, placed once, ready for pickup." : "Paid once, placed once.",
    closed: "This order is closed. Start a new one.",
  }[m];
}

// Redraw only when something changed, and follow new messages only if the
// reader is already at the bottom: polling must not undo their scrolling.
function drawChat(chat, items) {
  const html = items.map((n) => n.outerHTML).join("");
  if (html === chat.lastHtml) return;
  const atBottom = chat.scrollHeight - chat.scrollTop - chat.clientHeight < 40;
  const top = chat.scrollTop;
  chat.replaceChildren(...items);
  chat.lastHtml = html;
  chat.scrollTop = atBottom ? chat.scrollHeight : top;
}

// Buttons inside the phone are handled by one listener on the chat (see
// wiring), so a redraw that compares markup never loses a handler.
const helpButton = () => el("button", { type: "button", class: "app-btn alt help-btn", "data-action": "help", text: "Need help with your order? Start a new chat" });

function renderSupportPhone(view, o, s, m) {
  const items = [el("p", { class: "chat-topic", text: `Support · order ${o?.placed?.orderNumber || ""}` })];
  for (const msg of s.messages || []) items.push(msg.from === "member" ? el("div", { class: "bubble me", text: msg.text }) : zigLine(msg.text));
  const open = s.status === "Running";
  if (open && s.phase === "choosing" && s.offer) {
    items.push(el("div", { class: "choices", role: "group", "aria-label": "Ziggy's options" },
      s.offer.map((opt) => el("button", { type: "button", class: `app-btn ${opt.id === "other" ? "alt" : "pay"}`, "data-action": "choose", "data-option": opt.id, disabled: state.choosing, text: opt.label }))));
  }
  if (!s.offer || s.phase !== "choosing") state.choosing = false;
  if (open && ["thinking", "resolving"].includes(s.phase)) items.push(zigLine(null));
  if (!open) items.push(el("p", { class: "chat-closed", role: "status", text: "Chat closed" }), helpButton());
  drawChat($("chat"), items);

  const text = $("composer-text");
  const canMessage = open && ["listening", "choosing"].includes(s.phase) && !state.sending;
  text.disabled = !canMessage;
  $("composer-send").disabled = text.disabled;
  text.placeholder = !open ? "Chat closed" : s.phase === "choosing" ? "Pick an option, or type" : canMessage ? "Tell Ziggy what went wrong" : "Ziggy is working";
  if (text.value === DEFAULT_REQUEST) text.value = "";

  $("phone-caption").textContent = {
    invoking: "Temporal is starting a sandbox for Ziggy.",
    working: "Ziggy is working on it.",
    "s-draining": "Wait about a minute: the sandbox drains, then AgentCore ends it.",
    "s-member": "No sandbox is running. The member's tap or message starts a new one.",
    "s-review": "No sandbox is running. A decision on the Support desk card, or the timer, starts a new one.",
    "done-draining": "Support chat closed.",
    "s-closed": "Support chat closed.",
  }[m] || "";
}

function zigLine(text) {
  const bubble = text === null
    ? el("div", { class: "bubble zig", "aria-label": "Ziggy is typing" }, el("span", { class: "typing" }, el("span"), el("span"), el("span")))
    : el("div", { class: "bubble zig", text });
  return el("div", { class: "zig-line" }, el("img", { src: "assets/brand/ziggy-face.png", alt: text === null ? "" : "Ziggy" }), bubble);
}

function cart(o) {
  const q = o.quote;
  const rows = q.lines.map((l) =>
    el("div", { class: "cart-row" },
      el("span", { text: `${l.qty}× ${l.name || l.itemId}${l.options.length ? ", " + l.options.map((x) => x.replace("milk:", "") .replace("no-cream", "no cream").replace("add:", "+ ")).join(", ") : ""}` }),
      el("span", { class: "amt", text: money(l.linePrice ?? 0) }),
      l.note || l.vegan ? el("small", { text: [l.note, l.vegan && l.note ? "vegan" : ""].filter(Boolean).join(" · ") }) : null));
  const others = (q.alternatives || []).filter((a) => a.eligible && (!q.reward || a.id !== q.reward.id)).map((a) => `${a.name} (${money(a.discount)} cr)`);
  const reward = q.reward ? el("div", { class: "reward-pick" }, el("strong", { text: q.reward.name }), ` saves ${money(q.reward.discount)} cr.`, others.length ? ` Ziggy also checked ${others.join(" and ")}.` : "") : null;
  const total = el("div", { class: "cart-total" },
    el("span", { text: "Subtotal" }), el("span", { class: "amt", text: `${money(q.subtotal)} cr` }),
    q.reward ? [el("span", { text: q.reward.name }), el("span", { class: "amt", text: `−${money(q.reward.discount)} cr` })] : null,
    el("strong", { text: "Total" }), el("strong", { class: "amt", text: `${money(q.total)} cr` }),
    el("span", { class: "earn", text: `Earns ${q.earns} Stardust · ready ${q.readyAt ? q.readyAt.slice(11, 16) : ""}` }));
  let action = null;
  if (o.phase === "confirm" && o.status === "Running") {
    action = [
      el("button", { type: "button", class: "app-btn pay", disabled: state.paying, onclick: () => pay(q.quoteId), text: state.paying ? "Paying…" : `Pay ${money(q.total)} cr` }),
      el("button", { type: "button", class: "app-btn alt", onclick: () => $("composer-text").focus(), text: "Change something" }),
    ];
  } else if (o.placed) {
    action = el("div", { class: "paid", text: `✓ Paid · order ${o.placed.orderNumber} · charged once` });
  } else if (o.phase === "placing") {
    action = el("div", { class: "paid", text: "Paying…" });
  }
  return el("div", { class: "cart", "aria-label": "Cart" }, rows, reward, total, action);
}

const ENDPOINTS = {
  get_recent_orders: "GET /members/{member}/orders",
  get_menu: "GET /cafes/{cafe}/menu",
  get_stock: "GET /cafes/{cafe}/stock",
  get_rewards: "GET /members/{member}/rewards",
  quote_cart: "POST /quotes",
  charge_payment: "POST /payments",
  place_order: "POST /orders",
  award_stardust: "POST /members/{member}/stardust",
  issue_refund: "POST /refunds",
  place_comp: "POST /comps",
};

function renderAgent(view, o, m) {
  $("agent-title").textContent = "Ziggy is ordering for the crew";
  $("tools-title").textContent = "Tool calls · store API";
  const work = o?.work || [];
  const done = (tool) => work.some((w) => w.tool === tool && w.status === "completed");
  const quoted = !!o?.quote;
  const phase = o?.phase;
  const signals = (o?.proof?.rows || []).filter((r) => r.kind === "signal");
  const paidAt = signals.find((r) => r.name === "pay")?.at;
  const placedBy = (o?.proof?.rows || []).find((r) => r.name === "charge_payment")?.worker;
  const placed = ["preparing", "ready", "placed"].includes(phase);
  const readyAt = signals.find((r) => r.name === "kitchen_ready")?.at;
  const step = (status, label, detail, time) => ({ status, label, detail, time });
  const steps = [
    step(work.length ? "completed" : o ? "active" : "pending", "Understand the request", o ? "Repeat an order · dietary needs · best reward" : "", ""),
    step(done("get_stock") ? "completed" : work.length ? "active" : "pending", "Rebuild the order for today", done("get_stock") ? "History, menu and stock read from the store" : work.length ? "Reading the store" : "", ""),
    step(quoted ? "completed" : done("get_rewards") ? "active" : "pending", "Price the cart, pick the best reward", quoted ? (o.quote.reward ? `${o.quote.reward.name} saves ${money(o.quote.reward.discount)} cr` : `Total ${money(o.quote.total)} cr`) : "", ""),
    step(phase === "confirm" || phase === "chat" ? "waiting" : paidAt ? "completed" : "pending", "The member confirms the cart", phase === "confirm" ? "Waiting on the member · as long as it takes" : paidAt ? "Paid" : "", phase === "confirm" && view.serverTime && o.messages.length ? ago(o.messages[o.messages.length - 1].at, view.serverTime) : clock(paidAt)),
    step(placed ? "completed" : phase === "placing" ? "active" : "pending", "Charge, place, award Stardust", placed ? `${sandboxName(o, placedBy)} · nothing before it ran again` : phase === "placing" || phase === "confirm" ? "Runs on a new sandbox" : "", ""),
    step(placed ? "completed" : "pending", "Tell the member it's placed", placed && o.placed ? `Ready ${o.placed.ready} · ${o.placed.points} Stardust` : "", ""),
    step(phase === "preparing" ? (readyAt ? "completed" : "waiting") : phase === "ready" ? "completed" : "pending", "The kitchen makes the order", phase === "preparing" && !readyAt ? "Waiting on the kitchen · no sandbox needed" : readyAt ? "Marked ready" : phase === "placed" ? "Not marked ready in time" : "", phase === "preparing" && !readyAt && view.serverTime && o.messages.length ? ago(o.messages[o.messages.length - 1].at, view.serverTime) : clock(readyAt)),
    step(phase === "ready" ? "completed" : readyAt ? "active" : "pending", "Tell the member it's ready", phase === "ready" ? "Ready for pickup" : readyAt ? "Runs on a new sandbox" : "", ""),
  ];
  $("steps").replaceChildren(...steps.map((s, i) =>
    el("li", { class: "step", "data-status": s.status },
      el("span", { class: "dot", text: s.status === "completed" ? "✓" : String(i + 1) }),
      el("span", { class: "step-label", text: s.label }),
      s.detail ? el("span", { class: s.status === "waiting" ? "step-note" : "step-detail", text: s.detail }) : null,
      el("span", { class: "step-time", text: s.time }))));

  const doneLabel = phase === "ready" ? "Ready for pickup" : "Ordered";
  const badge = { none: ["pending", "Ready"], invoking: ["active", "Starting"], working: ["active", "Working"], draining: ["waiting", "Waiting for the member"], waiting: ["waiting", "Waiting for the member"], "kitchen-draining": ["waiting", "Waiting for the kitchen"], kitchen: ["waiting", "Waiting for the kitchen"], "done-draining": ["completed", doneLabel], done: ["completed", doneLabel], closed: ["skipped", "Closed"] }[m];
  $("agent-badge").dataset.status = badge[0];
  $("agent-badge").textContent = badge[1];
  $("zig-stage").dataset.phase = m === "working" || m === "invoking" ? "busy" : m === "waiting" || m === "kitchen" ? "paused" : "idle";
  $("speech").textContent = {
    none: "Send the order from the phone to start.",
    invoking: "Waking up in a new sandbox.",
    working: { placing: "Charging the card and placing the order.", preparing: "Telling the member their order is ready." }[phase] || "Reading the order history, menu, stock and rewards.",
    draining: "Cart's ready. When my sandbox goes idle, AgentCore ends it.",
    waiting: "My sandbox ended. Paying starts a new one, and I pick up right here.",
    "kitchen-draining": "Order's in. When my sandbox goes idle, AgentCore ends it.",
    kitchen: "My sandbox ended. When the kitchen marks the order ready, a new one tells the member.",
    "done-draining": phase === "ready" ? "Charged once, ordered once, and the member knows it's ready. Back to orbit." : `Charged once, ordered once${o?.placed ? `, ${o.placed.points} Stardust posted` : ""}. Back to orbit.`,
    done: phase === "ready" ? "Charged once, ordered once, and the member knows it's ready. Back to orbit." : `Charged once, ordered once${o?.placed ? `, ${o.placed.points} Stardust posted` : ""}. Back to orbit.`,
    closed: "That order closed without a payment.",
  }[m];
  $("model-line").textContent = o?.model ? (o.model.startsWith("stub") ? "stub model, offline" : o.model) : "model";

  const rows = (o?.proof?.rows || []).filter((r) => r.kind === "tool" || r.kind === "checkout");
  const results = work.map((w) => w.text);
  let toolIndex = 0;
  const checkoutText = {
    charge_payment: o?.placed ? `one charge · ${o.placed.paymentId} · ${money(o.placed.amount)} cr` : "",
    place_order: o?.placed ? `${o.placed.orderNumber} · ready ${o.placed.ready}` : "",
    award_stardust: o?.placed ? `${o.placed.points} Stardust posted` : "",
  };
  const items = rows.map((r) => {
    const result = r.kind === "tool" ? (r.status === "completed" ? results[toolIndex++] || "" : "") : checkoutText[r.name] || "";
    const label = { completed: "Done", active: "Running", pending: "Queued", failed: "Failed" }[r.status] || r.status;
    return el("li", {},
      el("span", { class: "status-badge", "data-status": r.status === "failed" ? "retrying" : r.status, text: label }),
      el("code", { text: ENDPOINTS[r.name] || r.name }),
      el("span", { class: "where", text: `${r.kind === "checkout" ? "Idempotency-Key · " : ""}${r.worker ? sandboxName(o, r.worker) : ""}` }),
      result ? el("span", { class: "result", text: result }) : null);
  });
  if (!items.length) items.push(el("li", { class: "empty-row", text: o ? "No tool calls yet." : "Ziggy's requests to the store API show here." }));
  if (o && ["confirm", "chat"].includes(phase)) items.push(el("li", { class: "empty-row", text: "Payment, order and Stardust run after the member pays." }));
  if (phase === "preparing" && !readyAt) items.push(el("li", { class: "empty-row", text: "Ziggy messages the member when the kitchen marks the order ready." }));
  $("tools").replaceChildren(...items);
}

function renderSupportAgent(view, c, s, m) {
  const now = view.serverTime;
  const work = s.work || [];
  const esc = s.escalation;
  const res = s.resolution;
  const phase = s.phase;
  const order = c?.placed;
  const memberSaid = (s.messages || []).some((msg) => msg.from === "member");
  const offered = work.some((w) => w.tool === "offer_choices");
  const acting = esc || res || phase === "resolving";
  const step = (status, label, detail, time = "") => ({ status, label, detail, time });
  const steps = [
    step("completed", "Read the order", order ? `${order.orderNumber} · ${money(order.amount)} cr paid · passed in when the chat started` : ""),
    step(memberSaid ? "completed" : s.status === "Running" ? "waiting" : "skipped", "Hear what went wrong", memberSaid ? "" : "Waiting on the member", !memberSaid && s.idleUntil ? `closes in ${countdown(s.idleUntil, now)}` : ""),
    step(phase === "choosing" ? "waiting" : offered ? "completed" : "pending", "Offer choices · offer_choices", phase === "choosing" ? "Tool call waiting on the member's tap" : "", phase === "choosing" && s.idleUntil ? `closes in ${countdown(s.idleUntil, now)}` : ""),
    step(acting ? "completed" : "pending", "Café policy check · inside the tool", esc ? `Over the limit: ${esc.why}` : res ? "Within the limit: no approval needed" : "Plain code; the agent never sees it"),
    step(!esc ? (res ? "skipped" : "pending") : esc.decidedBy ? (esc.approved ? "completed" : "skipped") : "waiting", "Human review · inside the tool",
      !esc ? (res ? "Not needed" : "") : esc.decidedBy ? (esc.decidedBy === "timer" ? "Nobody reviewed it: approved by the timer" : `${esc.approved ? "Approved" : "Declined"} by ${esc.decidedBy === "support" ? "a support agent" : "a manager"}`) : `Waiting on ${esc.tier === "support" ? "a support agent" : "a manager"} · the agent doesn't know`,
      esc && !esc.decidedBy && phase === "review" ? `${esc.tier === "support" ? "to a manager" : "auto-approves"} in ${countdown(esc.until, now)}` : ""),
    step(res ? "completed" : phase === "resolving" ? "active" : "pending", "Carry it out · store Activity", res ? res.summary : ""),
    step(s.status !== "Running" ? "completed" : "pending", "Close the chat", s.closed ? (s.closed.outcome === "resolved" ? "Resolved" : "Expired: no reply from the member") : ""),
  ];
  $("agent-title").textContent = "Ziggy is helping with the order";
  $("steps").replaceChildren(...steps.map((st, i) =>
    el("li", { class: "step", "data-status": st.status },
      el("span", { class: "dot", text: st.status === "completed" ? "✓" : String(i + 1) }),
      el("span", { class: "step-label", text: st.label }),
      st.detail ? el("span", { class: st.status === "waiting" ? "step-note" : "step-detail", text: st.detail }) : null,
      el("span", { class: "step-time", text: st.time }))));

  const badge = { invoking: ["active", "Starting"], working: ["active", "Working"], "s-draining": ["waiting", phase === "review" ? "Tool waiting on review" : "Waiting for the member"], "s-member": ["waiting", "Waiting for the member"], "s-review": ["waiting", "Tool waiting on review"], "done-draining": ["completed", "Chat closed"], "s-closed": ["completed", "Chat closed"] }[m];
  $("agent-badge").dataset.status = badge[0];
  $("agent-badge").textContent = badge[1];
  $("zig-stage").dataset.phase = m === "working" || m === "invoking" ? "busy" : m === "s-member" || m === "s-review" ? "paused" : "idle";
  $("speech").textContent = {
    invoking: "Waking up in a new sandbox.",
    working: phase === "resolving" ? "Calling the store." : "Thinking it over.",
    "s-draining": "Waiting. When my sandbox goes idle, AgentCore ends it.",
    "s-member": "My sandbox ended. The member's answer starts a new one.",
    "s-review": "My tool call hasn't come back yet, and my sandbox ended. I don't know why it's waiting.",
    "done-draining": "All sorted. Back to orbit.",
    "s-closed": s.closed?.outcome === "expired" ? "The member went quiet, so the chat closed." : "All sorted. Back to orbit.",
  }[m];
  $("model-line").textContent = s.model ? (s.model.startsWith("stub") ? "stub model, offline" : s.model) : "model";

  // The agent's tools run in the Workflow; the store calls they make are Activities.
  $("tools-title").textContent = "Tool calls · in the Workflow, then the store API";
  const chatStart = new Date(s.messages?.[0]?.at || 0);
  const storeRows = (c?.proof?.rows || []).filter((r) => r.kind === "checkout" && ["issue_refund", "place_comp", "award_stardust"].includes(r.name) && new Date(r.at) >= chatStart);
  const calls = [
    ...work.map((w) => ({ at: w.at, li: el("li", {},
      el("span", { class: "status-badge", "data-status": w.status, text: w.status === "completed" ? "Done" : "Failed" }),
      el("code", { text: w.tool }),
      el("span", { class: "where", text: "tool · in the Workflow" }),
      el("span", { class: "result", text: w.text })) })),
    ...storeRows.map((r) => ({ at: r.at, li: el("li", {},
      el("span", { class: "status-badge", "data-status": r.status === "failed" ? "retrying" : r.status, text: { completed: "Done", active: "Running", pending: "Queued", failed: "Failed" }[r.status] || r.status }),
      el("code", { text: ENDPOINTS[r.name] || r.name }),
      el("span", { class: "where", text: `Activity · Idempotency-Key · ${r.worker ? sandboxName(c, r.worker) : ""}` }),
      res ? el("span", { class: "result", text: res.summary }) : null) })),
  ].sort((a, b) => new Date(a.at) - new Date(b.at)).map((x) => x.li);
  if (phase === "choosing") calls.push(el("li", {}, el("span", { class: "status-badge", "data-status": "waiting", text: "Waiting" }), el("code", { text: "offer_choices" }), el("span", { class: "where", text: "tool · in the Workflow" }), el("span", { class: "result", text: "waiting for the member's tap" })));
  if (phase === "review" && esc) calls.push(el("li", {}, el("span", { class: "status-badge", "data-status": "waiting", text: "Waiting" }), el("code", { text: esc.tool }), el("span", { class: "where", text: "tool · in the Workflow" }), el("span", { class: "result", text: `${esc.what}: waiting for review` })));
  if (!calls.length) calls.push(el("li", { class: "empty-row", text: "Ziggy's tool calls show here." }));
  $("tools").replaceChildren(...calls);
}

function renderKitchen(view, o, m, s) {
  for (const id of ["staff-approve", "staff-deny"]) $(id).hidden = !s;
  $("kitchen-ready").hidden = !!s;
  if (s) return renderSupportDesk(view, s);
  $("kitchen-title").textContent = view.profile ? `Kitchen · ${view.profile.cafe}` : "Kitchen";
  const marked = kitchenSignaled(o);
  if (o?.phase !== "preparing" || marked) state.marking = false;
  const number = o?.placed?.orderNumber;
  const [status, label, text] =
    o?.phase === "ready" ? ["completed", "Ready", `Order ${number} is ready. Ziggy told the member.`]
    : o?.phase === "preparing" && marked ? ["active", "Marked ready", `Order ${number} is ready. Ziggy is telling the member.`]
    : o?.phase === "preparing" ? ["waiting", "Preparing", `Order ${number} is on the line, ready about ${o.placed.ready}. Mark it ready when it's done.`]
    : o?.phase === "placing" ? ["active", "Incoming", "A paid order is on its way."]
    : o?.phase === "placed" ? ["skipped", "Not marked", `Order ${number} was not marked ready in time.`]
    : ["pending", o ? "No paid order" : "No order", "Paid orders show here."];
  $("kitchen-badge").dataset.status = status;
  $("kitchen-badge").textContent = label;
  $("kitchen-text").textContent = text;
  const btn = $("kitchen-ready");
  btn.disabled = !(o?.phase === "preparing" && o.status === "Running" && !marked && !state.marking);
  btn.textContent = state.marking ? "Marking ready…" : marked || o?.phase === "ready" ? "Marked ready" : "Mark order ready";
}

function renderSupportDesk(view, s) {
  $("kitchen-title").textContent = "Support desk";
  const esc = s.escalation;
  const pending = esc && !esc.decidedBy && s.phase === "review";
  if (!pending) state.reviewing = false;
  const who = esc?.tier === "manager" ? "a manager" : "a support agent";
  const [status, label, text] =
    pending ? ["waiting", esc.tier === "manager" ? "Manager review" : "Needs review", `${esc.what} for "${esc.reason}". Over the café's limit: ${esc.why}. Waiting on ${who}; ${esc.tier === "support" ? "goes to a manager" : "approved automatically"} in ${countdown(esc.until, view.serverTime)}.`]
    : esc?.decidedBy ? [esc.approved ? "completed" : "skipped", esc.approved ? "Approved" : "Declined", `${esc.what}: ${esc.decidedBy === "timer" ? "nobody reviewed it, so the timer approved it" : `${esc.approved ? "approved" : "declined"} by ${esc.decidedBy === "support" ? "a support agent" : "a manager"}`}.`]
    : ["pending", "Nothing to review", "Requests over the café's limits wait here for a person, and the agent's tool call waits with them."];
  $("kitchen-badge").dataset.status = status;
  $("kitchen-badge").textContent = label;
  $("kitchen-text").textContent = text;
  $("staff-approve").disabled = $("staff-deny").disabled = !pending || state.reviewing;
  $("staff-approve").textContent = pending ? `Approve as ${esc.tier === "manager" ? "manager" : "support"}` : "Approve";
}

function renderBackstage(view, o, m) {
  const list = sandboxes(view, o);
  const rows = o?.proof?.rows || [];
  const span = (identity) => {
    const mine = rows.filter((r) => r.worker === identity || (r.kind === "worker" && r.name === identity));
    return mine.length ? `${clock(mine[0].at)} to ${clock(mine[mine.length - 1].at)}` : "";
  };
  $("sandboxes").replaceChildren(...list.map((s) =>
    el("li", {},
      el("span", { class: "status-badge", "data-status": s.live ? "active" : "skipped", text: s.live ? "Running" : "Ended" }),
      el("span", { text: `Sandbox ${s.n}` }),
      el("span", { class: "detail", text: `${s.identity}${s.live ? (view.worker.startedAt ? ` · started ${clock(view.worker.startedAt)}` : "") : ` · ${span(s.identity)}`}` }))));
  if (!list.length) $("sandboxes").append(el("li", {}, el("span", { class: "status-badge", "data-status": "pending", text: "None" }), el("span", { text: "No sandbox has run for this order yet" })));
  const running = view?.worker?.running;
  $("compute-badge").className = `badge ${running ? "good" : "idle"}`;
  $("compute-badge").textContent = running ? "1 running" : "Idle · 0 running";
  const showBanner = ["waiting", "kitchen", "s-member", "s-review"].includes(m);
  $("no-sandbox").hidden = !showBanner;
  const last = list.length ? `Sandbox ${list[list.length - 1].n}` : "The sandbox";
  if (m === "waiting") $("no-sandbox-text").textContent = `${last} drained while the member looked at the cart, and AgentCore ended it. Nothing runs or bills now. Tapping Pay starts a new one.`;
  if (m === "s-member") $("no-sandbox-text").textContent = `${last} drained while the member decides, and AgentCore ended it. Nothing runs or bills now. Their tap or message starts a new one.`;
  if (m === "s-review") $("no-sandbox-text").textContent = `${last} drained in the middle of the agent's tool call, which is waiting for a review. Nothing runs or bills now. A decision, or the timer, starts a new one.`;
  if (m === "kitchen") $("no-sandbox-text").textContent = `${last} drained while the kitchen makes the order, and AgentCore ended it. Nothing runs or bills now. Marking the order ready starts a new one.`;

  const entries = o?.ledger?.entries || [];
  const count = (kind) => entries.filter((e) => e.kind === kind).length;
  const payment = entries.find((e) => e.kind === "payment")?.result;
  const order = entries.find((e) => e.kind === "order")?.result;
  $("stat-charges").textContent = count("payment");
  $("stat-charges-note").textContent = payment ? `${money(payment.amount)} cr · ${payment.paymentId}` : "none until the member pays";
  $("stat-orders").textContent = count("order");
  $("stat-orders-note").textContent = order ? `${order.orderNumber} · ${order.cafeName}` : o?.quote ? "cart only" : "no cart yet";
  $("stat-twice").textContent = Math.max(0, count("payment") - 1);
  $("stat-replays").textContent = `duplicates rejected: ${o?.ledger?.replays ?? 0}`;
  const refunds = entries.filter((e) => e.kind === "refund").map((e) => e.result);
  const comps = entries.filter((e) => e.kind === "comp").map((e) => e.result);
  const orderAward = entries.find((e) => e.kind === "stardust");
  const gifts = entries.filter((e) => e.kind === "stardust" && e !== orderAward).map((e) => e.result);
  $("stat-refunds").textContent = money(refunds.reduce((t, r) => t + Number(r.amount), 0));
  $("stat-refunds-note").textContent = refunds.length ? refunds.map((r) => r.refundId).join(", ") : "no refunds";
  $("stat-goodwill").textContent = comps.length + gifts.length;
  $("stat-goodwill-note").textContent = [...comps.map((x) => `${x.orderNumber} · ${money(x.value)} cr, charged 0`), ...gifts.map((g) => `+${g.points} Stardust`)].join(" · ") || "nothing given";
  $("stat-model").textContent = o?.proof?.modelCalls ?? 0;
  $("stat-repeats").textContent = `repeated after resume: ${o?.proof?.repeats ?? 0}`;

  const text = (r) => {
    switch (r.kind) {
      case "worker": return [`${sandboxName(o, r.name)} picked up work`, "active"];
      case "signal": return [`Signal received · ${r.name}`, "completed"];
      case "timer": return ["Timer fired · no Signal came in time", "waiting"];
      case "model": return [`invoke_model ${r.status} · ${sandboxName(o, r.worker)}`, r.status];
      default: return [`${r.name} ${r.status} · ${ENDPOINTS[r.name] || ""}`, r.status];
    }
  };
  const events = rows.slice(-14).map((r) => {
    const [t, status] = text(r);
    return el("li", { "data-status": status === "pending" ? "pending" : status === "failed" ? "retrying" : status }, el("time", { text: clock(r.at) }), el("span", { text: t }));
  });
  if (m === "s-member") events.push(el("li", { "data-status": "waiting" }, el("time", { text: clock(view.serverTime) }), el("span", { text: "Waiting for Signal · message or choose, or the idle timer · no sandbox" })));
  if (m === "s-review") events.push(el("li", { "data-status": "waiting" }, el("time", { text: clock(view.serverTime) }), el("span", { text: "Waiting for Signal · review, or the review timer · no sandbox" })));
  if (o && m === "kitchen") events.push(el("li", { "data-status": "waiting" }, el("time", { text: clock(view.serverTime) }), el("span", { text: "Waiting for Signal · kitchen_ready · no sandbox" })));
  if (o && m === "waiting") events.push(el("li", { "data-status": "waiting" }, el("time", { text: clock(view.serverTime) }), el("span", { text: "Waiting for Signal · pay · no sandbox" })));
  if (!events.length) events.push(el("li", {}, el("time", { text: "" }), el("span", { text: "No history yet." })));
  $("events").replaceChildren(...events);
}

// ---------- phone sizing ----------
function fitPhone() {
  const stage = $("phone-stage");
  const scale = Math.min(stage.clientWidth / 393, stage.clientHeight / 852);
  $("phone").style.setProperty("--phone-scale", String(scale));
  $("phone-fit").style.width = `${393 * scale}px`;
  $("phone-fit").style.height = `${852 * scale}px`;
}

// ---------- wiring ----------
document.addEventListener("DOMContentLoaded", () => {
  fitPhone();
  window.addEventListener("resize", fitPhone);
  const text = $("composer-text");
  text.addEventListener("input", () => (text.dataset.touched = "1"));
  text.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      $("composer").requestSubmit();
    }
  });
  $("composer").addEventListener("submit", (e) => {
    e.preventDefault();
    const value = text.value.trim();
    if (!value || text.disabled) return;
    text.value = "";
    if (!state.view?.order && !state.pending) startOrder(value);
    else if (state.supportId) sendMessage(value);
    else sendMessage(value);
  });
  $("kitchen-ready").addEventListener("click", markReady);
  $("staff-approve").addEventListener("click", () => review(true));
  $("staff-deny").addEventListener("click", () => review(false));
  $("chat").addEventListener("click", (e) => {
    const btn = e.target.closest("button[data-action]");
    if (!btn || btn.disabled) return;
    if (btn.dataset.action === "help") startSupport();
    if (btn.dataset.action === "choose") choose(btn.dataset.option);
  });
  $("new-order").addEventListener("click", () => {
    setOrder(null);
    state.view = { ...state.view, order: null };
    text.dataset.touched = "";
    text.value = DEFAULT_REQUEST;
    render();
    poll();
  });
  render();
  poll();
});
