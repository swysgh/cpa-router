// §5.3 交互测试（Node，无浏览器）
//
// 读 panel.html，抽出 <script> 块，在 vm 沙箱里用一个假 DOM 跑起来，然后驱动
// 「新建组 / 编辑组 -> 选择成员 -> 保存」的真实路径，断言真正 POST 出去的 body。
//
// 运行：node scripts/panel-member-pick.test.mjs
// 失败退出码非 0，并打印 PASS n / FAIL m。
//
// 期望值全部写死为字面量，不复制实现里的判定逻辑。

import fs from "node:fs";
import path from "node:path";
import vm from "node:vm";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const panelPath = path.join(here, "..", "panel.html");
const html = fs.readFileSync(panelPath, "utf8");

const scriptMatch = html.match(/<script>([\s\S]*?)<\/script>/);
if (!scriptMatch) {
  console.error("FAIL  panel.html 里找不到 <script> 块");
  process.exit(1);
}
const code = scriptMatch[1];

// ---------------------------------------------------------------------------
// 假 DOM
// ---------------------------------------------------------------------------

function descendants(node) {
  const out = [];
  for (const c of node.children) {
    out.push(c);
    out.push(...descendants(c));
  }
  return out;
}

function matchesSimple(el, sel) {
  if (sel.startsWith("#")) return el.id === sel.slice(1);
  if (sel.startsWith(".")) return el.classList.contains(sel.slice(1));
  return el.tagName === sel.toUpperCase();
}

function selectAllSimple(root, selector) {
  const parts = String(selector).trim().split(/\s+/);
  let scope = [root];
  for (const part of parts) {
    const next = [];
    for (const n of scope) {
      for (const d of descendants(n)) if (matchesSimple(d, part)) next.push(d);
    }
    scope = next;
  }
  return scope;
}

class Element {
  constructor(tag) {
    this.tagName = String(tag || "div").toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.attributes = {};
    this.dataset = {};
    this.style = {};
    this._text = "";
    this._html = "";
    this._classes = new Set();
    this.value = "";
    this.checked = false;
    this.disabled = false;
    this.draggable = false;
    this.type = "";
    this.id = "";
    this.title = "";
    this.placeholder = "";
    this.dataTransfer = null;
    this.onclick = null;
    this.onchange = null;
    this.oninput = null;
    this.ondragstart = null;
    this.ondragend = null;
    this.ondragover = null;
    this.ondrop = null;
    const self = this;
    this.classList = {
      add(...c) { c.forEach((x) => self._classes.add(x)); },
      remove(...c) { c.forEach((x) => self._classes.delete(x)); },
      contains(c) { return self._classes.has(c); },
      toggle(c, force) {
        const on = force === undefined ? !self._classes.has(c) : !!force;
        if (on) self._classes.add(c);
        else self._classes.delete(c);
        return on;
      },
      toString() { return [...self._classes].join(" "); },
    };
  }
  get className() { return [...this._classes].join(" "); }
  set className(v) { this._classes = new Set(String(v || "").split(/\s+/).filter(Boolean)); }
  get textContent() { return this._text; }
  set textContent(v) {
    this.children.forEach((c) => { c.parentNode = null; });
    this.children = [];
    this._text = String(v);
  }
  get innerHTML() { return this._html; }
  // 注意：假 DOM 不解析 HTML 字符串。写在 innerHTML 里的元素不会变成 children；
  // 选择弹窗里的 chip 因此必须用 createElement + onclick 属性创建。
  set innerHTML(v) {
    this.children.forEach((c) => { c.parentNode = null; });
    this.children = [];
    this._html = String(v);
  }
  appendChild(child) {
    if (child.parentNode) child.parentNode.removeChild(child);
    this.children.push(child);
    child.parentNode = this;
    return child;
  }
  removeChild(child) {
    const i = this.children.indexOf(child);
    if (i >= 0) this.children.splice(i, 1);
    child.parentNode = null;
    return child;
  }
  insertBefore(node, ref) {
    if (node.parentNode) node.parentNode.removeChild(node);
    const i = ref ? this.children.indexOf(ref) : -1;
    if (i >= 0) this.children.splice(i, 0, node);
    else this.children.push(node);
    node.parentNode = this;
    return node;
  }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  get firstChild() { return this.children[0] || null; }
  get nextSibling() {
    if (!this.parentNode) return null;
    const i = this.parentNode.children.indexOf(this);
    return this.parentNode.children[i + 1] || null;
  }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attributes, k) ? this.attributes[k] : null; }
  getBoundingClientRect() { return { top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0 }; }
  querySelector(sel) { return selectAllSimple(this, sel)[0] || null; }
  querySelectorAll(sel) { return selectAllSimple(this, sel); }
  addEventListener() {}
  removeEventListener() {}
  focus() {}
  blur() {}
  scrollIntoView() {}
}

