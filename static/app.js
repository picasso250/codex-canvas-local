const form = document.querySelector("#promptForm");
const promptInput = document.querySelector("#prompt");
const imageInput = document.querySelector("#referenceImages");
const referenceList = document.querySelector("#referenceList");
const referenceMeta = document.querySelector("#referenceMeta");
const fileInputSummary = document.querySelector("#fileInputSummary");
const promptMeta = document.querySelector("#promptMeta");
const runButton = document.querySelector("#runButton");
const refreshButton = document.querySelector("#refreshButton");
const statusPill = document.querySelector("#statusPill");
const logDetails = document.querySelector("#logDetails");
const logSummary = document.querySelector("#logSummary");
const logOutput = document.querySelector("#logOutput");
const gallery = document.querySelector("#gallery");
const imageCount = document.querySelector("#imageCount");
const jobsEl = document.querySelector("#jobs");
const upgradeModal = document.querySelector("#upgradeModal");
const upgradeDismiss = document.querySelector("#upgradeDismiss");
const usageLimits = document.querySelector("#usageLimits");

let activeJobId = null;
let pollTimer = null;
const autoCollapsedJobs = new Set();
const upgradeNoticeKey = "codexCanvas.upgradeNotice.gptImage2Workdir.v1";
const previewUrls = new WeakMap();

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const prompt = promptInput.value.trim();
  if (!prompt) {
    setStatus("需要提示词", "warn");
    promptInput.focus();
    return;
  }

  runButton.disabled = true;
  setStatus("排队中", "busy");
  setLog("提交任务...\n", "提交任务");
  logDetails.open = true;
  clearGallery();

  try {
    const referenceFiles = [...imageInput.files];
    const references = referenceFiles.length ? await uploadReferences(referenceFiles) : [];
    const codexPrompt = buildImagePrompt(prompt, references);

    const response = await fetch("/api/work/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt: codexPrompt }),
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
promptInput.addEventListener("paste", handlePromptPaste);
imageInput.addEventListener("change", renderReferenceList);

refreshButton.addEventListener("click", async () => {
  if (activeJobId) await fetchJob(activeJobId);
  await loadJobs();
});

upgradeDismiss.addEventListener("click", dismissUpgradeNotice);

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
      <span class="job-status ${job.status}">${jobStatusText(job.status)}</span>
      <span class="job-prompt">${escapeHTML(displayPrompt(job.prompt))}</span>
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
  maybeCollapseLog(job);
  renderImages(job.images || []);
}

function renderImages(images) {
  gallery.innerHTML = "";
  gallery.classList.toggle("empty", images.length === 0);
  imageCount.textContent = `${images.length} 张`;

  if (!images.length) {
    const empty = document.createElement("div");
    empty.className = "empty-state";
    empty.textContent = "生成完成后，图片会显示在这里。";
    gallery.appendChild(empty);
    return;
  }

  for (const image of images) {
    const item = document.createElement("a");
    item.className = "image-card";
    item.href = image.url;
    item.target = "_blank";
    item.rel = "noreferrer";
    item.innerHTML = `
      <img src="${image.url}" alt="${escapeHTML(image.name)}">
      <span>${escapeHTML(image.name)}</span>
    `;
    gallery.appendChild(item);
  }
}

function clearGallery() {
  renderImages([]);
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
  if (job.status === "succeeded") return `完成 · ${(job.images || []).length} 张图片`;
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

function maybeCollapseLog(job) {
  const done = job.status === "succeeded" || job.status === "failed";
  if (!done || autoCollapsedJobs.has(job.id)) return;
  logDetails.open = false;
  autoCollapsedJobs.add(job.id);
}

function renderReferenceList() {
  const files = [...imageInput.files];
  referenceMeta.textContent = `${files.length} 张参考图`;
  fileInputSummary.textContent = files.length ? `${files.length} 个文件` : "未选择文件";
  if (!files.length) {
    referenceList.textContent = "未选择图片。";
    referenceList.classList.remove("has-files");
    return;
  }

  referenceList.classList.add("has-files");
  referenceList.innerHTML = files
    .map(
      (file, index) => `
        <figure class="reference-thumb">
          <img src="${escapeAttr(previewURL(file))}" alt="${escapeHTML(file.name)}">
          <figcaption>${index + 1}. ${escapeHTML(file.name)}</figcaption>
        </figure>
      `,
    )
    .join("");
}

function previewURL(file) {
  if (!previewUrls.has(file)) previewUrls.set(file, URL.createObjectURL(file));
  return previewUrls.get(file);
}

function handlePromptPaste(event) {
  const pastedImages = [...(event.clipboardData?.files || [])].filter((file) => file.type.startsWith("image/"));
  if (!pastedImages.length) return;

  event.preventDefault();
  appendReferenceFiles(pastedImages);
  renderReferenceList();
  setStatus(`已粘贴 ${pastedImages.length} 张图`, "ok");
}

function appendReferenceFiles(files) {
  const dataTransfer = new DataTransfer();
  for (const file of imageInput.files) dataTransfer.items.add(file);
  for (const file of files) dataTransfer.items.add(namedPastedImage(file));
  imageInput.files = dataTransfer.files;
}

function namedPastedImage(file) {
  if (file.name && file.name !== "image.png") return file;
  const ext = imageExtension(file.type);
  return new File([file], `pasted-${Date.now()}-${Math.random().toString(36).slice(2, 7)}${ext}`, {
    type: file.type,
    lastModified: file.lastModified || Date.now(),
  });
}

function imageExtension(type) {
  if (type === "image/jpeg") return ".jpg";
  if (type === "image/webp") return ".webp";
  if (type === "image/gif") return ".gif";
  return ".png";
}

function renderPromptMeta() {
  promptMeta.textContent = `${promptInput.value.trim().length} 字`;
}

async function uploadReferences(files) {
  setStatus("上传参考图", "busy");
  const path = `uploads/${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
  const formData = new FormData();
  for (const file of files) formData.append("files", file);

  const response = await fetch(`/api/work/files/upload?path=${encodeURIComponent(path)}`, {
    method: "POST",
    body: formData,
  });
  if (!response.ok) throw new Error(await response.text());

  const data = await response.json();
  return data.files || [];
}

function buildImagePrompt(prompt, references) {
  if (!references.length) return `use skill $imagegen : ${prompt}`;
  const refText = references.map((file, index) => `${index + 1}. ${file.path}`).join(" ");
  return `you have ${refText} ; use skill $imagegen : ${prompt}`;
}

function displayPrompt(prompt) {
  const marker = "use skill $imagegen :";
  const index = String(prompt || "").indexOf(marker);
  if (index === -1) return prompt;
  return String(prompt).slice(index + marker.length).trim();
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
  return escapeHTML(value);
}

async function loadUsageLimits() {
  if (!usageLimits) return;
  usageLimits.textContent = "限额读取中";
  usageLimits.className = "usage-chip busy";
  try {
    const response = await fetch("/api/usage-limits");
    if (!response.ok) throw new Error(await response.text());
    const result = await response.json();
    if (!result.ok) throw new Error(result.error || "读取失败");
    usageLimits.textContent = formatUsageLimits(result.data);
    usageLimits.title = formatUsageLimitsTitle(result);
    usageLimits.className = "usage-chip ok";
  } catch (error) {
    usageLimits.textContent = "限额不可用";
    usageLimits.title = error.message;
    usageLimits.className = "usage-chip warn";
  }
}

function formatUsageLimits(data) {
  const limits = Array.isArray(data?.limits) ? data.limits : [];
  const parts = limits
    .filter((limit) => Number.isFinite(limit.remaining_percent))
    .map((limit) => {
      const reset = limit.reset_time ? ` ${limit.reset_time}` : "";
      return `${shortLimitName(limit.name)} ${limit.remaining_percent}%${reset}`;
    });
  return parts.length ? parts.join(" · ") : "限额未知";
}

function formatUsageLimitsTitle(result) {
  const data = result.data || {};
  const lines = Array.isArray(data.limits)
    ? data.limits.map((limit) => {
        const reset = limit.reset_time ? `，重置：${limit.reset_time}` : "";
        return `${limit.name}: ${limit.remaining_percent}%${reset}`;
      })
    : [];
  if (Number.isFinite(data.credit_balance)) lines.push(`剩余额度: ${data.credit_balance}`);
  if (Number.isFinite(data.turns)) lines.push(`Turns: ${data.turns}`);
  if (result.updatedAt) lines.push(`更新: ${new Date(result.updatedAt).toLocaleString()}`);
  return lines.join("\n");
}

function shortLimitName(name) {
  return String(name || "限额")
    .replace("使用限额", "")
    .replace("5 小时", "5小时")
    .replace("GPT-", "G")
    .trim();
}

function showUpgradeNotice() {
  if (!upgradeModal || localStorage.getItem(upgradeNoticeKey) === "dismissed") return;
  upgradeModal.hidden = false;
  document.body.classList.add("modal-open");
  upgradeDismiss.focus();
}

function dismissUpgradeNotice() {
  localStorage.setItem(upgradeNoticeKey, "dismissed");
  upgradeModal.hidden = true;
  document.body.classList.remove("modal-open");
}

loadJobs();
loadUsageLimits();
renderReferenceList();
renderPromptMeta();
showUpgradeNotice();
