const DRIVERS_ENDPOINT = "/api/drivers";
const OPTIONS_ENDPOINT = "/api/options";
const CLICKS_ENDPOINT = "/api/clicks";

const state = {
  page: 1,
  pageSize: 20,
  drivers: [],
  total: 0,
  hits: {},
  hitsTotal: 0,
  // 渲染代次：上报响应回来时若列表已重渲染，就不要再往旧 DOM 上写
  gen: 0
};

const elements = {
  filters: document.querySelector("#filters"),
  keyword: document.querySelector("#keyword"),
  version: document.querySelector("#version"),
  os: document.querySelector("#os"),
  language: document.querySelector("#language"),
  resultCount: document.querySelector("#resultCount"),
  results: document.querySelector("#results"),
  pageInfo: document.querySelector("#pageInfo"),
  prevPage: document.querySelector("#prevPage"),
  nextPage: document.querySelector("#nextPage"),
  pageSize: document.querySelector("#pageSize"),
  totalHits: document.querySelector("#totalHits"),
  totalHitsRow: document.querySelector("#totalHitsRow")
};

const languageDisplayMap = {
  "Chinese (Simplified)": "中文（简体）",
  "Chinese (Traditional)": "中文（繁体）",
  "English (US)": "英语（美式）",
  English: "英语（美式）",
  Chinese: "中文（简体）"
};

function collectFilters() {
  return {
    keyword: elements.keyword.value.trim(),
    version: elements.version.value.trim(),
    os: elements.os.value,
    language: elements.language.value
  };
}

async function loadDrivers(filters) {
  const params = new URLSearchParams();
  Object.entries(filters).forEach(([key, value]) => {
    if (value) params.set(key, value);
  });
  params.set("page", String(state.page));
  params.set("pageSize", String(state.pageSize));

  const response = await fetch(`${DRIVERS_ENDPOINT}?${params.toString()}`);
  if (!response.ok) throw new Error("驱动数据加载失败");

  const payload = await response.json();
  return {
    items: Array.isArray(payload.items) ? payload.items : [],
    total: Number(payload.total ?? 0),
    hits: payload.clicks && typeof payload.clicks === "object" ? payload.clicks : {},
    hitsTotal: Number(payload.clicks_total ?? 0)
  };
}

async function loadOptions() {
  const response = await fetch(OPTIONS_ENDPOINT);
  if (!response.ok) throw new Error("筛选项加载失败");

  const payload = await response.json();
  populateSelect(elements.os, Array.isArray(payload.os) ? payload.os : []);
  populateSelect(
    elements.language,
    Array.isArray(payload.languages) ? payload.languages : [],
    "Chinese (Simplified)"
  );
}

function mapLanguageDisplay(value) {
  const normalized = String(value || "").trim();
  return languageDisplayMap[normalized] || normalized;
}

function populateSelect(select, values, defaultValue) {
  const currentValue = select.value;
  const options = [...new Set(values.filter(Boolean))].sort((a, b) => a.localeCompare(b, "zh-CN"));
  select.querySelectorAll("option:not(:first-child)").forEach((option) => option.remove());

  options.forEach((value) => {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = select.id === "language" ? mapLanguageDisplay(value) : value;
    select.append(option);
  });

  if (currentValue && options.includes(currentValue)) {
    select.value = currentValue;
  } else if (defaultValue && options.includes(defaultValue)) {
    select.value = defaultValue;
  }
}

