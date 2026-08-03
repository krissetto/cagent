package toolinstall

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func executableName(name string) string {
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(name), ".exe") {
		return name + ".exe"
	}
	return name
}

func isExecutable(info os.FileInfo) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode()&0o111 != 0
}
