// gifts-book-server：电子礼簿零依赖静态服务器
//
// 目的：双击即可通过 http://127.0.0.1 访问礼簿应用，解决 file:// 协议下
// fetch() 被拦截导致 PDF-LIB 打印引擎无法加载字体的问题。
// 纯 Go 标准库实现，无任何第三方依赖，无运行时要求。
//
// 用法：将本程序放到 index.html 同级目录后双击（或命令行运行），
// 自动启动服务并打开系统默认浏览器。
//
// 可选参数：
//
//	-port  int    服务端口，默认 8765（被占用时自动换空闲端口）
//	-dir   string 静态根目录，默认为本程序所在目录
//	-addr  string 监听地址，默认 127.0.0.1（仅本机；如需副屏/局域网访问可改为 0.0.0.0）
//	-open  bool   启动后自动打开浏览器，默认 true
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maxStoreSize = 64 << 20 // 单个备份文件上限 64MB

func main() {
	exeDir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		exeDir, _ = os.Getwd()
	}

	port := flag.Int("port", 8765, "服务端口（0 表示自动选择空闲端口）")
	dir := flag.String("dir", exeDir, "静态文件根目录，默认为本程序所在目录")
	addr := flag.String("addr", "127.0.0.1", "监听地址，默认仅本机")
	open := flag.Bool("open", true, "启动后自动打开浏览器")
	flag.Parse()

	// 补全应用所需但不一定在系统 mime 表中的类型，确保字体/图标能正确加载
	registerMIME()

	root, err := filepath.Abs(*dir)
	if err != nil || !dirExists(root) {
		fmt.Fprintf(os.Stderr, "错误：静态目录不存在：%s\n", *dir)
		waitExit(1)
	}

	listener, listenPort, err := listen(*addr, *port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：端口监听失败：%v\n", err)
		waitExit(1)
	}

	baseURL := fmt.Sprintf("http://%s:%d", *addr, listenPort)
	indexURL := baseURL + "/index.html"

	fmt.Println("==============================================")
	fmt.Println("  电子礼簿 本地服务已启动")
	fmt.Println("  访问地址：", indexURL)
	fmt.Println("  服务目录：", root)
	fmt.Println("  停止服务：关闭本窗口或按 Ctrl+C")
	fmt.Println("==============================================")

	mux := http.NewServeMux()
	fileServer := http.FileServer(http.Dir(root))

	// 本地数据文件存储 API：礼簿应用把加密快照自动备份到 <root>/data/ 目录。
	// 无论目录是否可用都注册路由（不可用时返回 503），
	// 否则请求会落入静态处理器返回 index.html，应用端会误判"自动备份可用"
	api := &storeAPI{dir: filepath.Join(root, "data")}
	if err := os.MkdirAll(api.dir, 0o755); err != nil {
		api.disabled = true
		fmt.Fprintf(os.Stderr, "警告：无法创建数据目录 %s：%v（自动备份不可用，应用将拒绝启动）\n", api.dir, err)
	}
	mux.HandleFunc("GET /api/store", api.guard(api.list))
	mux.HandleFunc("GET /api/store/{name}", api.guard(api.get))
	mux.HandleFunc("PUT /api/store/{name}", api.guard(api.put))
	mux.HandleFunc("DELETE /api/store/{name}", api.guard(api.delete))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// API 路径不匹配任何已注册方法时显式 404，禁止回落到静态文件
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// "/" 与 "/index.html" 直接返回文件内容。不用 http.ServeFile——它对
		// 以 /index.html 结尾的 URL 也会做规范化 301 跳转（与静态跳转形成死循环），
		// ServeContent 则原样返回文件
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			indexPath := filepath.Join(root, "index.html")
			f, err := os.Open(indexPath)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			fi, err := f.Stat()
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeContent(w, r, "index.html", fi.ModTime(), f)
			return
		}
		// 静态资源禁止缓存，避免升级应用后浏览器使用旧文件
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if *open {
		go openBrowser(indexURL)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Println("\n正在停止服务…")
		// 仅 Close server（会连带关闭 listener），Serve 将返回 ErrServerClosed 走正常退出路径
		server.Close()
	}()

	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "服务异常退出：%v\n", err)
		waitExit(1)
	}
}

// listen 在指定地址与端口上监听；端口被占用且用户未强制指定时自动换空闲端口
func listen(addr string, port int) (net.Listener, int, error) {
	if port > 0 {
		l, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
		if err != nil {
			// 仅当使用默认端口时才自动回退，显式指定的端口报错更有助排查
			if port != 8765 {
				return nil, 0, err
			}
			l2, p2, err2 := listen(addr, 0)
			if err2 != nil {
				return nil, 0, err
			}
			fmt.Printf("提示：端口 %d 已被占用，已自动改用端口 %d\n", port, p2)
			return l2, p2, nil
		}
		return l, port, nil
	}
	l, err := net.Listen("tcp", net.JoinHostPort(addr, "0"))
	if err != nil {
		return nil, 0, err
	}
	return l, l.Addr().(*net.TCPAddr).Port, nil
}

