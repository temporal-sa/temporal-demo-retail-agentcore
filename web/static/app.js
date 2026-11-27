// The crew order page. It polls GET /api/view once a second while an order runs, one
// request at a time, and draws everything from that one answer. The API reads Temporal
// with Describe, the Event History and Worker heartbeats, never a Query (guide 15).
"use strict";

const DEFAULT_REQUEST = "Same crew order as last Friday, for Titan Ring. Mo's vegan now. Use whatever reward saves the most.";
const FAST_POLL_MS = 1000;
const SLOW_POLL_MS = 4000;

const $ = (id) => document.getElementById(id);
const state = { orderId: new URLSearchParams(location.search).get("order"), view: null, pending: null, paying: false, sending: false, timer: null };

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
    const view = await api("GET", "/api/view" + (state.orderId ? `?order=${state.orderId}` : ""));
    state.view = view;
    if (view.order) state.pending = null;
    $("error-banner").hidden = true;
    render();
    const o = view.order;
    if (o && (o.status === "Running" || view.worker.running)) next = FAST_POLL_MS;
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
  const url = new URL(location.href);
  if (id) url.searchParams.set("order", id);
  else url.searchParams.delete("order");
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

async function sendMessage(text) {
  state.sending = true;
  render();
  try {
    await api("POST", `/api/orders/${state.orderId}/messages`, { text });
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
  if (o.phase === "placed") return running ? "done-draining" : "done";
  if (o.phase === "expired" || o.status !== "Running") return "closed";
  if (o.phase === "confirm" || o.phase === "chat") return running ? "draining" : "waiting";
  if (!running) return "invoking";
  return "working";
}

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
  const m = moment(view, o);
  renderHeader(view, o);
  renderPhone(view, o, m);
  renderHow(view, o, m);
  renderAgent(view, o, m);
  renderBackstage(view, o, m);
}

