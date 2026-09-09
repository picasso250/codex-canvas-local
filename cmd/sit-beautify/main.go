// sit-beautify 批量美化"坐下的照片"：按文件名顺序，逐张（严格串行）调用
// imagegen daemon（127.0.0.1:53166 的 POST /ask），单线程不并发，
// 与 Go 主服务 runPicJob 中 picSem(chan struct{},1) 的语义一致。
//
// 用法（在项目根目录下）：
//
//	go run ./cmd/sit-beautify -dry-run            # 只列出计划，不执行
//	go run ./cmd/sit-beautify -limit 1            # 只处理第 1 张（试跑）
//	go run ./cmd/sit-beautify                     # 全部串行处理（已完成的自动跳过）
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	daemonURL = "http://127.0.0.1:53166"

	srcDir = `C:\Users\MECHREV\Documents\xwechat_files\wxid_hx39hicfgg7i22_0bcb\msg\file\2026-09\坐下的照片`

	// 完全相同尺寸组（先处理这批）：6336 × 8870，标准 7 寸 5:7
	targetW = 6336
	targetH = 8870

	promptPrefix = "生成图片 "

	// 最终确认版提示词（人名照片部分逐张复用同一模板）
	promptTemplate = `以人物形象照的标准美化一下第二张照片，让质感更好一些，皮肤质感更好一些，脸部妆容更加精致一些，头发的光影质感也更好一些。人物脸型不要调整太多，脸部形状保持原来的不做大的调整，但是可以微调。背景调整用渐变光影的质感参考第一张图。照片是12.6cm*17.2cm的7寸照片，帮我扩图，增加人物左右留白，左右两只手臂最外侧到照片边缘距离都是2cm，使得手臂边缘和照片最两边的左右留白距离一样，保持对称。同时下面是桌面，帮我补充桌面更多且桌面全部为白色，桌子上手最下面到照片下部边缘至少2cm。使得整体人物在画面中比例缩小一些。输出的成品比例保持原比例不变。最终输出规格统一按标准7寸照片 12.7cm×17.8cm（宽高比5:7）执行，左右留白与桌面留白均按此规格计算，画面比例与当前照片保持一致。`
)

var (
	refGlob = "背景渐变质感参考*.png"
	outRoot = filepath.Join(".", "tmp", "sit-beautify-out") // 相对项目根（运行目录）
)

type askRequest struct {
	RequestID     string   `json:"request_id"`
	Prompt        string   `json:"prompt"`
	StableSeconds float64  `json:"stable_seconds"`
	Images        []string `json:"images"`
	Workdir       string   `json:"workdir"`
}

type askResponse struct {
	OK       bool     `json:"ok"`
	Response string   `json:"response"`
	Images   []string `json:"images"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
}

type daemonStatus struct {
	OK               bool   `json:"ok"`
	Busy             bool   `json:"busy"`
	QueueLength      int    `json:"queue_length"`
	RunningRequestID string `json:"running_request_id"`
}

type jobRecord struct {
	Index      int       `json:"index"`
	Name       string    `json:"name"`
	Photo      string    `json:"photo"`
	RequestID  string    `json:"request_id"`
	OK         bool      `json:"ok"`
	Code       string    `json:"code"`
	Message    string    `json:"message"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished"`
	Images     []string  `json:"images"`
	ElapsedSec float64   `json:"elapsed_seconds"`
}

