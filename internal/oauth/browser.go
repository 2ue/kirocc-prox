package oauth

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenBrowser asks the operating system to open url in the user's default
// browser. Errors are returned so callers running in a server context can
// log a "please open this URL" fallback instead.
//
// On darwin uses "open"; on windows "rundll32" + url.dll; on linux/freebsd
// uses "xdg-open"; on other platforms returns ErrUnsupportedOS.
func OpenBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "linux", "freebsd", "openbsd", "netbsd":
		return exec.Command("xdg-open", url).Start()
	default:
		return fmt.Errorf("oauth: unsupported platform: %s", runtime.GOOS)
	}
}