function renderHeader(view, o) {
  const p = view.profile;
  $("brand-line").textContent = p ? `${p.brand.name} · Ziggy crew orders` : "Ziggy crew orders";
  $("app-brand").textContent = p ? p.brand.name : "Café app";
  $("app-cafe").textContent = p ? `Pickup · ${p.cafe} café` : "Pickup";
  const extra = o?.placed?.points || 0;
  $("app-points").textContent = p ? `${(p.stardust + extra).toLocaleString()} ${p.brand.rewards}` : "";
  $("phone-clock").textContent = new Date(view.serverTime || Date.now()).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  $("session-line").textContent = o?.workflowId ? o.workflowId.replace(/-[0-9a-f]{32}-/, "-…-") : "";
  for (const id of ["temporal-link", "history-link"]) {
    $(id).hidden = !o?.temporalUrl;
    if (o?.temporalUrl) $(id).href = o.temporalUrl;
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
    if (["planning", "placing"].includes(o.phase) && o.status === "Running") items.push(zigLine(null));
  }
  chat.replaceChildren(...items);
  chat.scrollTop = chat.scrollHeight;

  const text = $("composer-text");
  const canStart = !o;
  const canMessage = o && ["confirm", "chat"].includes(o.phase) && o.status === "Running" && !state.sending;
  text.disabled = !(canStart || canMessage);
  $("composer-send").disabled = text.disabled;
  if (canStart && !text.value && !text.dataset.touched) text.value = DEFAULT_REQUEST;
  text.placeholder = canMessage ? "Ask for a change" : o?.phase === "placed" ? "Order placed" : "Ziggy is working";
  if (o && text.value === DEFAULT_REQUEST) text.value = "";

  $("phone-caption").textContent = {
    none: "The member's phone. Send the order to start.",
    invoking: "Temporal is starting a sandbox for Ziggy.",
    working: "Ziggy is working out the order.",
    draining: "Wait about a minute before tapping Pay: the sandbox drains, then AgentCore ends it.",
    waiting: "No sandbox is running. Tapping Pay starts a new one.",
    "done-draining": "Paid once, placed once.",
    done: "Paid once, placed once.",
    closed: "This order is closed. Start a new one.",
  }[m];
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

const FLOW = [
  { kind: "Temporal Cloud", name: "CrewOrderWorkflow" },
  { kind: "AgentCore endpoint", name: "Serverless Worker" },
  { kind: "AgentCore sandbox", name: "Sandbox" },
  { kind: "Activity", name: "Bedrock · Claude Sonnet 5.5" },
  { kind: "Activity → REST", name: "Store API (mock)" },
];

function renderHow(view, o, m) {
  const local = view.workerLocation === "local";
  const live = sandboxes(view, o).find((s) => s.live);
  const liveName = live ? `Sandbox ${live.n}` : "Sandbox";
  const model = o?.model && o.model.startsWith("stub") ? "Stub model (offline)" : "Bedrock · Claude Sonnet 5.5";
  const lit = {
    none: [0, 0, 0, 0, 0], invoking: [1, 1, 0, 0, 0], working: [1, 1, 1, 1, 1], draining: [1, 0, 1, 0, 0],
    waiting: [1, 0, 0, 0, 0], "done-draining": [1, 0, 1, 0, 0], done: [1, 0, 0, 0, 0], closed: [1, 0, 0, 0, 0],
  }[m];
  const notes = [
    m === "waiting" ? "Holds the cart, every model answer and tool result. Waits on a Signal" : "Event History records every step",
    m === "invoking" ? (local ? "The local invoker stands in for Temporal Cloud here" : "Temporal Cloud is invoking it: tasks wait and no Worker polls") : "Invoked when tasks wait and no Worker polls",
    live ? "Temporal Worker + Strands agent, empty on start" : "No sandbox running",
    "Each model call is an Activity",
    "Stateless: menus, stock, rewards, payments, orders",
  ];
  const status = ["waiting", "active", "active", "active", "active"];
  if (m !== "waiting" && m !== "none") status[0] = "completed";
  $("flow").replaceChildren(...FLOW.map((n, i) =>
    el("li", {},
      el("div", { class: "flow-node", "data-status": status[i], "data-lit": lit[i] ? "true" : "false" },
        el("span", { class: "eyebrow", text: n.kind }),
        el("span", { class: "name", text: i === 2 ? (live ? liveName : "No sandbox") : i === 3 ? model : n.name }),
        el("span", { class: "note", text: notes[i] })),
      i < FLOW.length - 1 ? el("span", { class: "flow-arrow", "aria-hidden": "true", "data-lit": lit[i] && lit[i + 1] ? "true" : "false", text: i === 2 ? "+" : "→" }) : null)));

  const debounce = local ? "about 20 seconds" : "30 seconds";
  const how = {
    none: { badge: ["pending", "no order yet"], steps: [["The phone", "starts a CrewOrderWorkflow on Temporal Cloud. Nothing else runs yet."], ["Temporal", "sees a task with no Worker polling and invokes the AgentCore endpoint."], ["AgentCore", "starts a sandbox, which runs a Temporal Worker and the Strands agent."]], callout: ["pending", "Watch for", "The sandbox ending while the member looks at the cart, and a new one finishing the order without repeating anything."] },
    invoking: { badge: ["active", "starting a sandbox"], steps: [["Temporal", "has a task on the Task Queue and no Worker polling it."], [local ? "The local invoker" : "Temporal Cloud", "invokes the AgentCore endpoint, and AgentCore starts a sandbox."], ["The sandbox", "starts with an empty process and a Temporal Worker that polls for the task."]], callout: ["active", "Why a new sandbox can pick up", "Nothing about the order lives in the sandbox. The Event History on Temporal Cloud has all of it."] },
    working: { badge: ["active", `${liveName} is working`], steps: [["The Strands agent", "decides which tools to call. Every model call and tool call is an Activity in the sandbox."], ["Each tool Activity", "is one request to the store API: order history, menu, stock, rewards, a quote."], ["Temporal", "writes every model answer and tool result to the Event History as it completes."]], callout: ["active", "Where the state lives", "In the Event History on Temporal Cloud, not in the sandbox. The sandbox only holds the step in progress."] },
    draining: { badge: ["waiting", `${liveName} is idle`], steps: [["The Workflow", "showed the cart through its memo and waits for the pay Signal. It has no tasks to hand out."], ["The Worker", `sees no Activity for ${debounce}, drains and releases the session.`], ["AgentCore", "then ends the idle sandbox. Wait for it before tapping Pay."]], callout: ["waiting", "What is kept", "Every model answer, every tool result and the priced cart, in the Event History."] },
    waiting: { badge: ["waiting", "waiting for the member, no sandbox"], steps: [["The Workflow", "is waiting for the pay Signal. It has no tasks to hand out."], ["The Worker", "drained, and AgentCore ended the sandbox."], ["Nothing", "is running, and nothing is billed, however long the member takes."]], callout: ["waiting", "What is kept", "The model's answers, the tool results and the priced cart. Run the agent again from scratch and it could build a different cart from the one on the phone."] },
    "done-draining": null,
    done: { badge: ["completed", `order placed by ${liveName === "Sandbox" ? "a new sandbox" : liveName}`], steps: [["The pay Signal", "created a task, and Temporal invoked the AgentCore endpoint again. AgentCore started a new sandbox, a process with no memory."], ["The Worker", "replayed the Event History. Model calls and store reads returned their recorded results, so the agent carried on with the same cart without asking the model again."], ["Only new work ran", "the payment, the order and the Stardust, each sent with the order as its Idempotency-Key, then one model call for the reply."]], callout: ["completed", "Why the card was charged once", "History stops a finished step from running again. The Idempotency-Key covers the one gap left: a sandbox lost mid-call after the store took the payment."] },
    closed: { badge: ["skipped", "order closed"], steps: [["The order", "ended without a payment."], ["Nothing", "was charged."], ["Start", "a new order from the phone."]], callout: ["skipped", "Closed", "Orders wait 30 minutes for the member."] },
  };
  const h = how[m] || how.done;
  $("how-badge").dataset.status = h.badge[0];
  $("how-badge").textContent = `Now: ${h.badge[1]}`;
  $("how-steps").replaceChildren(...h.steps.map(([who, what]) => el("li", {}, el("strong", { text: who }), ` ${what}`)));
  $("how-callout").dataset.status = h.callout[0];
  $("how-callout-title").textContent = h.callout[1];
  $("how-callout-text").textContent = h.callout[2];
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
};

function renderAgent(view, o, m) {
  const work = o?.work || [];
  const done = (tool) => work.some((w) => w.tool === tool && w.status === "completed");
  const quoted = !!o?.quote;
  const phase = o?.phase;
  const signals = (o?.proof?.rows || []).filter((r) => r.kind === "signal");
  const paidAt = signals.find((r) => r.name === "pay")?.at;
  const placedBy = (o?.proof?.rows || []).find((r) => r.name === "charge_payment")?.worker;
  const step = (status, label, detail, time) => ({ status, label, detail, time });
  const steps = [
    step(work.length ? "completed" : o ? "active" : "pending", "Understand the request", o ? "Repeat an order · dietary needs · best reward" : "", ""),
    step(done("get_stock") ? "completed" : work.length ? "active" : "pending", "Rebuild the order for today", done("get_stock") ? "History, menu and stock read from the store" : work.length ? "Reading the store" : "", ""),
    step(quoted ? "completed" : done("get_rewards") ? "active" : "pending", "Price the cart, pick the best reward", quoted ? (o.quote.reward ? `${o.quote.reward.name} saves ${money(o.quote.reward.discount)} cr` : `Total ${money(o.quote.total)} cr`) : "", ""),
    step(phase === "confirm" || phase === "chat" ? "waiting" : paidAt ? "completed" : "pending", "The member confirms the cart", phase === "confirm" ? "Waiting on the member · as long as it takes" : paidAt ? "Paid" : "", phase === "confirm" && view.serverTime && o.messages.length ? ago(o.messages[o.messages.length - 1].at, view.serverTime) : clock(paidAt)),
    step(phase === "placed" ? "completed" : phase === "placing" ? "active" : "pending", "Charge, place, award Stardust", phase === "placed" ? `${sandboxName(o, placedBy)} · nothing before it ran again` : phase === "placing" || phase === "confirm" ? "Runs on a new sandbox" : "", ""),
    step(phase === "placed" ? "completed" : "pending", "Tell the member", phase === "placed" && o.placed ? `Ready ${o.placed.ready} · ${o.placed.points} Stardust` : "", ""),
  ];
  $("steps").replaceChildren(...steps.map((s, i) =>
    el("li", { class: "step", "data-status": s.status },
      el("span", { class: "dot", text: s.status === "completed" ? "✓" : String(i + 1) }),
      el("span", { class: "step-label", text: s.label }),
      s.detail ? el("span", { class: s.status === "waiting" ? "step-note" : "step-detail", text: s.detail }) : null,
      el("span", { class: "step-time", text: s.time }))));

  const badge = { none: ["pending", "Ready"], invoking: ["active", "Starting"], working: ["active", "Working"], draining: ["waiting", "Waiting for the member"], waiting: ["waiting", "Waiting for the member"], "done-draining": ["completed", "Ordered"], done: ["completed", "Ordered"], closed: ["skipped", "Closed"] }[m];
  $("agent-badge").dataset.status = badge[0];
  $("agent-badge").textContent = badge[1];
  $("zig-stage").dataset.phase = m === "working" || m === "invoking" ? "busy" : m === "waiting" ? "paused" : "idle";
  $("speech").textContent = {
    none: "Send the order from the phone to start.",
    invoking: "Waking up in a new sandbox.",
    working: "Reading the order history, menu, stock and rewards.",
    draining: "Cart's ready. When my sandbox goes idle, AgentCore ends it.",
    waiting: "My sandbox ended. Paying starts a new one, and I pick up right here.",
    "done-draining": `Charged once, ordered once${o?.placed ? `, ${o.placed.points} Stardust posted` : ""}. Back to orbit.`,
    done: `Charged once, ordered once${o?.placed ? `, ${o.placed.points} Stardust posted` : ""}. Back to orbit.`,
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
  $("tools").replaceChildren(...items);
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
  const showBanner = m === "waiting";
  $("no-sandbox").hidden = !showBanner;
  if (showBanner) $("no-sandbox-text").textContent = `${list.length ? `Sandbox ${list[list.length - 1].n}` : "The sandbox"} drained while the member looked at the cart, and AgentCore ended it. Nothing runs or bills now. Tapping Pay starts a new one.`;

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
  $("stat-model").textContent = o?.proof?.modelCalls ?? 0;
  $("stat-repeats").textContent = `repeated after resume: ${o?.proof?.repeats ?? 0}`;

  const text = (r) => {
    switch (r.kind) {
      case "worker": return [`${sandboxName(o, r.name)} picked up work`, "active"];
      case "signal": return [`Signal received · ${r.name}`, "completed"];
      case "model": return [`invoke_model ${r.status} · ${sandboxName(o, r.worker)}`, r.status];
      default: return [`${r.name} ${r.status} · ${ENDPOINTS[r.name] || ""}`, r.status];
    }
  };
  const events = rows.slice(-14).map((r) => {
    const [t, status] = text(r);
    return el("li", { "data-status": status === "pending" ? "pending" : status === "failed" ? "retrying" : status }, el("time", { text: clock(r.at) }), el("span", { text: t }));
  });
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
    else sendMessage(value);
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
