/* 定时任务：参考项目 admin/console/index.html「定时任务」段的 Go 面板移植版。
   协议与参考一致：/api/schedules → /panel/api/schedules，last_result 是完整 JSON。
   严格 CSP：页面无内联处理器，全部按钮走事件委托。 */
window.WBSchedules = (() => {
  const TASKS = {
    refresh_balances: "刷新平台总积分", refresh_credits: "更新积分到期快照",
    sync_models: "同步模型列表", daily_checkin: "签到领取积分",
    refresh_growth_tasks: "更新成长任务列表", run_growth_tasks: "自动做成长任务",
    run_growth_plan: "WorkBuddy 成长计划巡检", keepalive_tokens: "token 保活刷新",
    ban_recheck: "风控封号复核与自动解封", travel: "猫猫旅行巡检",
    activity: "每日活跃上报", school: "开学季活动", cooldown_probe: "冷却探活",
    streak_bonus: "连登奖励与抽奖", blackcat: "夜猫子任务"
  };
  let rows = [], editing = 0, pollTimer = null, loading = false;
  const $ = id => document.getElementById(id);
  const esc = s => String(s == null ? "" : s).replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]));
  const fmt = v => { if (!v) return "—"; const d = new Date(v); return isNaN(d.getTime()) ? esc(v) : esc(d.toLocaleString("zh-CN", {hour12:false})); };
  function summary(s) {
    const raw = s.last_result || "";
    if (!raw) return "<span class=\"muted\">尚未运行</span>";
    let o = null; try { o = JSON.parse(raw); } catch (e) {}
    if (!o) return "<span class=\"tag warn\">历史结果（旧格式）</span>";
    if (o.error) return "<span class=\"bad\">失败：" + esc(o.error) + "</span>";
    if (o.skipped) return "<span class=\"muted\">已跳过：" + esc(o.skipped) + "</span>";
    if (o.summary) return esc(o.summary) + (o.elapsed_s ? " · " + esc(o.elapsed_s) + "s" : "");
    if (o.claimed) return "新领 " + esc(o.claimed) + " 个 · +" + esc(o.credit || 0) + " 积分";
    if (o.refreshed != null) return "刷新 " + esc(o.refreshed) + " 个";
    return esc(raw.slice(0, 120));
  }
  async function load(quiet) {
    if (loading) return; loading = true;
    try {
      const d = await api("schedules"); rows = d.items || []; render();
      clearTimeout(pollTimer);
      if (rows.some(s => { try { return JSON.parse(s.last_result || "{}").running; } catch (e) { return false; } })) pollTimer = setTimeout(() => load(true), 4000);
    } catch (e) { if (!quiet) toast("读取定时任务失败：" + e.message, "err"); }
    finally { loading = false; }
  }
  function render() {
    const box = $("scheduleList"); if (!box) return;
    if (!rows.length) { box.innerHTML = "<div class=\"state\">还没有定时任务</div>"; return; }
    box.innerHTML = "<div class=\"tbl-wrap\"><table class=\"acc\"><thead><tr>" +
      "<th>任务名</th><th>类型</th><th>执行周期</th><th>下次运行</th><th>上次结果</th><th>状态</th><th style=\"text-align:right\">操作</th>" +
      "</tr></thead><tbody>" + rows.map(s => "<tr>" +
        "<td>" + esc(s.display_name || s.name) + (s.stop_after ? "<small class=\"muted\" style=\"display:block\">停止于 " + fmt(s.stop_after) + "</small>" : "") + "</td>" +
        "<td class=\"muted\">" + esc(s.task_label || TASKS[s.task] || s.task) + "</td>" +
        "<td class=\"muted\">" + esc(s.cycle_label || ("每 " + s.interval_minutes + " 分钟")) + "</td>" +
        "<td class=\"muted\">" + fmt(s.next_run_at) + "</td>" +
        "<td>" + summary(s) + (s.last_result ? " <button type=\"button\" class=\"xs\" data-schedule=\"detail\" data-id=\"" + s.id + "\">详情</button>" : "") + "</td>" +
        "<td>" + (s.enabled ? "<span class=\"tag ok\">启用</span>" : "<span class=\"tag\">禁用</span>") + "</td>" +
        "<td style=\"text-align:right;white-space:nowrap\">" +
          "<button type=\"button\" class=\"xs\" data-schedule=\"run\" data-id=\"" + s.id + "\" title=\"立即运行\">▶</button> " +
          "<button type=\"button\" class=\"xs\" data-schedule=\"toggle\" data-id=\"" + s.id + "\" title=\"启停\">⏻</button> " +
          "<button type=\"button\" class=\"xs\" data-schedule=\"edit\" data-id=\"" + s.id + "\" title=\"编辑\">✎</button> " +
          "<button type=\"button\" class=\"xs\" data-schedule=\"del\" data-id=\"" + s.id + "\" title=\"删除\">✕</button>" +
        "</td></tr>").join("") + "</tbody></table></div>";
  }
  function taskOptions(sel) {
    $("scTask").innerHTML = Object.entries(TASKS).map(([k,v]) => "<option value=\"" + k + "\"" + (k === sel ? " selected" : "") + ">" + esc(v) + "</option>").join("");
  }
  function syncFormVisibility() {
    const mode = $("scCycle").value;
    $("scIntervalWrap").hidden = !["preset", "interval"].includes(mode);
    $("scCalendarWrap").hidden = !["daily", "weekly", "monthly"].includes(mode);
    $("scWeekWrap").hidden = mode !== "weekly";
    $("scMonthWrap").hidden = mode !== "monthly";
  }
  function openForm(row) {
    editing = row ? Number(row.id) : 0;
    $("scModalTitle").textContent = row ? "编辑定时任务" : "新建定时任务";
    taskOptions(row ? row.task : "refresh_balances");
    $("scName").value = row ? (row.name || "") : "";
    $("scName").removeAttribute("readonly");
    $("scTask").disabled = false;
    const t = row && row.timing ? row.timing : {mode:"preset", time:"09:00", weekdays:[0], monthday:1};
    $("scCycle").value = t.mode || "preset";
    $("scIv").value = row ? (row.interval_minutes || 60) : 60;
    $("scTime").value = t.time || "09:00";
    $("scMonthday").value = t.monthday || 1;
    document.querySelectorAll("[name=scWeekday]").forEach(el => { el.checked = (t.weekdays || [0]).includes(Number(el.value)); });
    $("scStop").value = row && row.stop_after ? String(row.stop_after).slice(0,16) : "";
    $("scEnabled").checked = row ? !!row.enabled : true;
    $("scErr").hidden = true;
    syncFormVisibility();
    $("scheduleModal").classList.add("on");
  }
  function closeForm() { $("scheduleModal").classList.remove("on"); editing = 0; }
  async function save() {
    const mode = $("scCycle").value;
    const weekdays = [...document.querySelectorAll("[name=scWeekday]:checked")].map(el => Number(el.value));
    if (mode === "weekly" && !weekdays.length) { $("scErr").textContent = "请至少选择一周中的一天"; $("scErr").hidden = false; return; }
    const minutes = Number($("scIv").value);
    if (!Number.isInteger(minutes) || minutes < 1 || minutes > 5256000) { $("scErr").textContent = "请输入有效的执行间隔"; $("scErr").hidden = false; return; }
    const body = {
      name: $("scName").value.trim(), task: $("scTask").value,
      interval_minutes: minutes, enabled: $("scEnabled").checked,
      stop_after: $("scStop").value && !isNaN(new Date($("scStop").value).getTime()) ? new Date($("scStop").value).toISOString() : null,
      timing: { mode, time: $("scTime").value || "09:00", weekdays: weekdays.length ? weekdays : [0], monthday: Number($("scMonthday").value) || 1 }
    };
    try {
      if (editing) await api("schedules/" + editing, {method:"PATCH", body: JSON.stringify(body)});
      else await api("schedules", {method:"POST", body: JSON.stringify(body)});
      closeForm(); await load(); toast("已保存", "ok");
    } catch (e) { $("scErr").textContent = e.message; $("scErr").hidden = false; }
  }
  function detail(id) {
    const s = rows.find(x => Number(x.id) === Number(id)); if (!s) return;
    let pretty = s.last_result || "（无）";
    try { pretty = JSON.stringify(JSON.parse(s.last_result), null, 2); } catch (e) {}
    const dlg = document.createElement("dialog");
    dlg.className = "gc-dialog";
    dlg.style.setProperty("--gc-dialog-width", "720px");
    dlg.innerHTML = "<div class=\"gc-dialog-head\"><h2>上次运行结果 — " + esc(s.display_name || s.name) + "</h2>" +
      "<button class=\"gc-dialog-close\" aria-label=\"关闭\" data-schedule-detail-close=\"1\">×</button></div>" +
      "<div class=\"gc-dialog-body\"><div class=\"gc-notice\">" + summary(s) + "</div>" +
      "<pre style=\"white-space:pre-wrap;overflow:auto;max-height:55vh;font:12px/1.6 var(--mono)\">" + esc(pretty) + "</pre></div>";
    document.body.append(dlg);
    dlg.addEventListener("click", ev => { if (ev.target.dataset.scheduleDetailClose || ev.target === dlg) dlg.remove(); });
    dlg.showModal();
  }

  async function action(kind, id) {
    try {
      if (kind === "run") { await api("schedules/" + id + "/run", {method:"POST"}); toast("任务已在后台启动", "ok"); }
      else if (kind === "toggle") await api("schedules/" + id + "/toggle", {method:"POST"});
      else if (kind === "del") { if (!confirm("确认删除该定时任务？")) return; await api("schedules/" + id, {method:"DELETE"}); toast("已删除", "ok"); }
      else if (kind === "edit") { openForm(rows.find(x => Number(x.id) === Number(id))); return; }
      else if (kind === "detail") { detail(id); return; }
      await load(true);
    } catch (e) { toast("操作失败：" + e.message, "err"); }
  }
  document.addEventListener("click", ev => {
    const b = ev.target.closest("[data-schedule]");
    if (b) { action(b.dataset.schedule, b.dataset.id); return; }
    if (ev.target.id === "btnScheduleAdd") openForm(null);
    if (ev.target.id === "btnScheduleRefresh") load();
    if (ev.target.id === "scCancel") closeForm();
    if (ev.target.id === "scSave") save();
    if (ev.target.id === "scheduleModal") closeForm();
  });
  document.addEventListener("change", ev => { if (ev.target.id === "scCycle") syncFormVisibility(); });
  return {load, enter: () => load(), leave: () => clearTimeout(pollTimer), action, openForm, closeForm, save};
})();