const byId = new Map();
function getById(id) {
  if (!byId.has(id)) {
    const e = new Element("div");
    e.id = id;
    byId.set(id, e);
  }
  return byId.get(id);
}

const documentElement = new Element("html");
function selectAllDocument(selector) {
  const parts = String(selector).trim().split(/\s+/);
  let scope;
  let i = 0;
  if (parts[0].startsWith("#")) {
    const el = byId.get(parts[0].slice(1));
    if (!el) return [];
    scope = [el];
    i = 1;
  } else {
    scope = [documentElement];
  }
  for (; i < parts.length; i++) {
    const next = [];
    for (const n of scope) {
      for (const d of descendants(n)) if (matchesSimple(d, parts[i])) next.push(d);
    }
    scope = next;
  }
  return scope;
}

const document = {
  documentElement,
  body: new Element("body"),
  createElement: (t) => new Element(t),
  getElementById: getById,
  querySelector: (sel) => selectAllDocument(sel)[0] || null,
  querySelectorAll: (sel) => selectAllDocument(sel),
  addEventListener() {},
  removeEventListener() {},
};

function makeStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: (k) => { m.delete(k); },
    clear: () => m.clear(),
  };
}

// ---------------------------------------------------------------------------
// 假 fetch：/state 样本 + runtime-models 清单 + 记录每次请求
// ---------------------------------------------------------------------------

const stateSample = {
  version: 1,
  plugin: { version: "0.1.0", name: "cpa-router" },
  config: {
    state_file: "plugins/cpa-router/groups.yaml",
    name_prefix: "",
    reload_interval: "2s",
    max_attempts: 6,
    attempt_timeout: "120s",
    total_timeout: "600s",
    all_cooling_policy: "wait",
    max_wait: "15s",
    log_level: "info",
    cooldown: { disable: false, statuses: [429] },
  },
  state_file: { path: "/CLIProxyAPI/plugins/cpa-router/groups.yaml", exists: true, mtime: 1759900000 },
  groups: [
    {
      name: "smart", call_name: "smart", strategy: "fallback", enabled: true,
      description: "", aliases: [], group_total: 0, group_ok: 0, group_fail: 0,
      members: [{ type: "model", name: "ink/glm-5.3", enabled: true }],
    },
    {
      name: "GLM", call_name: "GLM", strategy: "fallback", enabled: true,
      description: "", aliases: [], group_total: 128, group_ok: 127, group_fail: 1,
      members: [{ type: "model", name: "ink/glm-5.3", enabled: true }],
    },
    {
      name: "wrapper", call_name: "wrapper", strategy: "fallback", enabled: true,
      description: "", aliases: [], group_total: 3, group_ok: 3, group_fail: 0,
      members: [{ type: "group", name: "GLM", enabled: true }],
    },
  ],
  models: {
    "ink/glm-5.3": {
      cooldown_until: 1759900307, cooldown_secs: 184, cooldown_level: 2,
      last_status: 429, last_error: "rate limited", total: 40, ok: 38, fail: 2,
      status_429: 2, status_5xx: 0, last_used_at: 1759900000, avg_latency_ms: 1840,
    },
  },
  now: 1759900123,
};

const requests = [];
function fakeFetch(url, opts = {}) {
  const u = String(url);
  requests.push({ url: u, opts });
  let payload;
  if (u.includes("/plugins/cpa-router/state")) payload = stateSample;
  else if (u.includes("/model-prices/runtime-models")) payload = { models: ["ink/glm-5.3", "ink/other-model"] };
  else if (u.includes("/config")) payload = { "openai-compatibility": [] };
  else payload = { ok: true };
  return Promise.resolve({
    ok: true,
    status: 200,
    json: async () => payload,
  });
}

// ---------------------------------------------------------------------------
// 沙箱
// ---------------------------------------------------------------------------

const win = {};
win.self = win;
win.top = win;
win.location = { href: "http://localhost/", host: "localhost", search: "", pathname: "/" };

const sandbox = {
  window: win,
  location: win.location,
  navigator: { userAgent: "panel-member-pick.test.mjs" },
  history: { replaceState() {} },
  document,
  getComputedStyle: () => ({ getPropertyValue: () => "" }),
  localStorage: makeStorage(),
  sessionStorage: makeStorage(),
  fetch: fakeFetch,
  TextEncoder,
  TextDecoder,
  atob,
  btoa,
  URLSearchParams,
  setTimeout: () => 0,
  setInterval: () => 0,
  clearTimeout() {},
  clearInterval() {},
  confirm: () => true,
  console,
};

vm.createContext(sandbox);
vm.runInContext(code, sandbox, { filename: "panel.html" });

const flush = async (rounds = 10) => {
  for (let i = 0; i < rounds; i++) await new Promise((r) => setImmediate(r));
};

