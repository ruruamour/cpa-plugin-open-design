package opendesign

import (
	_ "embed"
	"strings"
)

// The console is embedded as source files so the management UI stays readable
// and the browser logic can be exercised by the Node tests in tests/.
//
//go:embed ui/console.html
var consoleHTML string

//go:embed ui/auth.js
var consoleAuthScript string

//go:embed ui/core.js
var consoleCoreScript string

//go:embed ui/api.js
var consoleAPIScript string

//go:embed ui/console.js
var consoleAppScript string

// consolePage returns the self-contained management console. The same bytes are
// served for both the resource route and the management console.html route.
func consolePage() []byte {
	page := strings.ReplaceAll(consoleHTML, "/*__OPEN_DESIGN_AUTH__*/", consoleAuthScript)
	page = strings.ReplaceAll(page, "/*__OPEN_DESIGN_CORE__*/", consoleCoreScript)
	page = strings.ReplaceAll(page, "/*__OPEN_DESIGN_API__*/", consoleAPIScript)
	page = strings.ReplaceAll(page, "/*__OPEN_DESIGN_CONSOLE__*/", consoleAppScript)
	return []byte(page)
}
