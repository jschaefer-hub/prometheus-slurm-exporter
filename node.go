/* Copyright 2021 Chris Read

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>. */

package main

import (
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// NodeMetrics stores metrics for each node
type NodeMetrics struct {
	memAlloc        uint64
	memTotal        uint64
	cpuAlloc        uint64
	cpuIdle         uint64
	cpuOther        uint64
	cpuTotal        uint64
	nodeStatus      string
	nodeStateFull   string
	nodeReason      string
	nodeReservation string
}

var (
	nodeNameRegex        = regexp.MustCompile(`NodeName=(\S+)`)
	nodeStateRegex       = regexp.MustCompile(`\bState=(\S+)`)
	nodeReasonRegex      = regexp.MustCompile(`Reason=(.*?)(?:\s*\[|$)`)
	nodeReservationRegex = regexp.MustCompile(`ReservationName=(\S+)`)
	resNameRegex         = regexp.MustCompile(`ReservationName=(\S+)`)
	resNodesRegex        = regexp.MustCompile(`Nodes=(\S+)`)
)

// nodeState holds the state flags and reason parsed from 'scontrol show node'
type nodeState struct {
	status      string
	reason      string
	reservation string
}

func NodeGetMetrics() map[string]*NodeMetrics {
	nodes := ParseNodeMetrics(NodeData())
	states := ParseNodeStates(NodeStateData())
	reservations := ParseNodeReservations(NodeReservationData())

	for name, metrics := range nodes {
		if state, ok := states[name]; ok {
			metrics.nodeStateFull = state.status
			metrics.nodeReason = state.reason
			if state.reservation != "" {
				metrics.nodeReservation = state.reservation
			}
		}
		if res, ok := reservations[name]; ok && res != "" {
			if metrics.nodeReservation == "" {
				metrics.nodeReservation = res
			} else {
				resList := strings.Split(metrics.nodeReservation, ",")
				for _, r := range strings.Split(res, ",") {
					resList = append(resList, r)
				}
				sort.Strings(resList)
				metrics.nodeReservation = strings.Join(RemoveDuplicates(resList), ",")
			}
		}
	}

	return nodes
}

// ExpandHostlist expands a Slurm node range expression like "node[01-03,05],node08"
// into individual node names ["node01", "node02", "node03", "node05", "node08"]
func ExpandHostlist(hostlist string) []string {
	hostlist = strings.TrimSpace(hostlist)
	if hostlist == "" || hostlist == "(null)" || hostlist == "ALL" {
		return nil
	}

	// Split top-level commas (avoiding commas inside brackets)
	var topItems []string
	var current strings.Builder
	bracketDepth := 0

	for _, char := range hostlist {
		switch char {
		case '[':
			bracketDepth++
			current.WriteRune(char)
		case ']':
			bracketDepth--
			current.WriteRune(char)
		case ',':
			if bracketDepth == 0 {
				if current.Len() > 0 {
					topItems = append(topItems, current.String())
					current.Reset()
				}
			} else {
				current.WriteRune(char)
			}
		default:
			current.WriteRune(char)
		}
	}
	if current.Len() > 0 {
		topItems = append(topItems, current.String())
	}

	var result []string
	for _, item := range topItems {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		openIdx := strings.Index(item, "[")
		closeIdx := strings.Index(item, "]")

		if openIdx != -1 && closeIdx != -1 && closeIdx > openIdx {
			prefix := item[:openIdx]
			inside := item[openIdx+1 : closeIdx]
			suffix := item[closeIdx+1:]

			parts := strings.Split(inside, ",")
			for _, part := range parts {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}

				if strings.Contains(part, "-") {
					rangeParts := strings.Split(part, "-")
					if len(rangeParts) == 2 {
						startStr := strings.TrimSpace(rangeParts[0])
						endStr := strings.TrimSpace(rangeParts[1])
						startNum, err1 := strconv.Atoi(startStr)
						endNum, err2 := strconv.Atoi(endStr)

						if err1 == nil && err2 == nil && startNum <= endNum {
							width := len(startStr)
							for i := startNum; i <= endNum; i++ {
								numStr := fmt.Sprintf("%0*d", width, i)
								expandedSuffixes := ExpandHostlist(prefix + numStr + suffix)
								result = append(result, expandedSuffixes...)
							}
							continue
						}
					}
				}

				expandedSuffixes := ExpandHostlist(prefix + part + suffix)
				result = append(result, expandedSuffixes...)
			}
		} else {
			result = append(result, item)
		}
	}

	return result
}