const els = {
  newGroup: document.getElementById("newGroup"),
  save: document.getElementById("save"),
  members: document.getElementById("members"),
  addMember: document.getElementById("addMember"),
  pickSearch: document.getElementById("pickSearch"),
  pickDone: document.getElementById("pickDone"),
  pickList: document.getElementById("pickList"),
};

function groupPosts() {
  return requests.filter(
    (r) => r.url.includes("/plugins/cpa-router/groups") && (r.opts.method || "GET") === "POST",
  );
}
function lastGroupBody() {
  const posts = groupPosts();
  if (!posts.length) throw new Error("没有捕获到 POST /plugins/cpa-router/groups");
  return JSON.parse(posts[posts.length - 1].opts.body);
}
function pickChips() {
  return document.querySelectorAll("#pickList .pickchip");
}
function chipByName(name) {
  return pickChips().find((c) => c.dataset.name === name) || null;
}
function memberRows() {
  return els.members.children.filter((c) => c.classList.contains("mrow"));
}
function openNewGroup() {
  els.newGroup.onclick();
  // 保存要求组名非空；否则会在写请求前就 return。
  document.getElementById("f_name").value = "panel-test-group";
}
// 新建组 -> 等模型目录加载 -> 打开选择弹窗 -> 等渲染。
async function preparePicker() {
  openNewGroup();
  await flush();
  els.addMember.onclick();
  await flush();
}
function searchFor(v) {
  els.pickSearch.value = v;
  els.pickSearch.oninput();
}
function deepEq(actual, expected) {
  const a = JSON.stringify(actual);
  const b = JSON.stringify(expected);
  if (a !== b) throw new Error(`期望 ${b}，实际 ${a}`);
}

// ---------------------------------------------------------------------------
// 断言
// ---------------------------------------------------------------------------

let pass = 0;
let fail = 0;
async function run(name, fn) {
  try {
    await fn();
    pass++;
    console.log("PASS  " + name);
  } catch (e) {
    fail++;
    console.log("FAIL  " + name + " :: " + ((e && e.message) || e));
  }
}

await flush();

await run("选择弹窗候选里含组调用名 smart", async () => {
  await preparePicker();
  const names = pickChips().map((c) => c.dataset.name);
  if (!names.includes("smart")) throw new Error("候选里没有组调用名 smart: " + JSON.stringify(names));
  els.pickDone.onclick();
});

await run("搜 smart 选 chip -> POST body 里是 {group:'smart',enabled:true}", async () => {
  await preparePicker();
  searchFor("smart");
  const chip = chipByName("smart");
  if (!chip) throw new Error("搜索 smart 后没有 smart chip");
  chip.onclick();
  els.pickDone.onclick();
  await els.save.onclick();
  await flush();
  deepEq(lastGroupBody().group.members[0], { group: "smart", enabled: true });
});

await run("搜 ink/glm-5.3 选 chip -> POST body 里是 {model:'ink/glm-5.3',enabled:true}", async () => {
  await preparePicker();
  searchFor("ink/glm-5.3");
  const chip = chipByName("ink/glm-5.3");
  if (!chip) throw new Error("搜索 ink/glm-5.3 后没有对应 chip");
  chip.onclick();
  els.pickDone.onclick();
  await els.save.onclick();
  await flush();
  deepEq(lastGroupBody().group.members[0], { model: "ink/glm-5.3", enabled: true });
});

await run("编辑含 {group:'GLM'} 的组、原样保存 -> 该成员仍是 {group:'GLM',enabled:true}", async () => {
  const wrapper = stateSample.groups.find((g) => g.name === "wrapper");
  if (!wrapper) throw new Error("样本里缺少 wrapper 组");
  if (typeof sandbox.openModal !== "function") throw new Error("沙箱里拿不到 openModal()");
  sandbox.openModal(wrapper);
  await flush();
  // 什么都不改，直接保存
  await els.save.onclick();
  await flush();
  deepEq(lastGroupBody().group.members[0], { group: "GLM", enabled: true });
});

await run("两行成员点第二行 ↑ -> 保存后 members 顺序反转", async () => {
  const two = {
    name: "two", call_name: "two", strategy: "fallback", enabled: true,
    description: "", aliases: [],
    members: [
      { type: "model", name: "ink/glm-5.3", enabled: true },
      { type: "group", name: "GLM", enabled: true },
    ],
  };
  sandbox.openModal(two);
  await flush();
  const rows = memberRows();
  if (rows.length !== 2) throw new Error("期望两行成员，实际 " + rows.length);
  const up = rows[1].querySelector(".up");
  if (!up) throw new Error("第二行没有 .up 按钮");
  up.onclick();
  await els.save.onclick();
  await flush();
  deepEq(lastGroupBody().group.members, [
    { group: "GLM", enabled: true },
    { model: "ink/glm-5.3", enabled: true },
  ]);
});

console.log(`PASS ${pass} / FAIL ${fail}`);
process.exit(fail ? 1 : 0);