func main() {
	// ---- flags（支持 -flag value 与 -flag=value） ----
	dryRun := false
	limit := -1
	jpgOnly := false
	noSkip := false
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			i++
			if i < len(args) {
				return args[i]
			}
			return ""
		}
		switch {
		case a == "-dry-run":
			dryRun = true
		case a == "-jpg-only":
			jpgOnly = true
		case a == "-no-skip":
			noSkip = true
		case a == "-limit":
			fmt.Sscanf(next(), "%d", &limit)
		case strings.HasPrefix(a, "-limit="):
			fmt.Sscanf(strings.TrimPrefix(a, "-limit="), "%d", &limit)
		}
	}

	if err := os.MkdirAll(outRoot, 0755); err != nil {
		fatal("create out dir: %v", err)
	}

	ref := findReference(srcDir)
	if ref == "" {
		fatal("reference image not found in %s", srcDir)
	}

	photos, err := collectTargets(srcDir, jpgOnly)
	if err != nil {
		fatal("collect targets: %v", err)
	}
	if len(photos) == 0 {
		fatal("no photo with size %dx%d found", targetW, targetH)
	}

	fmt.Printf("参考图（第一张）: %s\n", filepath.Base(ref))
	fmt.Printf("待处理 %d 张（%dx%d，文件名序）:\n", len(photos), targetW, targetH)
	for i, p := range photos {
		mark := ""
		if !noSkip && outputExists(p) {
			mark = "  [已处理，跳过]"
		}
		fmt.Printf("  %2d. %s%s\n", i+1, filepath.Base(p), mark)
	}

	if dryRun {
		fmt.Println("\n[dry-run] 计划如上，未执行。")
		return
	}
	if limit >= 0 && limit < len(photos) {
		fmt.Printf("\n[limit] 本次只处理前 %d 张\n", limit)
		photos = photos[:limit]
	}

	// ---- daemon 就绪检查（须空闲，否则不并发硬等） ----
	st, err := fetchStatus()
	if err != nil {
		fatal("daemon status: %v", err)
	}
	if !st.OK {
		fatal("daemon not ok: %+v", st)
	}
	if st.Busy || st.QueueLength > 0 {
		fatal("daemon is busy (request=%s queue=%d); 请等待空闲后再跑，避免并发",
			st.RunningRequestID, st.QueueLength)
	}
	fmt.Println("daemon 空闲，开始逐张串行提交...")

	// ---- 逐张串行 ----
	okCount, failCount := 0, 0
	for i, p := range photos {
		if !noSkip && outputExists(p) {
			fmt.Printf("\n[%2d/%d] %s 已有输出，跳过\n", i+1, len(photos), filepath.Base(p))
			continue
		}
		rec, err := submitOne(i+1, len(photos), p, ref)
		if err != nil {
			fmt.Printf("[%2d/%d] %s 提交失败: %v\n", i+1, len(photos), filepath.Base(p), err)
			failCount++
			continue
		}
		appendRecord(rec)
		if rec.OK {
			okCount++
			fmt.Printf("[%2d/%d] %s 完成，生成 %d 张\n", i+1, len(photos), filepath.Base(p), len(rec.Images))
		} else {
			failCount++
			fmt.Printf("[%2d/%d] %s 失败 [%s]: %s\n", i+1, len(photos), filepath.Base(p), rec.Code, rec.Message)
		}
	}

	fmt.Printf("\n全部结束：成功 %d，失败 %d。输出目录: %s\n", okCount, failCount, outRoot)
	if failCount > 0 {
		fmt.Println("失败的条目可用 -no-skip 之外的默认跳过逻辑配合重跑：再次运行本脚本只会补做未完成/失败的。")
		os.Exit(1)
	}
}

// findReference 在源目录中找"参考"图。
func findReference(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ok, _ := filepath.Match(refGlob, e.Name()); ok {
			return filepath.Join(dir, e.Name())
		}
	}
	// 兜底：名字含"参考"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.Contains(e.Name(), "参考") {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// collectTargets 收集尺寸恰好为 targetW×targetH 的图片，按文件名（UTF-8）升序。
func collectTargets(dir string, jpgOnly bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
			continue
		}
		if strings.Contains(e.Name(), "参考") {
			continue
		}
		if jpgOnly && ext != ".jpg" && ext != ".jpeg" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var photos []string
	for _, n := range names {
		full := filepath.Join(dir, n)
		w, h, err := imageSize(full)
		if err != nil {
			fmt.Printf("  忽略 %s（解析失败: %v）\n", n, err)
			continue
		}
		if w == targetW && h == targetH {
			photos = append(photos, full)
		}
	}
	return photos, nil
}

