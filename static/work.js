const form = document.querySelector("#promptForm");
const promptInput = document.querySelector("#prompt");
const promptMeta = document.querySelector("#promptMeta");
const runButton = document.querySelector("#runButton");
const refreshButton = document.querySelector("#refreshButton");
const statusPill = document.querySelector("#statusPill");
const logDetails = document.querySelector("#logDetails");
const logSummary = document.querySelector("#logSummary");
const logOutput = document.querySelector("#logOutput");
const jobsEl = document.querySelector("#jobs");
const fileList = document.querySelector("#fileList");
const pathInput = document.querySelector("#pathInput");
const goPathButton = document.querySelector("#goPathButton");
const upButton = document.querySelector("#upButton");
const fileUpload = document.querySelector("#fileUpload");
const uploadButton = document.querySelector("#uploadButton");
const fileInputSummary = document.querySelector("#fileInputSummary");
const refreshFilesButton = document.querySelector("#refreshFilesButton");

let activeJobId = null;
let pollTimer = null;
let currentPath = ".";

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const prompt = promptInput.value.trim();
  if (!prompt) {
    setStatus("需要任务", "warn");
    promptInput.focus();
    return;
  }

  runButton.disabled = true;
  setStatus("排队中", "busy");
  setLog("提交任务...\n", "提交任务");
  logDetails.open = true;

  try {
    const response = await fetch("/api/work/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt }),
    });
    if (!response.ok) throw new Error(await response.text());
    const data = await response.json();
    activeJobId = data.id;
    startPolling(data.id);
    await loadJobs();
  } catch (error) {
    runButton.disabled = false;
    setStatus("失败", "error");
    setLog(`${logOutput.textContent}提交失败：${error.message}\n`, "提交失败");
  }
});

promptInput.addEventListener("input", renderPromptMeta);
refreshButton.addEventListener("click", async () => {
  if (activeJobId) await fetchJob(activeJobId);
  await loadJobs();
});
refreshFilesButton.addEventListener("click", () => loadFiles(currentPath));
goPathButton.addEventListener("click", () => loadFiles(pathInput.value.trim() || "."));
pathInput.addEventListener("keydown", (event) => {
  if (event.key === "Enter") loadFiles(pathInput.value.trim() || ".");
});
upButton.addEventListener("click", () => loadFiles(parentPath(currentPath)));
fileUpload.addEventListener("change", () => {
  fileInputSummary.textContent = fileUpload.files.length ? `${fileUpload.files.length} 个文件` : "未选择文件";
});
uploadButton.addEventListener("click", uploadFiles);

function startPolling(id) {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = setInterval(() => fetchJob(id), 1200);
  fetchJob(id);
}

async function fetchJob(id) {
  const response = await fetch(`/api/work/jobs/${id}`);
  if (!response.ok) return;
  const job = await response.json();
  renderJob(job);
  await loadJobs();

  if (job.status === "succeeded" || job.status === "failed") {
    clearInterval(pollTimer);
    pollTimer = null;
    runButton.disabled = false;
    await loadFiles(currentPath);
  }
}

async function loadJobs() {
  const response = await fetch("/api/work/jobs");
  if (!response.ok) return;
  const jobs = await response.json();
  jobsEl.innerHTML = "";
  for (const job of jobs) {
    const button = document.createElement("button");
    button.className = `job-item ${job.id === activeJobId ? "active" : ""}`;
    button.type = "button";
    button.innerHTML = `
      <span class="job-status ${job.status}">${escapeHTML(job.mode || "pic")} · ${jobStatusText(job.status)}</span>
      <span class="job-prompt">${escapeHTML(job.prompt)}</span>
      <span class="job-time">${new Date(job.createdAt).toLocaleString()}</span>
    `;
    button.addEventListener("click", () => {
      activeJobId = job.id;
      renderJob(job);
      loadJobs();
      if (job.status === "queued" || job.status === "running") startPolling(job.id);
    });
    jobsEl.appendChild(button);
  }
}

function renderJob(job) {
  if (job.status === "queued") setStatus("排队中", "busy");
  if (job.status === "running") setStatus("运行中", "busy");
  if (job.status === "succeeded") setStatus("完成", "ok");
  if (job.status === "failed") setStatus("失败", "error");
  setLog(job.log || "等待日志。", logLabel(job));
}

