package service

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const webCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

var hashedAsset = regexp.MustCompile(`-[a-zA-Z0-9_-]{8,}\.(js|css)$`)

func checkWebDir(dir string) error {
	if err := rejectLinks(dir); err != nil {
		return err
	}
	index := filepath.Join(dir, "index.html")
	if err := rejectLinks(index); err != nil {
		return err
	}
	f, err := os.Open(index)
	if err != nil {
		return ErrFiles
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return ErrFiles
	}
	return nil
}

func newWebHandler(dir string, api http.Handler) (http.Handler, error) {
	if err := checkWebDir(dir); err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/agent" || strings.HasPrefix(r.URL.Path, "/agent/") {
			api.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", webCSP)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		// Static resources have no request-body contract. Closing avoids the
		// server draining an unread slow body after this handler returns.
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			w.Header().Set("Connection", "close")
			http.Error(w, "Request body is not supported.", http.StatusBadRequest)
			return
		}
		p := r.URL.Path
		if (r.Method != "GET" && r.Method != "HEAD") || r.URL.EscapedPath() != p || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p {
			http.NotFound(w, r)
			return
		}
		if p == "/" {
			p = "/index.html"
		}
		for _, part := range strings.Split(p, "/") {
			if strings.HasPrefix(part, ".") {
				http.NotFound(w, r)
				return
			}
		}
		file := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "/")))
		rel, err := filepath.Rel(dir, file)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			http.NotFound(w, r)
			return
		}
		if rejectLinks(file) != nil {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(file)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(p, "/assets/") && hashedAsset.MatchString(filepath.Base(file)) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	}), nil
}