func sanitizeName(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	var b strings.Builder
	for _, r := range base {
		if r == unicode.ReplacementChar || r == '\uf05c' || r == '/' || r == '\\' || r == ':' {
			b.WriteRune('_')
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '（' || r == '）' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func outputExists(photo string) bool {
	outDir := filepath.Join(outRoot, sanitizeName(filepath.Base(photo)))
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "generated_") {
			return true
		}
	}
	return false
}

func submitOne(index, total int, photo, ref string) (jobRecord, error) {
	rec := jobRecord{
		Index:   index,
		Name:    sanitizeName(filepath.Base(photo)),
		Photo:   photo,
		Started: time.Now(),
	}
	rec.RequestID = newID()

	workdir := filepath.Join(outRoot, rec.Name)
	if err := os.MkdirAll(workdir, 0755); err != nil {
		return rec, err
	}

	req := askRequest{
		RequestID:     rec.RequestID,
		Prompt:        promptPrefix + promptTemplate,
		StableSeconds: 5.0,
		Images:        []string{ref, photo}, // 第一张=背景参考图，第二张=目标照片
		Workdir:       workdir,
	}

	fmt.Printf("\n[%d/%d] %s 提交中 (request_id=%s)...\n", index, total, filepath.Base(photo), rec.RequestID)
	body, err := json.Marshal(req)
	if err != nil {
		return rec, err
	}
	resp, err := postJSON("/ask", body)
	if err != nil {
		rec.Finished = time.Now()
		rec.ElapsedSec = rec.Finished.Sub(rec.Started).Seconds()
		rec.Message = err.Error()
		return rec, err
	}

	rec.OK = resp.OK
	rec.Code = resp.Code
	rec.Message = resp.Message
	rec.Images = resp.Images
	rec.Finished = time.Now()
	rec.ElapsedSec = rec.Finished.Sub(rec.Started).Seconds()

	if resp.Response != "" {
		fmt.Printf("  [agent] %s\n", strings.ReplaceAll(resp.Response, "\n", " "))
	}
	if !resp.OK {
		fmt.Printf("  [daemon] code=%s message=%s\n", resp.Code, resp.Message)
	}
	return rec, nil
}

func appendRecord(rec jobRecord) {
	logPath := filepath.Join(outRoot, "jobs.jsonl")
	line, _ := json.Marshal(rec)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

func fetchStatus() (*daemonStatus, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(daemonURL + "/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st daemonStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// postJSON 与主服务 daemonPost 一致：无超时，可能等几分钟。
func postJSON(path string, body []byte) (*askResponse, error) {
	client := &http.Client{Timeout: 0}
	httpResp, err := client.Post(daemonURL+path, "application/json; charset=utf-8", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	var result askResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("bad daemon response %s: %v", httpResp.Status, err)
	}
	return &result, nil
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// imageSize 解析 PNG/JPEG 宽高（不依赖第三方库）。
func imageSize(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	head := make([]byte, 24)
	if _, err := io.ReadFull(f, head); err != nil {
		return 0, 0, err
	}

	if head[0] == 0x89 && head[1] == 'P' && head[2] == 'N' && head[3] == 'G' {
		// PNG: IHDR 在 16..23，宽 16..19，高 20..23（大端）
		w := int(head[16])<<24 | int(head[17])<<16 | int(head[18])<<8 | int(head[19])
		h := int(head[20])<<24 | int(head[21])<<16 | int(head[22])<<8 | int(head[23])
		return w, h, nil
	}

	if head[0] == 0xFF && head[1] == 0xD8 {
		// JPEG: 扫描段找 SOF
		info, err := f.Stat()
		if err != nil {
			return 0, 0, err
		}
		buf := make([]byte, info.Size())
		if _, err := f.ReadAt(buf, 0); err != nil {
			return 0, 0, err
		}
		i := 2
		for i+8 < len(buf) {
			if buf[i] != 0xFF {
				return 0, 0, errors.New("jpeg marker sync lost")
			}
			m := buf[i+1]
			if m == 0xD8 || m == 0x01 { // SOI / TEM（无长度）
				i += 2
				continue
			}
			if m >= 0xD0 && m <= 0xD7 { // RSTn（无长度）
				i += 2
				continue
			}
			if m == 0xFF { // 填充
				i++
				continue
			}
			segLen := int(buf[i+2])<<8 | int(buf[i+3])
			if (m >= 0xC0 && m <= 0xC3) || (m >= 0xC5 && m <= 0xC7) ||
				(m >= 0xC9 && m <= 0xCB) || (m >= 0xCD && m <= 0xCF) {
				h := int(buf[i+5])<<8 | int(buf[i+6])
				w := int(buf[i+7])<<8 | int(buf[i+8])
				return w, h, nil
			}
			i += 2 + segLen
		}
		return 0, 0, errors.New("jpeg SOF not found")
	}

	return 0, 0, fmt.Errorf("unsupported image format")
}

func fatal(format string, args ...any) {
	fmt.Printf("错误: "+format+"\n", args...)
	os.Exit(1)
}
