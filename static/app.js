const form = document.querySelector("#promptForm");
const promptInput = document.querySelector("#prompt");
const imageInput = document.querySelector("#referenceImages");
const referenceList = document.querySelector("#referenceList");
const runButton = document.querySelector("#runButton");
const clearButton = document.querySelector("#clearButton");
const refreshButton = document.querySelector("#refreshButton");
const statusPill = document.querySelector("#statusPill");
const logOutput = document.querySelector("#logOutput");
const gallery = document.querySelector("#gallery");
const imageCount = document.querySelector("#imageCount");
const jobsEl = document.querySelector("#jobs");

let activeJobId = null;
let pollTimer = null;

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const prompt = promptInput.value.trim();
  if (!prompt) {
    setStatus("Need prompt", "warn");
    promptInput.focus();
    return;
  }

  runButton.disabled = true;
  setStatus("Queued", "busy");
  logOutput.textContent = "提交任务...\n";
  clearGallery();

  try {
    const formData = new FormData();
    formData.append("prompt", prompt);
    for (const file of imageInput.files) {
      formData.append("images", file);
    }

    const response = await fetch("/api/jobs", {
      method: "POST",
      body: formData,
    });
    if (!response.ok) throw new Error(await response.text());
    const data = await response.json();
    activeJobId = data.id;
    startPolling(data.id);
    await loadJobs();
  } catch (error) {
    runButton.disabled = false;
    setStatus("Failed", "error");
    logOutput.textContent += `提交失败：${error.message}\n`;
  }
});

clearButton.addEventListener("click", () => {
  promptInput.value = "";
  imageInput.value = "";
  renderReferenceList();
  promptInput.focus();
});

imageInput.addEventListener("change", renderReferenceList);

refreshButton.addEventListener("click", async () => {
  if (activeJobId) await fetchJob(activeJobId);
  await loadJobs();
});

function startPolling(id) {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = setInterval(() => fetchJob(id), 1200);
  fetchJob(id);
}

async function fetchJob(id) {
  const response = await fetch(`/api/jobs/${id}`);
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
  const response = await fetch("/api/jobs");
  if (!response.ok) return;
  const jobs = await response.json();
  jobsEl.innerHTML = "";
  for (const job of jobs) {
    const button = document.createElement("button");
    button.className = `job-item ${job.id === activeJobId ? "active" : ""}`;
    button.type = "button";
    button.innerHTML = `
      <span class="job-status ${job.status}">${job.status}</span>
      <span class="job-prompt">${escapeHTML(job.prompt)}</span>
      <span class="job-time">${new Date(job.createdAt).toLocaleString()}${referenceSuffix(job)}</span>
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
  logOutput.textContent = job.log || "等待日志。";
  logOutput.scrollTop = logOutput.scrollHeight;

  if (job.status === "queued") setStatus("Queued", "busy");
  if (job.status === "running") setStatus("Running", "busy");
  if (job.status === "succeeded") setStatus("Done", "ok");
  if (job.status === "failed") setStatus("Failed", "error");

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

function renderReferenceList() {
  const files = [...imageInput.files];
  if (!files.length) {
    referenceList.textContent = "未选择图片。";
    referenceList.classList.remove("has-files");
    return;
  }

  referenceList.classList.add("has-files");
  referenceList.innerHTML = files
    .map((file) => `<span>${escapeHTML(file.name)}</span>`)
    .join("");
}

function referenceSuffix(job) {
  const count = (job.referenceImages || []).length;
  return count ? ` · ${count} 张参考图` : "";
}

function escapeHTML(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

loadJobs();
renderReferenceList();
