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
//   -port  int    服务端口，默认 8765（被占用时自动换空闲端口）
//   -dir   string 静态根目录，默认为本程序所在目录
//   -addr  string 监听地址，默认 127.0.0.1（仅本机；如需副屏/局域网访问可改为 0.0.0.0）
//   -open  bool   启动后自动打开浏览器，默认 true
package main

import (
	"flag"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
