package httpapi

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// staticIndexName is the file of the frontend build that answers the
// navigation of the application.
const staticIndexName = "index.html"

// staticAPIPrefix marks the paths that belong to the API of the control端.
//
// The whole tree is refused, so an API path that does not exist can never
// fall into the fallback of the frontend.
const staticAPIPrefix = "api/"

// StaticHandler serves the frontend build of the control端 that lies in dir.
//
// The frontend is a single page application: a path that names no file is
// a navigation of the application and is answered with the index of the
// build. A path that names a file is answered with the file itself or with
// a 404, so a missing asset never hides behind the index.
//
// The API is never answered with the index: a request below /api/ that
// does not exist is a 404 in the common error format, because the frontend
// parses the answer of the API as JSON.
//
// A directory that does not exist is reported with an error instead of a
// blank page, because a misconfigured deployment has to be visible.
func StaticHandler(dir string) http.Handler {
	root, err := staticRoot(dir)
	if err != nil {
		if errors.Is(err, errNoWebDir) {
			return staticError{
				status:  http.StatusNotFound,
				code:    "web_not_configured",
				message: "未配置前端静态资源目录",
			}
		}
		return staticError{
			status:  http.StatusInternalServerError,
			code:    "web_unavailable",
			message: "前端静态资源目录不可用",
		}
	}
	return &staticHandler{root: root}
}

// errNoWebDir reports a control端 that was started without a frontend.
var errNoWebDir = errors.New("no frontend directory is configured")

// staticRoot returns the absolute path of the directory of the build.
func staticRoot(dir string) (string, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return "", errNoWebDir
	}
	root, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve the frontend directory %s: %w", dir, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("inspect the frontend directory %s: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("the frontend path %s is not a directory", root)
	}
	return root, nil
}

// staticHandler serves the files of a frontend build.
type staticHandler struct {
	// root is the absolute path of the directory of the build.
	root string
}

// staticError answers every request with the same error. It serves the
// requests of a frontend build that cannot be read.
type staticError struct {
	status  int
	code    string
	message string
}

// ServeHTTP answers the request with the error of the handler.
func (e staticError) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writeError(w, e.status, e.code, e.message)
}

// ServeHTTP serves the file of the build that the request names.
func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}

	name, ok := staticName(r.URL.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_path", "请求路径非法")
		return
	}
	if staticIsAPIPath(name) {
		writeError(w, http.StatusNotFound, "not_found", "未找到请求的资源")
		return
	}

	target := filepath.Join(h.root, filepath.FromSlash(name))
	if !staticWithinRoot(h.root, target) {
		writeError(w, http.StatusBadRequest, "invalid_path", "请求路径非法")
		return
	}

	info, err := os.Stat(target)
	switch {
	case err == nil && info.IsDir():
		// A directory of the build is answered with its own index, if it
		// holds one. The list of a directory is never served.
		index := filepath.Join(target, staticIndexName)
		indexInfo, indexErr := os.Stat(index)
		if indexErr != nil || indexInfo.IsDir() {
			writeError(w, http.StatusNotFound, "not_found", "未找到请求的资源")
			return
		}
		staticServeFile(w, r, index, indexInfo)
		return
	case err == nil:
		staticServeFile(w, r, target, info)
		return
	case !errors.Is(err, fs.ErrNotExist):
		// The file could not be read, e.g. because the permissions of the
		// build are wrong. The error is reported instead of being hidden
		// behind the index.
		writeError(w, http.StatusInternalServerError, "internal_error", "读取静态资源失败")
		return
	}

	// The build holds no such file. A path that names a file is a missing
	// asset, a path that names no file is a navigation of the application.
	if !staticIsNavigation(name) {
		writeError(w, http.StatusNotFound, "not_found", "未找到请求的资源")
		return
	}
	index := filepath.Join(h.root, staticIndexName)
	indexInfo, err := os.Stat(index)
	if err != nil || indexInfo.IsDir() {
		writeError(w, http.StatusNotFound, "not_found", "未找到请求的资源")
		return
	}
	staticServeFile(w, r, index, indexInfo)
}

// staticName turns the path of a request into the name of a file of the
// build.
//
// The name is relative to the root of the build and uses slashes. A path
// that leaves the root is refused: the answer of a request must never be a
// file outside of the build.
func staticName(requestPath string) (string, bool) {
	// The standard library has already decoded the path of the request, so
	// an encoded ".." arrives as a segment of its own.
	for _, segment := range strings.Split(requestPath, "/") {
		if segment == ".." {
			return "", false
		}
	}
	return strings.TrimPrefix(path.Clean("/"+requestPath), "/"), true
}

// staticIsAPIPath reports whether the name belongs to the API.
//
// The API of the control端 lives below /api/v1. The whole tree is refused,
// so a new API path can never fall into the fallback of the frontend.
func staticIsAPIPath(name string) bool {
	return name == "api" || strings.HasPrefix(name, staticAPIPrefix)
}

// staticIsNavigation reports whether the name is a navigation of the
// application instead of a file of the build.
//
// The extension of the last element tells the two apart: the navigation of
// the application has no extension, a file of the build has one.
func staticIsNavigation(name string) bool {
	if name == "" {
		return true
	}
	return !strings.Contains(path.Base(name), ".")
}

// staticWithinRoot reports whether the path lies inside the root.
//
// The check is the second line of defense against a path that leaves the
// directory of the build: the segments of the path are already refused,
// so the path is joined and cleaned again here.
func staticWithinRoot(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

// staticServeFile writes the file to the answer.
func staticServeFile(w http.ResponseWriter, r *http.Request, name string, info os.FileInfo) {
	file, err := os.Open(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "读取静态资源失败")
		return
	}
	defer func() { _ = file.Close() }()

	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