function escapeHtml(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function escapeOfficialNvidiaURL(value) {
  try {
    const url = new URL(String(value || ""));
    const hostname = url.hostname.toLowerCase();
    const officialHosts = ["nvidia.com", "nvidia.cn", "nvidia.com.tw", "nvidia.co.uk"];
    const isOfficialHost = officialHosts.some(
      (host) => hostname === host || hostname.endsWith(`.${host}`)
    );
    if (!isOfficialHost) return "#";

    // 2019 年及更早的记录里 detail_url 是 http，官方域的 https 均可达，
    // 升级协议而不是直接丢弃，否则这些老驱动的详情链接全是死链。
    if (url.protocol === "http:") url.protocol = "https:";

    // 仍然只放行 https：javascript://www.nvidia.com/%0aalert(1) 这类
    // 会被 URL 解析出官方 hostname，必须靠这一步挡住。
    return url.protocol === "https:" ? escapeHtml(url.href) : "#";
  } catch {
    return "#";
  }
}

function formatCount(value) {
  return Number(value || 0).toLocaleString("zh-CN");
}

// 计数为 0 的记录什么都不渲染，冷启动时页面上不会多出上万个"0 次点击"
function hitsTemplate(driverId) {
  const count = Number(state.hits[driverId] || 0);
  return count > 0 ? `<span class="hits">${formatCount(count)} 次点击</span>` : "";
}

function driverTemplate(driver) {
  const releaseTime = driver.release_time || "未知日期";
  const driverId = Number(driver.id) || 0;
  return `
    <article class="driver-row">
      <div class="driver-title">
        <h2>${escapeHtml(driver.driver_name)}</h2>
        <div class="meta">
          <span class="pill">${escapeHtml(driver.driver_version)}</span>
          <span>${escapeHtml(driver.os)}</span>
          <span>${escapeHtml(driver.language)}</span>
          ${hitsTemplate(driverId)}
        </div>
      </div>
      <span class="date">${escapeHtml(releaseTime)}</span>
      <div class="link-group">
        <a class="text-link text-link--secondary" href="${escapeOfficialNvidiaURL(driver.detail_url)}" data-driver-id="${driverId}" target="_blank" rel="noopener noreferrer">详情</a>
      </div>
    </article>
  `;
}

function renderTotalHits() {
  if (!elements.totalHits || !elements.totalHitsRow) return;
  // 冷启动总数为 0 时整行隐藏，不在首页挂一个"0 次"
  elements.totalHitsRow.hidden = !(state.hitsTotal > 0);
  elements.totalHits.textContent = formatCount(state.hitsTotal);
}

function render() {
  const pageCount = Math.max(1, Math.ceil(state.total / state.pageSize));
  state.page = Math.min(state.page, pageCount);
  state.gen += 1;
  elements.resultCount.textContent = String(state.total);
  elements.pageInfo.textContent = `第 ${state.page} / ${pageCount} 页`;
  elements.prevPage.disabled = state.page <= 1;
  elements.nextPage.disabled = state.page >= pageCount;
  elements.results.innerHTML = state.drivers.length
    ? state.drivers.map(driverTemplate).join("")
    : '<div class="empty">没有匹配的驱动记录</div>';
  renderTotalHits();
}

function showLoading(message) {
  elements.results.innerHTML = `<div class="empty">${escapeHtml(message)}</div>`;
}

function showLoadError() {
  state.drivers = [];
  state.total = 0;
  state.hits = {};
  state.hitsTotal = 0;
  renderTotalHits();
  elements.resultCount.textContent = "0";
  elements.pageInfo.textContent = "第 1 / 1 页";
  elements.prevPage.disabled = true;
  elements.nextPage.disabled = true;
  elements.results.innerHTML = '<div class="empty">数据加载失败，请稍后重试</div>';
}

async function search({ resetPage = true } = {}) {
  if (resetPage) state.page = 1;
  showLoading("正在检索驱动数据...");

  try {
    const result = await loadDrivers(collectFilters());
    state.drivers = result.items;
    state.total = result.total;
    state.hits = result.hits;
    state.hitsTotal = result.hitsTotal;
    render();
  } catch {
    showLoadError();
  }
}

function updateHitsDisplay(driverId) {
  renderTotalHits();

  const link = elements.results.querySelector(`a[data-driver-id="${driverId}"]`);
  const meta = link?.closest(".driver-row")?.querySelector(".meta");
  if (!meta) return;

  let span = meta.querySelector(".hits");
  if (!span) {
    span = document.createElement("span");
    span.className = "hits";
    meta.append(span);
  }
  span.textContent = `${formatCount(state.hits[driverId])} 次点击`;
}

// 用服务端返回的权威计数更新，不做乐观 +1：
// 筛选有 180ms 防抖，乐观数字一定会被随后的重渲染打回去。
async function postClick(driverId) {
  const gen = state.gen;
  try {
    const response = await fetch(CLICKS_ENDPOINT, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id: driverId }),
      keepalive: true
    });
    if (!response.ok) return;

    const payload = await response.json();
    state.hits[driverId] = Number(payload.clicks ?? 0);
    state.hitsTotal = Number(payload.total ?? 0);

    // 列表已经重渲染过，服务端已记账，跳过 DOM 更新即可
    if (state.gen !== gen) return;
    updateHitsDisplay(driverId);
  } catch {
    // 上报失败绝不能影响跳转，静默处理
  }
}

function onDetailClick(event) {
  const link = event.target.closest("a[data-driver-id]");
  if (!link) return;
  // 不 preventDefault：原生跳转、Ctrl+点击、右键菜单的行为完全保留
  if (link.getAttribute("href") === "#") return;

  const driverId = Number(link.dataset.driverId);
  if (!Number.isInteger(driverId) || driverId <= 0) return;
  void postClick(driverId);
}

elements.filters.addEventListener("submit", (event) => {
  event.preventDefault();
  void search();
});

elements.filters.addEventListener("reset", () => {
  window.setTimeout(() => void search(), 0);
});

elements.pageSize.addEventListener("change", () => {
  state.pageSize = Number(elements.pageSize.value);
  void search();
});

elements.prevPage.addEventListener("click", () => {
  state.page -= 1;
  void search({ resetPage: false });
});

elements.nextPage.addEventListener("click", () => {
  state.page += 1;
  void search({ resetPage: false });
});

// 必须委托到 #results：render() 每次都整体重写 innerHTML，
// 逐个给 <a> 绑定会在第一次翻页或筛选后全部失效。
elements.results.addEventListener("click", onDetailClick);
// 中键在后台开新标签页不会触发 click。查驱动的人常这么开一排标签，
// 只监听 click 会系统性漏计。
elements.results.addEventListener("auxclick", (event) => {
  if (event.button === 1) onDetailClick(event);
});

let inputTimer;
elements.filters.addEventListener("input", () => {
  window.clearTimeout(inputTimer);
  inputTimer = window.setTimeout(() => void search(), 180);
});

showLoading("正在加载筛选项...");
loadOptions()
  .then(() => search())
  .catch(showLoadError);