// ParseNodeReservations takes the output of 'scontrol show reservation -o' and returns a
// map of node name to its comma-separated, lowercased/sorted reservation names.
func ParseNodeReservations(input []byte) map[string]string {
	reservations := make(map[string][]string)
	lines := strings.Split(string(input), "\n")

	for _, line := range lines {
		if len(strings.TrimSpace(line)) == 0 {
			continue
		}

		nameMatch := resNameRegex.FindStringSubmatch(line)
		nodesMatch := resNodesRegex.FindStringSubmatch(line)
		if nameMatch == nil || nodesMatch == nil {
			continue
		}

		resName := nameMatch[1]
		nodesStr := nodesMatch[1]
		if resName == "" || resName == "(null)" || nodesStr == "" || nodesStr == "(null)" {
			continue
		}

		nodeList := ExpandHostlist(nodesStr)
		for _, nodeName := range nodeList {
			reservations[nodeName] = append(reservations[nodeName], resName)
		}
	}

	result := make(map[string]string)
	for nodeName, resList := range reservations {
		sort.Strings(resList)
		result[nodeName] = strings.Join(RemoveDuplicates(resList), ",")
	}

	return result
}

// NodeReservationData executes 'scontrol show reservation -o' to get reservation details
func NodeReservationData() []byte {
	cmd := exec.Command("scontrol", "show", "reservation", "-o")
	out, err := cmd.Output()
	if err != nil {
		return []byte{}
	}
	return out
}

// ParseNodeStates takes the output of 'scontrol show node -o' and returns a
// map of node name to its comma-separated, lowercased state flags (e.g.
// "idle,maintenance,powered_down,reserved") along with its reason and reservation, if any.
func ParseNodeStates(input []byte) map[string]nodeState {
	states := make(map[string]nodeState)
	lines := strings.Split(string(input), "\n")

	for _, line := range lines {
		if len(strings.TrimSpace(line)) == 0 {
			continue
		}

		nameMatch := nodeNameRegex.FindStringSubmatch(line)
		if nameMatch == nil {
			continue
		}
		nodeName := nameMatch[1]

		status := ""
		if stateMatch := nodeStateRegex.FindStringSubmatch(line); stateMatch != nil {
			flags := strings.Split(stateMatch[1], "+")
			for i, flag := range flags {
				flags[i] = strings.ToLower(flag)
			}
			status = strings.Join(flags, ",")
		}

		reason := ""
		if reasonMatch := nodeReasonRegex.FindStringSubmatch(line); reasonMatch != nil {
			reason = strings.ToLower(strings.TrimSpace(reasonMatch[1]))
			reason = strings.ReplaceAll(reason, " ", "_")
		}

		reservation := ""
		if resMatch := nodeReservationRegex.FindStringSubmatch(line); resMatch != nil {
			resVal := strings.TrimSpace(resMatch[1])
			if resVal != "(null)" && resVal != "N/A" {
				reservation = resVal
			}
		}

		states[nodeName] = nodeState{status: status, reason: reason, reservation: reservation}
	}

	return states
}

// NodeStateData executes 'scontrol show node' to get the detailed state and
// reason for each node
func NodeStateData() []byte {
	cmd := exec.Command("scontrol", "show", "node", "-o")
	out, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}
	return out
}

