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
	accountDocument, err := fs.ReadFile(root, "account.html")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(accountDocument)
			return
		case "/account.html":
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}
