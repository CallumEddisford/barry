package core

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Route struct {
	URLPattern *regexp.Regexp
	ParamKeys  []string
	HTMLPath   string
	ServerPath string
	FilePath   string
}

type Router struct {
	config   Config
	env      string
	onReload func()
	routes   []Route
}

type RuntimeContext struct {
	Env         string
	EnableWatch bool
	OnReload    func()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Status() int {
	if r.status == 0 {
		return 200
	}
	return r.status
}

func NewRouter(config Config, ctx RuntimeContext) *Router {
	r := &Router{
		config:   config,
		env:      ctx.Env,
		onReload: ctx.OnReload,
	}
	r.loadRoutes()

	if ctx.EnableWatch {
		go r.watchEverything()
	}

	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	path := strings.Trim(req.URL.Path, "/")

	recorder := &statusRecorder{ResponseWriter: w, status: 200}

	if path == "" {
		r.serveStatic("routes/index.html", "routes/index.server.go", recorder, req, map[string]string{}, "")
	} else {
		found := false
		for _, route := range r.routes {
			if matches := route.URLPattern.FindStringSubmatch(path); matches != nil {
				params := map[string]string{}
				for i, key := range route.ParamKeys {
					params[key] = matches[i+1]
				}
				r.serveStatic(route.HTMLPath, route.ServerPath, recorder, req, params, path)
				found = true
				break
			}
		}
		if !found {
			renderErrorPage(recorder, r.config, r.env, http.StatusNotFound, "Page not found", req.URL.Path)

		}
	}

	if r.env == "dev" && shouldLogRequest(req.URL.Path) {
		duration := time.Since(start).Milliseconds()
		fmt.Printf("%s %d %dms\n", req.URL.Path, recorder.Status(), duration)
	}
}

func (r *Router) serveStatic(htmlPath, serverPath string, w http.ResponseWriter, req *http.Request, params map[string]string, resolvedPath string) {
	if _, err := os.Stat(htmlPath); err != nil {
		renderErrorPage(w, r.config, r.env, http.StatusNotFound, "Page not found", req.URL.Path)
		return
	}

	routeKey := strings.TrimPrefix(resolvedPath, "/")

	if r.config.CacheEnabled {
		cacheDir := filepath.Join(r.config.OutputDir, routeKey)
		htmlPath := filepath.Join(cacheDir, "index.html")
		gzPath := htmlPath + ".gz"

		if r.env == "prod" && acceptsGzip(req) {
			if _, err := os.Stat(gzPath); err == nil {
				data, _ := os.ReadFile(gzPath)
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Type", "text/html")
				if r.config.DebugHeaders {
					w.Header().Set("X-Barry-Cache", "HIT")
				}
				w.Write(data)
				return
			}
		}

		if _, err := os.Stat(htmlPath); err == nil {
			data, _ := os.ReadFile(htmlPath)
			w.Header().Set("Content-Type", "text/html")
			if r.config.DebugHeaders {
				w.Header().Set("X-Barry-Cache", "HIT")
			}
			w.Write(data)
			return
		}
	}

	layoutPath := ""
	if content, err := os.ReadFile(htmlPath); err == nil {
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "<!-- layout:") && strings.HasSuffix(line, "-->") {
				layoutPath = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "<!-- layout:"), "-->"))
				break
			}
		}
	}

	data := map[string]interface{}{}
	if _, err := os.Stat(serverPath); err == nil {
		result, err := ExecuteServerFile(serverPath, params, r.env == "dev")
		if err != nil {
			if IsNotFoundError(err) {
				renderErrorPage(w, r.config, r.env, http.StatusNotFound, "Page not found", req.URL.Path)
				return
			}
			http.Error(w, "Server logic error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		data = result
	}

	var componentFiles []string
	filepath.Walk("components", func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".html") {
			componentFiles = append(componentFiles, path)
		}
		return nil
	})

	var tmplFiles []string
	if layoutPath != "" {
		tmplFiles = append(tmplFiles, layoutPath)
	}
	tmplFiles = append(tmplFiles, htmlPath)
	tmplFiles = append(tmplFiles, componentFiles...)

	tmpl := template.New("").Funcs(BarryTemplateFuncs(r.env, r.config.OutputDir))
	tmpl, err := tmpl.ParseFiles(tmplFiles...)
	if err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	layoutName := "layout"

	var rendered bytes.Buffer
	err = tmpl.ExecuteTemplate(&rendered, layoutName, data)
	if err != nil {
		http.Error(w, "Template execution error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	html := rendered.Bytes()

	if r.env == "dev" {
		liveReloadScript := `
<script>
	if (typeof WebSocket !== "undefined") {
		const ws = new WebSocket("ws://" + location.host + "/__barry_reload");
		ws.onmessage = e => {
			if (e.data === "reload") location.reload();
		};
	}
</script>
</body>`
		html = bytes.Replace(html, []byte("</body>"), []byte(liveReloadScript), 1)
	}

	if r.config.CacheEnabled {
		_ = SaveCachedHTML(r.config, routeKey, html)
	}

	w.Header().Set("Content-Type", "text/html")
	if r.config.DebugHeaders {
		w.Header().Set("X-Barry-Cache", "MISS")
	}
	w.Write(html)
}

func (r *Router) loadRoutes() {
	r.routes = []Route{}

	filepath.Walk("routes", func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}

		htmlPath := filepath.Join(path, "index.html")
		if _, err := os.Stat(htmlPath); err != nil {
			return nil
		}

		rel := strings.TrimPrefix(path, "routes")
		parts := strings.Split(strings.Trim(rel, "/"), "/")
		paramKeys := []string{}
		pattern := ""

		for _, part := range parts {
			if strings.HasPrefix(part, "_") {
				key := part[1:]
				paramKeys = append(paramKeys, key)
				pattern += "/([^/]+)"
			} else {
				pattern += "/" + part
			}
		}

		regex := regexp.MustCompile("^" + strings.TrimPrefix(pattern, "/") + "$")

		r.routes = append(r.routes, Route{
			URLPattern: regex,
			ParamKeys:  paramKeys,
			HTMLPath:   filepath.Join(path, "index.html"),
			ServerPath: filepath.Join(path, "index.server.go"),
			FilePath:   path,
		})

		return nil
	})
}