async function loadFiles(path) {
  const response = await fetch(`/api/work/files?path=${encodeURIComponent(path || ".")}`);
  if (!response.ok) {
    setStatus("文件失败", "error");
    return;
  }
  const data = await response.json();
  currentPath = data.path || ".";
  pathInput.value = currentPath;
  renderFiles(data.entries || []);
}

function renderFiles(entries) {
  fileList.innerHTML = "";
  if (!entries.length) {
    const empty = document.createElement("div");
    empty.className = "empty-state compact-empty";
    empty.textContent = "当前目录为空。";
    fileList.appendChild(empty);
    return;
  }

  for (const entry of entries) {
    const row = document.createElement("div");
    row.className = "file-row";
    const icon = entry.isDir ? "DIR" : entry.isImage ? "IMG" : "FILE";
    const name = entry.isDir
      ? `<button type="button" class="file-link" data-open="${escapeAttr(entry.path)}">${escapeHTML(entry.name)}</button>`
      : `<span>${escapeHTML(entry.name)}</span>`;
    const preview = entry.previewUrl ? `<img src="${entry.previewUrl}" alt="${escapeHTML(entry.name)}">` : `<span class="file-icon">${icon}</span>`;
    const actions = entry.isDir
      ? `<button type="button" class="secondary" data-delete="${escapeAttr(entry.path)}">删除</button>`
      : `<a class="file-action" href="${entry.downloadUrl}">下载</a><button type="button" class="secondary" data-delete="${escapeAttr(entry.path)}">删除</button>`;
    row.innerHTML = `
      <div class="file-preview">${preview}</div>
      <div class="file-main">
        ${name}
        <span>${formatSize(entry.size)} · ${new Date(entry.modTime).toLocaleString()}</span>
      </div>
      <div class="file-actions">${actions}</div>
    `;
    fileList.appendChild(row);
  }

  fileList.querySelectorAll("[data-open]").forEach((button) => {
    button.addEventListener("click", () => loadFiles(button.dataset.open));
  });
  fileList.querySelectorAll("[data-delete]").forEach((button) => {
    button.addEventListener("click", () => deletePath(button.dataset.delete));
  });
}

async function uploadFiles() {
  if (!fileUpload.files.length) {
    setStatus("未选择文件", "warn");
    return;
  }
  const formData = new FormData();
  for (const file of fileUpload.files) formData.append("files", file);
  const response = await fetch(`/api/work/files/upload?path=${encodeURIComponent(currentPath)}`, {
    method: "POST",
    body: formData,
  });
  if (!response.ok) {
    setStatus("上传失败", "error");
    return;
  }
  fileUpload.value = "";
  fileInputSummary.textContent = "未选择文件";
  setStatus("已上传", "ok");
  await loadFiles(currentPath);
}

async function deletePath(path) {
  if (!confirm(`删除 ${path}？`)) return;
  const response = await fetch(`/api/work/files?path=${encodeURIComponent(path)}`, { method: "DELETE" });
  if (!response.ok) {
    setStatus("删除失败", "error");
    return;
  }
  setStatus("已删除", "ok");
  await loadFiles(currentPath);
}

function setStatus(text, tone) {
  statusPill.textContent = text;
  statusPill.className = `status-pill ${tone || ""}`;
}

function setLog(text, summary) {
  logOutput.textContent = text;
  logOutput.scrollTop = logOutput.scrollHeight;
  logSummary.textContent = summary;
}

function logLabel(job) {
  if (job.status === "queued") return "排队中";
  if (job.status === "running") return lastLogLine(job.log) || "运行中";
  if (job.status === "succeeded") return "完成";
  if (job.status === "failed") return "失败";
  return "等待任务";
}

function lastLogLine(log) {
  return String(log || "")
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
    .at(-1);
}

function renderPromptMeta() {
  promptMeta.textContent = `${promptInput.value.trim().length} 字`;
}

function parentPath(path) {
  const parts = String(path || ".").split(/[\\/]+/).filter(Boolean);
  parts.pop();
  return parts.length ? parts.join("/") : ".";
}

function formatSize(size) {
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / 1024 / 1024).toFixed(1)} MB`;
}

function jobStatusText(status) {
  if (status === "queued") return "排队";
  if (status === "running") return "运行";
  if (status === "succeeded") return "完成";
  if (status === "failed") return "失败";
  return status;
}

function escapeHTML(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function escapeAttr(value) {
  return escapeHTML(value).replaceAll("`", "&#096;");
}

loadJobs();
loadFiles(".");
renderPromptMeta();
