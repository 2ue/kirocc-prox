// kirocc-pro admin dashboard — vanilla JS, no dependencies.
(() => {
  "use strict";

  const REFRESH_MS = 30_000;
  const AUTO_KEY = "kirocc.admin.autoRefresh";
  const PAGES = ["dashboard", "accounts", "credsfile", "stats", "settings"];

  let refreshTimer = null;
  let currentPage = "dashboard";
  let multiAccount = false;

  // ---------- DOM helpers ------------------------------------------------

  const $ = (id) => document.getElementById(id);

  function el(tag, attrs = {}, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") node.className = v;
      else if (k === "style") Object.assign(node.style, v);
      else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
      else if (v === true) node.setAttribute(k, "");
      else if (v !== false && v !== null && v !== undefined) node.setAttribute(k, v);
    }
    for (const c of children) {
      if (c == null) continue;
      node.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    }
    return node;
  }

  // ---------- formatting -------------------------------------------------

  function fmtNum(n) {
    if (n == null || isNaN(n)) return "—";
    if (n === 0) return "0";
    if (Math.abs(n) >= 1e9) return (n / 1e9).toFixed(2) + "B";
    if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(2) + "M";
    if (Math.abs(n) >= 1e3) return (n / 1e3).toFixed(1) + "k";
    return String(Math.round(n * 100) / 100);
  }

  function fmtRelative(iso) {
    if (!iso) return "—";
    const t = new Date(iso).getTime();
    if (!t || t < 0) return "—";
    const d = Date.now() - t;
    if (d < 0) {
      const ad = -d;
      if (ad < 60_000) return Math.round(ad / 1000) + " 秒后";
      if (ad < 3_600_000) return Math.round(ad / 60_000) + " 分钟后";
      if (ad < 86_400_000) return Math.round(ad / 3_600_000) + " 小时后";
      return Math.round(ad / 86_400_000) + " 天后";
    }
    if (d < 60_000) return Math.round(d / 1000) + " 秒前";
    if (d < 3_600_000) return Math.round(d / 60_000) + " 分钟前";
    if (d < 86_400_000) return Math.round(d / 3_600_000) + " 小时前";
    return Math.round(d / 86_400_000) + " 天前";
  }

  function isZeroTime(iso) {
    if (!iso) return true;
    const t = new Date(iso).getTime();
    return !t || t < 86_400_000; // anything pre-1970+1d is the Go zero value
  }

  // ---------- progress bar ----------------------------------------------

  // colour goes from accent (full) -> muted (empty) via OKLCH interpolation.
  function barColor(remaining, total) {
    if (!total || total <= 0) return "var(--muted)";
    const frac = Math.max(0, Math.min(1, remaining / total));
    // Hue stays in copper band; chroma & lightness fall as we run out.
    const l = 55 + frac * 15;
    const c = 0.05 + frac * 0.10;
    return `oklch(${l}% ${c} 50)`;
  }

  function bar(remaining, total) {
    const pct = total > 0 ? Math.max(0, Math.min(100, (remaining / total) * 100)) : 0;
    const wrap = el("div", { class: "bar" });
    wrap.style.setProperty("--pct", pct + "%");
    wrap.style.setProperty("--bar", barColor(remaining, total));
    wrap.appendChild(el("span"));
    return wrap;
  }

  // ---------- network ----------------------------------------------------

  // Central fetch wrapper:
  //   - tags every request with X-Requested-With so the CSRF middleware
  //     accepts state-changing methods (DELETE / POST / PUT)
  //   - on 401, redirects to the login page (session expired / server
  //     restart rotated the secret)
  async function apiFetch(url, opts) {
    opts = opts || {};
    const headers = Object.assign(
      { Accept: "application/json", "X-Requested-With": "XMLHttpRequest" },
      opts.headers || {},
    );
    const r = await fetch(url, Object.assign({}, opts, { headers }));
    if (r.status === 401) {
      const here = location.pathname + location.hash;
      location.href = "/admin/login?next=" + encodeURIComponent(here);
      throw new Error("session expired, redirecting to login");
    }
    return r;
  }

  async function getJSON(url) {
    const r = await apiFetch(url);
    if (!r.ok) throw new Error(`${r.status} ${r.statusText}: ${url}`);
    return r.json();
  }

  async function postJSON(url) {
    const r = await apiFetch(url, { method: "POST" });
    if (!r.ok) throw new Error(`${r.status} ${r.statusText}: ${url}`);
    return r.json();
  }

  // ---------- renderers --------------------------------------------------

  function renderHealth(h) {
    $("statAccounts").textContent = h.total_accounts;
    $("statAccountsBreakdown").textContent =
      `可用 ${h.active} · 冷却 ${h.cooldown} · 禁用 ${h.disabled}`;
    $("statActive").textContent = h.active;
    $("statCooldown").textContent = h.cooldown;
    $("statDisabled").textContent = h.disabled;
    $("statCredits").textContent = fmtNum(h.total_credits_remaining);
    $("lastUpdated").textContent = "更新于 " + fmtRelative(h.generated_at);
    const openBanner = $("openModeBanner");
    if (openBanner) openBanner.hidden = h.admin_key_set !== false;
    multiAccount = !!h.multi_account;
    const singleBanner = $("singleModeBanner");
    if (singleBanner) singleBanner.hidden = multiAccount;
    const actions = $("accountsActions");
    if (actions) actions.hidden = !multiAccount;
  }

  const STATUS_CN = {
    active: "可用",
    cooldown: "冷却中",
    disabled: "已禁用",
    banned: "已封禁",
    unknown: "未知",
  };

  function statusPill(s) {
    const key = s || "disabled";
    return el("span", { class: "pill " + key }, STATUS_CN[key] || STATUS_CN.unknown);
  }

  // Active region filter for the accounts page. "" = 全部.
  let accountsRegionFilter = "";
  // Last loaded rows (so a filter chip click can re-render without refetch).
  let accountsLastRows = [];

  function renderAccounts(rows) {
    accountsLastRows = rows || [];
    const cards = $("accountsCards");
    if (!cards) return;
    cards.replaceChildren();
    if (!rows || rows.length === 0) {
      cards.appendChild(el("div", { class: "cards-empty muted" }, "暂无账号"));
      $("accountsMeta").textContent = "0 个账号";
      $("accountFilters").hidden = true;
      return;
    }

    // Rebuild the region filter chip set from the current rows. We keep
    // the active one selected; if it's gone after a refresh, fall back to "".
    const regions = Array.from(new Set(rows.map(r => r.region || "").filter(Boolean))).sort();
    rebuildRegionFilter(regions);

    const visible = rows.filter(r =>
      accountsRegionFilter === "" || (r.region || "") === accountsRegionFilter);
    for (const r of visible) {
      cards.appendChild(buildAccountCard(r));
    }
    if (accountsRegionFilter) {
      $("accountsMeta").textContent =
        `${visible.length} / ${rows.length} 个账号 · 过滤地区 ${accountsRegionFilter}`;
    } else {
      $("accountsMeta").textContent = `${rows.length} 个账号`;
    }
  }

  function rebuildRegionFilter(regions) {
    const host = $("accountRegionFilter");
    const wrap = $("accountFilters");
    if (!host || !wrap) return;
    if (regions.length <= 1) {
      // Only one region (or none) — no point showing the filter.
      wrap.hidden = true;
      return;
    }
    wrap.hidden = false;
    // Drop the active region if it's no longer represented.
    if (accountsRegionFilter && !regions.includes(accountsRegionFilter)) {
      accountsRegionFilter = "";
    }
    host.replaceChildren();
    const mk = (val, label) => el("button", {
      class: "preset-chip" + (accountsRegionFilter === val ? " is-active" : ""),
      type: "button",
      "data-region": val,
      onclick: () => {
        accountsRegionFilter = val;
        renderAccounts(accountsLastRows);
      },
    }, label);
    host.appendChild(mk("", "全部"));
    for (const r of regions) host.appendChild(mk(r, r));
  }

  // Map raw auth_type values to cockpit-tools style display strings.
  // "social" with a GitHub-shaped profile_arn → "Signed in with GitHub", etc.
  function authMethodLabel(authType, profileArn) {
    const t = (authType || "").toLowerCase();
    const arn = profileArn || "";
    if (t === "social") {
      if (/github/i.test(arn)) return "Signed in with GitHub";
      if (/google/i.test(arn)) return "Signed in with Google";
      return "Signed in with Social";
    }
    if (t === "idc") return "Signed in with IdC";
    if (t === "iam") return "Signed in with IAM";
    if (t === "builderid" || t === "builder") return "Signed in with Builder ID";
    return authType ? `Signed in with ${authType}` : "";
  }

  // Extract the Kiro user id from a profile ARN.
  // arn:aws:codewhisperer:us-east-1:....:profile/USER_ID  →  USER_ID
  function userIDFromARN(arn) {
    if (!arn) return "";
    const i = arn.lastIndexOf("/");
    if (i < 0) return arn;
    return arn.slice(i + 1);
  }

  function fmtDateYMD(iso) {
    if (!iso || isZeroTime(iso)) return "—";
    const d = new Date(iso);
    if (!d.getTime()) return "—";
    return `${d.getFullYear()}/${String(d.getMonth() + 1).padStart(2,"0")}/${String(d.getDate()).padStart(2,"0")}`;
  }

  function fmtDateTime(iso) {
    if (!iso || isZeroTime(iso)) return "—";
    const d = new Date(iso);
    if (!d.getTime()) return "—";
    return `${d.getFullYear()}/${String(d.getMonth() + 1).padStart(2,"0")}/${String(d.getDate()).padStart(2,"0")} ${String(d.getHours()).padStart(2,"0")}:${String(d.getMinutes()).padStart(2,"0")}`;
  }

  function daysUntil(iso) {
    if (!iso || isZeroTime(iso)) return null;
    const t = new Date(iso).getTime();
    if (!t) return null;
    return Math.max(0, Math.ceil((t - Date.now()) / 86_400_000));
  }

  function buildAccountCard(r) {
    const c = r.credits || {};
    const total = c.total || 0;
    const used  = c.used || 0;
    const rem   = c.remaining || 0;
    const pct   = total > 0 ? Math.round((rem / total) * 100) : null;

    const statusCls = r.status || "disabled";
    const card = el("div", {
      class: "acct-card is-" + statusCls,
      "data-id": r.id,
    });

    // --- head: label + status pill, provider chip below ---
    card.appendChild(el("div", { class: "acct-card-head" },
      el("div", { class: "acct-card-ident" },
        el("div", { class: "acct-card-email" }, r.label || r.id),
        r.plan_name
          ? el("div", { class: "acct-card-plan" }, r.plan_name)
          : null,
      ),
      el("div", { class: "acct-card-meta" },
        el("span", { class: "chip-provider" }, r.provider || "kiro"),
        r.region ? el("span", { class: "chip-region" }, r.region) : null,
        statusPill(r.status),
      ),
    ));

    // Per-account proxy badge (only shown when configured).
    if (r.proxy_url) {
      card.appendChild(el("div", { class: "acct-card-proxy" },
        el("span", { class: "muted small" }, "via "),
        el("code", { class: "mono small" }, r.proxy_url),
      ));
    }

    // --- identity rows: auth method + user id ---
    const authLine = authMethodLabel(r.auth_type, r.profile_arn);
    const userID   = userIDFromARN(r.profile_arn);
    if (authLine || userID) {
      const ident = el("div", { class: "acct-card-identlines" });
      if (authLine) ident.appendChild(el("div", { class: "acct-card-authline" }, authLine));
      if (userID)   ident.appendChild(el("div", { class: "acct-card-userid" },
        el("span", { class: "k" }, "用户 ID: "),
        el("span", { class: "v" }, userID),
      ));
      card.appendChild(ident);
    }

    // --- quota readout (ring + figures + bar) ---
    if (total > 0) {
      const ringClass = "quota-ring" +
        (pct >= 50 ? " is-high" : pct >= 20 ? " is-mid" : " is-low");
      const quotaLabel = (r.provider || "kiro") === "kiro" ? "User Prompt credits" : "credits";
      const ring = el("div", { class: ringClass },
        el("div", { class: "quota-ring-value" }, pct + "%"),
      );
      ring.style.setProperty("--pct", pct);
      ring.style.setProperty("--pct-deg", (pct * 3.6) + "deg");
      card.appendChild(el("div", { class: "acct-card-quota" },
        el("div", { class: "acct-card-quota-label" }, quotaLabel),
        el("div", { class: "acct-card-quota-readout" },
          ring,
          el("div", { class: "acct-card-quota-figures" },
            el("div", {},
              el("span", { class: "mono" }, fmtNum(used)),
              " / ",
              el("span", { class: "mono dim" }, fmtNum(total)),
              el("span", { class: "muted small" }, " used"),
            ),
            el("div", { class: "acct-card-quota-rem" },
              el("span", { class: "mono accent" }, fmtNum(rem)),
              el("span", { class: "muted small" }, " left"),
            ),
          ),
        ),
        bar(rem, total),
      ));
    } else {
      card.appendChild(el("div", { class: "acct-card-noquota muted" },
        "暂无额度数据 · 点击刷新"));
    }

    // --- cycle dates: 配额周期剩余 N 天 + 周期结束 + 上次刷新 ---
    const cycleRows = el("dl", { class: "acct-card-cycle" });
    const addCycle = (k, v) => {
      cycleRows.appendChild(el("dt", {}, k));
      cycleRows.appendChild(el("dd", {}, v));
    };
    const daysLeft = daysUntil(r.next_reset_at);
    if (daysLeft != null) {
      addCycle("配额周期剩余", daysLeft + " 天");
    }
    if (r.next_reset_at && !isZeroTime(r.next_reset_at)) {
      addCycle("周期结束", fmtDateYMD(r.next_reset_at));
    }
    if (r.last_quota_at && !isZeroTime(r.last_quota_at)) {
      addCycle("上次刷新", fmtDateTime(r.last_quota_at));
    }
    if (r.bonus && r.bonus.total > 0) {
      const bonusRem = r.bonus.total - r.bonus.used;
      addCycle("赠送", `${fmtNum(bonusRem)} / ${fmtNum(r.bonus.total)} · ${r.bonus.expires_in_days}d`);
    }
    if (r.status === "cooldown" && r.cooldown_until && !isZeroTime(r.cooldown_until)) {
      addCycle("冷却至", fmtRelative(r.cooldown_until));
    }
    if (cycleRows.childElementCount > 0) card.appendChild(cycleRows);

    // --- 24h activity (compact line) ---
    const s24 = r.stats_24h || {};
    card.appendChild(el("div", { class: "acct-card-24h" },
      el("span", { class: "muted small" }, "24h "),
      el("span", { class: "mono" }, fmtNum(s24.requests || 0)),
      el("span", { class: "muted small" }, " req · "),
      el("span", { class: "mono" }, fmtNum(s24.input_tokens || 0)),
      el("span", { class: "muted small" }, " in · "),
      el("span", { class: "mono" }, fmtNum(s24.output_tokens || 0)),
      el("span", { class: "muted small" }, " out · 上次使用 "),
      el("span", { class: "mono" }, isZeroTime(r.last_used_at) ? "从未" : fmtRelative(r.last_used_at)),
    ));

    // --- actions ---
    const actions = el("div", { class: "acct-card-actions" },
      el("button", {
        class: "btn tiny",
        onclick: () => onRefresh(r.id),
        title: "强制刷新配额",
      }, "↻ 刷新配额"),
      r.status === "disabled" || r.status === "banned"
        ? el("button", { class: "btn tiny", onclick: () => onEnable(r.id) }, "启用")
        : el("button", { class: "btn tiny danger", onclick: () => onDisable(r.id) }, "停用"),
    );
    if (multiAccount) {
      actions.appendChild(el("button", {
        class: "btn tiny danger",
        onclick: () => onDelete(r.id, r.label),
      }, "删除"));
    }
    card.appendChild(actions);

    return card;
  }

  function renderUsage(resp) {
    const body = $("usageBody");
    body.replaceChildren();
    const rows = (resp && resp.rows) || [];
    if (rows.length === 0) {
      body.appendChild(el("tr", { class: "empty" },
        el("td", { colspan: "6", class: "muted" }, "时间窗口内无请求")));
      $("usageMeta").textContent = "0 行";
      return;
    }
    rows.sort((a, b) =>
      (b.input_tokens + b.output_tokens) - (a.input_tokens + a.output_tokens));
    for (const r of rows) {
      body.appendChild(el("tr", {},
        el("td", {}, r.model || "—"),
        el("td", { class: "num mono" }, fmtNum(r.requests)),
        el("td", { class: "num mono" }, fmtNum(r.success) + " / " + fmtNum(r.failed)),
        el("td", { class: "num mono" }, fmtNum(r.input_tokens)),
        el("td", { class: "num mono" }, fmtNum(r.output_tokens)),
        el("td", { class: "num mono" }, fmtLatency(r.avg_latency_ms || 0)),
      ));
    }
    $("usageMeta").textContent = `${rows.length} 个模型`;
  }

  function fmtLatency(ms) {
    if (!ms || ms <= 0) return "—";
    if (ms >= 1000) return (ms / 1000).toFixed(2) + "s";
    return Math.round(ms) + "ms";
  }

  function renderUsageByKey(resp) {
    const body = $("usageByKeyBody");
    if (!body) return;
    body.replaceChildren();
    const rows = (resp && resp.rows) || [];
    if (rows.length === 0) {
      body.appendChild(el("tr", { class: "empty" },
        el("td", { colspan: "7", class: "muted" }, "时间窗口内无请求")));
      $("usageByKeyMeta").textContent = "0 个密钥";
      return;
    }
    rows.sort((a, b) =>
      (b.input_tokens + b.output_tokens) - (a.input_tokens + a.output_tokens));
    for (const r of rows) {
      body.appendChild(el("tr", {},
        el("td", {}, r.label || "—"),
        el("td", { class: "mono small" }, r.api_key_id || "（legacy）"),
        el("td", { class: "num mono" }, fmtNum(r.requests)),
        el("td", { class: "num mono" }, fmtNum(r.success) + " / " + fmtNum(r.failed)),
        el("td", { class: "num mono" }, fmtNum(r.input_tokens)),
        el("td", { class: "num mono" }, fmtNum(r.output_tokens)),
        el("td", { class: "num mono" }, fmtLatency(r.avg_latency_ms || 0)),
      ));
    }
    $("usageByKeyMeta").textContent = `${rows.length} 个密钥在使用`;
  }

  function renderUsageByDevice(resp) {
    const body = $("usageByDeviceBody");
    if (!body) return;
    body.replaceChildren();
    const rows = (resp && resp.rows) || [];
    if (rows.length === 0) {
      body.appendChild(el("tr", { class: "empty" },
        el("td", { colspan: "7", class: "muted" }, "时间窗口内无请求")));
      $("usageByDeviceMeta").textContent = "0 个设备";
      return;
    }
    rows.sort((a, b) =>
      (b.input_tokens + b.output_tokens) - (a.input_tokens + a.output_tokens));
    for (const r of rows) {
      // device = device_id (fingerprint hash); label = UA excerpt set
      // backend-side from CellStats.DeviceLabel.
      const fp = r.device || "—";
      const ua = r.label || "（未知 UA）";
      body.appendChild(el("tr", {},
        el("td", {}, ua),
        el("td", { class: "mono small muted", title: fp }, fp.length > 8 ? fp.slice(0, 8) + "…" : fp),
        el("td", { class: "num mono" }, fmtNum(r.requests)),
        el("td", { class: "num mono" }, fmtNum(r.success) + " / " + fmtNum(r.failed)),
        el("td", { class: "num mono" }, fmtNum(r.input_tokens)),
        el("td", { class: "num mono" }, fmtNum(r.output_tokens)),
        el("td", { class: "num mono" }, fmtLatency(r.avg_latency_ms || 0)),
      ));
    }
    $("usageByDeviceMeta").textContent = `${rows.length} 个设备指纹`;
  }

  // dashUsage24h fetches the per-key + per-device summaries used by the
  // dashboard top row. Independent of the stats page loaders.
  async function loadDashUsage() {
    try {
      // group=model gives cost totals; group=api_key/device give active
      // counts; window=5m sample drives RPM/TPM; window=168h drives
      // 7-day uptime. Five small queries in parallel.
      const [byModel, byKey, byDevice, fiveMin, sevenDay] = await Promise.all([
        getJSON("/admin/usage?window=24h&group=model"),
        getJSON("/admin/usage?window=24h&group=api_key"),
        getJSON("/admin/usage?window=24h&group=device"),
        getJSON("/admin/usage?window=5m&group=model"),
        getJSON("/admin/usage?window=168h&group=model"),
      ]);
      const totals = byModel.totals || {};
      const setTxt = (id, v) => { const e = $(id); if (e) e.textContent = v; };
      setTxt("dashReqs", fmtNum(totals.requests || 0));
      setTxt("dashReqsBreak", `成功 ${fmtNum(totals.success || 0)} · 失败 ${fmtNum(totals.failed || 0)}`);

      const totalTok = (totals.input_tokens || 0) + (totals.output_tokens || 0);
      setTxt("dashTokens", fmtNum(totalTok));
      setTxt("dashTokensBreak", `输入 ${fmtNum(totals.input_tokens || 0)} · 输出 ${fmtNum(totals.output_tokens || 0)}`);

      const keyRows = byKey.rows || [];
      setTxt("dashActiveKeys", keyRows.length);
      // Top key by tokens
      let top = null;
      for (const r of keyRows) {
        const t = (r.input_tokens || 0) + (r.output_tokens || 0);
        if (!top || t > top.total) top = { total: t, label: r.label, id: r.api_key_id };
      }
      if (top && top.total > 0) {
        setTxt("dashTopKeyTokens", fmtNum(top.total));
        setTxt("dashTopKeyLabel", (top.label || "（未命名）") + (top.id ? " · " + top.id : ""));
      } else {
        setTxt("dashTopKeyTokens", "—");
        setTxt("dashTopKeyLabel", "暂无数据");
      }

      const devRows = byDevice.rows || [];
      setTxt("dashActiveDevices", devRows.length);

      // Health & throughput cards (24h + 5min + 7day samples)
      // --- avg latency: rows-weighted mean since we already have avg per row
      let latSumTokens = 0, latSumWeight = 0;
      for (const r of (byModel.rows || [])) {
        if (r.avg_latency_ms > 0) {
          latSumTokens += r.avg_latency_ms * r.requests;
          latSumWeight += r.requests;
        }
      }
      const avgLatency = latSumWeight > 0 ? latSumTokens / latSumWeight : 0;
      setTxt("dashAvgLatency", fmtLatency(avgLatency));

      // --- error rate (24h)
      const reqsT = totals.requests || 0;
      const failsT = totals.failed || 0;
      const errPct = reqsT > 0 ? (failsT / reqsT) * 100 : 0;
      setTxt("dashErrorRate", reqsT > 0 ? errPct.toFixed(1) + "%" : "—");
      setTxt("dashErrorRateFoot", `${fmtNum(failsT)} / ${fmtNum(reqsT)}`);

      // --- RPM & TPM (5-minute window averaged)
      const fmTotals = (fiveMin && fiveMin.totals) || {};
      const fmReqs = fmTotals.requests || 0;
      const fmTokens = (fmTotals.input_tokens || 0) + (fmTotals.output_tokens || 0);
      setTxt("dashRPM", (fmReqs / 5).toFixed(1));
      setTxt("dashTPM", fmtNum(Math.round(fmTokens / 5)));

      // --- 7d uptime
      const sdTotals = (sevenDay && sevenDay.totals) || {};
      const sdReqs = sdTotals.requests || 0;
      const sdSucc = sdTotals.success || 0;
      const sdPct = sdReqs > 0 ? (sdSucc / sdReqs) * 100 : 0;
      setTxt("dashUptime", sdReqs > 0 ? sdPct.toFixed(2) + "%" : "—");
      setTxt("dashUptimeFoot", `${fmtNum(sdSucc)} / ${fmtNum(sdReqs)}`);
    } catch (e) {
      console.warn("loadDashUsage", e);
    }
  }

  function fmtTime(iso) {
    if (!iso) return "—";
    const d = new Date(iso);
    if (isNaN(d.getTime())) return "—";
    const hh = String(d.getHours()).padStart(2, "0");
    const mm = String(d.getMinutes()).padStart(2, "0");
    const ss = String(d.getSeconds()).padStart(2, "0");
    return `${hh}:${mm}:${ss}`;
  }

  function shortTraceID(id) {
    if (!id) return "—";
    return id.length > 12 ? id.slice(0, 12) : id;
  }

  // Cached last-fetched rows so export uses the SAME data that's visible.
  // Pagination is client-side: we keep the full result set in memory and
  // slice per page on every render. Avoids a server-side cursor for what's
  // currently a bounded data set (≤1000 rows).
  let recentLastRows = [];
  let recentPage = 1;       // 1-indexed
  let recentPageSize = 10;

  function renderRecent(rows) {
    recentLastRows = rows || [];
    recentPage = 1;
    refreshRecentFilterOptions(recentLastRows);
    renderRecentPage();
  }

  function renderRecentPage() {
    const body = $("recentBody");
    const all = recentLastRows;
    body.replaceChildren();
    if (all.length === 0) {
      body.appendChild(el("tr", { class: "empty" },
        el("td", { colspan: "11", class: "muted" }, "暂无请求记录")));
      $("recentMeta").textContent = "0 条";
      $("recentPagination").hidden = true;
      return;
    }

    const pageSize = recentPageSize;
    const totalPages = Math.max(1, Math.ceil(all.length / pageSize));
    if (recentPage > totalPages) recentPage = totalPages;
    if (recentPage < 1) recentPage = 1;
    const start = (recentPage - 1) * pageSize;
    const slice = all.slice(start, start + pageSize);

    for (const r of slice) {
      const total = (r.input_tokens || 0) + (r.output_tokens || 0);
      const keyDisp = r.api_key_label
        ? `${r.api_key_label}\n${r.api_key_id?.slice(0, 8) || ''}`
        : (r.api_key_id ? r.api_key_id.slice(0, 8) : '（legacy）');

      body.appendChild(el("tr", {},
        el("td", { class: "mono small" }, fmtTime(r.timestamp)),
        el("td", { class: "mono small" }, r.resolved_model || r.requested_model || "—"),
        el("td", { class: "mono small", title: r.api_key_id || "" }, keyDisp),
        el("td", {
          class: "mono small",
          title: r.device_id ? `指纹 ${r.device_id}` : "",
        }, r.device || (r.device_id ? r.device_id.slice(0, 8) : "—")),
        el("td", {}, statusPill(r.status)),
        el("td", { class: "num mono" }, r.latency_ms > 0 ? fmtLatency(r.latency_ms) : "—"),
        el("td", { class: "num mono" }, fmtNum(r.input_tokens || 0)),
        el("td", { class: "num mono" }, fmtNum(r.output_tokens || 0)),
        el("td", { class: "num mono" }, fmtNum(total)),
      ));
    }

    // Footer: meta + pagination controls
    const showFrom = start + 1;
    const showTo = Math.min(start + pageSize, all.length);
    $("recentMeta").textContent =
      `共 ${all.length} 条 · 第 ${showFrom}–${showTo} 条`;
    const pg = $("recentPagination");
    pg.hidden = totalPages <= 1;
    $("recentPageInfo").textContent = `第 ${recentPage} / ${totalPages} 页`;
    $("recentFirst").disabled = recentPage === 1;
    $("recentPrev").disabled  = recentPage === 1;
    $("recentNext").disabled  = recentPage === totalPages;
    $("recentLast").disabled  = recentPage === totalPages;
  }

  // Pagination actions
  function recentGoToPage(p) {
    const all = recentLastRows;
    const totalPages = Math.max(1, Math.ceil(all.length / recentPageSize));
    recentPage = Math.max(1, Math.min(totalPages, p));
    renderRecentPage();
  }
  function recentChangePageSize(size) {
    const oldFirstIdx = (recentPage - 1) * recentPageSize;
    recentPageSize = size;
    // Try to keep approximately the same scroll position by aligning to the
    // page that contains the previously-first-visible record.
    recentPage = Math.floor(oldFirstIdx / size) + 1;
    renderRecentPage();
  }

  // Refresh dropdown options for model + api_key filters from the current
  // rows. Preserves the user's current selection if still valid.
  function refreshRecentFilterOptions(rows) {
    const modelSet = new Set();
    const keySet = new Map(); // id → label
    for (const r of rows) {
      if (r.resolved_model) modelSet.add(r.resolved_model);
      if (r.api_key_id) keySet.set(r.api_key_id, r.api_key_label || r.api_key_id);
    }
    syncSelect("recentModel", [...modelSet].sort(), (v) => v);
    syncSelect("recentKey",   [...keySet.entries()].sort(),
      ([id, _label]) => id,
      ([_id, label]) => label);
  }

  function syncSelect(id, items, valueFn, labelFn) {
    const sel = $(id);
    if (!sel) return;
    const prev = sel.value;
    sel.replaceChildren(el("option", { value: "" }, "全部"));
    for (const item of items) {
      sel.appendChild(el("option",
        { value: valueFn(item) },
        labelFn ? labelFn(item) : valueFn(item)));
    }
    if (prev && [...sel.options].some(o => o.value === prev)) {
      sel.value = prev;
    }
  }

  // CSV export. Wraps fields with quotes; doubles internal quotes per RFC 4180.
  function exportRecentCSV() {
    if (recentLastRows.length === 0) { toast("没有数据可导出", "error"); return; }
    const cols = [
      "timestamp", "resolved_model", "requested_model", "api_key_id",
      "api_key_label", "device_id", "device", "status", "latency_ms",
      "input_tokens", "output_tokens", "credential_id", "trace_id",
    ];
    const esc = (v) => {
      if (v == null) return "";
      const s = String(v);
      if (/[",\n]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
      return s;
    };
    const lines = [cols.join(",")];
    for (const r of recentLastRows) {
      lines.push(cols.map(c => esc(r[c])).join(","));
    }
    downloadBlob(lines.join("\n") + "\n", "text/csv", "kirocc-events.csv");
  }

  function exportRecentJSON() {
    if (recentLastRows.length === 0) { toast("没有数据可导出", "error"); return; }
    downloadBlob(JSON.stringify(recentLastRows, null, 2),
      "application/json", "kirocc-events.json");
  }

  function downloadBlob(content, mime, filename) {
    const blob = new Blob([content], { type: mime });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
    toast(`已导出 ${filename} (${recentLastRows.length} 条)`, "success");
  }

  function renderTimeline(resp) {
    const svg = $("spark");
    while (svg.firstChild) svg.removeChild(svg.firstChild);
    const data = (resp && resp.timeline) || [];
    if (data.length === 0) {
      $("timelineRange").textContent = "暂无数据";
      return;
    }
    const w = 600, h = 80, pad = 4;
    const maxReq = Math.max(1, ...data.map((d) => d.requests || 0));
    const step = (w - pad * 2) / Math.max(1, data.length - 1);
    const points = data.map((d, i) => {
      const x = pad + i * step;
      const y = pad + (h - pad * 2) * (1 - (d.requests || 0) / maxReq);
      return [x, y];
    });
    const linePath = points.map(([x, y], i) => `${i === 0 ? "M" : "L"}${x.toFixed(1)},${y.toFixed(1)}`).join(" ");
    const areaPath = linePath + ` L${points[points.length - 1][0].toFixed(1)},${h - pad} L${points[0][0].toFixed(1)},${h - pad} Z`;

    const ns = "http://www.w3.org/2000/svg";
    const area = document.createElementNS(ns, "path");
    area.setAttribute("d", areaPath);
    area.setAttribute("class", "area");
    svg.appendChild(area);
    const line = document.createElementNS(ns, "path");
    line.setAttribute("d", linePath);
    line.setAttribute("class", "line");
    svg.appendChild(line);

    const startStr = new Date(resp.window_start).toLocaleTimeString();
    const endStr   = new Date(resp.window_end).toLocaleTimeString();
    $("timelineRange").textContent = `${startStr} → ${endStr} · 峰值 ${fmtNum(maxReq)} 请求/${resp.bucket}`;
  }

  // ---------- token chart (dashboard) ------------------------------------

  // Range presets, ordered to match the button group. Bucket sizes chosen
  // so each range yields 24–48 columns — enough resolution to read,
  // not so many that small SQLite scans get slow.
  const TOKEN_CHART_RANGES = {
    "24h": { window: "24h",  bucket: "1h"  },
    "7d":  { window: "168h", bucket: "4h"  },
    "15d": { window: "360h", bucket: "12h" },
    "30d": { window: "720h", bucket: "24h" },
  };
  let tokenChartRangeKey = "24h";
  let tokenChartType = "tokens";    // tokens | stacked | requests | cost
  let tokenChartCache = null;       // last fetched timeline response (for type-switch without refetch)

  // CHART_TYPES describes each renderable series set. `series` is an
  // ordered list of (name, color CSS variable, accessor) tuples; the
  // renderer either draws them as overlapping lines (tokens / requests)
  // or as a stacked area chart (stacked / cost). `unit` controls the
  // y-axis formatter; `legendTotalLabel` decides what label shows
  // beside the legend total.
  const CHART_TYPES = {
    tokens: {
      // Stack input/output by default — Claude Code traffic typically
      // has input >> output (often 1000:1), so a non-stacked dual line
      // chart makes the output line invisible at the X-axis.
      title: "Tokens（输入/输出堆叠）",
      stacked: true,
      unit: "tokens",
      series: [
        { name: "输入",  color: "neon",  get: d => d.input_tokens },
        { name: "输出",  color: "cyan",  get: d => d.output_tokens },
      ],
    },
    requests: {
      title: "请求数（成功/失败）",
      stacked: true,
      unit: "count",
      series: [
        { name: "成功", color: "neon",   get: d => d.success },
        { name: "失败", color: "crimson", get: d => d.failed },
      ],
    },
  };

  async function loadTokenChart(rangeKey) {
    const cfg = TOKEN_CHART_RANGES[rangeKey || tokenChartRangeKey];
    if (!cfg) return;
    tokenChartRangeKey = rangeKey || tokenChartRangeKey;
    document.querySelectorAll("#tokenChartRange .preset-chip").forEach(b => {
      b.classList.toggle("is-active", b.dataset.range === tokenChartRangeKey);
    });
    try {
      tokenChartCache = await getJSON(`/admin/usage/timeline?window=${cfg.window}&bucket=${cfg.bucket}`);
      renderTokenChart(tokenChartCache, tokenChartRangeKey);
    } catch (e) {
      console.warn("loadTokenChart", e);
    }
  }

  function setTokenChartType(t) {
    if (!CHART_TYPES[t]) return;
    tokenChartType = t;
    document.querySelectorAll("#tokenChartType .preset-chip").forEach(b => {
      b.classList.toggle("is-active", b.dataset.type === t);
    });
    if (tokenChartCache) renderTokenChart(tokenChartCache, tokenChartRangeKey);
  }

  function renderTokenChart(resp, rangeKey) {
    const svg = $("tokenChart");
    if (!svg) return;
    while (svg.firstChild) svg.removeChild(svg.firstChild);
    const data = (resp && resp.timeline) || [];
    const meta = $("tokenChartMeta");
    const specRaw = CHART_TYPES[tokenChartType];

    if (data.length === 0) {
      meta.textContent = "暂无数据";
      renderTokenChartLegend(specRaw, []);
      renderTokenChartXAxis([], rangeKey);
      return;
    }

    const spec = specRaw;
    const seriesValues = spec.series.map(s => data.map(d => s.get(d) || 0));
    const seriesTotals = seriesValues.map(vals => vals.reduce((a, b) => a + b, 0));

    // If every series is zero, render an empty-state message instead
    // of a blank chart.
    if (seriesTotals.every(t => t === 0)) {
      meta.textContent = `${data.length} 个桶 · ${spec.title} · 无数据`;
      renderTokenChartLegend(spec, seriesTotals);
      renderTokenChartXAxis(data, rangeKey);
      const ns = "http://www.w3.org/2000/svg";
      const txt = document.createElementNS(ns, "text");
      txt.setAttribute("x", "500"); txt.setAttribute("y", "120");
      txt.setAttribute("text-anchor", "middle");
      txt.setAttribute("class", "axis-label");
      txt.textContent = `当前窗口内无 ${spec.title} 数据`;
      svg.appendChild(txt);
      return;
    }

    // For stacked charts, peak Y is the per-bucket SUM across series.
    // For overlapping charts, peak Y is the per-bucket MAX across series.
    let peak = 0;
    if (spec.stacked) {
      for (let i = 0; i < data.length; i++) {
        let sum = 0;
        for (let s = 0; s < spec.series.length; s++) sum += seriesValues[s][i];
        if (sum > peak) peak = sum;
      }
    } else {
      for (let s = 0; s < spec.series.length; s++) {
        for (const v of seriesValues[s]) if (v > peak) peak = v;
      }
    }
    const peakY = Math.max(1, peak);

    // Geometry
    const W = 1000, H = 240;
    const padL = 56, padR = 12, padT = 14, padB = 22;
    const innerW = W - padL - padR;
    const innerH = H - padT - padB;
    const n = data.length;
    const step = n > 1 ? innerW / (n - 1) : 0;
    const baseY = padT + innerH;

    const xy = (i, v) => [
      padL + i * step,
      padT + innerH * (1 - v / peakY),
    ];

    const ns = "http://www.w3.org/2000/svg";

    // Y grid + labels
    for (let i = 1; i <= 4; i++) {
      const y = padT + (innerH * i) / 5;
      const line = document.createElementNS(ns, "line");
      line.setAttribute("x1", padL); line.setAttribute("x2", W - padR);
      line.setAttribute("y1", y); line.setAttribute("y2", y);
      line.setAttribute("class", "grid");
      svg.appendChild(line);
    }
    for (let i = 0; i <= 5; i++) {
      const v = (peakY * (5 - i)) / 5;
      const y = padT + (innerH * i) / 5;
      const txt = document.createElementNS(ns, "text");
      txt.setAttribute("x", padL - 6);
      txt.setAttribute("y", y + 4);
      txt.setAttribute("text-anchor", "end");
      txt.setAttribute("class", "axis-label");
      txt.textContent = formatAxisValue(v, spec.unit);
      svg.appendChild(txt);
    }

    // Draw series. For stacked, accumulate Y from the bottom up so each
    // series sits on top of the previous one. For overlapping, each
    // series renders independently at the value's height.
    const accum = new Array(n).fill(0);
    for (let s = 0; s < spec.series.length; s++) {
      const ser = spec.series[s];
      const vals = seriesValues[s];
      const topPoints = [];
      const botPoints = [];
      for (let i = 0; i < n; i++) {
        const base = spec.stacked ? accum[i] : 0;
        const top  = spec.stacked ? accum[i] + vals[i] : vals[i];
        topPoints.push(xy(i, top));
        botPoints.push(xy(i, base));
      }
      if (spec.stacked) for (let i = 0; i < n; i++) accum[i] += vals[i];

      const linePath = topPoints
        .map(([x, y], i) => `${i === 0 ? "M" : "L"}${x.toFixed(1)},${y.toFixed(1)}`)
        .join(" ");
      const closePath = spec.stacked
        ? linePath + " " + botPoints.slice().reverse()
            .map(([x, y]) => `L${x.toFixed(1)},${y.toFixed(1)}`).join(" ") + " Z"
        : linePath + ` L${topPoints[n - 1][0].toFixed(1)},${baseY.toFixed(1)} L${topPoints[0][0].toFixed(1)},${baseY.toFixed(1)} Z`;

      const area = document.createElementNS(ns, "path");
      area.setAttribute("d", closePath);
      area.setAttribute("class", "token-area series-" + ser.color);
      svg.appendChild(area);
      if (!spec.stacked) {
        const line = document.createElementNS(ns, "path");
        line.setAttribute("d", linePath);
        line.setAttribute("class", "token-line series-" + ser.color);
        svg.appendChild(line);
      }
    }

    // Hover overlay: one invisible rect per bucket. Hovering shows
    // a vertical crosshair + populates the tooltip div with bucket
    // values. Mouseleave clears.
    const crosshair = document.createElementNS(ns, "line");
    crosshair.setAttribute("class", "token-crosshair");
    crosshair.setAttribute("y1", padT);
    crosshair.setAttribute("y2", baseY);
    crosshair.setAttribute("x1", -10); crosshair.setAttribute("x2", -10);
    svg.appendChild(crosshair);

    const overlayW = n > 1 ? step : innerW;
    for (let i = 0; i < n; i++) {
      const rect = document.createElementNS(ns, "rect");
      const cx = padL + i * step;
      rect.setAttribute("x", (cx - overlayW / 2).toFixed(1));
      rect.setAttribute("y", padT);
      rect.setAttribute("width", overlayW.toFixed(1));
      rect.setAttribute("height", innerH);
      rect.setAttribute("class", "token-hover-rect");
      rect.dataset.idx = i;
      svg.appendChild(rect);
    }
    bindTokenChartHover(data, spec, seriesValues, resp.bucket, padL, step, baseY, padT);

    renderTokenChartLegend(spec, seriesTotals);
    meta.textContent =
      `${n} 个桶 · 峰值 ${formatAxisValue(peak, spec.unit)}/${resp.bucket} · ${spec.title}`;
    renderTokenChartXAxis(data, rangeKey);
  }

  // bindTokenChartHover wires up the SVG-rect overlay -> tooltip flow.
  // Uses event delegation on the svg so the rects don't all need
  // individual listeners. Tooltip is positioned in the DOM via px
  // coords derived from the rect's SVG x + the svg's bounding rect.
  function bindTokenChartHover(data, spec, seriesValues, bucketStr, padL, step, baseY, padT) {
    const svg = $("tokenChart");
    const tip = $("tokenChartTooltip");
    if (!svg || !tip) return;
    const crosshair = svg.querySelector(".token-crosshair");

    const onMove = (ev) => {
      const target = ev.target;
      if (!target.classList || !target.classList.contains("token-hover-rect")) {
        return;
      }
      const idx = parseInt(target.dataset.idx, 10);
      if (isNaN(idx) || !data[idx]) return;
      const d = data[idx];
      // Crosshair in SVG coords
      const cxSVG = padL + idx * step;
      crosshair.setAttribute("x1", cxSVG);
      crosshair.setAttribute("x2", cxSVG);

      // Tooltip in CSS coords (relative to wrap container)
      const wrap = svg.parentElement;
      const wrapRect = wrap.getBoundingClientRect();
      const svgRect = svg.getBoundingClientRect();
      // Map SVG x → CSS x via svg's actual rendered width
      const scaleX = svgRect.width / 1000;
      const tipX = svgRect.left - wrapRect.left + cxSVG * scaleX;

      // Build tooltip body
      const lines = [];
      const t = new Date(d.start);
      lines.push(`<div class="tip-head">${fmtTipTime(t, bucketStr)}</div>`);
      const total = spec.series.reduce((sum, _, i) => sum + (seriesValues[i][idx] || 0), 0);
      for (let s = 0; s < spec.series.length; s++) {
        const v = seriesValues[s][idx] || 0;
        lines.push(
          `<div class="tip-row series-${spec.series[s].color}">` +
          `<span class="tip-swatch"></span>` +
          `<span class="tip-name">${spec.series[s].name}</span>` +
          `<span class="tip-val">${formatAxisValue(v, spec.unit)}</span>` +
          `</div>`
        );
      }
      if (spec.series.length > 1) {
        lines.push(`<div class="tip-row tip-total"><span class="tip-name">合计</span><span class="tip-val">${formatAxisValue(total, spec.unit)}</span></div>`);
      }
      tip.innerHTML = lines.join("");
      tip.hidden = false;

      // Position: anchor near top of chart, offset to avoid overflow.
      const tipW = tip.offsetWidth;
      const wrapW = wrap.offsetWidth;
      let left = tipX + 12;
      if (left + tipW > wrapW) left = tipX - tipW - 12;
      if (left < 0) left = 4;
      tip.style.left = left + "px";
      tip.style.top = "8px";
    };

    const onLeave = (ev) => {
      // Hide tooltip only when leaving the SVG entirely
      if (ev.relatedTarget && svg.contains(ev.relatedTarget)) return;
      tip.hidden = true;
      crosshair.setAttribute("x1", -10);
      crosshair.setAttribute("x2", -10);
    };

    svg.onmousemove = onMove;
    svg.onmouseleave = onLeave;
  }

  function fmtTipTime(d, bucketStr) {
    // Show "M/D HH:MM" — the bucket size is implied by the chart range.
    const m = d.getMonth() + 1;
    const day = d.getDate();
    const hh = String(d.getHours()).padStart(2, "0");
    const mm = String(d.getMinutes()).padStart(2, "0");
    return `${m}/${day} ${hh}:${mm} · ${bucketStr}`;
  }

  function formatAxisValue(v, _unit) {
    return fmtNum(v);
  }

  function renderTokenChartLegend(spec, totals) {
    const host = $("tokenChartLegend");
    if (!host) return;
    host.replaceChildren();
    for (let i = 0; i < spec.series.length; i++) {
      const s = spec.series[i];
      const totVal = totals[i] != null ? totals[i] : 0;
      host.appendChild(el("span", { class: "legend-item legend-" + s.color },
        el("span", { class: "legend-swatch" }),
        el("span", {}, s.name),
        el("span", { class: "mono accent" }, formatAxisValue(totVal, spec.unit)),
      ));
    }
  }

  function renderTokenChartXAxis(buckets, rangeKey) {
    const host = $("tokenChartXAxis");
    if (!host) return;
    host.replaceChildren();
    if (buckets.length === 0) {
      host.appendChild(el("span", { class: "muted" }, "—"));
      return;
    }
    // Pick up to 6 evenly spaced ticks
    const tickCount = Math.min(6, buckets.length);
    const step = (buckets.length - 1) / (tickCount - 1 || 1);
    const fmt = (iso) => {
      const d = new Date(iso);
      if (rangeKey === "24h") {
        return String(d.getHours()).padStart(2, "0") + ":00";
      }
      return `${d.getMonth() + 1}/${d.getDate()}`;
    };
    for (let i = 0; i < tickCount; i++) {
      const idx = Math.round(i * step);
      const b = buckets[idx];
      host.appendChild(el("span", { class: "tick" }, fmt(b.start)));
    }
  }
  // ---------- actions ----------------------------------------------------

  async function onRefresh(id) {
    try {
      const r = await apiFetch(`/admin/accounts/${encodeURIComponent(id)}/refresh`, {
        method: "POST",
      });
      if (!r.ok) {
        const msg = await readErrorMessage(r);
        throw new Error(msg);
      }
      toast("已发起配额刷新", "success");
    } catch (e) {
      toast("刷新失败：" + e.message, "error");
    } finally {
      loadAccounts();
      loadHealth();
    }
  }

  // readErrorMessage extracts a human-readable error from a non-ok response.
  // Backends here return either {"error": "..."} JSON or plain text.
  async function readErrorMessage(r) {
    const text = await r.text().catch(() => "");
    let body = text;
    try {
      const j = JSON.parse(text);
      body = j.error || j.message || text;
    } catch (_) { /* keep raw text */ }
    return friendlyRefreshError(r.status, body);
  }

  // friendlyRefreshError maps known upstream auth/quota failures to
  // actionable Chinese messages. Falls back to the raw error otherwise.
  function friendlyRefreshError(status, body) {
    const s = (body || "").toLowerCase();
    if (s.includes("token refresh:") && s.includes("429")) {
      return "刷新被上游限流（429），稍后再试。后台 poller 也可能正在重试同一凭据，等几分钟。";
    }
    if (s.includes("token refresh:") && (s.includes("400") || s.includes("invalid_grant"))) {
      return "refresh_token 已失效，请到「账号」页用 OAuth 重新登录该账号。";
    }
    if (s.includes("bearer token") && s.includes("invalid")) {
      return "access_token 已失效且未配置 refresh，请重新 OAuth 授权。";
    }
    if (status === 502 || status === 503) {
      return `上游错误 (${status}): ${body}`;
    }
    return body || `HTTP ${status}`;
  }
  async function onDisable(id) {
    try { await postJSON(`/admin/accounts/${encodeURIComponent(id)}/disable`); }
    catch (e) { console.warn("disable failed", e); }
    finally { loadAccounts(); loadHealth(); }
  }
  async function onEnable(id) {
    try { await postJSON(`/admin/accounts/${encodeURIComponent(id)}/enable`); }
    catch (e) { console.warn("enable failed", e); }
    finally { loadAccounts(); loadHealth(); }
  }
  async function onDelete(id, label) {
    if (!confirm(`确定要删除账号 "${label || id}" 吗？此操作会从 JSON 文件移除该条目。`)) return;
    try {
      const r = await apiFetch(`/admin/accounts/${encodeURIComponent(id)}`, { method: "DELETE" });
      if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
      toast("已删除 " + id, "success");
    } catch (e) {
      toast("删除失败：" + e.message, "error");
    } finally {
      loadAccounts();
      loadHealth();
    }
  }
  async function onRefreshAllQuota() {
    const rows = document.querySelectorAll("#accountsBody tr[data-id]");
    if (rows.length === 0) return;
    toast(`正在刷新 ${rows.length} 个账号的配额…`);
    for (const r of rows) {
      try { await postJSON(`/admin/accounts/${encodeURIComponent(r.dataset.id)}/refresh`); }
      catch (e) { console.warn("refresh failed", r.dataset.id, e); }
    }
    toast("配额已批量刷新", "success");
    loadAccounts();
    loadHealth();
  }

  // ---------- loaders ----------------------------------------------------

  async function loadHealth()    { try { renderHealth(await getJSON("/admin/health")); } catch (e) { console.warn(e); } }
  async function loadAccounts()  { try { renderAccounts(await getJSON("/admin/accounts")); } catch (e) { console.warn(e); } }
  async function loadUsage() {
    try {
      const [model, byKey, byDev] = await Promise.all([
        getJSON("/admin/usage?window=24h&group=model"),
        getJSON("/admin/usage?window=24h&group=api_key"),
        getJSON("/admin/usage?window=24h&group=device"),
      ]);
      renderUsage(model);
      renderUsageByKey(byKey);
      renderUsageByDevice(byDev);
    } catch (e) { console.warn(e); }
  }
  async function loadTimeline()  { try { renderTimeline(await getJSON("/admin/usage/timeline?bucket=10m&window=2h")); } catch (e) { console.warn(e); } }
  async function loadRecent() {
    try {
      const lim = $("recentLimit").value || "500";
      const params = new URLSearchParams({ limit: lim });
      const add = (key, id) => { const v = $(id)?.value || ""; if (v) params.set(key, v); };
      add("status",     "recentStatus");
      add("model",      "recentModel");
      add("api_key_id", "recentKey");
      renderRecent(await getJSON("/admin/usage/recent?" + params.toString()));
    } catch (e) { console.warn(e); }
  }

  function loadAll() {
    loadHealth();          // always: drives banners + top stats
    if (currentPage === "dashboard") {
      loadTimeline();
      loadDashUsage();
      loadTokenChart(tokenChartRangeKey);
    }
    if (currentPage === "accounts")  loadAccounts();
    if (currentPage === "credsfile") loadCredsFile();
    if (currentPage === "stats")     { loadUsage(); loadRecent(); }
    if (currentPage === "settings")  { loadSettings(); loadAPIKeys(); loadOptimizations(); }
  }

  // ---------- tab routing -----------------------------------------------

  // Settings sub-sections, ordered to match the sidebar sub-nav.
  const SETTINGS_SECTIONS = ["runtime", "auth", "remote", "system", "network", "streaming", "optimizations", "api"];
  let currentSection = "runtime";

  function navigateTo(page, section) {
    if (!PAGES.includes(page)) page = "dashboard";
    currentPage = page;
    document.querySelectorAll(".page").forEach(el => {
      el.hidden = el.dataset.page !== page;
    });
    // Top-level tab highlight: only the parent entry, not the sub-nav links.
    document.querySelectorAll("#tabs > a[data-tab]").forEach(a => {
      a.classList.toggle("active", a.dataset.tab === page);
    });
    // Sub-nav: only the parent of the active page is expanded; highlight the active section.
    document.querySelectorAll(".sidenav-sub").forEach(sub => {
      sub.classList.toggle("is-open", sub.dataset.parent === page);
    });
    if (page === "settings") {
      if (!SETTINGS_SECTIONS.includes(section)) section = "runtime";
      currentSection = section;
      document.querySelectorAll(".settings-card[data-section]").forEach(card => {
        card.hidden = card.dataset.section !== section;
      });
      document.querySelectorAll(".sidenav-sub a").forEach(a => {
        a.classList.toggle("active", a.dataset.section === section);
      });
    }
    loadAll();
  }

  // pageFromHash parses "#/page" or "#/page/section" and returns both.
  function pageFromHash() {
    const h = (location.hash || "").replace(/^#\//, "");
    if (!h) return { page: "dashboard", section: "" };
    const [page, section] = h.split("/");
    return { page: PAGES.includes(page) ? page : "dashboard", section: section || "" };
  }

  // ---------- 配额 page ------------------------------------------------

  // ---------- 认证文件 page --------------------------------------------

  let credsFileLoaded = false;

  function fmtBytes(n) {
    if (!n || n < 0) return "—";
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    return (n / (1024 * 1024)).toFixed(2) + " MB";
  }

  async function loadCredsFile() {
    const editor = $("credsEditor");
    const cardsHost = $("credsCards");
    if (!editor && !cardsHost) return;
    try {
      const r = await apiFetch("/admin/credsfile");
      if (r.status === 404) {
        $("credsPath").textContent = "—";
        $("credsSize").textContent = "—";
        $("credsMtime").textContent = "—";
        if ($("credsCount")) $("credsCount").textContent = "—";
        if (editor) {
          editor.value = "// single-account mode — no credentials JSON in use.\n// 启动时附加 -creds-json <path> 以启用多账号池.";
          editor.disabled = true;
        }
        if (cardsHost) {
          cardsHost.replaceChildren(el("div", { class: "cards-empty muted" },
            "single-account mode — credentials file not in use"));
        }
        $("credsFileActions").hidden = true;
        return;
      }
      if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
      const data = await r.json();
      $("credsPath").textContent = data.path || "—";
      $("credsSize").textContent = data.exists ? fmtBytes(data.size) : "(missing)";
      $("credsMtime").textContent = data.last_modified && !isZeroTime(data.last_modified)
        ? new Date(data.last_modified).toLocaleString()
        : "—";
      let entries = [];
      try { entries = data.content ? JSON.parse(data.content) : []; }
      catch (_) { entries = []; }
      if (!Array.isArray(entries)) entries = [];
      if ($("credsCount")) $("credsCount").textContent = entries.length;
      renderCredsCards(entries);
      if (editor && !credsFileLoaded) {
        editor.value = data.content || "";
        editor.disabled = false;
        credsFileLoaded = true;
      }
    } catch (e) {
      toast("读取认证文件失败：" + e.message, "error");
    }
  }

  function renderCredsCards(entries) {
    const host = $("credsCards");
    if (!host) return;
    host.replaceChildren();
    if (entries.length === 0) {
      host.appendChild(el("div", { class: "cards-empty muted" }, "no credentials"));
      return;
    }
    for (const c of entries) {
      const tok = c.kiro_auth_token_raw || {};
      const expIso = tok.expiresAt || "";
      const expDate = expIso ? new Date(expIso) : null;
      const expired = expDate && expDate.getTime() < Date.now();
      const card = el("div", {
        class: "creds-card" +
          (c.disabled ? " is-disabled" : "") +
          (expired ? " is-expired" : ""),
      });
      card.appendChild(el("div", { class: "creds-card-head" },
        el("div", {},
          el("div", { class: "creds-card-id" }, c.id || "(no id)"),
          c.label ? el("div", { class: "creds-card-label" }, c.label) : null,
        ),
        el("span", { class: "chip-provider" }, c.provider || "kiro"),
      ));
      const rows = el("dl", { class: "creds-card-rows" });
      const addRow = (k, v, cls) => {
        if (v == null || v === "") return;
        rows.appendChild(el("dt", {}, k));
        rows.appendChild(el("dd", cls ? { class: cls } : {}, String(v)));
      };
      addRow("auth", tok.authMethod || "—", "cyan");
      addRow("region", tok.region || tok.ssoRegion || "—");
      addRow("expires", expDate ? (expired ? "EXPIRED · " : "") + fmtRelative(expIso) : "—",
        expired ? null : "neon");
      addRow("priority", typeof c.priority === "number" ? c.priority : "—");
      addRow("status", c.disabled ? "disabled" : (expired ? "expired" : "active"));
      card.appendChild(rows);
      card.appendChild(el("div", { class: "creds-card-actions" },
        el("button", {
          class: "btn tiny",
          type: "button",
          onclick: () => copyToken(tok.accessToken),
        }, "复制 token"),
        el("button", {
          class: "btn tiny",
          type: "button",
          onclick: () => copyToClipboard(JSON.stringify(c, null, 2)),
        }, "复制 JSON"),
      ));
      host.appendChild(card);
    }
  }

  function copyToken(t) {
    if (!t) { toast("无 access token", "error"); return; }
    copyToClipboard(t, "已复制 access token");
  }

  function copyToClipboard(text, okMsg) {
    if (!text) { toast("无内容", "error"); return; }
    navigator.clipboard.writeText(text).then(
      () => toast(okMsg || "已复制", "success"),
      () => toast("复制失败", "error"),
    );
  }

  async function saveCredsFile() {
    const editor = $("credsEditor");
    if (!editor) return;
    const content = editor.value.trim();
    if (!content) { toast("内容为空，未保存", "error"); return; }
    try { JSON.parse(content); }
    catch (e) { toast("JSON 解析失败：" + e.message, "error"); return; }
    if (!confirm("覆盖磁盘上的凭据文件？该操作会立即生效于运行中的代理。")) return;
    try {
      const r = await apiFetch("/admin/credsfile", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ content }),
      });
      const text = await r.text();
      if (!r.ok) throw new Error(text);
      let result = {};
      try { result = JSON.parse(text); } catch (_) {}
      toast(`已保存 ${result.count || "?"} 个账号到 ${result.path || ""}`, "success");
      credsFileLoaded = false;
      loadCredsFile();
      loadAccounts();
      loadHealth();
    } catch (e) {
      toast("保存失败：" + e.message, "error");
    }
  }

  function bindCredsFilePage() {
    $("btnCredsReload")?.addEventListener("click", () => {
      credsFileLoaded = false;
      loadCredsFile();
    });
    $("btnCredsDownload")?.addEventListener("click", () => {
      location.href = "/admin/credsfile/download";
    });
    $("btnCredsUpload")?.addEventListener("click", () => $("credsFileUpload").click());
    $("credsFileUpload")?.addEventListener("change", (ev) => {
      const f = ev.target.files[0];
      if (!f) return;
      const reader = new FileReader();
      reader.onload = () => {
        $("credsEditor").value = reader.result;
        toast("文件已加载到编辑器，点击「保存编辑」生效", "success");
      };
      reader.readAsText(f);
    });
    $("btnCredsSave")?.addEventListener("click", saveCredsFile);
  }

  // ---------- toast ------------------------------------------------------

  let toastTimer = null;
  function toast(msg, kind) {
    const t = $("toast");
    if (!t) return;
    t.textContent = msg;
    t.className = "toast" + (kind ? " " + kind : "");
    t.hidden = false;
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, 3500);
  }

  // ---------- 系统设置 page --------------------------------------------

  // Last loaded settings snapshot — used to compute partial PATCH bodies.
  let settingsCur = null;

  function fillInput(id, val) {
    const e = $(id);
    if (!e) return;
    if (e.type === "checkbox") e.checked = !!val;
    else e.value = val == null ? "" : val;
  }

  function readInput(id) {
    const e = $(id);
    if (!e) return null;
    if (e.type === "checkbox") return e.checked;
    if (e.type === "number") {
      const n = Number(e.value);
      return Number.isFinite(n) ? n : 0;
    }
    return e.value;
  }

  async function loadSettings() {
    try {
      const s = await getJSON("/admin/settings");
      settingsCur = s;
      // §1 server runtime (readonly) - everything wired from /admin/settings.server
      const sv = s.server || {};
      const setText = (id, v) => { const e = $(id); if (e) e.textContent = v || "—"; };
      setText("srvProxyAddr",   sv.host && sv.port ? `${sv.host}:${sv.port}` : "—");
      setText("srvAdminAddr",   sv.admin_host && sv.admin_port ? `${sv.admin_host}:${sv.admin_port}` : "—");
      setText("srvTLSEnabled",  sv.tls_enabled ? "✓ 已启用 HTTPS" : "未启用（明文 HTTP）");
      setText("srvPublicURL",   sv.public_base_url || "（未设置）");
      setText("srvCredsPath",   sv.creds_path || "（单账号模式 / SQLite）");
      setText("srvPoolMode",    sv.multi_account ? "多账号 JSON 池" : "单账号 (SQLite)");
      const geo = sv.geoip || {};
      if (geo.loaded) {
        const buildAge = geo.build_epoch ? fmtRelative(geo.build_epoch * 1000) : "—";
        setText("srvGeoIP", `✓ ${geo.db_type} · 构建于 ${buildAge}`);
      } else {
        setText("srvGeoIP", "未加载（启动时传 -geoip-mmdb 启用）");
      }

      // §3 remote management
      const rm = s.remote_management || {};
      fillInput("cfgAllowRemote",   rm.allow_remote);
      fillInput("cfgDisablePanel",  rm.disable_panel);
      fillInput("cfgMgmtKey",       rm.mgmt_key);
      fillInput("cfgPanelRepo",     rm.panel_repo_url);

      // §5 system
      const sys = s.system || {};
      fillInput("cfgDebug",         sys.debug);
      fillInput("cfgCommercial",    sys.commercial_mode);
      fillInput("cfgLogToFile",     sys.logging_to_file);
      fillInput("cfgUsageStats",    sys.usage_stats_enabled);
      fillInput("cfgLogMaxMB",      sys.log_max_total_size_mb || 0);

      // §6 network
      const net = s.network || {};
      fillInput("cfgProxyURL",          net.proxy_url);
      fillInput("cfgRequestRetry",      net.request_retry || 0);
      fillInput("cfgMaxRetryCreds",     net.max_retry_creds || 0);
      fillInput("cfgMaxRetryInterval",  net.max_retry_interval);
      fillInput("cfgRoutingStrategy",   net.routing_strategy || "round-robin");
      fillInput("cfgAffinityTTL",       net.session_affinity_ttl);
      fillInput("cfgPreferredRegion",   net.preferred_region);
      fillInput("cfgForceModelPrefix",  net.force_model_prefix);
      fillInput("cfgSessionSticky",     net.session_sticky);

      // §8 streaming
      const stm = s.streaming || {};
      fillInput("cfgKeepaliveSec",        stm.keepalive_seconds || 0);
      fillInput("cfgBootstrapRetries",    stm.bootstrap_retries || 0);
      fillInput("cfgNonStreamKeepalive",  stm.non_streaming_keepalive_seconds || 0);

      // §10 API docs base URL — derive from current page so curl examples
      // match this very deployment.
      const apiBase = (sv.public_base_url && sv.public_base_url.replace(/\/$/, ""))
        || `${location.protocol}//${location.host}`;
      setText("apiBaseURL", apiBase + "/admin/*");
      setText("apiUrlListAccounts", apiBase);
      document.querySelectorAll("[data-page=\"settings\"] .api-url").forEach(s2 => {
        s2.textContent = apiBase;
      });

      // §9 optimizations
      const opt = s.optimizations || {};
      fillInput("optForceThinkingBudget",   opt.force_thinking_budget || 0);
      fillInput("optThinkingPromptMode",    opt.thinking_prompt_mode || "");
      fillInput("optThinkingToolContinue",  opt.thinking_tool_continue_mode || "");
      fillInput("optSystemMode",            opt.system_mode || "");
      fillInput("optUpstreamOrigin",        opt.upstream_origin || "");
      fillInput("optModelMappings",         opt.model_mappings || "");
      fillInput("optAuditLog",              opt.audit_log || "");
    } catch (e) {
      console.warn(e);
      toast("加载设置失败：" + e.message, "error");
    }
  }

  function buildSectionPatch(section) {
    switch (section) {
      case "remote_management":
        return { remote_management: {
          allow_remote:    readInput("cfgAllowRemote"),
          disable_panel:   readInput("cfgDisablePanel"),
          mgmt_key:        readInput("cfgMgmtKey"),
          panel_repo_url:  readInput("cfgPanelRepo"),
        }};
      case "system":
        return { system: {
          debug:                  readInput("cfgDebug"),
          commercial_mode:        readInput("cfgCommercial"),
          logging_to_file:        readInput("cfgLogToFile"),
          usage_stats_enabled:    readInput("cfgUsageStats"),
          log_max_total_size_mb:  readInput("cfgLogMaxMB"),
        }};
      case "network":
        return { network: {
          proxy_url:             readInput("cfgProxyURL"),
          request_retry:         readInput("cfgRequestRetry"),
          max_retry_creds:       readInput("cfgMaxRetryCreds"),
          max_retry_interval:    readInput("cfgMaxRetryInterval"),
          routing_strategy:      readInput("cfgRoutingStrategy"),
          session_affinity_ttl:  readInput("cfgAffinityTTL"),
          preferred_region:      readInput("cfgPreferredRegion"),
          force_model_prefix:    readInput("cfgForceModelPrefix"),
          session_sticky:        readInput("cfgSessionSticky"),
        }};
      case "streaming":
        return { streaming: {
          keepalive_seconds:                readInput("cfgKeepaliveSec"),
          bootstrap_retries:                readInput("cfgBootstrapRetries"),
          non_streaming_keepalive_seconds:  readInput("cfgNonStreamKeepalive"),
        }};
      case "optimizations":
        return { optimizations: {
          force_thinking_budget:        readInput("optForceThinkingBudget"),
          thinking_prompt_mode:         readInput("optThinkingPromptMode"),
          thinking_tool_continue_mode:  readInput("optThinkingToolContinue"),
          system_mode:                  readInput("optSystemMode"),
          upstream_origin:              readInput("optUpstreamOrigin"),
          model_mappings:               readInput("optModelMappings"),
          audit_log:                    readInput("optAuditLog"),
        }};
    }
    return null;
  }

  async function saveSettings(section, btn) {
    const body = buildSectionPatch(section);
    if (!body) return;
    if (btn) { btn.disabled = true; btn.textContent = "保存中…"; }
    try {
      const r = await apiFetch("/admin/settings", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      if (!r.ok) {
        const text = await r.text().catch(() => "");
        throw new Error(`${r.status} ${r.statusText}${text ? ": " + text : ""}`);
      }
      settingsCur = await r.json();
      toast("已保存", "success");
    } catch (e) {
      toast("保存失败：" + e.message, "error");
    } finally {
      if (btn) { btn.disabled = false; btn.textContent = "保存"; }
    }
  }

  // ---------- API keys (§4) --------------------------------------------

  // ---------- kirocc optimizations sub-page ----------------------------

  async function loadOptimizations() {
    try {
      const data = await getJSON("/admin/optimizations");
      renderOptFixes(data.fixes || []);
      renderOptKnobs(data.knobs || []);
    } catch (e) {
      console.warn("loadOptimizations", e);
    }
  }

  function renderOptFixes(fixes) {
    const host = $("optFixList");
    if (!host) return;
    host.replaceChildren();
    if (fixes.length === 0) {
      host.appendChild(el("div", { class: "cards-empty muted" }, "暂无数据"));
      return;
    }
    for (const f of fixes) {
      const isEnv = f.status === "env";
      const card = el("div", { class: "opt-fix" + (isEnv ? " is-env" : "") });
      card.appendChild(el("div", { class: "opt-fix-head" },
        el("span", { class: "opt-fix-num" }, "FIX " + String(f.id).padStart(2, "0")),
        el("span", { class: "opt-fix-title" }, f.title),
        el("span", { class: "pill " + (isEnv ? (f.env_value ? "active" : "disabled") : "active") },
          isEnv
            ? (f.env_value ? "ENV 已配置" : "可选")
            : "ALWAYS ON"),
      ));
      card.appendChild(el("p", { class: "opt-fix-detail" }, f.detail));
      if (isEnv) {
        const envRow = el("div", { class: "opt-fix-env" },
          el("code", { class: "mono small" }, f.env_var),
          el("span", { class: "muted small" }, " = "),
          el("code", { class: "mono small accent" }, f.env_value || "(未设置)"),
        );
        if (f.default_hint && !f.env_value) {
          envRow.appendChild(el("span", { class: "muted small" }, " · " + f.default_hint));
        }
        card.appendChild(envRow);
      }
      host.appendChild(card);
    }
  }

  function renderOptKnobs(knobs) {
    const host = $("optKnobList");
    if (!host) return;
    host.replaceChildren();
    if (knobs.length === 0) {
      host.appendChild(el("div", { class: "cards-empty muted" }, "暂无可调参数"));
      return;
    }
    for (const k of knobs) {
      const set = k.value !== "";
      const row = el("div", { class: "opt-knob" + (set ? " is-set" : "") });
      row.appendChild(el("div", { class: "opt-knob-head" },
        el("span", { class: "opt-knob-label" }, k.label),
        el("code", { class: "mono small" }, k.env_var),
        el("span", { class: "pill " + (set ? "active" : "disabled") },
          set ? "已配置" : "未设置"),
      ));
      row.appendChild(el("p", { class: "opt-knob-detail" }, k.detail));
      row.appendChild(el("div", { class: "opt-knob-value" },
        el("span", { class: "muted small" }, "当前值: "),
        el("code", { class: "mono " + (set ? "accent" : "dim") },
          k.value || "(空)"),
      ));
      host.appendChild(row);
    }
  }

  async function loadAPIKeys() {
    try {
      const data = await getJSON("/admin/api-keys");
      renderAPIKeys(data.keys || data || []);
    } catch (e) {
      console.warn(e);
      toast("加载 API 密钥失败：" + e.message, "error");
    }
  }

  function renderAPIKeys(rows) {
    const host = $("apiKeysList");
    if (!host) return;
    host.replaceChildren();
    if (!rows || rows.length === 0) {
      host.appendChild(el("div", { class: "cards-empty muted" }, "尚无 API 密钥"));
      return;
    }
    for (const k of rows) {
      host.appendChild(buildAPIKeyCard(k));
    }
  }

  function buildAPIKeyCard(k) {
    const nowSec = Math.floor(Date.now() / 1000);
    const expired = k.expires_at > 0 && k.expires_at <= nowSec;
    const overQuota = k.quota_limit > 0 && (k.used_tokens || 0) >= k.quota_limit;
    let statusCls, statusText;
    if (!k.enabled)      { statusCls = "disabled"; statusText = "已禁用"; }
    else if (expired)    { statusCls = "banned";   statusText = "已过期"; }
    else if (overQuota)  { statusCls = "cooldown"; statusText = "额度耗尽"; }
    else                 { statusCls = "active";   statusText = "可用"; }

    const card = el("div", {
      class: "api-key-card is-" + statusCls,
      "data-id": k.id,
    });

    // Head: label + status pill
    card.appendChild(el("div", { class: "api-key-head" },
      el("div", { class: "api-key-ident" },
        el("div", { class: "api-key-label" }, k.label || "(未命名)"),
        el("div", { class: "api-key-masked mono" }, k.key_masked || "—"),
      ),
      el("span", { class: "pill " + statusCls }, statusText),
    ));

    // Meta rows
    const meta = el("dl", { class: "api-key-meta" });
    const addMeta = (k2, v) => {
      meta.appendChild(el("dt", {}, k2));
      meta.appendChild(el("dd", {}, v));
    };
    addMeta("ID", el("code", { class: "mono small" }, k.id));
    addMeta("创建", k.created_at ? fmtDateTime(k.created_at * 1000) : "—");
    addMeta("过期",
      k.expires_at > 0
        ? (expired ? "已过期 · " : "") + fmtDateTime(k.expires_at * 1000)
        : el("span", { class: "muted" }, "永不过期"),
    );
    addMeta("额度",
      k.quota_limit > 0
        ? buildQuotaProgress(k.used_tokens || 0, k.quota_limit)
        : el("span", { class: "muted" }, "不限"),
    );
    card.appendChild(meta);

    // Actions
    card.appendChild(el("div", { class: "api-key-actions" },
      el("button", {
        class: "btn tiny",
        type: "button",
        onclick: () => copyToClipboard(k.id, "已复制 ID"),
      }, "复制 ID"),
      el("button", {
        class: "btn tiny",
        type: "button",
        onclick: () => copyToClipboard(k.key_masked || "", "已复制掩码"),
      }, "复制掩码"),
      el("button", {
        class: "btn tiny",
        type: "button",
        onclick: () => toggleAPIKey(k.id, !k.enabled),
      }, k.enabled ? "禁用" : "启用"),
      el("button", {
        class: "btn tiny",
        type: "button",
        onclick: () => rotateAPIKey(k.id),
      }, "轮换"),
      el("button", {
        class: "btn tiny danger",
        type: "button",
        onclick: () => deleteAPIKey(k.id, k.label),
      }, "删除"),
    ));

    return card;
  }

  // buildQuotaProgress returns a small "used / limit (NN%)" element with
  // a bar. used >= limit shows the bar full red.
  function buildQuotaProgress(used, limit) {
    const pct = limit > 0 ? Math.min(100, Math.round((used / limit) * 100)) : 0;
    const wrap = el("div", { class: "api-key-quota" });
    const text = el("div", { class: "api-key-quota-text" },
      el("span", { class: "mono" }, fmtNum(used)),
      el("span", { class: "muted small" }, " / "),
      el("span", { class: "mono dim" }, fmtNum(limit)),
      el("span", { class: "muted small" }, " tokens · "),
      el("span", { class: "mono" + (pct >= 90 ? " warn" : "") }, pct + "%"),
    );
    const b = bar(Math.max(0, limit - used), limit);
    wrap.appendChild(text);
    wrap.appendChild(b);
    return wrap;
  }

  // Per-dialog selected values for the preset pickers. Reset on open.
  const newKeyState = { expiresAt: 0, quotaLimit: 0 };

  function openAddAPIKeyDialog() {
    const dialog = $("addAPIKeyDialog");
    if (!dialog) return;
    $("newKeyLabel").value = "";
    $("newKeyExpiresCustom").value = "";
    $("newKeyExpiresCustom").hidden = true;
    $("newKeyQuotaCustom").value = "";
    $("newKeyQuotaCustom").hidden = true;
    setActiveChip("newKeyExpiresPicker", "0");
    setActiveChip("newKeyQuotaPicker", "0");
    newKeyState.expiresAt = 0;
    newKeyState.quotaLimit = 0;
    $("newKeyExpiresHint").textContent = "永不过期";
    $("newKeyQuotaHint").textContent = "不限制（input + output tokens）";
    if (typeof dialog.showModal === "function") dialog.showModal();
    setTimeout(() => $("newKeyLabel")?.focus(), 0);
  }

  function setActiveChip(pickerId, val) {
    const host = $(pickerId);
    if (!host) return;
    const key = pickerId.includes("Expires") ? "days" : "tokens";
    host.querySelectorAll(".preset-chip").forEach(b => {
      b.classList.toggle("is-active", b.dataset[key] === val);
    });
  }

  function fmtTokens(n) {
    if (!n) return "0";
    if (n >= 1_000_000) return (n / 1_000_000).toFixed(n % 1_000_000 ? 2 : 0) + "M";
    if (n >= 1_000) return (n / 1_000).toFixed(n % 1_000 ? 1 : 0) + "k";
    return String(n);
  }

  function fmtDateShort(unixSec) {
    if (!unixSec) return "—";
    const d = new Date(unixSec * 1000);
    return `${d.getFullYear()}/${String(d.getMonth() + 1).padStart(2,"0")}/${String(d.getDate()).padStart(2,"0")} ${String(d.getHours()).padStart(2,"0")}:${String(d.getMinutes()).padStart(2,"0")}`;
  }

  function bindExpiresPicker() {
    const host = $("newKeyExpiresPicker");
    if (!host) return;
    host.addEventListener("click", (ev) => {
      const b = ev.target.closest(".preset-chip");
      if (!b) return;
      const days = b.dataset.days;
      const custom = $("newKeyExpiresCustom");
      const hint = $("newKeyExpiresHint");
      if (days === "custom") {
        setActiveChip("newKeyExpiresPicker", "custom");
        custom.hidden = false;
        // Default to +30 days when first entering custom mode.
        if (!custom.value) {
          const d = new Date();
          d.setDate(d.getDate() + 30);
          const pad = (n) => String(n).padStart(2, "0");
          custom.value = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
        }
        newKeyState.expiresAt = Math.floor(new Date(custom.value).getTime() / 1000);
        hint.textContent = "过期于 " + fmtDateShort(newKeyState.expiresAt);
        custom.focus();
      } else {
        setActiveChip("newKeyExpiresPicker", days);
        custom.hidden = true;
        const n = parseInt(days, 10) || 0;
        if (n === 0) {
          newKeyState.expiresAt = 0;
          hint.textContent = "永不过期";
        } else {
          newKeyState.expiresAt = Math.floor(Date.now() / 1000) + n * 86400;
          hint.textContent = `${n} 天后（${fmtDateShort(newKeyState.expiresAt)}）`;
        }
      }
    });
    $("newKeyExpiresCustom").addEventListener("input", (ev) => {
      newKeyState.expiresAt = ev.target.value
        ? Math.floor(new Date(ev.target.value).getTime() / 1000)
        : 0;
      $("newKeyExpiresHint").textContent = newKeyState.expiresAt
        ? "过期于 " + fmtDateShort(newKeyState.expiresAt)
        : "永不过期";
    });
  }

  function bindQuotaPicker() {
    const host = $("newKeyQuotaPicker");
    if (!host) return;
    host.addEventListener("click", (ev) => {
      const b = ev.target.closest(".preset-chip");
      if (!b) return;
      const tokens = b.dataset.tokens;
      const custom = $("newKeyQuotaCustom");
      const hint = $("newKeyQuotaHint");
      if (tokens === "custom") {
        setActiveChip("newKeyQuotaPicker", "custom");
        custom.hidden = false;
        newKeyState.quotaLimit = parseInt(custom.value || "0", 10) || 0;
        hint.textContent = newKeyState.quotaLimit
          ? `上限 ${fmtTokens(newKeyState.quotaLimit)} tokens`
          : "请输入 token 数";
        custom.focus();
      } else {
        setActiveChip("newKeyQuotaPicker", tokens);
        custom.hidden = true;
        const n = parseInt(tokens, 10) || 0;
        newKeyState.quotaLimit = n;
        hint.textContent = n
          ? `上限 ${fmtTokens(n)} tokens`
          : "不限制（input + output tokens）";
      }
    });
    $("newKeyQuotaCustom").addEventListener("input", (ev) => {
      newKeyState.quotaLimit = parseInt(ev.target.value || "0", 10) || 0;
      $("newKeyQuotaHint").textContent = newKeyState.quotaLimit
        ? `上限 ${fmtTokens(newKeyState.quotaLimit)} tokens`
        : "请输入 token 数";
    });
  }

  async function submitAddAPIKey(ev) {
    ev.preventDefault();
    const label = ($("newKeyLabel").value || "").trim();
    if (!label) { toast("请输入标签", "error"); return; }
    const expiresAt = newKeyState.expiresAt || 0;
    const quotaLimit = newKeyState.quotaLimit || 0;
    try {
      const r = await apiFetch("/admin/api-keys", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          label,
          expires_at:  expiresAt,
          quota_limit: quotaLimit,
        }),
      });
      if (!r.ok) {
        const text = await r.text().catch(() => "");
        throw new Error(`${r.status} ${r.statusText}${text ? ": " + text : ""}`);
      }
      const created = await r.json();
      $("addAPIKeyDialog").close();
      showRevealKey(created.key || "");
      toast("已创建 API 密钥", "success");
      loadAPIKeys();
    } catch (e) {
      toast("创建失败：" + e.message, "error");
    }
  }

  function showRevealKey(plaintext) {
    const block = $("apiKeyRevealBlock");
    const value = $("apiKeyRevealValue");
    if (!block || !value) return;
    value.textContent = plaintext;
    block.hidden = false;
  }

  async function rotateAPIKey(id) {
    if (!confirm("确认轮换该密钥？旧密钥立即失效。")) return;
    try {
      const r = await apiFetch(`/admin/api-keys/${encodeURIComponent(id)}/rotate`, { method: "POST" });
      if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
      const data = await r.json();
      showRevealKey(data.key || data.plaintext || "");
      toast("已轮换密钥", "success");
      loadAPIKeys();
    } catch (e) {
      toast("轮换失败：" + e.message, "error");
    }
  }

  async function toggleAPIKey(id, enabled) {
    try {
      const r = await apiFetch(`/admin/api-keys/${encodeURIComponent(id)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ enabled }),
      });
      if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
      toast(enabled ? "已启用" : "已禁用", "success");
      loadAPIKeys();
    } catch (e) {
      toast("更新失败：" + e.message, "error");
    }
  }

  async function deleteAPIKey(id, label) {
    if (!confirm(`确认删除密钥 “${label || id}”？此操作不可恢复。`)) return;
    try {
      const r = await apiFetch(`/admin/api-keys/${encodeURIComponent(id)}`, { method: "DELETE" });
      if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
      toast("已删除", "success");
      loadAPIKeys();
    } catch (e) {
      toast("删除失败：" + e.message, "error");
    }
  }

  function copyRevealKey() {
    const v = $("apiKeyRevealValue");
    if (!v) return;
    const text = v.textContent;
    if (!text) { toast("无可复制内容", "error"); return; }
    navigator.clipboard.writeText(text).then(
      () => toast("已复制到剪贴板", "success"),
      () => toast("复制失败，请手动选中", "error"),
    );
  }

  // ---------- 添加账号 modal (3 tabs: OAuth / Token / Local) ----------

  const PROVIDER_LABELS = {
    kiro:  "Kiro",
    codex: "ChatGPT (Codex)",
  };

  let addProviderID = "kiro";
  let oauthCurrentState = "";
  let oauthPollTimer = null;

  function openAddProviderDialog(providerID) {
    addProviderID = providerID;
    $("addProviderTitle").textContent = "添加 " + (PROVIDER_LABELS[providerID] || providerID) + " 账号";
    $("oauthProviderName").textContent = PROVIDER_LABELS[providerID] || providerID;
    switchAddTab("oauth");
    resetOAuthBlock();
    $("addTokenPayload").value = "";
    $("localKiroRow").style.display = providerID === "kiro" ? "" : "none";
    openDialog("addProviderDialog");
  }

  function switchAddTab(tab) {
    document.querySelectorAll("[data-add-tab]").forEach(a => {
      a.classList.toggle("active", a.dataset.addTab === tab);
    });
    document.querySelectorAll("[data-add-tab-pane]").forEach(p => {
      p.hidden = p.dataset.addTabPane !== tab;
    });
  }

  function resetOAuthBlock() {
    stopOAuthPolling();
    $("oauthBlock").hidden = true;
    $("oauthStartBlock").hidden = false;
    $("oauthAuthURL").value = "";
    $("oauthLoopbackPort").textContent = "—";
    $("oauthManualPanel").hidden = true;
    $("oauthManualURL").value = "";
    setOAuthStatus("pending", "等待授权完成...");
    $("btnIveAuthed").disabled = true;
    oauthCurrentState = "";
  }

  function setOAuthStatus(status, message) {
    const box = $("oauthStatusBox");
    box.classList.remove("success", "error");
    if (status === "success") box.classList.add("success");
    else if (status === "error" || status === "expired") box.classList.add("error");
    $("oauthStatusText").textContent = message;
    $("oauthSpinner").hidden = status !== "pending";
  }

  async function startOAuthFlow() {
    try {
      const extras = {};
      const proxyURL = ($("oauthProxyURL")?.value || "").trim();
      if (proxyURL) extras.proxy_url = proxyURL;
      const r = await apiFetch("/admin/oauth/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ provider: addProviderID, extras }),
      });
      const text = await r.text();
      if (!r.ok) throw new Error(text);
      const data = JSON.parse(text);
      if (!data.auth_url || !data.state) throw new Error("missing auth_url/state");
      $("oauthAuthURL").value = data.auth_url;
      $("oauthLoopbackPort").textContent = data.loopback_port;
      $("oauthBlock").hidden = false;
      $("oauthStartBlock").hidden = true;
      oauthCurrentState = data.state;
      window.open(data.auth_url, "_blank", "noopener");
      startOAuthPolling(data.state);
    } catch (e) {
      toast("OAuth 启动失败：" + e.message, "error");
    }
  }

  function startOAuthPolling(state) {
    stopOAuthPolling();
    setOAuthStatus("pending", "等待授权完成... 0s / 600s");
    $("btnIveAuthed").disabled = false;
    let elapsedSec = 0;
    oauthPollTimer = setInterval(async () => {
      elapsedSec++;
      try {
        const r = await apiFetch("/admin/oauth/status?state=" + encodeURIComponent(state));
        if (!r.ok) return;
        const data = await r.json();
        if (data.status === "success") {
          stopOAuthPolling();
          setOAuthStatus("success", "授权完成。账号 " + (data.credential_id || "—") + " 已添加。");
          loadAccounts();
          loadHealth();
          setTimeout(() => closeDialog("addProviderDialog"), 1500);
        } else if (data.status === "error" || data.status === "expired") {
          stopOAuthPolling();
          setOAuthStatus(data.status, data.message || data.status);
        } else {
          setOAuthStatus("pending", `等待授权完成... ${elapsedSec}s / 600s`);
        }
      } catch (e) {
        console.warn("oauth poll", e);
      }
    }, 1000);
  }

  function stopOAuthPolling() {
    if (oauthPollTimer) { clearInterval(oauthPollTimer); oauthPollTimer = null; }
  }

  async function submitManualCallback() {
    const url = ($("oauthManualURL").value || "").trim();
    if (!url) { toast("请粘贴回调 URL", "error"); return; }
    if (!oauthCurrentState) { toast("无 OAuth 流程在等待", "error"); return; }
    try {
      const r = await apiFetch("/admin/oauth/manual_callback", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ state: oauthCurrentState, callback_url: url }),
      });
      const text = await r.text();
      if (!r.ok) throw new Error(text);
      toast("已提交，等待状态更新", "success");
    } catch (e) {
      toast("手动回调失败：" + e.message, "error");
    }
  }

  async function submitTokenPayload() {
    const raw = ($("addTokenPayload").value || "").trim();
    if (!raw) { toast("请粘贴内容", "error"); return; }
    let parsed;
    try { parsed = JSON.parse(raw); }
    catch (e) { toast("仅支持 JSON 格式（数组或单条对象）", "error"); return; }
    try {
      let r;
      if (Array.isArray(parsed)) {
        r = await apiFetch("/admin/accounts/import", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ accounts: parsed, mode: "append" }),
        });
      } else if (parsed.accounts && Array.isArray(parsed.accounts)) {
        r = await apiFetch("/admin/accounts/import", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(parsed),
        });
      } else {
        if (!parsed.provider) parsed.provider = addProviderID;
        r = await apiFetch("/admin/accounts", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(parsed),
        });
      }
      const text = await r.text();
      if (!r.ok) throw new Error(text);
      toast("导入完成", "success");
      loadAccounts(); loadHealth();
      closeDialog("addProviderDialog");
    } catch (e) {
      toast("导入失败：" + e.message, "error");
    }
  }

  async function localKiroImport() {
    if (!confirm("从本机 Kiro CLI 数据库读取当前登录的账号并添加到池？")) return;
    try {
      const r = await apiFetch("/admin/import/local-kiro", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({}),
      });
      const text = await r.text();
      if (!r.ok) throw new Error(text);
      const data = JSON.parse(text);
      toast("已导入：" + data.id + " · " + (data.region || "—"), "success");
      loadAccounts(); loadHealth();
      closeDialog("addProviderDialog");
    } catch (e) {
      toast("本机导入失败：" + e.message, "error");
    }
  }

  function onLocalFileChosen(ev) {
    const f = ev.target.files[0];
    if (!f) return;
    const reader = new FileReader();
    reader.onload = () => {
      $("addTokenPayload").value = reader.result;
      switchAddTab("token");
      toast("文件已加载，切换到「Token / JSON」标签，点击「导入」", "success");
    };
    reader.readAsText(f);
  }

  // ---------- modal dialogs --------------------------------------------

  function openDialog(id) {
    const d = $(id);
    if (!d) return;
    if (typeof d.showModal === "function") d.showModal();
    else d.setAttribute("open", "");
  }

  function closeDialog(id) {
    const d = $(id);
    if (!d) return;
    if (typeof d.close === "function") d.close();
    else d.removeAttribute("open");
    // Stop any active OAuth polling when the add-provider modal closes.
    if (id === "addProviderDialog") stopOAuthPolling();
  }

  // Click on the dialog backdrop (i.e. on the dialog element itself, outside
  // the inner form) dismisses the dialog.
  function bindDialogBackdropClose(id) {
    const d = $(id);
    if (!d) return;
    d.addEventListener("click", (ev) => {
      if (ev.target === d) closeDialog(id);
    });
  }

  function bindImportAccountForm() {
    const form = $("importAccountForm");
    if (!form) return;
    const fileInput = $("importFile");
    const textArea = form.querySelector("textarea[name='payload']");
    if (fileInput) {
      fileInput.addEventListener("change", () => {
        const f = fileInput.files[0];
        if (!f) return;
        const reader = new FileReader();
        reader.onload = () => { textArea.value = reader.result; };
        reader.readAsText(f);
      });
    }
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const fd = new FormData(form);
      const raw = (fd.get("payload") || "").toString().trim();
      const mode = fd.get("mode") || "append";
      if (!raw) {
        toast("请先粘贴 JSON 或选择文件", "error");
        return;
      }
      let parsed;
      try { parsed = JSON.parse(raw); }
      catch (e) { toast("JSON 解析失败：" + e.message, "error"); return; }
      const body = Array.isArray(parsed)
        ? { accounts: parsed, mode }
        : Object.assign({}, parsed, { mode: parsed.mode || mode });
      try {
        const r = await apiFetch("/admin/accounts/import", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        });
        const result = await r.json();
        if (!r.ok) throw new Error(result?.error || `${r.status} ${r.statusText}`);
        const parts = [`新增 ${result.added}`];
        if (result.skipped) parts.push(`跳过 ${result.skipped}`);
        if (result.removed) parts.push(`移除 ${result.removed}`);
        if (result.errors && result.errors.length) parts.push(`错误 ${result.errors.length}`);
        toast("导入完成：" + parts.join(" · "), result.errors?.length ? "error" : "success");
        form.reset();
        closeDialog("importAccountDialog");
        loadAccounts();
        loadHealth();
      } catch (e) {
        toast("导入失败：" + e.message, "error");
      }
    });
  }

  // ---------- auto-refresh toggle ---------------------------------------

  function setAuto(on) {
    localStorage.setItem(AUTO_KEY, on ? "1" : "0");
    $("autoToggle").checked = on;
    if (refreshTimer) { clearInterval(refreshTimer); refreshTimer = null; }
    if (on) refreshTimer = setInterval(loadAll, REFRESH_MS);
  }

  // ---------- bootstrap --------------------------------------------------

  document.addEventListener("DOMContentLoaded", () => {
    $("refreshAll").addEventListener("click", loadAll);
    $("autoToggle").addEventListener("change", (e) => setAuto(e.target.checked));
    $("recentLimit")?.addEventListener("change", loadRecent);
    $("recentStatus")?.addEventListener("change", loadRecent);
    $("recentModel")?.addEventListener("change", loadRecent);
    $("recentKey")?.addEventListener("change", loadRecent);
    $("btnExportCSV")?.addEventListener("click", exportRecentCSV);
    $("btnExportJSON")?.addEventListener("click", exportRecentJSON);
    $("recentPageSize")?.addEventListener("change", (ev) => {
      recentChangePageSize(parseInt(ev.target.value, 10) || 50);
    });
    $("recentFirst")?.addEventListener("click", () => recentGoToPage(1));
    $("recentPrev")?.addEventListener("click",  () => recentGoToPage(recentPage - 1));
    $("recentNext")?.addEventListener("click",  () => recentGoToPage(recentPage + 1));
    $("recentLast")?.addEventListener("click",  () => {
      const totalPages = Math.max(1, Math.ceil(recentLastRows.length / recentPageSize));
      recentGoToPage(totalPages);
    });
    $("recentJump")?.addEventListener("click", () => {
      const v = parseInt($("recentJumpPage").value, 10);
      if (!isNaN(v)) recentGoToPage(v);
    });
    $("recentJumpPage")?.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter") {
        ev.preventDefault();
        $("recentJump").click();
      }
    });

    // Tab routing
    document.querySelectorAll("#tabs a").forEach(a => {
      a.addEventListener("click", (ev) => {
        ev.preventDefault();
        location.hash = a.getAttribute("href");
      });
    });
    window.addEventListener("hashchange", () => {
      const r = pageFromHash();
      navigateTo(r.page, r.section);
    });

    // Account management — per-provider 添加 buttons open the unified modal.
    document.querySelectorAll("[data-add-provider]").forEach(b => {
      b.addEventListener("click", () => openAddProviderDialog(b.dataset.addProvider));
    });
    $("btnImportAccount")?.addEventListener("click", () => openDialog("importAccountDialog"));
    $("btnRefreshAllQuota")?.addEventListener("click", onRefreshAllQuota);
    bindCredsFilePage();

    // Modal dialogs: close buttons + backdrop close.
    document.querySelectorAll("[data-close-dialog]").forEach(b => {
      b.addEventListener("click", () => closeDialog(b.dataset.closeDialog));
    });
    bindDialogBackdropClose("addProviderDialog");
    bindDialogBackdropClose("importAccountDialog");

    // Tab switching inside the add-provider modal.
    document.querySelectorAll("[data-add-tab]").forEach(a => {
      a.addEventListener("click", (ev) => {
        ev.preventDefault();
        switchAddTab(a.dataset.addTab);
      });
    });

    // OAuth tab actions
    $("btnStartOAuth")?.addEventListener("click", startOAuthFlow);
    $("btnOpenAuthURL")?.addEventListener("click", () => {
      const u = $("oauthAuthURL").value;
      if (u) window.open(u, "_blank", "noopener");
    });
    $("btnCopyAuthURL")?.addEventListener("click", () => {
      const u = $("oauthAuthURL");
      u.select();
      navigator.clipboard.writeText(u.value).then(
        () => toast("已复制到剪贴板", "success"),
        () => toast("复制失败，请手动选中复制", "error")
      );
    });
    $("btnManualCallback")?.addEventListener("click", () => {
      const p = $("oauthManualPanel");
      p.hidden = !p.hidden;
    });
    $("btnSubmitManualCallback")?.addEventListener("click", submitManualCallback);
    $("btnIveAuthed")?.addEventListener("click", () => {
      toast("已在后台轮询，请稍候…", "success");
    });

    // Token tab + Local tab actions
    $("btnSubmitTokenPayload")?.addEventListener("click", submitTokenPayload);
    $("btnLocalKiroImport")?.addEventListener("click", localKiroImport);
    $("btnLocalFilePick")?.addEventListener("click", () => $("localFilePicker").click());
    $("localFilePicker")?.addEventListener("change", onLocalFileChosen);

    bindImportAccountForm();

    // Settings page bindings
    document.querySelectorAll("[data-save]").forEach(b => {
      b.addEventListener("click", () => saveSettings(b.dataset.save, b));
    });
    $("btnAddAPIKey")?.addEventListener("click", openAddAPIKeyDialog);
    $("addAPIKeyForm")?.addEventListener("submit", submitAddAPIKey);
    $("btnCopyRevealKey")?.addEventListener("click", copyRevealKey);
    bindDialogBackdropClose("addAPIKeyDialog");
    bindExpiresPicker();
    bindQuotaPicker();
    $("tokenChartRange")?.addEventListener("click", (ev) => {
      const b = ev.target.closest(".preset-chip");
      if (!b) return;
      loadTokenChart(b.dataset.range);
    });
    $("tokenChartType")?.addEventListener("click", (ev) => {
      const b = ev.target.closest(".preset-chip");
      if (!b) return;
      setTokenChartType(b.dataset.type);
    });

    const saved = localStorage.getItem(AUTO_KEY);
    setAuto(saved !== "0"); // default on
    {
      const r = pageFromHash();
      navigateTo(r.page, r.section);
    }
  });
})();