func renderErrorPage(w http.ResponseWriter, config Config, env string, status int, message, path string) {
	base := "routes/_error"
	statusFile := fmt.Sprintf("%s/%d.html", base, status)
	defaultFile := fmt.Sprintf("%s/index.html", base)

	context := map[string]interface{}{
		"Title":       fmt.Sprintf("%d - %s", status, message),
		"StatusCode":  status,
		"Message":     message,
		"Path":        path,
		"Description": message,
	}

	tryRender := func(file string) bool {
		contentBytes, err := os.ReadFile(file)
		if err != nil {
			return false
		}

		content := string(contentBytes)
		layoutPath := ""
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "<!-- layout:") && strings.HasSuffix(line, "-->") {
				layoutPath = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "<!-- layout:"), "-->"))
				break
			}
		}

		var tmplFiles []string
		if layoutPath != "" {
			tmplFiles = append(tmplFiles, layoutPath)
		}
		tmplFiles = append(tmplFiles, file)

		filepath.Walk("components", func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.HasSuffix(path, ".html") {
				if layoutPath != "" && filepath.Clean(path) == filepath.Clean("components/layouts/layout.html") {
					return nil
				}
				tmplFiles = append(tmplFiles, path)
			}
			return nil
		})

		tmpl := template.New("").Funcs(BarryTemplateFuncs(env, config.OutputDir))
		tmpl, err = tmpl.ParseFiles(tmplFiles...)
		if err != nil {
			fmt.Println("❌ Error parsing error page:", err)
			return false
		}

		w.WriteHeader(status)
		err = tmpl.ExecuteTemplate(w, "layout", context)
		if err != nil {
			fmt.Println("❌ Error executing error layout:", err)
			return false
		}
		return true
	}

	if tryRender(statusFile) || tryRender(defaultFile) {
		return
	}

	w.WriteHeader(status)
	w.Write([]byte(fmt.Sprintf("%d - %s", status, message)))
}

func (r *Router) watchEverything() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return
	}
	defer watcher.Close()

	watchDirs := []string{"routes", "components", "public"}

	addDirs := func() {
		for _, base := range watchDirs {
			filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
				if err == nil && info.IsDir() {
					_ = watcher.Add(path)
				}
				return nil
			})
		}
	}

	addDirs()

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
				r.loadRoutes()
				addDirs()
				if r.env == "dev" {
					println("🔄 Change detected:", event.Name)
					if r.onReload != nil {
						r.onReload()
					}
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			println("❌ Watch error:", err.Error())
		}
	}
}

func shouldLogRequest(path string) bool {
	return !strings.HasPrefix(path, "/.well-known") &&
		!strings.HasPrefix(path, "/favicon.ico") &&
		!strings.HasPrefix(path, "/robots.txt")
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}
