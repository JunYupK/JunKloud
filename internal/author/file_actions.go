//go:build linux

package author

import (
	"os"
	"path"
	"strings"

	"github.com/JunYupK/JunKloud/internal/problem"
)

func scopedPath(ws, rel string, rootOK bool) (string, error) {
	if rootOK && rel == "." {
		return ws, nil
	}
	if rel == "" || strings.Contains(rel, `\`) || path.Clean(rel) != rel {
		return "", invalid("workspace path must be clean and relative")
	}
	p, err := problem.ResolveAsset(ws, rel)
	if err != nil {
		return "", invalid("unsafe workspace path")
	}
	if info, err := os.Lstat(p); err == nil && !info.Mode().IsRegular() && !info.IsDir() {
		return "", invalid("workspace path must be regular")
	}
	return p, nil
}