func registerMIME() {
	mime.AddExtensionType(".html", "text/html; charset=utf-8")
	mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	mime.AddExtensionType(".mjs", "text/javascript; charset=utf-8")
	mime.AddExtensionType(".css", "text/css; charset=utf-8")
	mime.AddExtensionType(".json", "application/json; charset=utf-8")
	mime.AddExtensionType(".ttf", "font/ttf")
	mime.AddExtensionType(".woff", "font/woff")
	mime.AddExtensionType(".woff2", "font/woff2")
	mime.AddExtensionType(".webp", "image/webp")
	mime.AddExtensionType(".jpg", "image/jpeg")
	mime.AddExtensionType(".png", "image/png")
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("提示：未能自动打开浏览器，请手动访问上方地址。")
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// waitExit 在出错退出前稍作停留，避免双击运行时窗口一闪而过看不到错误信息
func waitExit(code int) {
	fmt.Println("按回车键关闭窗口…")
	fmt.Scanln()
	os.Exit(code)
}

// ==================== 本地数据文件存储 API ====================

type storeAPI struct {
	dir      string
	disabled bool
}

func (api *storeAPI) rejectIfDisabled(w http.ResponseWriter) bool {
	if api.disabled {
		jsonError(w, "data dir unavailable", http.StatusServiceUnavailable)
		return true
	}
	return false
}

// guard 为 API 请求做本地安全校验：
//  1. Host 校验：只接受 127.0.0.1 / localhost / ::1，防御 DNS rebinding
//     （攻击者域名解析到 127.0.0.1 时 Host 头是其自身域名，会被拒绝）
//  2. Origin 校验：浏览器请求必带 Origin 时，其主机必须是本机
//  3. 跨源写防护主要依赖 CORS 预检机制（本服务不返回任何 CORS 头，
//     恶意网页的 PUT/DELETE 会被浏览器拦截；GET 响应同样不可被跨源读取）
func (api *storeAPI) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocalHost(hostOnly(r.Host)) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			// Origin 形如 http://127.0.0.1:8765，用 URL 解析取主机名
			u, err := url.Parse(o)
			if err != nil || !isLocalHost(u.Hostname()) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

func hostOnly(s string) string {
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

func isLocalHost(h string) bool {
	h = strings.Trim(h, "[]")
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// validName 限制文件名字符集（字母/数字/汉字/._-），杜绝路径穿越
func validName(name string) bool {
	if name == "" || len(name) > 120 || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		case r >= 0x4e00 && r <= 0x9fff: // 汉字
		default:
			return false
		}
	}
	return true
}

func (api *storeAPI) list(w http.ResponseWriter, r *http.Request) {
	if api.rejectIfDisabled(w) {
		return
	}
	entries, err := os.ReadDir(api.dir)
	if err != nil {
		jsonError(w, "read dir failed", http.StatusInternalServerError)
		return
	}
	files := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, map[string]any{
			"name":     e.Name(),
			"size":     info.Size(),
			"modified": info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, map[string]any{"files": files})
}

func (api *storeAPI) get(w http.ResponseWriter, r *http.Request) {
	if api.rejectIfDisabled(w) {
		return
	}
	name := r.PathValue("name")
	if !validName(name) {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(api.dir, name))
}

func (api *storeAPI) put(w http.ResponseWriter, r *http.Request) {
	if api.rejectIfDisabled(w) {
		return
	}
	name := r.PathValue("name")
	if !validName(name) {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxStoreSize)
	tmp, err := os.CreateTemp(api.dir, ".tmp-*")
	if err != nil {
		jsonError(w, "write failed", http.StatusInternalServerError)
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.ReadFrom(r.Body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		if strings.Contains(err.Error(), "request body too large") {
			jsonError(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "write failed", http.StatusInternalServerError)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		jsonError(w, "write failed", http.StatusInternalServerError)
		return
	}
	// 原子替换：断电/中断不会留下半个文件
	if err := os.Rename(tmpPath, filepath.Join(api.dir, name)); err != nil {
		os.Remove(tmpPath)
		jsonError(w, "write failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (api *storeAPI) delete(w http.ResponseWriter, r *http.Request) {
	if api.rejectIfDisabled(w) {
		return
	}
	name := r.PathValue("name")
	if !validName(name) {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	err := os.Remove(filepath.Join(api.dir, name))
	if err != nil && !os.IsNotExist(err) {
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": msg})
}