// ParseNodeMetrics takes the output of sinfo with node data
// It returns a map of metrics per node
func ParseNodeMetrics(input []byte) map[string]*NodeMetrics {
	nodes := make(map[string]*NodeMetrics)
	lines := strings.Split(string(input), "\n")

	// Sort and remove all the duplicates from the 'sinfo' output
	sort.Strings(lines)
	linesUniq := RemoveDuplicates(lines)

	for _, line := range linesUniq {
		node := strings.Fields(line)
		nodeName := node[0]
		nodeStatus := node[4] // mixed, allocated, etc.

		nodes[nodeName] = &NodeMetrics{0, 0, 0, 0, 0, 0, "", "", "", ""}

		memAlloc, _ := strconv.ParseUint(node[1], 10, 64)
		memTotal, _ := strconv.ParseUint(node[2], 10, 64)

		cpuInfo := strings.Split(node[3], "/")
		cpuAlloc, _ := strconv.ParseUint(cpuInfo[0], 10, 64)
		cpuIdle, _ := strconv.ParseUint(cpuInfo[1], 10, 64)
		cpuOther, _ := strconv.ParseUint(cpuInfo[2], 10, 64)
		cpuTotal, _ := strconv.ParseUint(cpuInfo[3], 10, 64)

		nodes[nodeName].memAlloc = memAlloc
		nodes[nodeName].memTotal = memTotal
		nodes[nodeName].cpuAlloc = cpuAlloc
		nodes[nodeName].cpuIdle = cpuIdle
		nodes[nodeName].cpuOther = cpuOther
		nodes[nodeName].cpuTotal = cpuTotal
		nodes[nodeName].nodeStatus = nodeStatus
	}

	return nodes
}

// NodeData executes the sinfo command to get data for each node
// It returns the output of the sinfo command
func NodeData() []byte {
	cmd := exec.Command("sinfo", "-h", "-N", "-O", "NodeList,AllocMem,Memory,CPUsState,StateLong")
	out, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}
	return out
}

type NodeCollector struct {
	cpuAlloc *prometheus.Desc
	cpuIdle  *prometheus.Desc
	cpuOther *prometheus.Desc
	cpuTotal *prometheus.Desc
	memAlloc *prometheus.Desc
	memTotal *prometheus.Desc
}

// NewNodeCollector creates a Prometheus collector to keep all our stats in
// It returns a set of collections for consumption
func NewNodeCollector() *NodeCollector {
	labels := []string{"node", "status", "state_full", "reason", "reservations"}

	return &NodeCollector{
		cpuAlloc: prometheus.NewDesc("slurm_node_cpu_alloc", "Allocated CPUs per node", labels, nil),
		cpuIdle:  prometheus.NewDesc("slurm_node_cpu_idle", "Idle CPUs per node", labels, nil),
		cpuOther: prometheus.NewDesc("slurm_node_cpu_other", "Other CPUs per node", labels, nil),
		cpuTotal: prometheus.NewDesc("slurm_node_cpu_total", "Total CPUs per node", labels, nil),
		memAlloc: prometheus.NewDesc("slurm_node_mem_alloc", "Allocated memory per node", labels, nil),
		memTotal: prometheus.NewDesc("slurm_node_mem_total", "Total memory per node", labels, nil),
	}
}

// Send all metric descriptions
func (nc *NodeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- nc.cpuAlloc
	ch <- nc.cpuIdle
	ch <- nc.cpuOther
	ch <- nc.cpuTotal
	ch <- nc.memAlloc
	ch <- nc.memTotal
}

func (nc *NodeCollector) Collect(ch chan<- prometheus.Metric) {
	nodes := NodeGetMetrics()
	for node := range nodes {
		ch <- prometheus.MustNewConstMetric(nc.cpuAlloc, prometheus.GaugeValue, float64(nodes[node].cpuAlloc), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason, nodes[node].nodeReservation)
		ch <- prometheus.MustNewConstMetric(nc.cpuIdle, prometheus.GaugeValue, float64(nodes[node].cpuIdle), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason, nodes[node].nodeReservation)
		ch <- prometheus.MustNewConstMetric(nc.cpuOther, prometheus.GaugeValue, float64(nodes[node].cpuOther), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason, nodes[node].nodeReservation)
		ch <- prometheus.MustNewConstMetric(nc.cpuTotal, prometheus.GaugeValue, float64(nodes[node].cpuTotal), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason, nodes[node].nodeReservation)
		ch <- prometheus.MustNewConstMetric(nc.memAlloc, prometheus.GaugeValue, float64(nodes[node].memAlloc), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason, nodes[node].nodeReservation)
		ch <- prometheus.MustNewConstMetric(nc.memTotal, prometheus.GaugeValue, float64(nodes[node].memTotal), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason, nodes[node].nodeReservation)
	}
}
