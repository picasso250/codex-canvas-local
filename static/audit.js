const refreshButton = document.querySelector("#refreshButton");
const filterForm = document.querySelector("#filterForm");
const emailFilter = document.querySelector("#emailFilter");
const clearFilterButton = document.querySelector("#clearFilterButton");
const statusPill = document.querySelector("#statusPill");
const auditSummary = document.querySelector("#auditSummary");
const auditList = document.querySelector("#auditList");

let auditLines = [];

refreshButton.addEventListener("click", loadAudit);
filterForm.addEventListener("submit", (event) => {
  event.preventDefault();
  renderAudit();
});
emailFilter.addEventListener("change", renderAudit);
clearFilterButton.addEventListener("click", () => {
  emailFilter.value = "";
  renderAudit();
});
loadAudit();

async function loadAudit() {
  setStatus("加载中", "busy");
  auditSummary.textContent = "正在读取。";

  try {
    const selectedEmail = emailFilter.value;
    const response = await fetch("/api/audit");
    if (response.status === 403) {
      setStatus("无权限", "error");
      auditSummary.textContent = "当前用户没有权限查看审计记录。";
      auditList.innerHTML = "";
      return;
    }
    if (!response.ok) throw new Error(await response.text());

    const data = await response.json();
    auditLines = data.lines || [];
    renderEmailOptions(data.emails || [], selectedEmail);
    renderAudit();
    setStatus("就绪", "ok");
  } catch (error) {
    setStatus("失败", "error");
    auditSummary.textContent = `读取失败：${error.message}`;
    auditList.innerHTML = "";
  }
}

function renderEmailOptions(emails, selectedEmail) {
  emailFilter.innerHTML = `<option value="">全部用户</option>`;
  for (const email of emails) {
    const option = document.createElement("option");
    option.value = email;
    option.textContent = email;
    emailFilter.appendChild(option);
  }
  if (emails.includes(selectedEmail)) {
    emailFilter.value = selectedEmail;
  }
}

function renderAudit() {
  const email = emailFilter.value;
  const lines = email ? auditLines.filter((line) => auditLineEmail(line) === email) : auditLines;
  auditList.innerHTML = "";
  const valid = lines.filter((line) => line.event).length;
  const invalid = lines.length - valid;
  const filterText = email ? `，过滤：${email}` : "";
  auditSummary.textContent = `${valid} 条记录${invalid ? `，${invalid} 条解析失败` : ""}${filterText}`;

  if (!lines.length) {
    const empty = document.createElement("div");
    empty.className = "empty-state compact-empty";
    empty.textContent = "还没有审计记录。";
    auditList.appendChild(empty);
    return;
  }

  for (const line of lines) {
    auditList.appendChild(renderAuditLine(line));
  }
}

function auditLineEmail(line) {
  if (!line.event) return "";
  return line.event.email || "local";
}

function renderAuditLine(line) {
  const row = document.createElement("article");
  row.className = `audit-row ${line.error ? "audit-row-error" : ""}`;

  if (line.error) {
    row.innerHTML = `
      <div class="audit-row-head">
        <span class="job-status failed">解析失败</span>
        <strong>Line ${line.line}</strong>
        <span>${escapeHTML(line.error)}</span>
      </div>
      <pre>${escapeHTML(line.raw || "")}</pre>
    `;
    return row;
  }

  const event = line.event;
  row.innerHTML = `
    <div class="audit-row-head">
      <span class="job-status succeeded">${escapeHTML(event.event || "event")}</span>
      <strong>${escapeHTML(event.jobId || "-")}</strong>
      <span>${formatDate(event.createdAt)}</span>
    </div>
    <div class="audit-grid">
      <span>Email</span><strong>${escapeHTML(event.email || "local")}</strong>
      <span>IP</span><strong>${escapeHTML(event.ip || "-")}</strong>
      <span>WorkDir</span><strong>${escapeHTML(event.workDir || "-")}</strong>
      <span>UserAgent</span><strong>${escapeHTML(event.userAgent || "-")}</strong>
    </div>
    <div class="audit-prompt">${escapeHTML(event.prompt || "")}</div>
    <details class="audit-details">
      <summary>原始 JSON</summary>
      <pre>${escapeHTML(JSON.stringify(event, null, 2))}</pre>
    </details>
  `;
  return row;
}

function setStatus(text, cls = "") {
  statusPill.textContent = text;
  statusPill.className = `status-pill ${cls}`.trim();
}

function formatDate(value) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, (char) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  }[char]));
}
