package kubernetes

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/thyarles/lhc/internal/check"
)

type node struct {
	name       string
	cordoned   bool
	version    string
	conditions map[string]string
	addresses  map[string]bool
}

// nodesJSONPath is passed to kubectl as one argument, with no shell in
// between, so the {"|"} separators need no escaping. (Under a shell, a
// literal {'|'} closed the single quote and turned the separator into a
// pipe, the first time the Python version ran.)
const nodesJSONPath = `{range .items[*]}` +
	`{.metadata.name}{"|"}` +
	`{.spec.unschedulable}{"|"}` +
	`{.status.nodeInfo.kubeletVersion}{"|"}` +
	`{range .status.conditions[*]}{.type}{"="}{.status}{","}{end}{"|"}` +
	`{range .status.addresses[*]}{.address}{","}{end}` +
	`{"\n"}{end}`

var pressureConds = []string{"MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable"}

// getNodes returns the nodes, or the error text when the API is not usable.
// One jsonpath call carries name, cordon, version, every condition and every
// address, so node conditions cost no second query.
//
// jsonpath rather than the table because the ROLES column spelling changed
// twice and STATUS is a comma-joined token. --allow-missing-template-keys
// because .spec.unschedulable is omitempty and absent on every healthy node.
func getNodes(ctx context.Context, k kube) ([]node, string) {
	res := k.run(ctx, "get", "nodes", "-o", "jsonpath="+nodesJSONPath, "--allow-missing-template-keys=true")
	out := combined(res)
	if !res.OK() || out == "" || strings.HasPrefix(strings.ToLower(out), "error") {
		return nil, failure(res, "no output")
	}
	return parseNodes(res.Stdout), ""
}

func parseNodes(out string) []node {
	var nodes []node
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "|") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 5 {
			continue
		}
		n := node{
			name:       parts[0],
			cordoned:   strings.ToLower(strings.TrimSpace(parts[1])) == "true",
			version:    parts[2],
			conditions: map[string]string{},
			addresses:  map[string]bool{},
		}
		for _, c := range strings.Split(parts[3], ",") {
			if k, v, ok := strings.Cut(c, "="); ok {
				n.conditions[k] = v
			}
		}
		for _, a := range strings.Split(parts[4], ",") {
			if a != "" {
				n.addresses[a] = true
			}
		}
		nodes = append(nodes, n)
	}
	return nodes
}

func nodeRows(s *check.Section, env *check.Env, nodes []node, me *node) {
	var prevCordoned []string
	env.State.Load("cordoned", &prevCordoned)
	cordoned, pressures, notReady := []string{}, []string{}, []string{}

	for _, n := range nodes {
		isMe := me != nil && n.name == me.name
		label := n.name
		if isMe {
			label += " (this host)"
		}
		ready, ok := n.conditions["Ready"]
		if !ok {
			ready = "Unknown"
		}

		if ready != "True" {
			// A node that is down is UNHEALTHY for its own report and
			// CAUTION for everyone else's: every node runs this check, and
			// only one of them is the machine the report is about.
			st := check.Caution
			if isMe {
				st = check.Unhealthy
			}
			notReady = append(notReady, n.name)
			s.Add(label, fmt.Sprintf("NotReady (Ready=%s)", ready), st)
			continue
		}

		if n.cordoned {
			cordoned = append(cordoned, n.name)
			// Cordoned two runs running is deliberate maintenance, not
			// news: the same two-sighting rule services use for
			// 'activating'.
			if slices.Contains(prevCordoned, n.name) {
				s.Add(label, "Ready, SchedulingDisabled (cordoned)", check.Info)
			} else {
				s.Add(label, "Ready, SchedulingDisabled (cordoned)", check.Caution,
					check.Detail("cordoned since the last run"))
			}
			continue
		}

		// Pressure reading Unknown is a consequence of a kubelet that
		// stopped reporting, and Ready above already covers that. Only True
		// is an independent finding.
		var hot []string
		for _, c := range pressureConds {
			if n.conditions[c] == "True" {
				hot = append(hot, c)
				pressures = append(pressures, n.name+" "+c)
			}
		}
		if len(hot) > 0 {
			s.Add(label, "Ready · "+strings.Join(hot, ", "), check.Caution)
		} else {
			s.Add(label, fmt.Sprintf("Ready  (%s)", n.version), check.OK)
		}
	}

	save(env, "cordoned", cordoned)

	if len(notReady) > 0 {
		st := check.Caution
		if me != nil && slices.Contains(notReady, me.name) {
			st = check.Unhealthy
		}
		s.Alert(st, "Kubernetes node(s) NotReady: "+strings.Join(firstN(notReady, 3), ", ")+moreSuffix(len(notReady)))
	}
	var newCordons []string
	for _, c := range cordoned {
		if !slices.Contains(prevCordoned, c) {
			newCordons = append(newCordons, c)
		}
	}
	if len(newCordons) > 0 {
		s.Alert(check.Caution, "Kubernetes node(s) newly cordoned: "+strings.Join(firstN(newCordons, 3), ", "))
	}
	if len(pressures) > 0 {
		// DiskPressure stays CAUTION rather than UNHEALTHY: the disk check
		// already owns disk severity for this host, and escalating one fact
		// from two sections is noise.
		s.Alert(check.Caution, "Kubernetes node pressure: "+strings.Join(firstN(pressures, 3), ", ")+moreSuffix(len(pressures)))
	}
}
