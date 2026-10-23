#!/usr/bin/env node
/**
 * 礼单 → gifts-book BIN 备份生成器（Node 版，开发机用；Windows 分发版见 makebin/）
 *
 * 将礼簿数据生成系统可导入的 .bin 备份，再通过「创建事项页 → 选择 .bin 备份文件导入」
 * 导入为新事项。输入支持 .xlsx（首选，可直接用系统「导出为 Excel」的文件，自动跳过
 * 空行/总计行/已作废记录）与 UTF-8 编码 .csv。也可用 --decrypt 解密既有 BIN 核对。
 *
 * 用法：
 *   生成  node tools/make-bin.mjs --list 礼单.xlsx --name 事项名 --password 密码 --out 输出.bin
 *   解密  node tools/make-bin.mjs --decrypt 备份.bin --password 密码 [--out 明文.json]
 *
 * CSV 格式（UTF-8，首行表头，列顺序任意，除姓名/金额外均可省略，类型列亦可写作「收款类型」）：
 *   姓名,金额,类型,备注,礼品,关系,电话,住址
 *
 * 注意：payload 与每条记录使用同一密码加密（导入端不重加密，密码不一致会导致
 * 导入后全部空白）；该密码即导入后新事项的管理密码，丢失无法找回。
 */
import { readFileSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const here = join(dirname(fileURLToPath(import.meta.url)), "..", "static");
const CryptoJS = require(join(here, "crypto-js.min.js"));

const MAGIC = [0x45, 0x47, 0x4c, 0x42]; // "EGLB"
const HEADER_LENGTH = 8;
const FILE_VERSION = 0x01;

function parseArgs(argv) {
  const args = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith("--")) args[a.slice(2)] = argv[++i] ?? "";
    else args._.push(a);
  }
  return args;
}

/** 极简 CSV 解析：支持双引号包裹（字段内可含逗号/换行/转义引号 ""） */
function parseCsv(text) {
  const rows = [];
  let row = [], field = "", inQuotes = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inQuotes) {
      if (c === '"') {
        if (text[i + 1] === '"') { field += '"'; i++; }
        else inQuotes = false;
      } else field += c;
    } else if (c === '"') inQuotes = true;
    else if (c === ",") { row.push(field); field = ""; }
    else if (c === "\n" || c === "\r") {
      if (c === "\r" && text[i + 1] === "\n") i++;
      row.push(field); field = "";
      if (row.some((v) => v !== "")) rows.push(row);
      row = [];
    } else field += c;
  }
  row.push(field);
  if (row.some((v) => v !== "")) rows.push(row);
  return rows;
}

const REMARK_KEYS = { 备注: "custom", 礼品: "gift", 关系: "relation", 电话: "phone", 住址: "address" };
const VALID_TYPES = ["现金", "微信", "支付宝", "其他"];
const TOTAL_MARKERS = new Set(["总计", "合计", "汇总", "总计：", "合计："]);

/** 网格（首行允许是表头）→ 礼金记录，兼容系统导出的「礼金明细」表 */
function rowsToGifts(grid) {
  let headerIdx = -1;
  for (let i = 0; i < grid.length && i < 3; i++) {
    const has = new Set(grid[i].map((c) => String(c).trim()));
    if (has.has("姓名") && (has.has("金额") || has.has("收款类型"))) { headerIdx = i; break; }
  }
  if (headerIdx < 0) throw new Error("前 3 行中未找到含「姓名」和「金额」的表头");
  const col = {};
  grid[headerIdx].forEach((h, i) => {
    h = String(h).trim();
    if (!(h in col)) col[h] = i;
  });
  const get = (r, name) => (col[name] !== undefined && col[name] < r.length ? String(r[col[name]]).trim() : "");
  const typeCol = col["收款类型"] !== undefined ? "收款类型" : "类型";
  const gifts = [];
  const stats = { empty: 0, totals: 0, voided: 0 };
  for (let i = headerIdx + 1; i < grid.length; i++) {
    const r = grid[i];
    const name = get(r, "姓名");
    const amountStr = get(r, "金额");
    if (!name && !amountStr) { stats.empty++; continue; }
    if (TOTAL_MARKERS.has(name)) {
      // 系统导出的总计行特征：收款类型是"有效记录 X 条…"长文本、无登记时间。
      // 具备真实宾客特征（合法收款类型 + 有登记时间）的"总计"是普通宾客，照常导入
      const t = get(r, typeCol) || "现金";
      if (!VALID_TYPES.includes(t) || !get(r, "登记时间")) { stats.totals++; continue; }
    }
    if (!name) { stats.empty++; continue; }
    if (get(r, "状态").includes("作废")) { stats.voided++; continue; }
    const amount = Number(amountStr);
    if (!Number.isFinite(amount) || amount < 0) throw new Error(`第 ${i + 1} 行：金额无效「${amountStr}」`);
    let type = get(r, typeCol) || "现金";
    if (!VALID_TYPES.includes(type)) throw new Error(`第 ${i + 1} 行：类型须为 ${VALID_TYPES.join("/")}，实际「${type}」`);
    const remarkData = {};
    for (const [csvKey, remarkKey] of Object.entries(REMARK_KEYS)) {
      const v = get(r, csvKey);
      if (v) remarkData[remarkKey] = v;
    }
    gifts.push({ name, amount, type, remarkData, timestamp: new Date().toISOString(), guestLevel: parseGuestLevel(get(r, "宾客等级")) });
  }
  if (!gifts.length) throw new Error("未找到有效礼金记录");
  return { gifts, stats };
}

