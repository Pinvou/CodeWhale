package portal

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/*
var staticFiles embed.FS

func StaticHandler() http.Handler {
	root, _ := fs.Sub(staticFiles, "static")
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/account" || r.URL.Path == "/account/" {
			r.URL.Path = "/account.html"
		}
		files.ServeHTTP(w, r)
	})
}
