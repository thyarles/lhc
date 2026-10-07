// Package all imports every check for its registration side effect. This is
// the ONLY list of checks: adding a check means adding one line here.
package all

import (
	_ "github.com/thyarles/lhc-go/internal/checks/cpu"
	_ "github.com/thyarles/lhc-go/internal/checks/system"
)