/** 宾客等级 → 权重：「普宾」=0，「排序N」=101-N（与 CONFIG.GUEST_LEVELS 对应） */
function parseGuestLevel(s) {
  if (!s || s === "普宾") return 0;
  const m = /^排序(\d+)$/.exec(s);
  if (m) {
    const v = Number(m[1]);
    if (v >= 1 && v <= 100) return 101 - v;
  }
  return 0;
}

function gridFromCsv(text) {
  return parseCsv(text.replace(/^\uFEFF/, ""));
}

function gridFromXlsx(path) {
  const XLSX = require(join(here, "xlsx.full.min.js"));
  const wb = XLSX.read(readFileSync(path), { type: "buffer", sheetRows: 0 });
  const names = wb.SheetNames;
  const sheetName = names.includes("礼金明细") ? "礼金明细" : names[0];
  return XLSX.utils.sheet_to_json(wb.Sheets[sheetName], { header: 1, defval: "", raw: false });
}

function giftsFromFile(path) {
  if (/\.xlsx$/i.test(path)) {
    return rowsToGifts(gridFromXlsx(path));
  }
  const raw = readFileSync(path);
  if (!isValidUtf8(raw)) throw new Error("CSV 不是 UTF-8 编码（Excel 请另存为「CSV UTF-8」，或直接改用 .xlsx 文件）");
  return rowsToGifts(gridFromCsv(raw.toString("utf8")));
}

function isValidUtf8(buf) {
  const s = buf.toString("utf8");
  return Buffer.compare(Buffer.from(s, "utf8"), buf) === 0 || !/\uFFFD/.test(s);
}

/** 与 index.html 的 buildBackupBytes 完全一致的文件结构 */
function buildBin(payload, password) {
  // 应用的 CryptoService.encrypt 内部会 JSON.stringify 一次，而 buildBackupBytes 传入
  // 的已是字符串，故实际密文明文是"双重 JSON 编码"，这里保持一致
  const encrypted = CryptoJS.AES.encrypt(JSON.stringify(JSON.stringify(payload)), password).toString();
  const head = new Uint8Array(HEADER_LENGTH);
  head.set(MAGIC, 0);
  head[4] = FILE_VERSION;
  const payloadBytes = new Uint8Array(Buffer.from(encrypted, "base64"));
  const out = new Uint8Array(HEADER_LENGTH + payloadBytes.length);
  out.set(head, 0);
  out.set(payloadBytes, HEADER_LENGTH);
  return out;
}

function decryptBin(binPath, password) {
  const bytes = new Uint8Array(readFileSync(binPath));
  if (bytes.length < HEADER_LENGTH || MAGIC.some((m, i) => bytes[i] !== m)) {
    throw new Error("不是有效的 BIN 备份文件（魔数不符）");
  }
  if (bytes[4] > FILE_VERSION) throw new Error(`文件版本 v${bytes[4]} 过高`);
  const b64 = Buffer.from(bytes.slice(HEADER_LENGTH)).toString("base64");
  const plain = CryptoJS.AES.decrypt(b64, password).toString(CryptoJS.enc.Utf8);
  if (!plain) throw new Error("解密失败：密码错误或文件损坏");
  const once = JSON.parse(plain);
  return typeof once === "string" ? JSON.parse(once) : once;
}

const args = parseArgs(process.argv.slice(2));

try {
  main(args);
} catch (e) {
  console.error("出错:", e.message);
  process.exit(1);
}

function main(args) {

if (args.decrypt) {
  const payload = decryptBin(args.decrypt, args.password ?? "");
  const out = JSON.stringify(payload, null, 2);
  if (args.out) { writeFileSync(args.out, out); console.log(`已解密写入 ${args.out}`); }
  else console.log(out);
} else {
  const { list, name, password, out } = args;
  if (!list || !name || !password || !out) {
    console.error("用法: node tools/make-bin.mjs --list 礼单.xlsx --name 事项名 --password 密码 --out 输出.bin");
    console.error("解密: node tools/make-bin.mjs --decrypt 文件.bin --password 密码 [--out 明文.json]");
    process.exit(1);
  }
  const { gifts, stats } = giftsFromFile(list);
  const now = new Date();
  const day = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`;
  const payload = {
    version: 1,
    exportedAt: now.toISOString(),
    event: {
      name,
      startDateTime: args.start || `${day}T00:00`,
      endDateTime: args.end || `${day}T23:59`,
      theme: args.theme || "theme-festive",
      voiceName: "",
      coverType: "default",
      recorder: args.recorder || "",
      minSpeechAmount: 0,
      hidePrivacy: false,
    },
    gifts: gifts.map((g) => ({
      encryptedData: CryptoJS.AES.encrypt(JSON.stringify(g), password).toString(),
      guestLevelWeight: g.guestLevel,
      levelUpdateTime: 0,
    })),
  };
  writeFileSync(out, buildBin(payload, password));
  const total = Math.round(gifts.reduce((s, g) => s + g.amount, 0) * 100) / 100;
  const skipped = [];
  if (stats.voided) skipped.push(`已作废 ${stats.voided} 条`);
  if (stats.totals) skipped.push(`总计行 ${stats.totals} 条`);
  if (stats.empty) skipped.push(`空行 ${stats.empty} 条`);
  console.log(`已生成 ${out}`);
  console.log(`事项「${name}」，共 ${gifts.length} 条礼金，合计 ${total} 元。${skipped.length ? `（跳过：${skipped.join("、")}）` : ""}`);
  console.log("导入密码即本密码，请牢记。");
}
}
