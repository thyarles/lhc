// Package all imports every check for its registration side effect. This is
// the ONLY list of checks: adding a check means adding one line here.
package all

import (
	_ "github.com/thyarles/lhc/internal/checks/auth"
	_ "github.com/thyarles/lhc/internal/checks/cpu"
	_ "github.com/thyarles/lhc/internal/checks/crontabs"
	_ "github.com/thyarles/lhc/internal/checks/disk"
	_ "github.com/thyarles/lhc/internal/checks/docker"
	_ "github.com/thyarles/lhc/internal/checks/etc"
	_ "github.com/thyarles/lhc/internal/checks/fail2ban"
	_ "github.com/thyarles/lhc/internal/checks/kubernetes"
	_ "github.com/thyarles/lhc/internal/checks/logs"
	_ "github.com/thyarles/lhc/internal/checks/memory"
	_ "github.com/thyarles/lhc/internal/checks/network"
	_ "github.com/thyarles/lhc/internal/checks/packages"
	_ "github.com/thyarles/lhc/internal/checks/ports"
	_ "github.com/thyarles/lhc/internal/checks/processes"
	_ "github.com/thyarles/lhc/internal/checks/rootkit"
	_ "github.com/thyarles/lhc/internal/checks/services"
	_ "github.com/thyarles/lhc/internal/checks/suid"
	_ "github.com/thyarles/lhc/internal/checks/system"
	_ "github.com/thyarles/lhc/internal/checks/tools"
	_ "github.com/thyarles/lhc/internal/checks/updates"
	_ "github.com/thyarles/lhc/internal/checks/users"
)
