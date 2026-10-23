// 电子礼簿礼单导入器（Go 版，零依赖单文件，任意 Windows 电脑可直接运行）
//
// 将礼簿数据生成系统可导入的 .bin 备份，再通过
// 「创建事项页 → 选择 .bin 备份文件导入」导入为新事项。
// 输入支持：
//   .xlsx  —— 首选。可直接用系统「导出为 Excel」生成的文件（自动跳过
//             空行、末尾「总计」行与「已作废」记录，收款类型/宾客等级可还原）
//   .csv   —— UTF-8 编码（Excel 另存为「CSV UTF-8」），表头含姓名/金额列即可，
//             列顺序任意；收款类型列亦可写作「类型」
//
// 加密格式与 index.html 的 CryptoJS 完全一致：
//   OpenSSL "Salted__" 盐值格式 + EVP_BytesToKey(MD5) 派生 AES-256-CBC 密钥/IV + PKCS7 填充
//
// 用法：
//   生成  make-bin.exe --csv 礼单.xlsx --name 事项名 --password 密码 --out 输出.bin
//   解密  make-bin.exe --decrypt 备份.bin --password 密码 [--out 明文.json]
//   无参数双击运行时进入交互式问答
//
// 注意：payload 与每条记录使用同一密码加密（导入端不重加密，密码不一致会导致
// 导入后全部空白）；该密码即导入后新事项的管理密码，丢失无法找回。
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------- CryptoJS 兼容加密 ----------

// OpenSSL EVP_BytesToKey(MD5)：CryptoJS 密码模式的密钥派生算法
// D_i = MD5(D_{i-1} || password || salt)，拼接至取足 32 字节密钥 + 16 字节 IV
func evpKDF(password, salt []byte) (key, iv []byte) {
	var d, prev []byte
	for len(d) < 48 {
		h := md5.New()
		h.Write(prev)
		h.Write(password)
		h.Write(salt)
		prev = h.Sum(nil)
		d = append(d, prev...)
	}
	return d[:32], d[32:48]
}

