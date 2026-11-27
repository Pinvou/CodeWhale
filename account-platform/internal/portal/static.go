package portal

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

//go:embed static/*
var staticFiles embed.FS

var sharedStaticAssetName = regexp.MustCompile(`^[A-Za-z0-9_-]+-[A-Za-z0-9_-]{8,}\.(js|css|svg|png|jpe?g|webp|gif|avif|ico|woff2?|ttf|otf|wasm)$`)

// StaticHandler serves the three current Portal entry points and their assets.
// Feature routes and authentication stay owned by Service.Handler.
func StaticHandler() http.Handler {
	return staticHandler(nil)
}

// OpenSharedStaticHandler keeps HTML and request policy in Portal. A configured
// shared volume must already contain this image's exact assets before it serves.
func OpenSharedStaticHandler(directory string) (http.Handler, *os.Root, error) {
	volume, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, fmt.Errorf("open shared Portal asset root: %w", err)
	}
	defer volume.Close()
	shared, err := volume.OpenRoot("assets")
	if err != nil {
		return nil, nil, fmt.Errorf("open published Portal asset directory: %w", err)
	}
	if err := checkPortalAssetRelease(shared.FS()); err != nil {
		_ = shared.Close()
		return nil, nil, err
	}
	return staticHandler(shared), shared, nil
}

func checkPortalAssetRelease(shared fs.FS) error {
	assets, err := fs.ReadDir(staticFiles, "static/assets")
	if err != nil {
		return err
	}
	for _, asset := range assets {
		name := asset.Name()
		if !sharedStaticAssetName.MatchString(name) {
			return fmt.Errorf("unexpected embedded Portal asset name: %s", name)
		}
		embedded, err := staticFiles.ReadFile("static/assets/" + name)
		if err != nil {
			return err
		}
		info, err := fs.Stat(shared, name)
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(embedded)) {
			return fmt.Errorf("shared Portal asset is missing or differs: %s", name)
		}
		published, err := fs.ReadFile(shared, name)
		if err != nil || !bytes.Equal(embedded, published) {
			return fmt.Errorf("shared Portal asset cannot be read or differs: %s", name)
		}
	}
	return nil
}

func staticHandler(shared *os.Root) http.Handler {
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
			if shared != nil {
				name = strings.TrimPrefix(name, "assets/")
				if !sharedStaticAssetName.MatchString(name) {
					http.NotFound(w, r)
					return
				}
				info, err := shared.Stat(name)
				if err != nil || !info.Mode().IsRegular() {
					http.NotFound(w, r)
					return
				}
				asset, err := shared.Open(name)
				if err != nil {
					http.NotFound(w, r)
					return
				}
				defer asset.Close()
				// Embedded assets had no modification date. Keep that validator
				// contract when reading identical names from a mounted filesystem.
				http.ServeContent(w, r, name, time.Time{}, asset)
				return
			}
			info, err := fs.Stat(root, name)
			if err != nil || !info.Mode().IsRegular() {
				http.NotFound(w, r)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
