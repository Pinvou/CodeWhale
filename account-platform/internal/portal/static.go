package portal

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static/*
var staticFiles embed.FS

// StaticHandler serves the three current Portal entry points and their assets.
// Feature routes and authentication stay owned by Service.Handler.
func StaticHandler() http.Handler {
	root, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/" {
			copy := r.Clone(r.Context())
			u := *r.URL
			u.Path = "/account.html"
			copy.URL = &u
			files.ServeHTTP(w, copy)
			return
		}
		if r.URL.Path != "/admin.html" && r.URL.Path != "/portal-session-handoff.html" && !strings.HasPrefix(r.URL.Path, "/assets/") {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			name := strings.TrimPrefix(r.URL.Path, "/")
			info, err := fs.Stat(root, name)
			if err != nil || !info.Mode().IsRegular() {
				http.NotFound(w, r)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