// encryptString 复现 CryptoJS.AES.encrypt(plain, password).toString()
// 输出 base64("Salted__" + 8字节随机盐 + AES-256-CBC 密文)
func encryptString(plain, password string) (string, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, iv := evpKDF([]byte(password), salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := append([]byte(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
	full := append(append([]byte("Salted__"), salt...), out...)
	return base64.StdEncoding.EncodeToString(full), nil
}

// decryptString 复现 CryptoJS.AES.decrypt(b64, password) 的明文字节
func decryptString(b64, password string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, err
	}
	if len(raw) < 16 || string(raw[:8]) != "Salted__" {
		return nil, errors.New("不是 CryptoJS 盐值格式密文")
	}
	salt, ct := raw[8:16], raw[16:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("密文长度非法")
	}
	key, iv := evpKDF([]byte(password), salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ct)
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || !allBytes(out[len(out)-pad:], byte(pad)) {
		return nil, errors.New("解密失败：密码错误或数据损坏")
	}
	return out[:len(out)-pad], nil
}

func allBytes(b []byte, v byte) bool {
	for _, x := range b {
		if x != v {
			return false
		}
	}
	return true
}

// ---------- 数据结构（字段顺序与 index.html 一致） ----------

type Gift struct {
	Name       string            `json:"name"`
	Amount     float64           `json:"amount"`
	Type       string            `json:"type"`
	RemarkData map[string]string `json:"remarkData"`
	Timestamp  string            `json:"timestamp"`
	GuestLevel int               `json:"guestLevel"`
}

type GiftEntry struct {
	EncryptedData    string `json:"encryptedData"`
	GuestLevelWeight int    `json:"guestLevelWeight"`
	LevelUpdateTime  int    `json:"levelUpdateTime"`
}

type Event struct {
	Name            string  `json:"name"`
	StartDateTime   string  `json:"startDateTime"`
	EndDateTime     string  `json:"endDateTime"`
	Theme           string  `json:"theme"`
	VoiceName       string  `json:"voiceName"`
	CoverType       string  `json:"coverType"`
	Recorder        string  `json:"recorder"`
	MinSpeechAmount float64 `json:"minSpeechAmount"`
	HidePrivacy     bool    `json:"hidePrivacy"`
}

type Payload struct {
	Version    int         `json:"version"`
	ExportedAt string      `json:"exportedAt"`
	Event      Event       `json:"event"`
	Gifts      []GiftEntry `json:"gifts"`
}

// marshalNoEscape 与 JS JSON.stringify 对齐：不转义 <>&，中文保持原样
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ---------- BIN 文件构建/解析 ----------

var fileHeader = []byte{'E', 'G', 'L', 'B', 0x01, 0x00, 0x00, 0x00}

// buildBin 与 index.html 的 buildBackupBytes 完全一致。
// 注意：应用的 CryptoService.encrypt 内部会 JSON.stringify 一次，而 buildBackupBytes
// 传入的是已 stringify 的字符串，故 payload 实际为"双重 JSON 编码"，这里保持一致。
func buildBin(p *Payload, password string) ([]byte, error) {
	inner, err := marshalNoEscape(p)
	if err != nil {
		return nil, err
	}
	outer, err := marshalNoEscape(string(inner))
	if err != nil {
		return nil, err
	}
	encrypted, err := encryptString(string(outer), password)
	if err != nil {
		return nil, err
	}
	body, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, err
	}
	return append(append([]byte{}, fileHeader...), body...), nil
}

func parseBin(binPath, password string) (*Payload, error) {
	raw, err := os.ReadFile(binPath)
	if err != nil {
		return nil, err
	}
	if len(raw) < 8 || string(raw[:4]) != "EGLB" {
		return nil, errors.New("不是有效的 BIN 备份文件（魔数不符）")
	}
	if raw[4] > 0x01 {
		return nil, fmt.Errorf("文件版本 v%d 过高", raw[4])
	}
	plain, err := decryptString(base64.StdEncoding.EncodeToString(raw[8:]), password)
	if err != nil {
		return nil, errors.New("解密失败：密码错误或文件损坏")
	}
	var once any
	if err := json.Unmarshal(plain, &once); err != nil {
		return nil, err
	}
	// 兼容两种历史格式：应用导出为双重 JSON 编码，先解出外层字符串再解析
	if s, ok := once.(string); ok {
		once = nil
		if err := json.Unmarshal([]byte(s), &once); err != nil {
			return nil, err
		}
	}
	data, err := json.Marshal(once)
	if err != nil {
		return nil, err
	}
	var p Payload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ---------- 礼单读取（xlsx / csv） ----------

var remarkKeys = map[string]string{"备注": "custom", "礼品": "gift", "关系": "relation", "电话": "phone", "住址": "address"}
var validTypes = map[string]bool{"现金": true, "微信": true, "支付宝": true, "其他": true}
var totalMarkers = map[string]bool{"总计": true, "合计": true, "汇总": true, "总计：": true, "合计：": true}

type skipStats struct {
	empty  int
	totals int
	voided int
}

// rowsToGifts 将网格（首行允许是表头）转换为礼金记录。
// 兼容系统导出的「礼金明细」表：跳过空行、「总计」行与「已作废」记录。
func rowsToGifts(grid [][]string, source string) ([]Gift, skipStats, error) {
	var stats skipStats
	// 在前 3 行中找同时含「姓名」「金额」的表头行
	headerIdx := -1
	for i := 0; i < len(grid) && i < 3; i++ {
		has := map[string]bool{}
		for _, c := range grid[i] {
			has[strings.TrimSpace(c)] = true
		}
		if has["姓名"] && (has["金额"] || has["收款类型"]) {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return nil, stats, fmt.Errorf("%s 前 3 行中未找到含「姓名」和「金额」的表头", source)
	}
	col := map[string]int{}
	for i, h := range grid[headerIdx] {
		h = strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF"))
		if _, exists := col[h]; !exists {
			col[h] = i
		}
	}
	get := func(r []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(r) {
			return ""
		}
		return strings.TrimSpace(r[i])
	}
	typeCol := "类型"
	if _, ok := col["收款类型"]; ok {
		typeCol = "收款类型"
	}
	var gifts []Gift
	for i, r := range grid[headerIdx+1:] {
		lineNo := headerIdx + i + 2
		name := get(r, "姓名")
		amountStr := get(r, "金额")
		if name == "" && amountStr == "" {
			stats.empty++
			continue // 整行为空（导出末尾的分隔空行等）
		}
		if totalMarkers[name] {
			// 系统导出的总计行特征：收款类型是"有效记录 X 条…"长文本、无登记时间。
			// 具备真实宾客特征（合法收款类型 + 有登记时间）的"总计"是普通宾客，照常导入
			if !validTypes[get(r, typeCol)] || get(r, "登记时间") == "" {
				stats.totals++
				continue
			}
		}
		if name == "" {
			stats.empty++ // 姓名缺失的杂散行（如仅有金额的统计行）
			continue
		}
		if strings.Contains(get(r, "状态"), "作废") {
			stats.voided++
			continue // 已作废记录不导入
		}
		amount, err := strconv.ParseFloat(amountStr, 64)
		if err != nil || amount < 0 {
			return nil, stats, fmt.Errorf("%s 第 %d 行：金额无效「%s」", source, lineNo, amountStr)
		}
		typ := get(r, typeCol)
		if typ == "" {
			typ = "现金"
		}
		if !validTypes[typ] {
			return nil, stats, fmt.Errorf("%s 第 %d 行：类型须为 现金/微信/支付宝/其他，实际「%s」", source, lineNo, typ)
		}
		remark := map[string]string{}
		for csvKey, remarkKey := range remarkKeys {
			if v := get(r, csvKey); v != "" {
				remark[remarkKey] = v
			}
		}
		gifts = append(gifts, Gift{
			Name:       name,
			Amount:     amount,
			Type:       typ,
			RemarkData: remark,
			Timestamp:  time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			GuestLevel: parseGuestLevel(get(r, "宾客等级")),
		})
	}
	if len(gifts) == 0 {
		return nil, stats, fmt.Errorf("%s 中未找到有效礼金记录", source)
	}
	return gifts, stats, nil
}

// parseGuestLevel 还原宾客等级权重：「普宾」=0，「排序N」=101-N（与 index.html 的
// CONFIG.GUEST_LEVELS 对应），未知值按普宾处理
func parseGuestLevel(s string) int {
	if s == "" || s == "普宾" {
		return 0
	}
	if n, ok := strings.CutPrefix(s, "排序"); ok {
		if v, err := strconv.Atoi(n); err == nil && v >= 1 && v <= 100 {
			return 101 - v
		}
	}
	return 0
}

func giftsFromXlsx(path string) ([]Gift, skipStats, error) {
	grid, err := readXlsxGrid(path)
	if err != nil {
		return nil, skipStats{}, err
	}
	return rowsToGifts(grid, filepath.Base(path))
}

func giftsFromCsv(path string) ([]Gift, skipStats, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, skipStats{}, err
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) // 去 BOM
	if !utf8.Valid(raw) {
		return nil, skipStats{}, errors.New("CSV 不是 UTF-8 编码（Excel 请另存为「CSV UTF-8」，或直接改用 .xlsx 文件）")
	}
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1 // 礼单行常缺尾列，允许各行字段数不同
	rows, err := r.ReadAll()
	if err != nil {
		return nil, skipStats{}, err
	}
	return rowsToGifts(rows, filepath.Base(path))
}

func giftsFromFile(path string) ([]Gift, skipStats, error) {
	if strings.EqualFold(filepath.Ext(path), ".xlsx") {
		return giftsFromXlsx(path)
	}
	return giftsFromCsv(path)
}

// ---------- xlsx 读取（标准库 ZIP+XML，无第三方依赖） ----------

type xlsxSheetRef struct {
	Name string `xml:"name,attr"`
	Rid  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
}

type xlsxWorkbook struct {
	Sheets []xlsxSheetRef `xml:"sheets>sheet"`
}

type xlsxRel struct {
	ID     string `xml:"Id,attr"`
	Target string `xml:"Target,attr"`
}

type xlsxRels struct {
	Rels []xlsxRel `xml:"Relationship"`
}

type xlsxCell struct {
	Ref    string `xml:"r,attr"`
	Type   string `xml:"t,attr"`
	Value  string `xml:"v"`
	Inline string `xml:"is>t"`
}

type xlsxRow struct {
	R     int        `xml:"r,attr"`
	Cells []xlsxCell `xml:"c"`
}

type xlsxSheetData struct {
	Rows []xlsxRow `xml:"sheetData>row"`
}

type xlsxSi struct {
	Ts []string `xml:"t"`
	Rt []struct {
		T string `xml:"t"`
	} `xml:"r"`
}

type xlsxSst struct {
	Si []xlsxSi `xml:"si"`
}

// colIndex 将单元格引用（如 "BC12"）的列字母部分转为 0 起始列号
func colIndex(ref string) int {
	n := 0
	for _, c := range ref {
		if c < 'A' || c > 'Z' {
			break
		}
		n = n*26 + int(c-'A') + 1
	}
	return n - 1
}

func readXlsxGrid(path string) ([][]string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, errors.New("无法读取 xlsx 文件（确认是 .xlsx 格式而非旧版 .xls）")
	}
	defer zr.Close()

	load := func(name string) ([]byte, error) {
		for _, f := range zr.File {
			if f.Name == name {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("xlsx 内缺少 %s", name)
	}

	// 1. workbook.xml → 第一个工作表（优先名为「礼金明细」的）
	var wb xlsxWorkbook
	if b, err := load("xl/workbook.xml"); err != nil {
		return nil, err
	} else if err := xml.Unmarshal(b, &wb); err != nil {
		return nil, err
	}
	if len(wb.Sheets) == 0 {
		return nil, errors.New("xlsx 中没有工作表")
	}
	sheet := wb.Sheets[0]
	for _, s := range wb.Sheets {
		if s.Name == "礼金明细" {
			sheet = s
			break
		}
	}

	// 2. workbook.xml.rels → r:id 对应的 sheet 文件路径
	var rels xlsxRels
	if b, err := load("xl/_rels/workbook.xml.rels"); err != nil {
		return nil, err
	} else if err := xml.Unmarshal(b, &rels); err != nil {
		return nil, err
	}
	target := ""
	for _, rel := range rels.Rels {
		if rel.ID == sheet.Rid {
			target = rel.Target
			break
		}
	}
	if target == "" {
		return nil, errors.New("xlsx 工作表关系缺失")
	}
	// 兼容相对路径（worksheets/sheet1.xml）与绝对路径（/xl/worksheets/sheet1.xml，openpyxl/WPS 常见）
	sheetPath := strings.TrimPrefix(target, "/")
	if !strings.HasPrefix(sheetPath, "xl/") {
		sheetPath = "xl/" + sheetPath
	}

	// 3. sharedStrings.xml → 共享字符串表
	var sst xlsxSst
	if b, err := load("xl/sharedStrings.xml"); err == nil {
		_ = xml.Unmarshal(b, &sst)
	}
	strAt := func(i int) string {
		if i < 0 || i >= len(sst.Si) {
			return ""
		}
		si := sst.Si[i]
		var sb strings.Builder
		for _, t := range si.Ts {
			sb.WriteString(t)
		}
		for _, r := range si.Rt {
			sb.WriteString(r.T)
		}
		return sb.String()
	}

	// 4. sheet XML → 网格（xml.Unmarshal 保持文档顺序，按行下标枚举）
	var sd xlsxSheetData
	if b, err := load(sheetPath); err != nil {
		return nil, err
	} else if err := xml.Unmarshal(b, &sd); err != nil {
		return nil, err
	}
	type cellPos struct{ row, col int }
	values := map[cellPos]string{}
	maxRow, numCols := -1, 0
	for ri, row := range sd.Rows {
		// 行号取 r 属性（Excel 实际行号），缺失时回退切片下标
		if row.R > 0 {
			ri = row.R - 1
		}
		for _, c := range row.Cells {
			ci := colIndex(c.Ref)
			if ci < 0 {
				continue
			}
			var v string
			switch c.Type {
			case "s":
				n, _ := strconv.Atoi(strings.TrimSpace(c.Value))
				v = strAt(n)
			case "inlineStr":
				v = c.Inline
			default:
				v = strings.TrimSpace(c.Value)
			}
			values[cellPos{ri, ci}] = v
			if ri > maxRow {
				maxRow = ri
			}
			if ci+1 > numCols {
				numCols = ci + 1
			}
		}
	}
	if maxRow < 0 {
		return nil, errors.New("xlsx 工作表为空")
	}
	grid := make([][]string, maxRow+1)
	for r := range grid {
		grid[r] = make([]string, numCols)
	}
	for pos, v := range values {
		grid[pos.row][pos.col] = v
	}
	return grid, nil
}

// ---------- BIN 生成 ----------

// formatMoney 合计金额按两位小数取整后输出，避免浮点累加尾差
func formatMoney(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

func jsNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00") }

func runGenerate(listPath, name, password, out, start, end, theme, recorder string) error {
	gifts, stats, err := giftsFromFile(listPath)
	if err != nil {
		return err
	}
	now := jsNow()
	day := time.Now().Format("2006-01-02")
	if start == "" {
		start = day + "T00:00"
	}
	if end == "" {
		end = day + "T23:59"
	}
	if theme == "" {
		theme = "theme-festive"
	}
	p := &Payload{
		Version:    1,
		ExportedAt: now,
		Event: Event{
			Name: name, StartDateTime: start, EndDateTime: end, Theme: theme,
			VoiceName: "", CoverType: "default", Recorder: recorder,
			MinSpeechAmount: 0, HidePrivacy: false,
		},
	}
	total := 0.0
	for _, g := range gifts {
		gj, err := marshalNoEscape(g)
		if err != nil {
			return err
		}
		enc, err := encryptString(string(gj), password)
		if err != nil {
			return err
		}
		p.Gifts = append(p.Gifts, GiftEntry{EncryptedData: enc, GuestLevelWeight: g.GuestLevel, LevelUpdateTime: 0})
		total += g.Amount
	}
	data, err := buildBin(p, password)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("已生成 %s\n", out)
	skip := ""
	if stats.empty+stats.totals+stats.voided > 0 {
		parts := []string{}
		if stats.voided > 0 {
			parts = append(parts, fmt.Sprintf("已作废 %d 条", stats.voided))
		}
		if stats.totals > 0 {
			parts = append(parts, fmt.Sprintf("总计行 %d 条", stats.totals))
		}
		if stats.empty > 0 {
			parts = append(parts, fmt.Sprintf("空行 %d 条", stats.empty))
		}
		skip = fmt.Sprintf("（跳过：%s）", strings.Join(parts, "、"))
	}
	fmt.Printf("事项「%s」，共 %d 条礼金，合计 %s 元。%s\n导入密码即本密码，请牢记。\n",
		name, len(gifts), formatMoney(total), skip)
	return nil
}

func runDecrypt(binPath, password, out string) error {
	p, err := parseBin(binPath, password)
	if err != nil {
		return err
	}
	data, err := marshalNoEscape(p)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return err
	}
	if out != "" {
		if err := os.WriteFile(out, pretty.Bytes(), 0o644); err != nil {
			return err
		}
		fmt.Printf("已解密写入 %s\n", out)
	} else {
		fmt.Println(pretty.String())
	}
	total := 0.0
	for _, g := range p.Gifts {
		var rec Gift
		plain, err := decryptString(g.EncryptedData, password)
		if err != nil {
			return fmt.Errorf("记录解密失败: %w", err)
		}
		if err := json.Unmarshal(plain, &rec); err != nil {
			return fmt.Errorf("记录解析失败: %w", err)
		}
		total += rec.Amount
	}
	fmt.Printf("事项「%s」，共 %d 条礼金，合计 %s 元。\n", p.Event.Name, len(p.Gifts), formatMoney(total))
	return nil
}

func prefix(s []string, n int) []string {
	if len(s) > n { return s[:n] }
	return s
}

func prompt(reader *bufio.Reader, label string) string {
	fmt.Print(label)
	line, _ := reader.ReadString('\n')
	s := strings.TrimSpace(line)
	// Windows 下拖入含空格的文件路径时，cmd 会自动为路径包上一层英文引号，须去掉
	if len(s) >= 2 && s[0] == s[len(s)-1] && (s[0] == '"' || s[0] == '\'') {
		s = s[1 : len(s)-1]
	}
	return strings.TrimSpace(s)
}

func runInteractive() error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("==== 电子礼簿 礼单 → BIN 备份生成器 ====")
	fmt.Println("支持系统「导出为 Excel」的 .xlsx 文件，或 UTF-8 编码的 .csv 礼单。")
	listPath := prompt(reader, "礼单文件路径（.xlsx / .csv，可将文件拖入本窗口）：")
	name := prompt(reader, "事项名称（如：张府婚礼）：")
	password := prompt(reader, "管理密码（导入后即事项密码，务必牢记）：")
	if listPath == "" || name == "" || password == "" {
		return errors.New("礼单路径、事项名称、密码均不能为空")
	}
	out := name + ".bin"
	if err := runGenerate(listPath, name, password, out, "", "", "", ""); err != nil {
		return err
	}
	fmt.Printf("\n完成！请在「创建事项」页点击「选择 .bin 备份文件导入」，选择 %s 并输入密码。\n", filepath.Base(out))
	return nil
}

func main() {
	listPath := flag.String("list", "", "礼单文件（.xlsx 或 UTF-8 CSV，含姓名/金额列）")
	name := flag.String("name", "", "事项名称")
	password := flag.String("password", "", "管理密码（导入后即事项密码）")
	out := flag.String("out", "", "输出文件路径")
	start := flag.String("start", "", "事项开始时间（默认当天 00:00）")
	end := flag.String("end", "", "事项结束时间（默认当天 23:59）")
	theme := flag.String("theme", "", "主题（默认 theme-festive 喜庆红）")
	recorder := flag.String("recorder", "", "记账人（选填）")
	decrypt := flag.String("decrypt", "", "解密模式：指定要解密的 BIN 文件")
	dump := flag.String("dump", "", "诊断模式：打印 xlsx/csv 解析出的网格")
	flag.Parse()

	if *dump != "" {
		var grid [][]string
		var err error
		if strings.EqualFold(filepath.Ext(*dump), ".xlsx") {
			grid, err = readXlsxGrid(*dump)
		} else {
			raw, e := os.ReadFile(*dump)
			if e != nil { err = e } else {
				raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
				r := csv.NewReader(bytes.NewReader(raw))
				r.FieldsPerRecord = -1
				grid, err = r.ReadAll()
			}
		}
		if err != nil { fmt.Fprintln(os.Stderr, "出错:", err); os.Exit(1) }
		for i, row := range grid {
			fmt.Printf("行%d: %s\n", i+1, strings.Join(prefix(row, 13), " | "))
		}
		return
	}

	var err error
	switch {
	case *decrypt != "":
		err = runDecrypt(*decrypt, *password, *out)
	case *listPath != "" || *name != "" || *password != "":
		if *listPath == "" || *name == "" || *password == "" || *out == "" {
			fmt.Fprintln(os.Stderr, "用法: make-bin --list 礼单.xlsx --name 事项名 --password 密码 --out 输出.bin")
			fmt.Fprintln(os.Stderr, "解密: make-bin --decrypt 文件.bin --password 密码 [--out 明文.json]")
			os.Exit(2)
		}
		err = runGenerate(*listPath, *name, *password, *out, *start, *end, *theme, *recorder)
	default:
		err = runInteractive()
		if err == nil {
			fmt.Print("\n按回车键退出…")
			bufio.NewReader(os.Stdin).ReadString('\n')
		}
	}
	if err != nil {
		if len(os.Args) <= 1 {
			fmt.Fprintf(os.Stderr, "出错: %v\n", err)
			fmt.Print("\n按回车键退出…")
			bufio.NewReader(os.Stdin).ReadString('\n')
		} else {
			fmt.Fprintln(os.Stderr, "出错:", err)
		}
		os.Exit(1)
	}
}
