// Package all imports every check for its registration side effect. This is
// the ONLY list of checks: adding a check means adding one line here.
package all

import (
	_ "github.com/thyarles/lhc-go/internal/checks/auth"
	_ "github.com/thyarles/lhc-go/internal/checks/cpu"
	_ "github.com/thyarles/lhc-go/internal/checks/crontabs"
	_ "github.com/thyarles/lhc-go/internal/checks/disk"
	_ "github.com/thyarles/lhc-go/internal/checks/docker"
	_ "github.com/thyarles/lhc-go/internal/checks/etc"
	_ "github.com/thyarles/lhc-go/internal/checks/fail2ban"
	_ "github.com/thyarles/lhc-go/internal/checks/kubernetes"
	_ "github.com/thyarles/lhc-go/internal/checks/logs"
	_ "github.com/thyarles/lhc-go/internal/checks/memory"
	_ "github.com/thyarles/lhc-go/internal/checks/network"
	_ "github.com/thyarles/lhc-go/internal/checks/packages"
	_ "github.com/thyarles/lhc-go/internal/checks/ports"
	_ "github.com/thyarles/lhc-go/internal/checks/processes"
	_ "github.com/thyarles/lhc-go/internal/checks/rootkit"
	_ "github.com/thyarles/lhc-go/internal/checks/services"
	_ "github.com/thyarles/lhc-go/internal/checks/suid"
	_ "github.com/thyarles/lhc-go/internal/checks/system"
	_ "github.com/thyarles/lhc-go/internal/checks/tools"
	_ "github.com/thyarles/lhc-go/internal/checks/updates"
	_ "github.com/thyarles/lhc-go/internal/checks/users"
)
